package filter

import (
	"context"
	"encoding/binary"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/audio/dtmf"
	"govoice/core/coretest"
)

type gain struct {
	name   string
	k      int16
	resets int
	closed bool
	fail   error
	short  bool
}

func (g *gain) Name() string { return g.name }
func (g *gain) Process(p []int16, _ int) ([]int16, error) {
	if g.fail != nil {
		return nil, g.fail
	}
	out := make([]int16, len(p))
	for i, v := range p {
		out[i] = v * g.k
	}
	if g.short && len(out) > 0 {
		out = out[1:]
	}
	return out, nil
}
func (g *gain) Reset()       { g.resets++ }
func (g *gain) Close() error { g.closed = true; return g.fail }

func TestChainComposition(t *testing.T) {
	a, b := &gain{name: "a", k: 2}, &gain{name: "b", k: 3}
	c := NewChain(a, b)
	seen := map[string]int{}
	c.SetObserver(func(n string, d time.Duration) { seen[n]++ })
	in := []int16{1, 2, 3}
	out, err := c.Process(in, sr)
	if err != nil || out[0] != 6 || out[2] != 18 {
		t.Fatalf("%v %v", out, err)
	}
	if in[0] != 1 {
		t.Fatal("input mutated")
	}
	if seen["a"] != 1 || seen["b"] != 1 {
		t.Fatalf("observer %v", seen)
	}
	c.Reset()
	if a.resets != 1 || b.resets != 1 {
		t.Fatal("reset not propagated")
	}
	_ = c.Close()
	if !a.closed || !b.closed {
		t.Fatal("close not propagated")
	}
	// Empty chain: identity, but a copy.
	o, _ := NewChain().Process(in, sr)
	o[0] = 99
	if in[0] != 1 {
		t.Fatal("empty chain aliased input")
	}
	// Errors and length violations are reported with the filter name.
	if _, err := NewChain(&gain{name: "x", fail: errors.New("boom")}).Process(in, sr); err == nil {
		t.Fatal("want error")
	}
	if _, err := NewChain(&gain{name: "s", k: 1, short: true}).Process(in, sr); err == nil {
		t.Fatal("want length error")
	}
	d, _ := NewDenoiser(0.5)
	if got := NewChain(d, d).Delay(); got != 2*d.Delay() {
		t.Fatalf("delay %v", got)
	}
}

func pcmBytes(s []int16) []byte {
	b := make([]byte, 2*len(s))
	for i, v := range s {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(v))
	}
	return b
}

func inFrame(b []byte, rate, ch int) *core.InputAudioFrame {
	return &core.InputAudioFrame{Samples: b, Encoding: core.EncodingPCM16LE, SampleRate: rate, Channels: ch,
		Duration: time.Duration(len(b)/2) * time.Second / time.Duration(rate), Timestamp: time.Now()}
}

func startSession(t *testing.T, procs ...core.Processor) *core.Session {
	t.Helper()
	pipe, err := core.NewPipeline(procs...)
	if err != nil {
		t.Fatal(err)
	}
	s, err := core.NewSession(1, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func waitFrames(t *testing.T, sink *coretest.PassthroughProcessor, n int) []*core.InputAudioFrame {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var got []*core.InputAudioFrame
		for _, f := range sink.GetFrames() {
			if a, ok := f.(*core.InputAudioFrame); ok {
				got = append(got, a)
			}
		}
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("got %d of %d frames", len(got), n)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestProcessorCopyOnWriteAndPassThrough(t *testing.T) {
	sink := &coretest.PassthroughProcessor{}
	p := NewProcessor(NewChain(&gain{name: "g", k: 2}), sr)
	s := startSession(t, p, sink)

	orig := pcmBytes([]int16{1, 2, 3, 4})
	keep := append([]byte(nil), orig...)
	frames := []*core.InputAudioFrame{
		inFrame(orig, sr, 1),                      // filtered
		inFrame(pcmBytes([]int16{5, 6}), 8000, 1), // other rate: untouched
		inFrame(pcmBytes([]int16{5, 6}), sr, 2),   // stereo: untouched
		inFrame([]byte{1, 2, 3}, sr, 1),           // odd bytes: untouched
		inFrame(nil, sr, 1),                       // empty: untouched
	}
	for _, f := range frames {
		if err := s.Post(f); err != nil {
			t.Fatal(err)
		}
	}
	got := waitFrames(t, sink, len(frames))
	if v := int16(binary.LittleEndian.Uint16(got[0].Samples[2:])); v != 4 {
		t.Fatalf("filtered sample %d", v)
	}
	if string(orig) != string(keep) {
		t.Fatal("shared input buffer was mutated")
	}
	if got[0] == frames[0] {
		t.Fatal("frame was not copied")
	}
	for i := 1; i < len(frames); i++ {
		if got[i] != frames[i] {
			t.Errorf("frame %d should pass through untouched", i)
		}
	}
	// Metadata survives the copy.
	if got[0].Duration != frames[0].Duration || got[0].SampleRate != sr {
		t.Fatal("metadata lost")
	}
}

func TestProcessorFailsOpen(t *testing.T) {
	sink := &coretest.PassthroughProcessor{}
	p := NewProcessor(NewChain(&gain{name: "g", fail: errors.New("boom")}), sr)
	s := startSession(t, p, sink)
	f := inFrame(pcmBytes([]int16{1, 2}), sr, 1)
	_ = s.Post(f)
	if got := waitFrames(t, sink, 1); got[0] != f {
		t.Fatal("original frame must be forwarded on filter error")
	}
	if p.Errors.Load() != 1 {
		t.Fatal("error not counted")
	}
}

// zero silences audio; stands in for a filter that destroys keypad tones.
type zero struct{}

func (zero) Process(p []int16, _ int) ([]int16, error) { return make([]int16, len(p)), nil }
func (zero) Reset()                                    {}
func (zero) Close() error                              { return nil }

func detectedDigits(t *testing.T, filterFirst bool) string {
	t.Helper()
	det, err := dtmf.NewInputProcessor(dtmf.DefaultDetectorConfig())
	if err != nil {
		t.Fatal(err)
	}
	sink := &coretest.PassthroughProcessor{}
	fp := NewProcessor(NewChain(zero{}), sr)
	procs := []core.Processor{det, fp, sink}
	if filterFirst {
		procs = []core.Processor{fp, det, sink}
	}
	s := startSession(t, procs...)
	tone, _ := dtmf.Generate("5", dtmf.DefaultGeneratorConfig(sr))
	buf := append(make([]byte, 3200), tone...)
	buf = append(buf, make([]byte, 6400)...)
	for off := 0; off < len(buf); off += 640 {
		_ = s.Post(inFrame(buf[off:min(off+640, len(buf))], sr, 1))
	}
	time.Sleep(300 * time.Millisecond)
	var d string
	for _, f := range sink.GetFrames() {
		if v, ok := f.(*core.InputDTMFFrame); ok {
			d += v.Digit
		}
	}
	return d
}

// The documented order (DTMF before the filter chain) keeps keypad tones
// detectable even when the filter would destroy them.
func TestDTMFSeesUnfilteredAudio(t *testing.T) {
	if got := detectedDigits(t, false); got != "5" {
		t.Fatalf("dtmf before filter: %q", got)
	}
	if got := detectedDigits(t, true); got != "" {
		t.Fatalf("control: dtmf after a destructive filter should hear nothing, got %q", got)
	}
}

func TestPipelineWithDenoiser(t *testing.T) {
	d, _ := NewDenoiser(0.5)
	sink := &coretest.PassthroughProcessor{}
	s := startSession(t, NewProcessor(NewChain(d), sr), sink)
	clean := speechLike(1, 3)
	b := pcmBytes(mix(clean, whiteNoise(2, len(clean)), 5))
	for off := 0; off < len(b); off += 640 {
		_ = s.Post(inFrame(b[off:min(off+640, len(b))], sr, 1))
		time.Sleep(time.Millisecond) // the session may drop audio under burst; pace it
	}
	got := waitFrames(t, sink, 20)
	changed := false
	for _, f := range got {
		if len(f.Samples) != 640 {
			t.Fatalf("frame length %d", len(f.Samples))
		}
		changed = changed || string(f.Samples) != string(b[:640])
	}
	if !changed {
		t.Fatal("denoiser had no effect")
	}
}

// Per-session instances share nothing: concurrent sessions with different
// inputs give the same result as running alone.
func TestConcurrentSessionsAreIndependent(t *testing.T) {
	clean := speechLike(1, 2)
	in := mix(clean, whiteNoise(2, len(clean)), 5)
	d0, _ := NewDenoiser(0.5)
	want, _ := d0.Process(in, sr)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			d, _ := NewDenoiser(0.5)
			other := mix(speechLike(int64(g+20), 2), pinkNoise(int64(g), 2*sr), float64(g))
			for off := 0; off < len(in); off += 320 {
				_, _ = d.Process(other[off:off+320], sr)
			}
			d.Reset()
			var got []int16
			for off := 0; off < len(in); off += 320 {
				o, _ := d.Process(in[off:min(off+320, len(in))], sr)
				got = append(got, o...)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Errorf("goroutine %d differs at %d", g, i)
					return
				}
			}
		}(g)
	}
	wg.Wait()
}

func TestNoGoroutineLeak(t *testing.T) {
	before := runtime.NumGoroutine()
	for i := 0; i < 20; i++ {
		d, _ := NewDenoiser(0.5)
		_, _ = d.Process(make([]int16, 4000), sr)
		c := NewChain(d)
		_ = c.Close()
	}
	runtime.GC()
	time.Sleep(20 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > before {
		t.Fatalf("goroutines %d -> %d", before, after)
	}
}

var _ = context.Background
