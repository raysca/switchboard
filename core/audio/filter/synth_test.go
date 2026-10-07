package filter

import (
	"math"
	"math/rand"
)

const sr = 16000

// speechLike: harmonic series (gliding f0) shaped by three formants, with 4 Hz
// syllabic amplitude modulation and a pause every ~1.2 s. Returns int16 at
// about -20 dBFS RMS while voiced.
func speechLike(seed int64, secs float64) []float64 {
	rng := rand.New(rand.NewSource(seed))
	n := int(secs * sr)
	x := make([]float64, n)
	f0base := 100 + rng.Float64()*100
	phase := 0.0
	forms := [][3]float64{{700, 1200, 2600}, {300, 2200, 3000}, {500, 1500, 2500}, {350, 800, 2400}}
	for i := range x {
		t := float64(i) / sr
		f0 := f0base * (1 + 0.15*math.Sin(2*math.Pi*0.7*t))
		phase += 2 * math.Pi * f0 / sr
		syl := int(t*4) % len(forms)
		fm := forms[syl]
		var s float64
		for h := 1; float64(h)*f0 < 4000; h++ {
			f := float64(h) * f0
			env := 0.0
			for _, c := range fm {
				env += 1 / (1 + math.Pow((f-c)/120, 2))
			}
			s += env / float64(h) * math.Sin(float64(h)*phase)
		}
		am := math.Max(0, math.Sin(2*math.Pi*2*t+0.4)) // 2 Hz bursts with gaps
		am = math.Pow(am, 0.5)
		x[i] = s * am
	}
	scaleRMS(x, 2000)
	return x
}

func scaleRMS(x []float64, target float64) {
	var e float64
	for _, v := range x {
		e += v * v
	}
	r := math.Sqrt(e / float64(len(x)))
	if r == 0 {
		return
	}
	for i := range x {
		x[i] *= target / r
	}
}

func whiteNoise(seed int64, n int) []float64 {
	rng := rand.New(rand.NewSource(seed))
	x := make([]float64, n)
	for i := range x {
		x[i] = rng.NormFloat64()
	}
	return x
}

// pinkNoise uses Paul Kellet's economy filter.
func pinkNoise(seed int64, n int) []float64 {
	w := whiteNoise(seed, n)
	var b0, b1, b2 float64
	x := make([]float64, n)
	for i, v := range w {
		b0 = 0.99765*b0 + v*0.0990460
		b1 = 0.96300*b1 + v*0.2965164
		b2 = 0.57000*b2 + v*1.0526913
		x[i] = b0 + b1 + b2 + v*0.1848
	}
	return x
}

func hum(hz float64, n int) []float64 {
	x := make([]float64, n)
	for i := range x {
		t := float64(i) / sr
		for h := 1; h <= 4; h++ {
			x[i] += math.Sin(2*math.Pi*hz*float64(h)*t) / float64(h)
		}
	}
	return x
}

// babble sums several speech-like talkers with different pitches.
func babble(n int) []float64 {
	x := make([]float64, n)
	for s := int64(0); s < 5; s++ {
		for i, v := range speechLike(100+s, float64(n)/sr) {
			x[i] += v
		}
	}
	return x
}

func power(x []float64) float64 {
	var e float64
	for _, v := range x {
		e += v * v
	}
	return e / float64(len(x))
}

// mix scales noise to the requested SNR (dB) against clean and returns int16.
func mix(clean, noise []float64, snrDB float64) []int16 {
	g := math.Sqrt(power(clean) / (power(noise) * math.Pow(10, snrDB/10)))
	out := make([]int16, len(clean))
	for i := range out {
		out[i] = clip(clean[i] + g*noise[i])
	}
	return out
}

func clip(v float64) int16 { return int16(math.Max(-32768, math.Min(32767, math.Round(v)))) }

func toInt16(x []float64) []int16 {
	o := make([]int16, len(x))
	for i, v := range x {
		o[i] = clip(v)
	}
	return o
}

// sdr is 10log10(|clean|^2 / |out-clean|^2) over [from:], with out delayed by
// delay samples (the filter's algorithmic latency).
func sdr(clean []float64, out []int16, delay, from int) float64 {
	var s, e float64
	for i := from; i+delay < len(out) && i < len(clean); i++ {
		d := float64(out[i+delay]) - clean[i]
		s += clean[i] * clean[i]
		e += d * d
	}
	return 10 * math.Log10(s/e)
}

func snr(clean []float64, noisy []int16, from int) float64 { return sdr(clean, noisy, 0, from) }
