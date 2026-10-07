package filter

import (
	"fmt"
	"math"
	"time"
)

// Denoiser geometry. STFT 256-sample sqrt-Hann windows (analysis and
// synthesis) with a 64-sample hop (75% overlap, so the squared window sums to
// a constant). A 512 window would add 32 ms of delay, over the 20 ms budget.
const (
	DenoiseRate = 16000
	dnN         = 256
	dnHop       = 64
	dnBins      = dnN/2 + 1
	// Output sample n is final once input up to n+N-1 has arrived; the fixed
	// delay is N-1 samples = 15.9 ms.
	dnDelay = dnN - 1

	dnSmoothA   = 0.85 // power smoothing for the noise tracker
	dnSubWin    = 75   // frames per minimum-statistics sub-window (0.3 s)
	dnSubCount  = 5    // sub-windows kept (1.5 s total memory)
	dnBias      = 1.7  // minimum-of-smoothed-power underestimates the mean noise
	dnDDBeta    = 0.95 // decision-directed a-priori SNR smoothing
	dnNoiseMin  = 1.0  // power floor so digital silence cannot divide by zero
	dnGainAtt   = 0.4  // gain smoothing when rising (fast: protect onsets)
	dnGainRel   = 0.85 // gain smoothing when falling (slow: no musical noise)
	DefaultDnSt = 0.5
)

// Denoiser is a pure-Go spectral-gating noise suppressor for 16 kHz mono:
// minimum-statistics noise tracking, a decision-directed Wiener gain with
// over-subtraction, a spectral floor, and time/frequency gain smoothing.
//
// It adapts to stationary noise (hiss, hum, fans, road noise) over ~1.5 s. It
// does not remove babble or other speakers: those are not stationary. Latency
// is a fixed 255 samples (15.9 ms). One instance per session, not concurrency
// safe.
//
// ponytail: noise tracker assumes every bin has a pause within 1.5 s; continuous
// speech over that long inflates the estimate (more suppression, not less).
type Denoiser struct {
	over, floor float64

	win    [dnN]float64
	cosT   [dnN / 2]float64
	sinT   [dnN / 2]float64
	rev    [dnN]int
	in     [dnN]float64 // last N input samples (ring)
	inPos  int
	count  int // samples since last hop
	ola    [dnN]float64
	pend   []float64 // output FIFO (prefilled with the constant delay)
	re, im [dnN]float64

	frames  int
	sm      [dnBins]float64 // smoothed power
	curMin  [dnBins]float64
	sub     [dnSubCount][dnBins]float64
	subN    int // frames in current sub-window
	subIdx  int
	subFull int
	a2      [dnBins]float64 // previous clean-power estimate
	gPrev   [dnBins]float64
	g       [dnBins]float64
}

// NewDenoiser returns a denoiser. strength is 0..1: 0 is a gentle floor of
// -4 dB, 1 is aggressive (-24 dB floor, 3x over-subtraction). 0.5 is the
// conservative default and is what the server uses.
func NewDenoiser(strength float64) (*Denoiser, error) {
	if math.IsNaN(strength) || strength < 0 || strength > 1 {
		return nil, fmt.Errorf("denoise strength %v out of range 0..1", strength)
	}
	d := &Denoiser{
		over:  1 + 2*strength,
		floor: math.Pow(10, -(4+20*strength)/20),
	}
	for i := range d.win { // periodic sqrt-Hann
		d.win[i] = math.Sqrt(0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/dnN))
	}
	for i := range d.cosT {
		a := -2 * math.Pi * float64(i) / dnN
		d.cosT[i], d.sinT[i] = math.Cos(a), math.Sin(a)
	}
	for i := range d.rev {
		r, x := 0, i
		for b := 0; b < 8; b++ {
			r = r<<1 | x&1
			x >>= 1
		}
		d.rev[i] = r
	}
	d.Reset()
	return d, nil
}

func (d *Denoiser) Name() string         { return "denoise" }
func (d *Denoiser) Delay() time.Duration { return dnDelay * time.Second / DenoiseRate }
func (d *Denoiser) Close() error         { return nil }

func (d *Denoiser) Reset() {
	d.in = [dnN]float64{}
	d.ola = [dnN]float64{}
	d.inPos, d.count, d.frames = 0, 0, 0
	d.subN, d.subIdx, d.subFull = 0, 0, 0
	d.pend = append(d.pend[:0], make([]float64, dnHop-1)...)
	for k := range d.gPrev {
		d.gPrev[k], d.a2[k] = 1, 0
	}
}

// Process returns the denoised input delayed by Delay(). Any chunking of the
// same stream yields bit-identical output.
func (d *Denoiser) Process(pcm []int16, rate int) ([]int16, error) {
	if rate != DenoiseRate {
		return nil, fmt.Errorf("%w: denoise needs %d Hz, got %d", ErrUnsupportedRate, DenoiseRate, rate)
	}
	for _, s := range pcm {
		d.in[d.inPos] = float64(s)
		d.inPos = (d.inPos + 1) % dnN
		if d.count++; d.count == dnHop {
			d.count = 0
			d.frame()
		}
	}
	out := make([]int16, len(pcm))
	for i := range out {
		v := math.Round(d.pend[i])
		out[i] = int16(max(-32768, min(32767, v)))
	}
	d.pend = append(d.pend[:0], d.pend[len(pcm):]...)
	return out, nil
}

func (d *Denoiser) frame() {
	for k := 0; k < dnN; k++ {
		d.re[k] = d.win[k] * d.in[(d.inPos+k)%dnN] // inPos is the oldest sample
		d.im[k] = 0
	}
	d.fft(false)

	d.frames++
	for k := 0; k < dnBins; k++ {
		p := d.re[k]*d.re[k] + d.im[k]*d.im[k]
		if d.frames == 1 {
			d.sm[k] = p
		} else {
			d.sm[k] = dnSmoothA*d.sm[k] + (1-dnSmoothA)*p
		}
		if d.subN == 0 {
			d.curMin[k] = d.sm[k]
		} else if d.sm[k] < d.curMin[k] {
			d.curMin[k] = d.sm[k]
		}
	}
	d.subN++

	for k := 0; k < dnBins; k++ {
		nm := d.curMin[k]
		for i := 0; i < d.subFull; i++ {
			nm = min(nm, d.sub[i][k])
		}
		noise := max(nm*dnBias, dnNoiseMin)
		p := d.re[k]*d.re[k] + d.im[k]*d.im[k]
		gamma := p / noise
		xi := dnDDBeta*d.a2[k]/noise + (1-dnDDBeta)*max(gamma-1, 0)
		xi = max(xi, 0.003)
		d.g[k] = max(xi/(xi+d.over), d.floor)
	}
	// Smooth gains across frequency, then in time (fast up, slow down).
	for k := 0; k < dnBins; k++ {
		lo, hi := max(k-1, 0), min(k+1, dnBins-1)
		g := 0.25*d.g[lo] + 0.5*d.g[k] + 0.25*d.g[hi]
		a := dnGainRel
		if g > d.gPrev[k] {
			a = dnGainAtt
		}
		d.gPrev[k] = a*d.gPrev[k] + (1-a)*g // d.g[k] still needed by neighbours: write gPrev only
	}
	for k := 0; k < dnBins; k++ {
		g := d.gPrev[k]
		p := d.re[k]*d.re[k] + d.im[k]*d.im[k]
		d.a2[k] = g * g * p
		d.re[k] *= g
		d.im[k] *= g
		if k > 0 && k < dnN/2 {
			d.re[dnN-k], d.im[dnN-k] = d.re[k], -d.im[k]
		}
	}
	if d.subN == dnSubWin { // roll the sub-window
		d.sub[d.subIdx] = d.curMin
		d.subIdx = (d.subIdx + 1) % dnSubCount
		d.subFull = min(d.subFull+1, dnSubCount)
		d.subN = 0
	}

	d.fft(true)
	for k := 0; k < dnN; k++ {
		d.ola[k] += d.win[k] * d.re[k] / (dnN * 2) // sum of w^2 at 75% overlap is 2
	}
	d.pend = append(d.pend, d.ola[:dnHop]...)
	copy(d.ola[:], d.ola[dnHop:])
	for k := dnN - dnHop; k < dnN; k++ {
		d.ola[k] = 0
	}
}

// fft is an in-place radix-2 transform of re/im. inverse flips the sign and
// leaves the 1/N scaling to the caller.
func (d *Denoiser) fft(inverse bool) {
	for i := 0; i < dnN; i++ {
		if j := d.rev[i]; j > i {
			d.re[i], d.re[j] = d.re[j], d.re[i]
			d.im[i], d.im[j] = d.im[j], d.im[i]
		}
	}
	for size := 2; size <= dnN; size <<= 1 {
		half, step := size/2, dnN/size
		for start := 0; start < dnN; start += size {
			for j := 0; j < half; j++ {
				wr, wi := d.cosT[j*step], d.sinT[j*step]
				if inverse {
					wi = -wi
				}
				a, b := start+j, start+j+half
				tr := d.re[b]*wr - d.im[b]*wi
				ti := d.re[b]*wi + d.im[b]*wr
				d.re[b], d.im[b] = d.re[a]-tr, d.im[a]-ti
				d.re[a], d.im[a] = d.re[a]+tr, d.im[a]+ti
			}
		}
	}
}
