package dtmf

import (
	"encoding/binary"
	"errors"
	"math"
	"time"
)

// DetectorConfig holds the detection thresholds. Defaults (DefaultDetectorConfig):
//
//   - Window 25 ms Hann-weighted (400 samples at 16 kHz, 40 Hz bin width), hop
//     10 ms. Hann keeps the 73 Hz-apart 697/770 Hz tones about 35 dB apart.
//   - MinRMS 150 (about -47 dBFS): quieter windows are silence.
//   - MinPurity 0.75: the two chosen tones must carry at least 75% of the
//     window's energy. Speech, music and noise spread energy over many bins and
//     fail this; clean tones at 20 dB SNR score about 0.99.
//   - Twist: high-group power relative to low-group power must lie within
//     [-MaxForwardTwistDB, +MaxReverseTwistDB] = [-8, +4] dB (Bellcore GR-506).
//   - SecondPeakDB 10: within each group the winner must exceed the runner-up
//     by 10 dB, which rejects off-frequency tones and dense harmonics.
//   - MinWindows 3: the same digit must pass in 3 consecutive hops. Measured
//     over every sub-hop alignment, every 40 ms tone is accepted and no tone of
//     25 ms or less is, matching ITU-T Q.24 / Bellcore (accept >= 40 ms,
//     reject <= 23 ms). 4 hops missed most 40 ms tones.
//   - ReleaseWindows 2: a digit re-arms only after 2 consecutive windows that do
//     not show it (about 20 ms of gap), so one held key yields one digit while
//     the 40 ms minimum inter-digit pause of the standard is still seen.
type DetectorConfig struct {
	SampleRate        int
	Window, Hop       time.Duration
	MinRMS            float64
	MinPurity         float64
	MaxForwardTwistDB float64
	MaxReverseTwistDB float64
	SecondPeakDB      float64
	MinWindows        int
	ReleaseWindows    int
}

func DefaultDetectorConfig() DetectorConfig {
	return DetectorConfig{
		SampleRate: 16000, Window: 25 * time.Millisecond, Hop: 10 * time.Millisecond,
		MinRMS: 150, MinPurity: 0.75, MaxForwardTwistDB: 8, MaxReverseTwistDB: 4,
		SecondPeakDB: 10, MinWindows: 3, ReleaseWindows: 2,
	}
}

// Event is one detected key press, reported when the tone is confirmed.
type Event struct {
	Digit byte
	// At is the estimated onset, as stream time since the first sample.
	At time.Duration
}

// Detector finds DTMF key presses in a stream of PCM16LE mono samples. It is
// not safe for concurrent use.
type Detector struct {
	cfg      DetectorConfig
	n, hop   int
	win      []float64 // Hann window
	coeff    [8]float64
	norm     float64 // |X|^2 of a unit-purity tone = windowedEnergy * norm
	ring     []float64
	filled   int // samples currently in ring (<= n)
	sinceHop int
	total    int64 // samples consumed

	cand      int // candidate digit index+1 (0 none)
	candRun   int
	candStart int64
	active    int // digit currently held (confirmed), 0 none
	misses    int
}

func NewDetector(cfg DetectorConfig) (*Detector, error) {
	if cfg.SampleRate < 8000 || cfg.Window <= 0 || cfg.Hop <= 0 || cfg.Hop > cfg.Window ||
		cfg.MinWindows < 1 || cfg.ReleaseWindows < 1 || cfg.MinPurity <= 0 || cfg.MinPurity > 1 {
		return nil, errors.New("dtmf: invalid detector config")
	}
	d := &Detector{cfg: cfg}
	d.n = int(float64(cfg.SampleRate) * cfg.Window.Seconds())
	d.hop = int(float64(cfg.SampleRate) * cfg.Hop.Seconds())
	if d.hop < 1 || d.n < 64 {
		return nil, errors.New("dtmf: detector window too small")
	}
	d.win = make([]float64, d.n)
	var sw, sw2 float64
	for i := range d.win {
		d.win[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(d.n))
		sw += d.win[i]
		sw2 += d.win[i] * d.win[i]
	}
	d.norm = sw * sw / (2 * sw2) // unit-purity normaliser, see window()
	for i, f := range append(lowFreqs[:], highFreqs[:]...) {
		d.coeff[i] = 2 * math.Cos(2*math.Pi*f/float64(cfg.SampleRate))
	}
	d.ring = make([]float64, d.n)
	return d, nil
}

// Reset forgets all state, as at the start of a new stream.
func (d *Detector) Reset() {
	d.filled, d.sinceHop, d.total = 0, 0, 0
	d.cand, d.candRun, d.active, d.misses = 0, 0, 0, 0
}

// Write consumes PCM16LE samples (any chunking) and returns the key presses
// confirmed by them.
func (d *Detector) Write(pcm []byte) []Event {
	var out []Event
	for i := 0; i+1 < len(pcm); i += 2 {
		s := float64(int16(binary.LittleEndian.Uint16(pcm[i:])))
		if d.filled < d.n {
			d.ring[d.filled] = s
			d.filled++
		} else {
			copy(d.ring, d.ring[1:]) // ponytail: O(n) shift per sample; n=400 is cheap, use a ring index if profiling says otherwise
			d.ring[d.n-1] = s
		}
		d.total++
		d.sinceHop++
		if d.filled == d.n && d.sinceHop >= d.hop {
			d.sinceHop = 0
			if ev, ok := d.step(d.classify()); ok {
				out = append(out, ev)
			}
		}
	}
	return out
}

// classify returns the digit index+1 present in the current window, or 0.
func (d *Detector) classify() int {
	var energy float64
	var s1, s2 [8]float64
	for i, x := range d.ring {
		y := x * d.win[i]
		energy += y * y
		for k := 0; k < 8; k++ {
			s0 := y + d.coeff[k]*s1[k] - s2[k]
			s2[k], s1[k] = s1[k], s0
		}
	}
	if energy/float64(d.n) < d.cfg.MinRMS*d.cfg.MinRMS*d.winMeanSq() {
		return 0
	}
	var p [8]float64
	for k := 0; k < 8; k++ {
		p[k] = (s1[k]*s1[k] + s2[k]*s2[k] - d.coeff[k]*s1[k]*s2[k]) / (energy * d.norm)
	}
	lo, loSecond := peak(p[:4])
	hi, hiSecond := peak(p[4:])
	if p[lo]+p[4+hi] < d.cfg.MinPurity {
		return 0
	}
	ratio := math.Pow(10, d.cfg.SecondPeakDB/10)
	if p[lo] < ratio*loSecond || p[4+hi] < ratio*hiSecond {
		return 0
	}
	twist := 10 * math.Log10(p[4+hi]/p[lo])
	if twist < -d.cfg.MaxForwardTwistDB || twist > d.cfg.MaxReverseTwistDB {
		return 0
	}
	return lo*4 + hi + 1
}

// winMeanSq is mean(w^2), which scales a raw RMS threshold to windowed energy.
func (d *Detector) winMeanSq() float64 { return 3.0 / 8.0 }

func peak(p []float64) (idx int, second float64) {
	for i := range p {
		if p[i] > p[idx] {
			idx = i
		}
	}
	for i := range p {
		if i != idx && p[i] > second {
			second = p[i]
		}
	}
	return idx, second
}

func (d *Detector) step(c int) (Event, bool) {
	if c != 0 && c == d.active {
		d.misses = 0
		return Event{}, false
	}
	if d.active != 0 {
		if d.misses++; d.misses < d.cfg.ReleaseWindows {
			return Event{}, false
		}
		d.active, d.misses = 0, 0
	}
	if c == 0 || c != d.cand {
		d.cand, d.candRun = c, 0
		d.candStart = d.total - int64(d.n) // window start approximates onset
	}
	if c == 0 {
		return Event{}, false
	}
	if d.candRun++; d.candRun < d.cfg.MinWindows {
		return Event{}, false
	}
	d.active, d.misses, d.cand, d.candRun = c, 0, 0, 0
	r, col := (c-1)/4, (c-1)%4
	return Event{Digit: keypad[r][col], At: time.Duration(d.candStart) * time.Second / time.Duration(d.cfg.SampleRate)}, true
}
