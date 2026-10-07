// Package smartturn runs the Pipecat Smart Turn v3 end-of-turn model: Whisper
// log-mel features (this file), ONNX inference in a supervised child process
// (worker.go), and the async, cached turn analyzer (analyzer.go).
package smartturn

import (
	"math"
	"runtime"
	"slices"
	"sync"
)

// Whisper feature constants, as in pipecat's _whisper_features.py.
const (
	SampleRate  = 16000
	WindowSecs  = 8
	WindowLen   = SampleRate * WindowSecs // 128000
	nFFT        = 400
	hop         = 160
	nMels       = 80
	nBins       = nFFT/2 + 1
	NumFrames   = WindowLen / hop // 800 (the 801st frame is dropped)
	melFloor    = 1e-10
	normVarEps  = 1e-7
	FeatureSize = nMels * NumFrames
)

type melRow struct {
	lo int
	w  []float64
}

var (
	featOnce sync.Once
	hann     [nFFT]float64
	melBank  [nMels]melRow
	plan     fftPlan
)

func hzToMel(f float64) float64 {
	if f >= 1000 {
		return 15 + math.Log(f/1000)*27/math.Log(6.4)
	}
	return 3 * f / 200
}

func melToHz(m float64) float64 {
	if m >= 15 {
		return 1000 * math.Exp(math.Log(6.4)/27*(m-15))
	}
	return 200 * m / 3
}

func initFeatures() {
	for i := range hann {
		hann[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/nFFT)
	}
	mMin, mMax := hzToMel(0), hzToMel(SampleRate/2)
	var fr [nMels + 2]float64
	for i := range fr {
		fr[i] = melToHz(mMin + (mMax-mMin)*float64(i)/(nMels+1))
	}
	for m := 0; m < nMels; m++ {
		enorm := 2 / (fr[m+2] - fr[m])
		row := make([]float64, nBins)
		lo, hi := -1, -1
		for b := 0; b < nBins; b++ {
			f := float64(b) * (SampleRate / 2) / float64(nBins-1)
			v := math.Min(-(fr[m]-f)/(fr[m+1]-fr[m]), (fr[m+2]-f)/(fr[m+2]-fr[m+1]))
			if v > 0 {
				row[b] = v * enorm
				if lo < 0 {
					lo = b
				}
				hi = b
			}
		}
		if lo >= 0 {
			melBank[m] = melRow{lo, row[lo : hi+1]}
		}
	}
	plan = newFFTPlan(nFFT)
}

// Features converts 16 kHz mono PCM to the model's (80, 800) log-mel input,
// row-major by mel. Audio is cut to its last 8 s or front-padded with zeros
// (pipecat's truncate_audio_to_last_n_seconds), normalized to zero mean and
// unit variance, then run through Whisper's STFT/mel/log pipeline.
func Features(pcm []int16) []float32 {
	featOnce.Do(initFeatures)
	if len(pcm) > WindowLen {
		pcm = pcm[len(pcm)-WindowLen:]
	}
	x := make([]float64, WindowLen)
	off := WindowLen - len(pcm)
	var sum float64
	for i, s := range pcm {
		v := float64(float32(s) / 32768)
		x[off+i] = v
		sum += v
	}
	// Normalize in float32 like numpy does on the float32 buffer
	// ((x - mean) / sqrt(var + eps), each op rounded to float32): with a DC
	// offset and a quiet signal the rounding of x-mean is what fills the
	// low-energy mel bins, so float64 here drifts ~2e-5 from the reference.
	mean := float32(sum / WindowLen)
	var vr float64
	for _, v := range x {
		d := float64(float32(v) - mean)
		vr += d * d
	}
	std := float32(math.Sqrt(float64(float32(vr/WindowLen) + normVarEps)))
	for i, v := range x {
		x[i] = float64((float32(v) - mean) / std)
	}

	// Centered STFT with reflect padding; frame t covers padded[t*hop : t*hop+nFFT].
	pad := nFFT / 2
	at := func(i int) float64 { // padded[i]
		i -= pad
		if i < 0 {
			i = -i
		} else if i >= WindowLen {
			i = 2*(WindowLen-1) - i
		}
		return x[i]
	}
	logs := make([]float64, FeatureSize)
	// Frames are independent: split them over a few goroutines (the plan and
	// filter bank are read-only).
	par := min(4, runtime.NumCPU())
	maxs := make([]float64, par)
	var wg sync.WaitGroup
	for g := 0; g < par; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]complex128, nFFT)
			out := make([]complex128, nFFT)
			power := make([]float64, nBins)
			maxv := math.Inf(-1)
			for t := g * NumFrames / par; t < (g+1)*NumFrames/par; t++ {
				for i := 0; i < nFFT; i++ {
					buf[i] = complex(at(t*hop+i)*hann[i], 0)
				}
				plan.transform(buf, out)
				for b := 0; b < nBins; b++ {
					power[b] = real(out[b])*real(out[b]) + imag(out[b])*imag(out[b])
				}
				for m := 0; m < nMels; m++ {
					var acc float64
					for j, w := range melBank[m].w {
						acc += w * power[melBank[m].lo+j]
					}
					v := math.Log10(math.Max(melFloor, acc))
					logs[m*NumFrames+t] = v
					maxv = math.Max(maxv, v)
				}
			}
			maxs[g] = maxv
		}()
	}
	wg.Wait()
	maxv := slices.Max(maxs)
	res := make([]float32, FeatureSize)
	for i, v := range logs {
		res[i] = float32((math.Max(v, maxv-8) + 4) / 4)
	}
	return res
}

// fftPlan is a mixed-radix decimation-in-time complex FFT for sizes whose
// prime factors are small (400 = 2^4 * 5^2). ponytail: O(n * sum(factors));
// fine for n=400, not for large prime sizes.
type fftPlan struct {
	n       int
	factors []int
	tw      []complex128
}

func newFFTPlan(n int) fftPlan {
	p := fftPlan{n: n, tw: make([]complex128, n)}
	for r, m := 2, n; m > 1; {
		if m%r == 0 {
			p.factors = append(p.factors, r)
			m /= r
		} else {
			r++
		}
	}
	for k := range p.tw {
		s, c := math.Sincos(-2 * math.Pi * float64(k) / float64(n))
		p.tw[k] = complex(c, s)
	}
	return p
}

func (p *fftPlan) transform(in, out []complex128) { p.rec(out, in, p.n, 1, 0) }

func (p *fftPlan) rec(out, in []complex128, n, stride, fi int) {
	if n == 1 {
		out[0] = in[0]
		return
	}
	r := p.factors[fi]
	m := n / r
	for q := 0; q < r; q++ {
		p.rec(out[q*m:(q+1)*m], in[q*stride:], m, stride*r, fi+1)
	}
	var tmp [8]complex128
	for k := 0; k < m; k++ {
		for q := 0; q < r; q++ {
			tmp[q] = out[q*m+k]
		}
		for s := 0; s < r; s++ {
			idx := k + s*m
			sum := tmp[0]
			for q := 1; q < r; q++ {
				sum += tmp[q] * p.tw[(q*idx*stride)%p.n]
			}
			out[idx] = sum
		}
	}
}
