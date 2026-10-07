package mixer

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"govoice/core"
)

func wavBytes(rate int, pcm []int16) []byte {
	b := []byte("RIFF\x00\x00\x00\x00WAVEfmt ")
	b = binary.LittleEndian.AppendUint32(b, 16)
	b = binary.LittleEndian.AppendUint16(b, 1)
	b = binary.LittleEndian.AppendUint16(b, 1)
	b = binary.LittleEndian.AppendUint32(b, uint32(rate))
	b = binary.LittleEndian.AppendUint32(b, uint32(rate*2))
	b = binary.LittleEndian.AppendUint16(b, 2)
	b = binary.LittleEndian.AppendUint16(b, 16)
	b = append(b, "data"...)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(pcm)*2))
	for _, s := range pcm {
		b = binary.LittleEndian.AppendUint16(b, uint16(s))
	}
	return b
}

func pcmBytes(pcm []int16) []byte {
	b := make([]byte, 2*len(pcm))
	for i, s := range pcm {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(s))
	}
	return b
}

func samples(b []byte) []int16 {
	o := make([]int16, len(b)/2)
	for i := range o {
		o[i] = int16(binary.LittleEndian.Uint16(b[2*i:]))
	}
	return o
}

func rms(p []int16) float64 {
	var s float64
	for _, v := range p {
		s += float64(v) * float64(v)
	}
	return math.Sqrt(s / float64(len(p)))
}

func TestGainMath(t *testing.T) {
	if Scale(12345, GainQ(0)) != 12345 || Scale(-32768, GainQ(0)) != -32768 {
		t.Fatal("0 dB is not identity")
	}
	if got := Scale(20000, GainQ(-6.0206)); got < 9995 || got > 10005 {
		t.Fatalf("-6 dB: %d, want ~10000", got)
	}
	if Scale(30000, GainQ(6)) != 32767 || Scale(-30000, GainQ(6)) != -32768 {
		t.Fatal("+6 dB must clip, not wrap")
	}
	for _, db := range []float64{-30.1, 6.1, math.NaN()} {
		if ValidateGainDB(db) == nil {
			t.Errorf("%v accepted", db)
		}
	}
	if ValidateGainDB(-30) != nil || ValidateGainDB(6) != nil || ValidateGainDB(0) != nil {
		t.Error("range edges rejected")
	}
}

type capture struct{ out core.Frame }

func (c *capture) Push(f core.Frame) error                            { c.out = f; return nil }
func (c *capture) PushDirection(f core.Frame, _ core.Direction) error { c.out = f; return nil }

func TestGainProcessorIsCopyOnWrite(t *testing.T) {
	g, err := NewGain(-6.0206)
	if err != nil {
		t.Fatal(err)
	}
	in := &core.OutputAudioFrame{Samples: pcmBytes([]int16{1000, -2000}), SampleRate: 24000, Channels: 1, Seq: 7}
	orig := append([]byte(nil), in.Samples...)
	var c capture
	if err := g.ProcessFrame(context.Background(), in, core.Downstream, &c); err != nil {
		t.Fatal(err)
	}
	out := c.out.(*core.OutputAudioFrame)
	if string(in.Samples) != string(orig) || out == in || out.Seq != 7 {
		t.Fatal("input mutated or not copied")
	}
	if s := samples(out.Samples); s[0] < 499 || s[0] > 501 || s[1] > -999 || s[1] < -1001 {
		t.Fatalf("got %v", s)
	}
}

func TestLoadAssetResamplesAndBakesGain(t *testing.T) {
	src := make([]int16, 1600) // 100 ms at 16 kHz, constant
	for i := range src {
		src[i] = 10000
	}
	a, err := newAsset(wavBytes(16000, src), 24000, -6.0206)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(a.pcm); n < 2275 || n > 2280 { // 2400 minus the 5 ms loop crossfade
		t.Fatalf("resampled len %d, want ~2280", n)
	}
	if v := a.pcm[100]; v < 4995 || v > 5005 {
		t.Fatalf("gain not baked: %d", v)
	}
}

func TestLoadAssetRejects(t *testing.T) {
	dir := t.TempDir()
	st := wavBytes(24000, make([]int16, 1000))
	st[22] = 2 // stereo
	big := filepath.Join(dir, "big.wav")
	if err := os.WriteFile(big, make([]byte, MaxFileBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string][]byte{"stereo": st, "garbage": []byte("nope"), "short": wavBytes(24000, make([]int16, 10))} {
		if _, err := newAsset(b, 24000, 0); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := LoadAsset(big, 24000, 0); err == nil {
		t.Error("oversize accepted")
	}
	if _, err := LoadAsset(filepath.Join(dir, "missing.wav"), 24000, 0); err == nil {
		t.Error("missing accepted")
	}
	if _, err := LoadAsset(big, 24000, 3); err == nil {
		t.Error("positive background gain accepted")
	}
}

func ramp(n int) []int16 {
	p := make([]int16, n)
	for i := range p {
		p[i] = int16(i + 1)
	}
	return p
}

func TestCursorContinuityAcrossChunksAndLoopWrap(t *testing.T) {
	a := &Asset{pcm: ramp(1000), rate: 24000}
	// Chunking must not matter: 7-sample and 480-sample reads yield identical streams.
	var x, y []int16
	c1, c2 := a.NewCursor(true), a.NewCursor(true)
	for len(x) < 2500 {
		b := make([]byte, 14)
		c1.Mix(b, nil)
		x = append(x, samples(b)...)
	}
	for len(y) < 2520 {
		b := make([]byte, 960)
		c2.Mix(b, nil)
		y = append(y, samples(b)...)
	}
	for i := range x {
		if x[i] != y[i] || x[i] != int16(i%1000+1) {
			t.Fatalf("sample %d: %d vs %d", i, x[i], y[i])
		}
	}
}

func TestCursorMixKeepsSpeechRMSAndSaturates(t *testing.T) {
	amb := make([]int16, 4800) // quiet constant ambience
	for i := range amb {
		amb[i] = 100
	}
	c := (&Asset{pcm: amb, rate: 24000}).NewCursor(true)
	speech := make([]int16, 480)
	for i := range speech {
		speech[i] = int16(8000 * math.Sin(2*math.Pi*440*float64(i)/24000))
	}
	dst := make([]byte, 960)
	c.Mix(dst, pcmBytes(speech))
	mixed := samples(dst)
	diff := make([]int16, len(mixed))
	for i := range diff {
		diff[i] = mixed[i] - speech[i]
		if diff[i] != 100 {
			t.Fatalf("ambience not added at %d", i)
		}
	}
	if r := rms(mixed) / rms(speech); r < 0.99 || r > 1.02 {
		t.Fatalf("speech RMS changed by %.3f", r)
	}
	loud := pcmBytes([]int16{32760, -32760})
	out := make([]byte, 4)
	c.Mix(out, loud)
	if s := samples(out); s[0] != 32767 || s[1] != -32660 {
		t.Fatalf("saturation: %v", s)
	}
}

func TestCursorOneShotEndsInactive(t *testing.T) {
	c := (&Asset{pcm: ramp(4), rate: 24000}).NewCursor(false)
	b := make([]byte, 12)
	c.Mix(b, nil)
	if got := samples(b); got[3] != 4 || got[4] != 0 || c.Active() {
		t.Fatalf("%v active=%v", got, c.Active())
	}
}

func TestMixNoAllocs(t *testing.T) {
	c := (&Asset{pcm: ramp(1000), rate: 24000}).NewCursor(true)
	dst, src := make([]byte, 960), make([]byte, 960)
	if n := testing.AllocsPerRun(100, func() { c.Mix(dst, src) }); n != 0 {
		t.Fatalf("%v allocs per frame", n)
	}
}

func TestGainCopyPreservesEveryField(t *testing.T) {
	in := &core.OutputAudioFrame{Samples: pcmBytes([]int16{1000})}
	v := reflect.ValueOf(in).Elem()
	for i := 0; i < v.NumField(); i++ { // set every exported field to a non-zero value
		f := v.Field(i)
		if !f.CanSet() || v.Type().Field(i).Name == "Samples" {
			continue
		}
		switch f.Kind() {
		case reflect.Bool:
			f.SetBool(true)
		case reflect.String:
			f.SetString("x")
		case reflect.Int, reflect.Int64, reflect.Int32:
			f.SetInt(7)
		case reflect.Uint, reflect.Uint64, reflect.Uint32:
			f.SetUint(7)
		case reflect.Slice:
			f.Set(reflect.MakeSlice(f.Type(), 1, 1))
		}
	}
	in.Encoding = core.EncodingPCM16LE
	g, _ := NewGain(-6)
	var c capture
	if err := g.ProcessFrame(context.Background(), in, core.Downstream, &c); err != nil {
		t.Fatal(err)
	}
	out := *c.out.(*core.OutputAudioFrame)
	if bytes.Equal(out.Samples, in.Samples) {
		t.Fatal("gain not applied")
	}
	out.Samples = in.Samples
	if !reflect.DeepEqual(&out, in) {
		t.Fatalf("gain dropped a field:\n%+v\n%+v", out, *in)
	}
}

func TestLoadAssetAntiAliasesAndLoopsWithoutStep(t *testing.T) {
	tone := make([]int16, 48000/5) // 200 ms of 20 kHz at 48 kHz: above the 12 kHz output Nyquist
	for i := range tone {
		tone[i] = int16(10000 * math.Sin(2*math.Pi*20000*float64(i)/48000))
	}
	a, err := newAsset(wavBytes(48000, tone), 24000, 0)
	if err != nil {
		t.Fatal(err)
	}
	if r := rms(a.pcm[200 : len(a.pcm)-200]); r > 300 { // un-filtered linear resampling aliases it to 4 kHz at ~7000 rms
		t.Fatalf("aliased energy rms %.0f", r)
	}
	ramp := make([]int16, 4800)
	for i := range ramp {
		ramp[i] = int16(i * 4) // a sawtooth: 0 -> 19196, a big step at the loop point
	}
	a, err = newAsset(wavBytes(24000, ramp), 24000, 0)
	if err != nil {
		t.Fatal(err)
	}
	if d := int(a.pcm[0]) - int(a.pcm[len(a.pcm)-1]); d < -200 || d > 200 {
		t.Fatalf("loop step %d", d)
	}
}

func TestParseWAVFormats(t *testing.T) {
	ext := func(sub uint16, bits uint16) []byte {
		b := []byte("RIFF\x00\x00\x00\x00WAVEfmt ")
		b = binary.LittleEndian.AppendUint32(b, 40)
		for _, v := range []uint16{0xFFFE, 1} {
			b = binary.LittleEndian.AppendUint16(b, v)
		}
		b = binary.LittleEndian.AppendUint32(b, 24000)
		b = binary.LittleEndian.AppendUint32(b, 48000)
		b = binary.LittleEndian.AppendUint16(b, 2)
		b = binary.LittleEndian.AppendUint16(b, bits)
		b = binary.LittleEndian.AppendUint16(b, 22)
		b = binary.LittleEndian.AppendUint16(b, bits)
		b = binary.LittleEndian.AppendUint32(b, 4)
		b = binary.LittleEndian.AppendUint16(b, sub)
		b = append(b, make([]byte, 14)...)
		b = append(b, "data"...)
		b = binary.LittleEndian.AppendUint32(b, 2000)
		return append(b, make([]byte, 2000)...)
	}
	if _, _, err := parseWAV(ext(1, 16)); err != nil {
		t.Fatalf("extensible PCM16 rejected: %v", err)
	}
	if _, _, err := parseWAV(ext(3, 16)); err == nil {
		t.Fatal("extensible float accepted")
	}
	if _, _, err := parseWAV(wavBytes(1, make([]int16, 100))); err == nil {
		t.Fatal("1 Hz rate accepted (would upsample to gigabytes)")
	}
	b8 := wavBytes(24000, make([]int16, 100))
	b8[34] = 8
	if _, _, err := parseWAV(b8); err == nil {
		t.Fatal("8-bit accepted")
	}
}

func FuzzNewAsset(f *testing.F) {
	f.Add(wavBytes(24000, ramp(1000)))
	f.Add(wavBytes(48000, ramp(2000)))
	f.Add([]byte("RIFF\xff\xff\xff\xffWAVEdata\xff\xff\xff\xff"))
	f.Fuzz(func(t *testing.T, b []byte) {
		if a, err := newAsset(b, 24000, -30); err == nil && len(a.pcm) > 6*len(b) { // 8 kHz->24 kHz is the largest growth (3x samples)
			t.Fatalf("%d samples from %d bytes", len(a.pcm), len(b))
		}
	})
}
