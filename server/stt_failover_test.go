package server

import (
	"bytes"
	"context"
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
	"govoice/providers/deepgram"
	"govoice/providers/elevenlabs"
	"govoice/providers/google"
	"govoice/providers/xai"
	"govoice/server/flow"
	"govoice/vad"
)

// Each STT provider wraps errors differently; the class must survive that.
func TestSTTClassifyPerProvider(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"deepgram circuit open (raw from the dial)", resilience.ErrCircuitOpen, flow.ClassCircuit},
		{"deepgram circuit open wrapped", fmt.Errorf("dial: %w", resilience.ErrCircuitOpen), flow.ClassCircuit},
		{"deepgram dial 503", deepgram.MapError("stt", errors.New("dial"), nil), flow.ClassServer},
		{"deepgram hung send (500ms write deadline)", deepgram.MapError("stt", fmt.Errorf("write: %w", context.DeadlineExceeded), nil), flow.ClassTimeout},
		{"deepgram attempt timeout", deepgram.MapError("stt", resilience.ErrTimeout, nil), flow.ClassTimeout},
		{"deepgram bad request", &deepgram.ProviderError{Capability: "stt", ClientError: true}, flow.ClassClient},
		{"deepgram caller cancelled", deepgram.MapError("stt", context.Canceled, nil), ""},
		{"elevenlabs bad request", &elevenlabs.ProviderError{Capability: "stt", ClientError: true}, flow.ClassClient},
		{"elevenlabs dial 503", elevenlabs.MapError("stt", errors.New("dial"), nil), flow.ClassServer},
		{"elevenlabs attempt timeout", elevenlabs.MapError("stt", resilience.ErrTimeout, nil), flow.ClassTimeout},
		{"elevenlabs circuit open", elevenlabs.MapError("stt", resilience.ErrCircuitOpen, nil), flow.ClassCircuit},
		{"elevenlabs cancelled", elevenlabs.MapError("stt", context.Canceled, nil), ""},
		{"cartesia bad request", &cartesia.ProviderError{Capability: "stt", ClientError: true}, flow.ClassClient},
		{"cartesia dial 503", cartesia.MapError("stt", errors.New("dial"), nil), flow.ClassServer},
		{"cartesia attempt timeout", cartesia.MapError("stt", resilience.ErrTimeout, nil), flow.ClassTimeout},
		{"google circuit open", google.MapError("stt", resilience.ErrCircuitOpen), flow.ClassCircuit},
		{"google deadline", google.MapError("stt", context.DeadlineExceeded), flow.ClassTimeout},
		{"google cancelled", google.MapError("stt", context.Canceled), ""},
		{"google stream drop", google.MapError("stt", errors.New("rpc error: unavailable")), flow.ClassServer},
		{"xai stream drop is not a client error", xai.MapError("stt", errors.New("websocket: close 1006")), flow.ClassServer},
		{"xai circuit open keeps the chain", xai.MapError("stt", resilience.ErrCircuitOpen), flow.ClassCircuit},
		{"xai timeout keeps the chain", xai.MapError("stt", resilience.ErrTimeout), flow.ClassTimeout},
		{"xai 401", &xai.ProviderError{Capability: "stt", StatusCode: 401, Class: xai.ClassificationNonRetryable}, flow.ClassClient},
		{"xai 429 is an outage", &xai.ProviderError{Capability: "stt", StatusCode: 429, Class: xai.ClassificationNonRetryable}, flow.ClassServer},
	} {
		if got := sttClassify(tc.err); got != tc.want {
			t.Errorf("%s: sttClassify = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The session's own task budget is not a provider outage: a jump would not
// help, so it never fails over, while a real provider error still does.
func TestSTTFailoverIgnoresLocalTaskBudgetError(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		wantActive string
	}{
		{"local task budget", fmt.Errorf("stt: dial: %w", core.ErrTooManyTasks), "a"},
		{"provider error", errors.New("stt: stream dropped"), "b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, err := core.NewSTTFailover(core.STTFailoverConfig{
				Classify: sttClassify,
				Candidates: []core.STTCandidate{
					{Provider: "a", Proc: coretest.NewSTTProcessor(coretest.STTConfig{Provider: "a", Words: sttWords, FailAfter: 1, FailErr: tc.err})},
					{Provider: "b", Proc: coretest.NewSTTProcessor(coretest.STTConfig{Provider: "b", Words: sttWords})},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			pipe, err := core.NewPipeline(w, &coretest.PassthroughProcessor{})
			if err != nil {
				t.Fatal(err)
			}
			sess, err := core.NewSession(902, pipe, core.DefaultResourceBudget())
			if err != nil {
				t.Fatal(err)
			}
			if err := sess.Start(); err != nil {
				t.Fatal(err)
			}
			defer sess.Close()
			gen := sess.CurrentGeneration()
			post := func(f core.Frame) {
				t.Helper()
				if err := sess.Reenter(f, core.Downstream, 0, gen); err != nil {
					t.Fatal(err)
				}
			}
			post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: gen})
			for i := 0; i < 3; i++ {
				post(&core.InputAudioFrame{Samples: bytes.Repeat([]byte{byte(i)}, 64), Encoding: core.EncodingPCM16LE, SampleRate: 16000, Channels: 1})
			}
			if tc.wantActive == "b" {
				for end := time.Now().Add(3 * time.Second); w.Active() != "b"; time.Sleep(2 * time.Millisecond) {
					if time.Now().After(end) {
						t.Fatal("a provider error did not fail over")
					}
				}
				return
			}
			time.Sleep(300 * time.Millisecond) // a jump would have happened by now
			if got := w.Active(); got != "a" {
				t.Fatalf("active = %q: a local error caused a jump", got)
			}
		})
	}
}

var sttWords = []string{"book", "a", "table", "for", "two"}

func sttRegistry(primary coretest.STTConfig) *Registry {
	reg := NewRegistry()
	reg.Register("google.stt", func(*Config, StageSpec) (core.Processor, error) {
		primary.Provider, primary.Words = "google", sttWords
		return coretest.NewSTTProcessor(primary), nil
	})
	reg.Register("deepgram.stt", func(*Config, StageSpec) (core.Processor, error) {
		return coretest.NewSTTProcessor(coretest.STTConfig{Provider: "deepgram", Words: sttWords}), nil
	})
	reg.Register("google.llm", func(*Config, StageSpec) (core.Processor, error) {
		return coretest.NewLLMProcessor(coretest.LLMConfig{ResponseChunks: []string{"Heard: "}, EchoUser: true}), nil
	})
	reg.Register("google.tts", func(*Config, StageSpec) (core.Processor, error) {
		return coretest.NewTTSProcessor(coretest.TTSConfig{FramesPerUnit: 1}), nil
	})
	return reg
}

func TestBuildSTTWrapsOnlyWithFallbacksAndCountsSkips(t *testing.T) {
	reg := sttRegistry(coretest.STTConfig{})
	m := NewMetrics()
	reg.BindMetrics(m)
	build := func(stage string, sp StageSpec) (core.Processor, error) {
		if sp.Provider == "nope" {
			return nil, errors.New("unbuildable fallback")
		}
		return reg.factories[factoryKey(sp.Provider, stage)](DefaultConfig(ProfileDevelopment), sp)
	}
	plain, err := reg.buildSTT(nil, CallSpec{STT: StageSpec{Provider: "google"}}, build)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := plain.(*core.STTFailover); ok {
		t.Fatal("flow without fallbacks must build the old pipeline")
	}
	wrapped, err := reg.buildSTT(nil, CallSpec{STT: StageSpec{Provider: "google"}, STTFallbacks: []StageSpec{{Provider: "deepgram"}, {Provider: "nope"}}}, build)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := wrapped.(*core.STTFailover); !ok {
		t.Fatalf("got %T", wrapped)
	}
	// Every fallback unbuildable: the bare primary.
	if p, err := reg.buildSTT(nil, CallSpec{STT: StageSpec{Provider: "google"}, STTFallbacks: []StageSpec{{Provider: "nope"}}}, build); err != nil {
		t.Fatal(err)
	} else if _, ok := p.(*core.STTFailover); ok {
		t.Fatal("no usable fallback must not wrap")
	}
	var out strings.Builder
	m.WritePrometheus(&out)
	if !strings.Contains(out.String(), "voice_stt_fallbacks_skipped_total 2") {
		t.Fatalf("skip metric missing:\n%s", out.String())
	}
}

// End to end through the production call pipeline: the primary STT dies in the
// middle of the utterance, the fallback gets the buffered audio, and the LLM is
// asked exactly once with the whole utterance.
func TestCallPipelineSTTFailoverLLMGetsWholeUtteranceOnce(t *testing.T) {
	reg := sttRegistry(coretest.STTConfig{FailAfter: 3})
	m := NewMetrics()
	reg.BindMetrics(m)
	call := CallSpec{STT: StageSpec{Provider: "google"}, LLM: StageSpec{Provider: "google"}, TTS: StageSpec{Provider: "google"},
		STTFallbacks: []StageSpec{{Provider: "deepgram"}}, MaxMessages: 10, MaxTokens: 1000}
	vadProc := vad.NewProcessor(&mockVADExecutor{ready: true, available: 1, total: 1}, vad.DefaultProcessorConfig())
	pipe, err := reg.BuildCallPipeline(DefaultConfig(ProfileDevelopment), call, vadProc, coretest.NewTransportOutput(coretest.TransportConfig{}))
	if err != nil {
		t.Fatal(err)
	}
	stt, ok := pipe.At(1).(*core.STTFailover)
	if !ok {
		t.Fatalf("stage 1 is %T", pipe.At(1))
	}
	sess, err := core.NewSession(8, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var spoken strings.Builder
	errs := 0
	sess.OnFrame = func(f core.Frame, _ core.Direction) {
		mu.Lock()
		defer mu.Unlock()
		switch v := f.(type) {
		case *core.SynthesisUnitFrame:
			spoken.WriteString(v.Text)
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
	var sent []byte
	post := func(f core.Frame) {
		t.Helper()
		if err := sess.Reenter(f, core.Downstream, 1, gen); err != nil { // straight into the STT stage
			t.Fatal(err)
		}
	}
	audio := func(w int) {
		b := bytes.Repeat([]byte{byte(len(sent) / 64)}, 64)
		b[0] = byte(w)
		sent = append(sent, b...)
		post(&core.InputAudioFrame{Samples: b, Encoding: core.EncodingPCM16LE, SampleRate: 16000, Channels: 1})
	}
	post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: gen})
	for w := 0; w < 3; w++ {
		audio(w)
	}
	for end := time.Now().Add(3 * time.Second); stt.Active() != "deepgram"; time.Sleep(2 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("never failed over")
		}
	}
	audio(3)
	audio(4)
	// A real utterance: the turn manager (on by default) ignores speech shorter than
	// its 150 ms minimum, and this test is about failover, not backchannels.
	time.Sleep(300 * time.Millisecond)
	post(&core.UserStoppedSpeakingFrame{Gen: gen})

	const want = "Heard: book a table for two"
	for end := time.Now().Add(4 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		mu.Lock()
		s := spoken.String()
		mu.Unlock()
		if strings.Contains(s, want) {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("spoken %q, want %q", s, want)
		}
	}
	time.Sleep(300 * time.Millisecond) // a duplicate turn would arrive by now
	mu.Lock()
	s, e := spoken.String(), errs
	mu.Unlock()
	if strings.TrimSpace(s) != want || e != 0 {
		t.Fatalf("spoken %q (want exactly %q once), errors %d", s, want, e)
	}
	var out strings.Builder
	m.WritePrometheus(&out)
	if !strings.Contains(out.String(), `voice_stt_failover_total{from="google",to="deepgram",class="server_error"} 1`) {
		t.Fatalf("failover metric missing:\n%s", out.String())
	}
}

func TestConfigRejectsOutOfRangeSTTReplayBuffer(t *testing.T) {
	for _, d := range []time.Duration{-time.Second, maxSTTReplay + time.Second} {
		cfg := DefaultConfig(ProfileDevelopment)
		cfg.STT.ReplayBuffer = d
		if err := ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), "stt.replay_buffer") {
			t.Errorf("replay_buffer %v: err = %v", d, err)
		}
	}
}
