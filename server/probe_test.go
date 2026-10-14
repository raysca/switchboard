package server

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"govoice/core"
)

// probeFake is a stage processor for probeProcessor: on its trigger frame it emits `out` after
// `delay` (or fails with err, or never answers when hang is set).
type probeFake struct {
	trigger func(core.Frame) bool
	out     func() core.Frame
	delay   time.Duration
	err     error
	hang    bool
	sess    core.SessionController
}

func (p *probeFake) Attach(s core.SessionController, _ int) error { p.sess = s; return nil }
func (p *probeFake) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	if p.trigger(f) && !p.hang {
		if p.err != nil {
			return p.err
		}
		select {
		case <-time.After(p.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
		if p.out != nil {
			_ = e.PushDirection(p.out(), core.Downstream)
		}
		return nil
	}
	return e.PushDirection(f, dir)
}

func isUserMessage(f core.Frame) bool   { _, ok := f.(*core.UserMessageFrame); return ok }
func isSynthesisUnit(f core.Frame) bool { _, ok := f.(*core.SynthesisUnitFrame); return ok }
func isInputAudio(f core.Frame) bool    { _, ok := f.(*core.InputAudioFrame); return ok }

func TestProbeProcessorTimesTheFirstTokenOfAnLLM(t *testing.T) {
	p := &probeFake{trigger: isUserMessage, delay: 30 * time.Millisecond, out: func() core.Frame { return &core.LLMTextDeltaFrame{Delta: "ok"} }}
	d, err := probeProcessor(t.Context(), p, "llm")
	if err != nil || d < 30*time.Millisecond {
		t.Fatalf("d=%v err=%v", d, err)
	}
}

func TestProbeProcessorTimesTheFirstAudioOfATTS(t *testing.T) {
	p := &probeFake{trigger: isSynthesisUnit, delay: 10 * time.Millisecond, out: func() core.Frame { return &core.OutputAudioFrame{Samples: []byte{1, 2}} }}
	if d, err := probeProcessor(t.Context(), p, "tts"); err != nil || d <= 0 {
		t.Fatalf("d=%v err=%v", d, err)
	}
}

func TestProbeProcessorAcceptsSilenceOnAnSTTAndReportsNoLatency(t *testing.T) {
	p := &probeFake{trigger: func(core.Frame) bool { return false }} // swallows nothing, errors nothing
	if d, err := probeProcessor(t.Context(), p, "stt"); err != nil || d != 0 {
		t.Fatalf("d=%v err=%v", d, err)
	}
	bad := &probeFake{trigger: isInputAudio, err: errors.New("401 unauthorized")}
	if _, err := probeProcessor(t.Context(), bad, "stt"); err == nil {
		t.Fatal("an STT that fails on audio must fail the probe")
	}
}

func TestProbeProcessorReportsAProviderError(t *testing.T) {
	p := &probeFake{trigger: isUserMessage, err: errors.New("model not found")}
	if _, err := probeProcessor(t.Context(), p, "llm"); err == nil || !strings.Contains(err.Error(), "model not found") {
		t.Fatalf("err=%v", err)
	}
}

func TestProbeProcessorTimesOutWithoutLeakingTasks(t *testing.T) {
	old := probeTimeout
	probeTimeout = 150 * time.Millisecond
	defer func() { probeTimeout = old }()
	before := runtime.NumGoroutine()
	p := &probeFake{trigger: isUserMessage, hang: true}
	start := time.Now()
	_, err := probeProcessor(t.Context(), p, "llm")
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
		t.Fatalf("err=%v after %v", err, time.Since(start))
	}
	for end := time.Now().Add(2 * time.Second); runtime.NumGoroutine() > before && time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
	}
	if n := runtime.NumGoroutine(); n > before {
		t.Fatalf("%d goroutines left after the probe, %d before", n, before)
	}
}

func TestProbeDetailIsRedactedAndShort(t *testing.T) {
	long := strings.Repeat("x", 1000)
	if got := probeDetail("client_error", errors.New(long), func(m string) string { return m }); len(got) > 330 {
		t.Fatalf("detail is %d bytes", len(got))
	}
	got := probeDetail("client_error", errors.New("bad key sk-secret-canary"), func(m string) string { return strings.ReplaceAll(m, "sk-secret-canary", "[redacted]") })
	if strings.Contains(got, "sk-secret-canary") || !strings.HasPrefix(got, "client_error: ") {
		t.Fatalf("detail %q", got)
	}
}

func TestCircuitStateReadsTheGaugesAndTheWorstEndpointWins(t *testing.T) {
	var m Metrics
	for _, tc := range []struct {
		code int64
		want string
	}{{0, "closed"}, {1, "open"}, {2, "half_open"}} {
		m.LLMCircuitStateGoogle.Store(tc.code)
		if got := m.circuitState("google", "llm"); got != tc.want {
			t.Errorf("code %d: %q, want %q", tc.code, got, tc.want)
		}
	}
	if got := m.circuitState("echo", "llm"); got != "unknown" {
		t.Errorf("echo: %q", got)
	}
	m.circuits.set("llm", "nvidia", "a:443", 0)
	m.circuits.set("llm", "nvidia", "b:443", 1)
	if got := m.circuitState("nvidia", "llm"); got != "open" {
		t.Errorf("worst of two endpoints: %q", got)
	}
}

func TestProbeProcessorTimesTheFirstStreamedDeltaNotTheCompleteText(t *testing.T) {
	p := &probeFake{trigger: isUserMessage, delay: 10 * time.Millisecond, out: func() core.Frame { return &core.LLMTextDeltaFrame{Delta: "o"} }}
	if d, err := probeProcessor(t.Context(), p, "llm"); err != nil || d <= 0 {
		t.Fatalf("d=%v err=%v", d, err)
	}
}

func TestProbeProcessorIgnoresAnEmptyAudioMarker(t *testing.T) {
	old := probeTimeout
	probeTimeout = 200 * time.Millisecond
	defer func() { probeTimeout = old }()
	p := &probeFake{trigger: isSynthesisUnit, out: func() core.Frame { return &core.OutputAudioFrame{} }}
	if _, err := probeProcessor(t.Context(), p, "tts"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("an empty audio frame counted as first audio: err=%v", err)
	}
}

// blockedClose never returns from Close, like a provider wedged on its own lock.
type blockedClose struct {
	probeFake
	release chan struct{}
}

func (b *blockedClose) Close() error { <-b.release; return nil }

func TestProbeProcessorReturnsEvenWhenTheProviderCloseHangs(t *testing.T) {
	b := &blockedClose{probeFake: probeFake{trigger: isUserMessage, out: func() core.Frame { return &core.LLMTextDeltaFrame{Delta: "o"} }}, release: make(chan struct{})}
	defer close(b.release)
	done := make(chan error, 1)
	go func() { _, err := probeProcessor(t.Context(), b, "llm"); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("probeProcessor hung on the provider's Close")
	}
}
