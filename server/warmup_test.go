package server

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
	"govoice/vad"
)

type warmProc struct {
	fakeTransportSink
	calls atomic.Int32
	err   error
	block chan struct{}
}

func (w *warmProc) Warm(ctx context.Context) error {
	w.calls.Add(1)
	if w.block != nil {
		<-w.block
	}
	return w.err
}

func TestWarmupConfigValidate(t *testing.T) {
	f := false
	for name, tc := range map[string]struct {
		w  WarmupConfig
		ok bool
	}{
		"zero value":            {WarmupConfig{}, true},
		"predial":               {WarmupConfig{Predial: true}, true},
		"on_start w/o predial":  {WarmupConfig{OnStart: true}, false},
		"predial when disabled": {WarmupConfig{Enabled: &f, Predial: true}, false},
		"disabled":              {WarmupConfig{Enabled: &f}, true},
		"timeout too big":       {WarmupConfig{Timeout: time.Minute}, false},
		"spare ttl too small":   {WarmupConfig{SpareTTL: time.Millisecond}, false},
		"all set":               {WarmupConfig{Predial: true, OnStart: true, Timeout: time.Second, SpareTTL: 5 * time.Second}, true},
	} {
		if err := tc.w.Validate(); (err == nil) != tc.ok {
			t.Errorf("%s: err=%v", name, err)
		}
	}
}

// A slow or failing Warm must not delay or fail call setup, and runs only
// when predial is on.
func TestWarmerErrorOrHangDoesNotAffectCallSetup(t *testing.T) {
	for _, predial := range []bool{false, true} {
		cfg := DefaultConfig(ProfileDevelopment)
		cfg.Warmup.Predial = predial
		cfg.Warmup.Timeout = time.Second
		hang := &warmProc{err: errors.New("boom"), block: make(chan struct{})}
		bad := &warmProc{err: errors.New("boom")}
		reg := NewRegistry()
		reg.Register("google.stt", func(*Config, StageSpec) (core.Processor, error) { return hang, nil })
		reg.Register("google.llm", func(*Config, StageSpec) (core.Processor, error) { return &contextSinkProc{}, nil })
		reg.Register("google.tts", func(*Config, StageSpec) (core.Processor, error) { return bad, nil })
		vadProc := vad.NewProcessor(&mockVADExecutor{ready: true, available: 4, total: 4}, vad.DefaultProcessorConfig())
		start := time.Now()
		pipe, err := reg.BuildCallPipeline(cfg, CallSpec{STT: StageSpec{Provider: "google"}, LLM: StageSpec{Provider: "google"}, TTS: StageSpec{Provider: "google"}, MaxMessages: 10, MaxTokens: 100}, vadProc, &fakeTransportSink{})
		if err != nil || pipe == nil || time.Since(start) > 500*time.Millisecond {
			t.Fatalf("predial=%v: setup err=%v took %v", predial, err, time.Since(start))
		}
		time.Sleep(50 * time.Millisecond)
		want := int32(0)
		if predial {
			want = 1
		}
		if hang.calls.Load() != want || bad.calls.Load() != want {
			t.Fatalf("predial=%v: warm calls hang=%d bad=%d, want %d", predial, hang.calls.Load(), bad.calls.Load(), want)
		}
		close(hang.block)
	}
}
