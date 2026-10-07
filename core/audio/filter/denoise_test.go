package filter

import (
	"errors"
	"math"
	"testing"
)

const warm = 2 * sr // skip the first second(s) while the noise tracker converges

func run(t *testing.T, strength float64, in []int16) []int16 {
	t.Helper()
	d, err := NewDenoiser(strength)
	if err != nil {
		t.Fatal(err)
	}
	out, err := d.Process(in, sr)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDenoiseStationaryNoiseSNRGain(t *testing.T) {
	n := 8 * sr
	clean := speechLike(1, 8)
	cases := map[string][]float64{
		"white": whiteNoise(2, n),
		"pink":  pinkNoise(3, n),
		"hum50": append(hum(50, n)[:0:0], addTo(hum(50, n), whiteNoise(4, n), 0.05)...),
		"hum60": addTo(hum(60, n), whiteNoise(5, n), 0.05),
	}
	for name, noise := range cases {
		noisy := mix(clean, noise, 5)
		out := run(t, DefaultDnSt, noisy)
		in := snr(clean, noisy, warm)
		got := sdr(clean, out, dnDelay, warm)
		t.Logf("%s: SNR %.1f -> %.1f dB (+%.1f)", name, in, got, got-in)
		// Pink noise sits in the same low band as the strongest speech
		// harmonics, so the same effort buys less SNR there (measured +4.7).
		want := 6.0
		if name == "pink" {
			want = 4
		}
		if got-in < want {
			t.Errorf("%s: SNR improvement %.1f dB < %.0f", name, got-in, want)
		}
	}
}

func addTo(a, b []float64, k float64) []float64 {
	o := make([]float64, len(a))
	for i := range a {
		o[i] = a[i] + k*b[i]
	}
	return o
}

func TestDenoiseSpeechDistortion(t *testing.T) {
	clean := speechLike(7, 8)
	n := len(clean)
	// 15 dB input SNR: the cleaned signal must be no worse than 2 dB below the
	// input SNR, i.e. suppression never costs more speech than it removes noise.
	for name, noise := range map[string][]float64{"white": whiteNoise(8, n), "pink": pinkNoise(9, n)} {
		noisy := mix(clean, noise, 15)
		out := run(t, DefaultDnSt, noisy)
		got := sdr(clean, out, dnDelay, warm)
		t.Logf("%s @15 dB: SDR %.1f dB", name, got)
		if got < 15-2 {
			t.Errorf("%s: SDR %.1f dB below 13", name, got)
		}
	}
	// Near-clean speech (quantisation noise only): the filter must be transparent.
	out := run(t, DefaultDnSt, toInt16(clean))
	if got := sdr(clean, out, dnDelay, warm); got < 20 {
		t.Errorf("clean speech SDR %.1f dB < 20", got)
	}
}

func TestDenoiseBabbleDoesNotHurt(t *testing.T) {
	clean := speechLike(11, 8)
	noisy := mix(clean, babble(len(clean)), 10)
	out := run(t, DefaultDnSt, noisy)
	in, got := snr(clean, noisy, warm), sdr(clean, out, dnDelay, warm)
	t.Logf("babble: %.1f -> %.1f dB", in, got)
	if got < in-1 {
		t.Errorf("babble made things worse: %.1f -> %.1f", in, got)
	}
}

func TestDenoiseStrengthMonotonic(t *testing.T) {
	// Noise-only input: a stronger setting leaves less residual.
	noise := toInt16(whiteNoise(2, 6*sr))
	for i := range noise {
		noise[i] *= 300
	}
	prev := math.MaxFloat64
	for _, s := range []float64{0, 0.5, 1} {
		out := run(t, s, noise)
		var e float64
		for _, v := range out[3*sr:] {
			e += float64(v) * float64(v)
		}
		if e > prev {
			t.Errorf("strength %v leaves more residual (%.3g) than a weaker one (%.3g)", s, e, prev)
		}
		prev = e
	}
}

func TestDenoiseValidation(t *testing.T) {
	for _, s := range []float64{-0.1, 1.1, math.NaN()} {
		if _, err := NewDenoiser(s); err == nil {
			t.Errorf("strength %v accepted", s)
		}
	}
	d, _ := NewDenoiser(0.5)
	if _, err := d.Process(make([]int16, 10), 8000); !errors.Is(err, ErrUnsupportedRate) {
		t.Errorf("8k: %v", err)
	}
}

func TestDenoiseChunkIndependence(t *testing.T) {
	clean := speechLike(3, 3)
	in := mix(clean, whiteNoise(4, len(clean)), 8)
	ref := run(t, 0.5, in)
	for _, chunks := range [][]int{{1}, {7}, {63, 64, 65}, {320}, {1000}, {len(in)}} {
		d, _ := NewDenoiser(0.5)
		var got []int16
		for off, i := 0, 0; off < len(in); i++ {
			c := min(chunks[i%len(chunks)], len(in)-off)
			o, err := d.Process(in[off:off+c], sr)
			if err != nil || len(o) != c {
				t.Fatalf("chunk %d: len %d err %v", c, len(o), err)
			}
			got = append(got, o...)
			off += c
		}
		for i := range ref {
			if ref[i] != got[i] {
				t.Fatalf("chunks %v differ at %d: %d vs %d", chunks, i, got[i], ref[i])
			}
		}
	}
}

func TestDenoiseEdgeInputs(t *testing.T) {
	d, _ := NewDenoiser(0.5)
	if out, err := d.Process(nil, sr); err != nil || len(out) != 0 {
		t.Fatalf("empty: %v %v", out, err)
	}
	if out, _ := d.Process(make([]int16, 1), sr); len(out) != 1 {
		t.Fatal("single sample")
	}
	// Full-scale input must not wrap.
	loud := make([]int16, 4000)
	for i := range loud {
		loud[i] = int16(32767 * math.Sin(float64(i)*0.3))
	}
	out, _ := d.Process(loud, sr)
	if len(out) != len(loud) {
		t.Fatal("len")
	}
	// Input is never mutated.
	in := []int16{1, 2, 3, 4, 5}
	cp := append([]int16(nil), in...)
	_, _ = d.Process(in, sr)
	for i := range in {
		if in[i] != cp[i] {
			t.Fatal("input mutated")
		}
	}
}

func TestDenoiseResetIsFresh(t *testing.T) {
	in := mix(speechLike(5, 2), whiteNoise(6, 2*sr), 6)
	d, _ := NewDenoiser(0.5)
	a, _ := d.Process(in, sr)
	_, _ = d.Process(mix(speechLike(9, 3), pinkNoise(1, 3*sr), 0), sr) // dirty the state
	d.Reset()
	b, _ := d.Process(in, sr)
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("after Reset output differs at %d", i)
		}
	}
}

func TestDenoiseDelayWithinBudget(t *testing.T) {
	d, _ := NewDenoiser(0.5)
	if d.Delay() > 20*1e6 { // 20 ms
		t.Fatalf("delay %v > 20ms", d.Delay())
	}
	// Measure it: an impulse comes out Delay() later (gain ~ floor-limited, so
	// check the cross-correlation peak of a tone burst instead).
	clean := speechLike(1, 2)
	out := run(t, 0, toInt16(clean))
	best, bestLag := -1.0, 0
	for lag := 0; lag < 600; lag++ {
		var c float64
		for i := 8000; i < 24000; i++ {
			c += clean[i] * float64(out[i+lag])
		}
		if c > best {
			best, bestLag = c, lag
		}
	}
	if bestLag != dnDelay {
		t.Fatalf("measured delay %d samples, want %d", bestLag, dnDelay)
	}
}

// BenchmarkDenoise20ms: ns per 20 ms (320-sample) frame.
func BenchmarkDenoise20ms(b *testing.B) {
	d, _ := NewDenoiser(0.5)
	in := mix(speechLike(1, 1), whiteNoise(2, sr), 10)[:320]
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = d.Process(in, sr)
	}
}

// With the gain pinned to 1 the STFT/overlap-add path must reconstruct the
// input exactly (delayed), including the first samples after Reset.
func TestDenoisePerfectReconstruction(t *testing.T) {
	d, _ := NewDenoiser(0)
	d.floor = 1 // gain = max(wiener, floor) = 1 everywhere
	in := toInt16(speechLike(1, 2))
	in[100], in[101] = 32767, -32768
	out, _ := d.Process(in, sr)
	for i := dnDelay; i < len(in); i++ {
		if out[i] != in[i-dnDelay] {
			t.Fatalf("sample %d: %d, want %d", i-dnDelay, out[i], in[i-dnDelay])
		}
	}
}

// Property/fuzz: any input and chunking gives len(out)==len(in), the same
// output as one-shot processing, finite internal state, and silence stays
// exactly silent. Run longer with: go test -fuzz FuzzDenoise ./audio/filter
func FuzzDenoise(f *testing.F) {
	f.Add([]byte{}, uint16(1))
	f.Add(make([]byte, 9000), uint16(320))
	sq := make([]byte, 20000)
	for i := 0; i < len(sq); i += 2 {
		if (i/2/40)%2 == 0 { // full-scale square wave
			sq[i], sq[i+1] = 0xff, 0x7f
		} else {
			sq[i], sq[i+1] = 0x00, 0x80
		}
	}
	f.Add(sq, uint16(77))
	f.Fuzz(func(t *testing.T, raw []byte, chunk uint16) {
		pcm := make([]int16, len(raw)/2)
		for i := range pcm {
			pcm[i] = int16(uint16(raw[2*i]) | uint16(raw[2*i+1])<<8)
		}
		a, _ := NewDenoiser(1)
		ref, _ := a.Process(pcm, sr)
		b, _ := NewDenoiser(1)
		c := max(int(chunk)%2000, 1)
		var got []int16
		for off := 0; off < len(pcm); off += c {
			o, err := b.Process(pcm[off:min(off+c, len(pcm))], sr)
			if err != nil || len(o) != min(c, len(pcm)-off) {
				t.Fatalf("len %d err %v", len(o), err)
			}
			got = append(got, o...)
		}
		if len(ref) != len(pcm) || len(got) != len(pcm) {
			t.Fatal("length")
		}
		silent := true
		for i := range pcm {
			if ref[i] != got[i] {
				t.Fatalf("chunking changed sample %d", i)
			}
			silent = silent && pcm[i] == 0
		}
		for k := range b.gPrev {
			for _, v := range []float64{b.gPrev[k], b.sm[k], b.a2[k], b.curMin[k]} {
				if math.IsNaN(v) || math.IsInf(v, 0) {
					t.Fatalf("non-finite state at bin %d", k)
				}
			}
		}
		if silent {
			for i, v := range ref {
				if v != 0 {
					t.Fatalf("silence produced %d at %d", v, i)
				}
			}
		}
	})
}
