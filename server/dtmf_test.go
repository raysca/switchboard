package server

import (
	"context"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/audio/dtmf"
	"govoice/core/coretest"
	"govoice/vad"
)

func dtmfTestPipeline(t *testing.T, cfg *Config) (*core.Pipeline, []StageSpec) {
	t.Helper()
	reg := NewRegistry()
	var llmSpecs []StageSpec
	reg.Register("google.stt", func(*Config, StageSpec) (core.Processor, error) { return &coretest.PassthroughProcessor{}, nil })
	reg.Register("google.tts", func(*Config, StageSpec) (core.Processor, error) { return &coretest.PassthroughProcessor{}, nil })
	reg.Register("google.llm", func(_ *Config, s StageSpec) (core.Processor, error) {
		llmSpecs = append(llmSpecs, s)
		return &coretest.PassthroughProcessor{}, nil
	})
	vadProc := vad.NewProcessor(&mockVADExecutor{ready: true, available: 1, total: 1}, vad.DefaultProcessorConfig())
	call := CallSpec{STT: StageSpec{Provider: "google"}, TTS: StageSpec{Provider: "google"},
		LLM:          StageSpec{Provider: "google", Tools: []core.ToolDefinition{{Name: "lookup"}}},
		LLMFallbacks: []StageSpec{{Provider: "google"}}}
	pipe, err := reg.BuildCallPipeline(cfg, call, vadProc, &fakeTransportSink{})
	if err != nil {
		t.Fatal(err)
	}
	return pipe, llmSpecs
}

func TestDTMFDisabledByDefaultLeavesPipelineAndToolsUnchanged(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Idle.UserTimeout = 0 // quiet-caller handling is on by default and adds a processor
	pipe, specs := dtmfTestPipeline(t, cfg)
	if pipe.Len() != 11 { // turn processor (manager on by default) and the async trigger (every call)
		t.Fatalf("pipeline has %d processors, want 11", pipe.Len())
	}
	for _, s := range specs {
		for _, d := range s.Tools {
			if d.Name == dtmf.ToolName {
				t.Fatal("send_dtmf registered while disabled")
			}
		}
	}
}

func TestDTMFEnabledStagesAndTool(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.DTMF.Detect, cfg.DTMF.Aggregate, cfg.DTMF.SendTool = true, true, true
	pipe, specs := dtmfTestPipeline(t, cfg)
	procs := pipe.Processors()
	if _, ok := procs[0].(*dtmf.InputProcessor); !ok {
		t.Fatalf("processor 0 is %T, want the detector before VAD", procs[0])
	}
	if _, ok := procs[1].(*vad.Processor); !ok {
		t.Fatalf("processor 1 is %T, want VAD", procs[1])
	}
	for i, p := range procs {
		if _, ok := p.(*core.UserContextProcessor); ok {
			if _, ok := procs[i-1].(*dtmf.Aggregator); !ok {
				t.Fatalf("processor before user context is %T, want the aggregator", procs[i-1])
			}
		}
	}
	if _, ok := procs[len(procs)-2].(*dtmf.OutputProcessor); !ok {
		t.Fatalf("processor before transport is %T, want the tone output", procs[len(procs)-2])
	}
	if len(specs) != 2 {
		t.Fatalf("built %d llm stages", len(specs))
	}
	for i, s := range specs { // primary and fallback both get the tool
		var has bool
		for _, d := range s.Tools {
			has = has || d.Name == dtmf.ToolName
		}
		if !has || s.ToolExecutor == nil {
			t.Fatalf("llm stage %d lacks send_dtmf", i)
		}
	}
	if len(specs[0].Tools) != 2 {
		t.Fatalf("existing tools lost: %+v", specs[0].Tools)
	}
	// Existing tools still reach the original executor chain (nil here: unknown).
	r, _ := specs[0].ToolExecutor.Execute(context.Background(), core.ToolCall{Name: "lookup"})
	if r.Status != core.ToolFailed {
		t.Fatalf("unexpected status %v", r.Status)
	}
}

// With the turn manager on, a key-press run still commits a user turn and
// reaches the LLM: it bypasses the finalizer, so no VAD turn id is needed.
func TestDTMFCommitsTurnWithTurnManager(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Turn.Manager = true
	cfg.DTMF.Detect, cfg.DTMF.Aggregate = true, true
	reg := NewRegistry()
	llm := &coretest.PassthroughProcessor{}
	reg.Register("google.stt", func(*Config, StageSpec) (core.Processor, error) { return &coretest.PassthroughProcessor{}, nil })
	reg.Register("google.tts", func(*Config, StageSpec) (core.Processor, error) { return &coretest.PassthroughProcessor{}, nil })
	reg.Register("google.llm", func(*Config, StageSpec) (core.Processor, error) { return llm, nil })
	vadProc := vad.NewProcessor(&mockVADExecutor{ready: true, available: 1, total: 1}, vad.DefaultProcessorConfig())
	pipe, err := reg.BuildCallPipeline(cfg, CallSpec{STT: StageSpec{Provider: "google"}, TTS: StageSpec{Provider: "google"}, LLM: StageSpec{Provider: "google"}}, vadProc, &fakeTransportSink{})
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
	defer s.Close()
	for _, d := range []string{"4", "2", "#"} {
		if err := s.Post(&core.InputDTMFFrame{Digit: d}); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		for _, f := range llm.GetFrames() {
			if m, ok := f.(*core.UserMessageFrame); ok && m.Content == "DTMF: 42#" {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("DTMF run never reached the LLM as a user message")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// send_dtmf through the registry-built pipeline: the tool call returns after the
// tones, as paced NonSpeech audio, have reached the transport stage.
func TestSendDTMFToolPlaysTonesToTransport(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.DTMF.SendTool = true
	reg := NewRegistry()
	var spec StageSpec
	reg.Register("google.stt", func(*Config, StageSpec) (core.Processor, error) { return &coretest.PassthroughProcessor{}, nil })
	reg.Register("google.tts", func(*Config, StageSpec) (core.Processor, error) { return &coretest.PassthroughProcessor{}, nil })
	reg.Register("google.llm", func(_ *Config, s StageSpec) (core.Processor, error) {
		spec = s
		return &coretest.PassthroughProcessor{}, nil
	})
	transport := &coretest.PassthroughProcessor{}
	vadProc := vad.NewProcessor(&mockVADExecutor{ready: true, available: 1, total: 1}, vad.DefaultProcessorConfig())
	pipe, err := reg.BuildCallPipeline(cfg, CallSpec{STT: StageSpec{Provider: "google"}, TTS: StageSpec{Provider: "google"}, LLM: StageSpec{Provider: "google"}}, vadProc, transport)
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
	defer s.Close()
	r, err := spec.ToolExecutor.Execute(context.Background(), core.ToolCall{ID: "c1", Name: dtmf.ToolName, Arguments: []byte(`{"digits":"5#"}`)})
	if err != nil || r.Status != core.ToolSucceeded {
		t.Fatalf("%+v %v", r, err)
	}
	want, _ := dtmf.Generate("5#", dtmf.DefaultGeneratorConfig(24000))
	var got int
	for _, f := range transport.GetFrames() {
		if a, ok := f.(*core.OutputAudioFrame); ok && a.NonSpeech {
			got += len(a.Samples)
		}
	}
	if got != len(want) {
		t.Fatalf("transport received %d of %d tone bytes", got, len(want))
	}
}
