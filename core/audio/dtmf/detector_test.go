package dtmf

import (
	"encoding/binary"
	"math"
	"math/rand"
	"slices"
	"testing"
	"time"

	"govoice/core/transport/resample"
)

const rate = 16000

func pcm(samples []float64) []byte {
	b := make([]byte, 0, len(samples)*2)
	for _, v := range samples {
		b = binary.LittleEndian.AppendUint16(b, uint16(int16(math.Max(-32768, math.Min(32767, math.Round(v))))))
	}
	return b
}

func tones(f1, f2, a1, a2 float64, d time.Duration) []float64 {
	out := make([]float64, int(rate*d.Seconds()))
	for i := range out {
		t := float64(i) / rate
		out[i] = a1*math.Sin(2*math.Pi*f1*t) + a2*math.Sin(2*math.Pi*f2*t)
	}
	return out
}

func silence(d time.Duration) []float64 { return make([]float64, int(rate*d.Seconds())) }

func addNoiseSNR(x []float64, snrDB float64, rng *rand.Rand) []float64 {
	var p float64
	for _, v := range x {
		p += v * v
	}
	p /= float64(len(x))
	sigma := math.Sqrt(p / math.Pow(10, snrDB/10))
	out := make([]float64, len(x))
	for i, v := range x {
		out[i] = v + sigma*rng.NormFloat64()
	}
	return out
}

func detect(t *testing.T, b []byte) string {
	t.Helper()
	d, err := NewDetector(DefaultDetectorConfig())
	if err != nil {
		t.Fatal(err)
	}
	var got []byte
	// Odd chunk sizes prove the detector is independent of framing.
	for len(b) > 0 {
		n := min(len(b), 2*333)
		for _, e := range d.Write(b[:n]) {
			got = append(got, e.Digit)
		}
		b = b[n:]
	}
	return string(got)
}

func digitTone(dg byte, a1, a2 float64, d time.Duration) []float64 {
	lo, hi, _ := digitFreqs(dg)
	return tones(lo, hi, a1, a2, d)
}

func TestDetectorAccuracyTable(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	cases := []struct {
		name string
		make func(dg byte) []float64
	}{
		{"clean", func(dg byte) []float64 { return digitTone(dg, 8000, 8000, 80*time.Millisecond) }},
		{"quiet", func(dg byte) []float64 { return digitTone(dg, 400, 400, 80*time.Millisecond) }},
		{"20dB SNR", func(dg byte) []float64 {
			return addNoiseSNR(digitTone(dg, 8000, 8000, 80*time.Millisecond), 20, rng)
		}},
		{"+10% low amplitude", func(dg byte) []float64 { return digitTone(dg, 8800, 8000, 80*time.Millisecond) }},
		{"-10% high amplitude", func(dg byte) []float64 { return digitTone(dg, 8000, 7200, 80*time.Millisecond) }},
		{"forward twist -6dB", func(dg byte) []float64 { return digitTone(dg, 10000, 5000, 80*time.Millisecond) }},
		{"reverse twist +3dB", func(dg byte) []float64 { return digitTone(dg, 5000, 7000, 80*time.Millisecond) }},
		{"long press 300ms", func(dg byte) []float64 { return digitTone(dg, 8000, 8000, 300*time.Millisecond) }},
	}
	for _, c := range cases {
		for i := 0; i < len(Digits); i++ {
			dg := Digits[i]
			x := append(append(silence(100*time.Millisecond), c.make(dg)...), silence(100*time.Millisecond)...)
			if got := detect(t, pcm(x)); got != string(dg) {
				t.Errorf("%s digit %c: got %q", c.name, dg, got)
			}
		}
	}
}

func TestDetectorSequenceAndRepeats(t *testing.T) {
	var x []float64
	x = append(x, silence(50*time.Millisecond)...)
	for _, dg := range []byte("1122#*") {
		x = append(x, digitTone(dg, 8000, 8000, 70*time.Millisecond)...)
		x = append(x, silence(40*time.Millisecond)...) // minimum standard pause
	}
	if got := detect(t, pcm(x)); got != "1122#*" {
		t.Fatalf("got %q", got)
	}
}

func TestDetectorRejects(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	noise := make([]float64, rate*2)
	for i := range noise {
		noise[i] = 4000 * rng.NormFloat64()
	}
	cases := map[string][]float64{
		"silence":            silence(time.Second),
		"short 30ms blip":    append(digitTone('5', 8000, 8000, 30*time.Millisecond), silence(200*time.Millisecond)...),
		"single low tone":    tones(770, 770, 8000, 0, 300*time.Millisecond),
		"single high tone":   tones(1336, 1336, 8000, 0, 300*time.Millisecond),
		"two low tones":      tones(697, 852, 8000, 8000, 300*time.Millisecond),
		"off-frequency pair": tones(740, 1280, 8000, 8000, 300*time.Millisecond),
		"off by 4%":          tones(697*1.04, 1209*1.04, 8000, 8000, 300*time.Millisecond),
		"1kHz test tone":     tones(1000, 1000, 8000, 0, 300*time.Millisecond),
		"twist forward 12dB": tones(697, 1209, 12000, 3000, 300*time.Millisecond),
		"twist reverse 8dB":  tones(697, 1209, 3000, 7600, 300*time.Millisecond),
		"white noise":        noise,
	}
	for name, x := range cases {
		if got := detect(t, pcm(x)); got != "" {
			t.Errorf("%s: false digits %q", name, got)
		}
	}
}

// speechLike builds a voiced signal: a gliding pitch with a harmonic series
// shaped by moving formants, syllable-rate amplitude modulation and noise.
func speechLike(rng *rand.Rand, d time.Duration) []float64 {
	n := int(rate * d.Seconds())
	out := make([]float64, n)
	f0 := 90 + rng.Float64()*130
	f1, f2 := 300+rng.Float64()*600, 900+rng.Float64()*1500
	phase := 0.0
	for i := range out {
		t := float64(i) / rate
		pitch := f0 * (1 + 0.15*math.Sin(2*math.Pi*0.7*t+1))
		phase += 2 * math.Pi * pitch / rate
		form1 := f1 * (1 + 0.3*math.Sin(2*math.Pi*1.3*t))
		form2 := f2 * (1 + 0.2*math.Sin(2*math.Pi*0.9*t+2))
		var v float64
		for h := 1; float64(h)*pitch < 4000; h++ {
			fh := float64(h) * pitch
			g := 1/(1+math.Pow((fh-form1)/150, 2)) + 0.7/(1+math.Pow((fh-form2)/200, 2)) + 0.02
			v += g * math.Sin(float64(h)*phase) / float64(h)
		}
		env := 0.5 + 0.5*math.Sin(2*math.Pi*4*t)
		out[i] = 9000*env*v + 300*rng.NormFloat64()
	}
	return out
}

func chirp(f0, f1 float64, d time.Duration) []float64 {
	n := int(rate * d.Seconds())
	out := make([]float64, n)
	T := d.Seconds()
	for i := range out {
		t := float64(i) / rate
		out[i] = 9000 * math.Sin(2*math.Pi*(f0*t+(f1-f0)*t*t/(2*T)))
	}
	return out
}

func TestDetectorNoFalsePositivesOnSpeechLikeSignals(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	for i := 0; i < 40; i++ {
		if got := detect(t, pcm(speechLike(rng, time.Second))); got != "" {
			t.Fatalf("speech-like #%d produced digits %q", i, got)
		}
	}
	for _, c := range [][2]float64{{300, 3000}, {3000, 300}, {600, 1700}, {1700, 600}, {100, 4000}} {
		for _, d := range []time.Duration{200 * time.Millisecond, time.Second, 3 * time.Second} {
			if got := detect(t, pcm(chirp(c[0], c[1], d))); got != "" {
				t.Fatalf("chirp %v over %v produced %q", c, d, got)
			}
		}
	}
}

func TestDetectorSpeechThenTone(t *testing.T) {
	rng := rand.New(rand.NewSource(4))
	x := append(speechLike(rng, time.Second), silence(100*time.Millisecond)...)
	x = append(x, digitTone('9', 8000, 8000, 100*time.Millisecond)...)
	x = append(x, silence(100*time.Millisecond)...)
	if got := detect(t, pcm(x)); got != "9" {
		t.Fatalf("got %q", got)
	}
}

func TestGeneratorRoundTrip(t *testing.T) {
	for _, sr := range []int{16000, 24000} {
		b, err := Generate(Digits, DefaultGeneratorConfig(sr))
		if err != nil {
			t.Fatal(err)
		}
		if want := len(Digits) * int(float64(sr)*0.16) * 2; len(b) != want {
			t.Fatalf("rate %d: %d bytes, want %d", sr, len(b), want)
		}
		if sr != rate {
			continue // the detector input is 16 kHz
		}
		if got := detect(t, b); got != Digits {
			t.Fatalf("round trip got %q want %q", got, Digits)
		}
	}
}

func TestGeneratorFrequencyAndLevel(t *testing.T) {
	cfg := DefaultGeneratorConfig(24000)
	cfg.LevelDBFS = -6
	b, _ := Generate("5", cfg) // 770 + 1336
	x := make([]float64, len(b)/2)
	var peak float64
	for i := range x {
		x[i] = float64(int16(binary.LittleEndian.Uint16(b[2*i:])))
		peak = math.Max(peak, math.Abs(x[i]))
	}
	if want := 32767 * math.Pow(10, -6.0/20); math.Abs(peak-want)/want > 0.03 {
		t.Fatalf("peak %.0f, want ~%.0f", peak, want)
	}
	x = x[:int(24000*0.08)]
	pw := func(f float64) float64 {
		var re, im float64
		for i, v := range x {
			a := 2 * math.Pi * f * float64(i) / 24000
			re += v * math.Cos(a)
			im += v * math.Sin(a)
		}
		return re*re + im*im
	}
	for _, f := range []float64{697, 852, 941, 1209, 1477, 1633, 2000} {
		if pw(770) < 100*pw(f) || pw(1336) < 100*pw(f) {
			t.Errorf("bin %.0f too strong vs generated tones", f)
		}
	}
}

func TestGeneratorValidation(t *testing.T) {
	if _, err := Generate("1x", DefaultGeneratorConfig(16000)); err == nil {
		t.Fatal("invalid digit accepted")
	}
	bad := DefaultGeneratorConfig(16000)
	bad.Tone = 10 * time.Millisecond
	if _, err := Generate("1", bad); err == nil {
		t.Fatal("short tone accepted")
	}
}

// muLawRoundTrip applies G.711 mu-law companding (encode then decode).
func muLawRoundTrip(x []float64) []float64 {
	out := make([]float64, len(x))
	for i, v := range x {
		s := int(math.Max(-32635, math.Min(32635, math.Round(v))))
		sign := 0
		if s < 0 {
			sign, s = 0x80, -s
		}
		s += 0x84
		exp := 7
		for m := 0x4000; s&m == 0 && exp > 0; m >>= 1 {
			exp--
		}
		b := ^(sign | exp<<4 | (s>>(exp+3))&0x0f) & 0xff
		// decode
		b = ^b & 0xff
		mag := ((b&0x0f)<<3 + 0x84) << ((b >> 4) & 7)
		mag -= 0x84
		if b&0x80 != 0 {
			mag = -mag
		}
		out[i] = float64(mag)
	}
	return out
}

func toneAt(rateHz int, dg byte, amp float64, d time.Duration) []float64 {
	lo, hi, _ := digitFreqs(dg)
	out := make([]float64, int(float64(rateHz)*d.Seconds()))
	for i := range out {
		t := float64(i) / float64(rateHz)
		out[i] = amp * (math.Sin(2*math.Pi*lo*t) + math.Sin(2*math.Pi*hi*t))
	}
	return out
}

// Telephone-network path: 8 kHz G.711 mu-law, upsampled to the 16 kHz the
// transport feeds the detector, at a low line level (-30 dBFS per tone).
func TestDetectorSurvivesMuLawUpsampleAndLowLevel(t *testing.T) {
	amp := 32767 * math.Pow(10, -30.0/20)
	var x []float64
	for i := 0; i < len(Digits); i++ {
		x = append(x, make([]float64, 400)...) // 50 ms
		x = append(x, toneAt(8000, Digits[i], amp, 50*time.Millisecond)...)
	}
	x = append(x, make([]float64, 800)...)
	r, err := resample.NewResampler(8000, 16000)
	if err != nil {
		t.Fatal(err)
	}
	up, err := r.ResamplePCM16(pcm(muLawRoundTrip(x)))
	if err != nil {
		t.Fatal(err)
	}
	if got := detect(t, up); got != Digits {
		t.Fatalf("got %q want %q", got, Digits)
	}
}

// Q.24/Bellcore: a 40 ms tone must be accepted and a <= 23 ms one rejected,
// whatever its alignment to the 10 ms analysis hop.
func TestDetectorMinimumDurationAllAlignments(t *testing.T) {
	for off := 0; off < 160; off += 9 {
		lead := make([]float64, 800+off)
		for _, c := range []struct {
			d    time.Duration
			want string
		}{{40 * time.Millisecond, "8"}, {23 * time.Millisecond, ""}} {
			x := append(append(lead, digitTone('8', 8000, 8000, c.d)...), silence(100*time.Millisecond)...)
			if got := detect(t, pcm(x)); got != c.want {
				t.Fatalf("%v tone at offset %d: got %q want %q", c.d, off, got, c.want)
			}
		}
	}
}

// A loud held digit repeated with the minimum 40 ms pause is several presses.
func TestDetectorRepeatAfterMinimumPauseLoud(t *testing.T) {
	var x []float64
	for i := 0; i < 4; i++ {
		x = append(x, digitTone('0', 15000, 15000, 40*time.Millisecond)...)
		x = append(x, silence(40*time.Millisecond)...)
	}
	if got := detect(t, pcm(x)); got != "0000" {
		t.Fatalf("got %q", got)
	}
}

// Events and onsets do not depend on how the stream is chunked.
func TestDetectorFramingIndependent(t *testing.T) {
	b, _ := Generate("147*0#", DefaultGeneratorConfig(rate))
	b = append(make([]byte, 1234), b...)
	var ref []Event
	for _, chunk := range []int{1, 160, 320, 480, 960, 333, 4096} { // samples
		d, _ := NewDetector(DefaultDetectorConfig())
		var got []Event
		for x := b; len(x) > 0; {
			n := min(len(x), 2*chunk)
			got = append(got, d.Write(x[:n])...)
			x = x[n:]
		}
		if ref == nil {
			ref = got
		}
		if len(got) != 6 || !slices.Equal(got, ref) {
			t.Fatalf("chunk %d: %v vs %v", chunk, got, ref)
		}
	}
}

// harmonic is an instrument-like note: a fundamental with decaying overtones.
func harmonic(f, amp float64, d time.Duration) []float64 {
	out := make([]float64, int(rate*d.Seconds()))
	for i := range out {
		t := float64(i) / rate
		for h := 1; h <= 6 && float64(h)*f < rate/2; h++ {
			out[i] += amp / float64(h) * math.Sin(2*math.Pi*float64(h)*f*t)
		}
	}
	return out
}

func mix(xs ...[]float64) []float64 {
	out := make([]float64, len(xs[0]))
	for _, x := range xs {
		for i := range out {
			out[i] += x[i]
		}
	}
	return out
}

func TestDetectorRejectsCallProgressTonesMusicAndSibilants(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	sib := make([]float64, rate)
	prev := 0.0
	for i := range sib { // first difference of white noise: energy above 4 kHz
		w := 6000 * rng.NormFloat64()
		sib[i], prev = w-prev, w
	}
	note := func(f float64) []float64 { return harmonic(f, 6000, time.Second) }
	cases := map[string][]float64{
		"US ringback 440+480":   tones(440, 480, 8000, 8000, 2*time.Second),
		"US dial tone 350+440":  tones(350, 440, 8000, 8000, 2*time.Second),
		"busy/reorder 480+620":  tones(480, 620, 8000, 8000, 2*time.Second),
		"UK ringback 400+450":   tones(400, 450, 8000, 8000, 2*time.Second),
		"call waiting 440":      tones(440, 440, 8000, 0, time.Second),
		"sibilant noise":        sib,
		"F5+E6 instrument dyad": mix(note(698.46), note(1318.51)),
		"G5+D6 instrument dyad": mix(note(783.99), note(1174.66)),
		"A5+E6+A6 chord":        mix(note(880), note(1318.51), note(1760)),
		"Bb5+F6 chord":          mix(note(932.33), note(1396.91)),
	}
	for name, x := range cases {
		if got := detect(t, pcm(x)); got != "" {
			t.Errorf("%s: false digits %q", name, got)
		}
	}
}

func BenchmarkDetector20msFrame(b *testing.B) {
	x := pcm(speechLike(rand.New(rand.NewSource(1)), time.Second))
	d, _ := NewDetector(DefaultDetectorConfig())
	const fr = 640
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		off := (i * fr) % (len(x) - fr)
		d.Write(x[off : off+fr])
	}
}
