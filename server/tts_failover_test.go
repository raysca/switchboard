package server

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/coretest"
	"govoice/core/resilience"
	"govoice/providers/cartesia"
	"govoice/providers/elevenlabs"
	"govoice/providers/google"
	"govoice/providers/xai"
	"govoice/server/flow"
	"govoice/vad"
)

// Every TTS provider's own error type must keep its failover class: circuit
// open, timeout and client errors may not flatten into server_error.
func TestTTSClassifyEveryProvider(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"circuit raw", resilience.ErrCircuitOpen, flow.ClassCircuit},
		{"circuit google", google.MapError("tts", resilience.ErrCircuitOpen), flow.ClassCircuit},
		{"circuit xai", xai.MapError("tts", resilience.ErrCircuitOpen), flow.ClassCircuit},
		{"circuit cartesia", cartesia.MapError("tts", resilience.ErrCircuitOpen, nil), flow.ClassCircuit},
		{"circuit elevenlabs", elevenlabs.MapError("tts", resilience.ErrCircuitOpen, nil), flow.ClassCircuit},
		{"timeout cartesia", cartesia.MapError("tts", resilience.ErrTimeout, nil), flow.ClassTimeout},
		{"timeout elevenlabs", elevenlabs.MapError("tts", fmt.Errorf("x: %w", resilience.ErrTimeout), nil), flow.ClassTimeout},
		{"timeout google", google.MapError("tts", resilience.ErrTimeout), flow.ClassTimeout},
		{"failover watchdog", fmt.Errorf("core: tts a stalled on unit 1: %w", resilience.ErrTimeout), flow.ClassTimeout},
		{"client cartesia", &cartesia.ProviderError{Capability: "tts", ClientError: true}, flow.ClassClient},
		{"client elevenlabs", &elevenlabs.ProviderError{Capability: "tts", ClientError: true}, flow.ClassClient},
		{"server cartesia", &cartesia.ProviderError{Capability: "tts", Retryable: true}, flow.ClassServer},
		{"server elevenlabs", &elevenlabs.ProviderError{Capability: "tts"}, flow.ClassServer},
	} {
		if got := ttsClassify(tc.err); got != tc.want {
			t.Errorf("%s: class %q, want %q", tc.name, got, tc.want)
		}
	}
	if ttsClassify(errors.New("context canceled")) != flow.ClassServer || ttsClassify(nil) != "" {
		t.Error("plain errors are server_error, nil is none")
	}
}

func ttsFailoverRegistry(primaryFails, fallbackFails bool) *Registry {
	reg := NewRegistry()
	reg.Register("google.stt", func(*Config, StageSpec) (core.Processor, error) { return passSTTStage{}, nil })
	reg.Register("google.llm", func(*Config, StageSpec) (core.Processor, error) {
		return coretest.NewLLMProcessor(coretest.LLMConfig{ResponseChunks: []string{"First sentence. ", "Second sentence. ", "Third one."}}), nil
	})
	tts := func(fail bool) ProcessorFactory {
		return func(*Config, StageSpec) (core.Processor, error) {
			cfg := coretest.TTSConfig{FramesPerUnit: 2}
			if fail {
				cfg.FailWith = errors.New("503 upstream")
			}
			return coretest.NewTTSProcessor(cfg), nil
		}
	}
	reg.Register("google.tts", tts(primaryFails))
	reg.Register("xai.tts", tts(fallbackFails))
	return reg
}

func TestBuildTTSWrapsOnlyWithFallbacks(t *testing.T) {
	reg := ttsFailoverRegistry(false, false)
	build := func(stage string, sp StageSpec) (core.Processor, error) {
		if sp.Provider == "nope" {
			return nil, errors.New("unbuildable fallback")
		}
		return reg.factories[factoryKey(sp.Provider, stage)](DefaultConfig(ProfileDevelopment), sp)
	}
	m := NewMetrics()
	reg.BindMetrics(m)
	plain, err := reg.buildTTS(nil, CallSpec{TTS: StageSpec{Provider: "google"}}, build)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := plain.(*core.TTSFailover); ok {
		t.Fatal("flow without fallbacks must build the old pipeline")
	}
	wrapped, err := reg.buildTTS(nil, CallSpec{TTS: StageSpec{Provider: "google"}, TTSFallbacks: []StageSpec{{Provider: "xai"}, {Provider: "nope"}}}, build)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := wrapped.(*core.TTSFailover); !ok {
		t.Fatalf("got %T", wrapped)
	}
	var out strings.Builder
	m.WritePrometheus(&out)
	if !strings.Contains(out.String(), "voice_tts_fallbacks_skipped_total 1") {
		t.Fatalf("skip metric missing:\n%s", out.String())
	}
}

// End to end through the production call pipeline: the primary TTS fails every
// unit before audio; each unit is spoken exactly once, in order, from the
// fallback, with no error reaching the client, and the switch is counted.
func TestCallPipelineTTSFailoverSpeaksEachUnitOnce(t *testing.T) {
	reg := ttsFailoverRegistry(true, false)
	m := NewMetrics()
	reg.BindMetrics(m)
	call := CallSpec{STT: StageSpec{Provider: "google"}, LLM: StageSpec{Provider: "google"}, TTS: StageSpec{Provider: "google"},
		TTSFallbacks: []StageSpec{{Provider: "xai", Voice: "eve"}}, MaxMessages: 10, MaxTokens: 1000}
	vadProc := vad.NewProcessor(&mockVADExecutor{ready: true, available: 1, total: 1}, vad.DefaultProcessorConfig())
	pipe, err := reg.BuildCallPipeline(DefaultConfig(ProfileDevelopment), call, vadProc, coretest.NewTransportOutput(coretest.TransportConfig{}))
	if err != nil {
		t.Fatal(err)
	}
	sess, err := core.NewSession(6, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var finals []uint64
	seen := map[uint64]bool{}
	errs := 0
	sess.OnFrame = func(f core.Frame, _ core.Direction) {
		mu.Lock()
		defer mu.Unlock()
		switch v := f.(type) {
		case *core.SynthesisUnitFrame:
			seen[v.UnitID] = true
		case *core.OutputAudioFrame:
			// OnFrame fires per hop: count a unit's final frame once.
			if v.IsFinal && (len(finals) == 0 || finals[len(finals)-1] != v.UnitID) {
				finals = append(finals, v.UnitID)
			}
		case *core.ErrorFrame:
			errs++
		}
	}
	if err := sess.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = sess.Close()
		if n := sess.LeakedTasks(); n != 0 {
			t.Errorf("leaked %d tasks", n)
		}
	}()
	gen := sess.CurrentGeneration()
	if err := sess.Reenter(&core.FinalTranscriptionFrame{Text: "hello", Turn: "text-1", Gen: gen, Committed: true, Timestamp: time.Now()},
		core.Downstream, CallUserContextIndex, gen); err != nil {
		t.Fatal(err)
	}
	state := func() (int, []uint64, int) {
		mu.Lock()
		defer mu.Unlock()
		return len(seen), append([]uint64(nil), finals...), errs
	}
	for end := time.Now().Add(3 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if u, f, _ := state(); u > 0 && len(f) == u {
			break
		}
		if time.Now().After(end) {
			u, f, e := state()
			t.Fatalf("units=%d finals=%v errs=%d", u, f, e)
		}
	}
	time.Sleep(50 * time.Millisecond)
	u, f, e := state()
	for i := 1; i < len(f); i++ {
		if f[i] != f[i-1]+1 {
			t.Fatalf("units out of order or duplicated: %v", f)
		}
	}
	if len(f) != u || e != 0 {
		t.Fatalf("units=%d finals=%v errs=%d", u, f, e)
	}
	var out strings.Builder
	m.WritePrometheus(&out)
	if !strings.Contains(out.String(), `voice_tts_failover_total{from="google",to="xai",class="server_error"} 1`) {
		t.Fatalf("metric missing (one switch: later units go straight to the fallback):\n%s", out.String())
	}
}

// The one pipeline TTS rate must be one every provider can output, so a
// candidate can never be built at a rate the others cannot match.
func TestTTSRateValidatedAtConfigLoad(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Providers.GoogleTTS.SampleRate = 11025
	if err := ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), "sample_rate") {
		t.Fatalf("11025 accepted: %v", err)
	}
	cfg.Providers.GoogleTTS.SampleRate = 16000
	if err := ValidateConfig(cfg); err != nil && strings.Contains(err.Error(), "sample_rate") {
		t.Fatal(err)
	}
}
