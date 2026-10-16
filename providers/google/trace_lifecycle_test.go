package google

import (
	"context"
	"sync"
	"testing"
	"time"

	"govoice/core"
)

type lifecycleRecorder struct {
	mu        sync.Mutex
	events    []core.TraceEvent
	starts    []core.TraceEvent
	ends      int
	endErrors map[string][]error
}
type lifecycleSpan struct {
	r    *lifecycleRecorder
	name string
}

func (r *lifecycleRecorder) StartSpan(_ context.Context, ev core.TraceEvent) core.Span {
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.starts = append(r.starts, ev)
	r.mu.Unlock()
	return lifecycleSpan{r: r, name: ev.Name}
}
func (lifecycleSpan) SetAttribute(string, any) {}
func (s lifecycleSpan) AddEvent(ev core.TraceEvent) {
	s.r.mu.Lock()
	s.r.events = append(s.r.events, ev)
	s.r.mu.Unlock()
}
func (s lifecycleSpan) End(err error) {
	s.r.mu.Lock()
	s.r.ends++
	if s.r.endErrors == nil {
		s.r.endErrors = make(map[string][]error)
	}
	s.r.endErrors[s.name] = append(s.r.endErrors[s.name], err)
	s.r.mu.Unlock()
}
func (r *lifecycleRecorder) count(name string, turn core.TurnID) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, ev := range r.events {
		if ev.Name == name && ev.TurnID == turn {
			n++
		}
	}
	return n
}
func (r *lifecycleRecorder) startCount(name string, turn core.TurnID) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, ev := range r.starts {
		if ev.Name == name && ev.TurnID == turn && ev.Attributes()["provider"] == "google" {
			n++
		}
	}
	return n
}
func waitLifecycle(t *testing.T, f func() bool) {
	t.Helper()
	until := time.Now().Add(2 * time.Second)
	for !f() {
		if time.Now().After(until) {
			t.Fatal("trace lifecycle did not arrive")
		}
		time.Sleep(time.Millisecond)
	}
}

type panicGoogleSTTOpener struct{}

func (panicGoogleSTTOpener) OpenStream(context.Context, STTConfig) (STTStream, error) {
	panic("private-google-open-secret")
}
func assertGoogleSTTPanicTrace(t *testing.T, call func(), r *lifecycleRecorder) {
	t.Helper()
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected opener panic")
			}
		}()
		call()
	}()
	r.mu.Lock()
	ends := append([]error(nil), r.endErrors["stt.stream"]...)
	r.mu.Unlock()
	if len(ends) != 1 || ends[0] == nil || ends[0].Error() != "provider_failure" {
		t.Fatalf("open panic trace ends=%v", ends)
	}
}
func TestGoogleSTTTraceOpenAndRotationPanicEndFailure(t *testing.T) {
	for _, rotation := range []bool{false, true} {
		cfg := DefaultSTTConfig()
		cfg.StreamOpener = panicGoogleSTTOpener{}
		p, err := NewSTTProcessor(cfg)
		if err != nil {
			t.Fatal(err)
		}
		pipe, _ := core.NewPipeline(p)
		s, _ := core.NewSession(303, pipe, core.DefaultResourceBudget())
		if err := p.Attach(s, 0); err != nil {
			t.Fatal(err)
		}
		r := &lifecycleRecorder{}
		ctx := core.ContextWithTraceContext(context.Background(), r, core.TracePolicy{})
		p.turnID, p.streamGen = "turn-panic", 1
		if rotation {
			p.active = &activeStream{stream: newMockSTTStream(), turnID: "turn-panic", gen: 1}
		}
		if rotation {
			assertGoogleSTTPanicTrace(t, func() { _ = p.rotateStream(ctx) }, r)
		} else {
			// The open runs on a session task now: its panic fails the session,
			// not the caller, and the span still ends as a provider failure.
			if err := s.Start(); err != nil {
				t.Fatal(err)
			}
			_ = p.openPrimaryStream(ctx)
			waitLifecycle(t, func() bool {
				r.mu.Lock()
				defer r.mu.Unlock()
				return len(r.endErrors["stt.stream"]) == 1
			})
			r.mu.Lock()
			ends := append([]error(nil), r.endErrors["stt.stream"]...)
			r.mu.Unlock()
			if ends[0] == nil || ends[0].Error() != "provider_failure" {
				t.Fatalf("open panic trace ends=%v", ends)
			}
			waitLifecycle(t, func() bool { return s.Err() != nil }) // the panic still fails the session
			_ = s.Close()
		}
	}
}

func TestGoogleSTTTraceFirstFinalOnceAcrossResults(t *testing.T) {
	stream := newMockSTTStream()
	cfg := DefaultSTTConfig()
	cfg.StreamOpener = &mockSTTOpener{onOpenStream: func(STTConfig) *mockSTTStream { return stream }}
	p, err := NewSTTProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	pipe, _ := core.NewPipeline(p)
	s, _ := core.NewSession(301, pipe, core.DefaultResourceBudget())
	r := &lifecycleRecorder{}
	if err := s.SetTracer(r, core.TracePolicy{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Post(&core.TurnEventFrame{Kind: core.TurnEventStarted, Turn: "turn-stt", Gen: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.Post(&core.InputAudioFrame{Samples: make([]byte, 640), SampleRate: 16000, Channels: 1, Gen: 1}); err != nil {
		t.Fatal(err)
	}
	waitLifecycle(t, func() bool { return r.count("stt.stream", "turn-stt") == 1 })
	stream.PushResults(&STTResult{Transcript: "first", IsFinal: true}, &STTResult{Transcript: "second", IsFinal: false})
	waitLifecycle(t, func() bool { return r.count("stt.first_final", "turn-stt") == 1 })
	stream.PushResult(&STTResult{Transcript: "last", IsFinal: true})
	if got := r.count("stt.first_final", "turn-stt"); got != 1 {
		t.Fatalf("first final count=%d", got)
	}
}

func TestGoogleTTSTraceFirstAudioOnce(t *testing.T) {
	cfg := DefaultTTSConfig()
	cfg.Client = &mockTTSClient{synthesizeFn: func(context.Context, string, string, int) ([]byte, error) { return make([]byte, 2400), nil }}
	p, err := NewTTSProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	pipe, _ := core.NewPipeline(p)
	s, _ := core.NewSession(302, pipe, core.DefaultResourceBudget())
	r := &lifecycleRecorder{}
	if err := s.SetTracer(r, core.TracePolicy{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Post(&core.SynthesisUnitFrame{Turn: "turn-tts", Gen: 1, UnitID: 1, Text: "hello", IsFinal: true}); err != nil {
		t.Fatal(err)
	}
	waitLifecycle(t, func() bool { return r.count("tts.first_audio", "turn-tts") == 1 })
	if got := r.count("tts.first_audio", "turn-tts"); got != 1 {
		r.mu.Lock()
		events := append([]core.TraceEvent(nil), r.events...)
		r.mu.Unlock()
		t.Fatalf("first audio count=%d events=%v", got, events)
	}
	if got := r.startCount("tts.request", "turn-tts"); got != 1 {
		t.Fatalf("request count=%d", got)
	}
}
