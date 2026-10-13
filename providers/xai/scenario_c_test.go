package xai

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/coretest"
)

// Mock Google STT Processor for Scenario C
type mockGoogleSTT struct {
	transcripts []string
}

func (m *mockGoogleSTT) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	switch frame := f.(type) {
	case *core.InputAudioFrame:
		// When audio is received, simulate Google STT final transcription
		if len(m.transcripts) > 0 {
			text := m.transcripts[0]
			m.transcripts = m.transcripts[1:]
			return e.Push(&core.FinalTranscriptionFrame{
				Text:      text,
				Turn:      "turn-c-1",
				Gen:       frame.Gen,
				Timestamp: time.Now(),
			})
		}
	}
	return e.PushDirection(f, dir)
}

// Mock Google TTS Processor for Scenario C
type mockGoogleTTS struct {
	mu            sync.Mutex
	synthesized   []string
	synthesizedWg sync.WaitGroup
}

func (m *mockGoogleTTS) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	switch frame := f.(type) {
	case *core.SynthesisUnitFrame:
		m.mu.Lock()
		m.synthesized = append(m.synthesized, frame.Text)
		m.mu.Unlock()
		m.synthesizedWg.Done()

		// Emit headerless PCM audio frame (24000 Hz)
		return e.Push(&core.OutputAudioFrame{
			Samples:    make([]byte, 960), // 20ms chunk
			Encoding:   core.EncodingPCM16LE,
			SampleRate: 24000,
			Channels:   1,
			Duration:   20 * time.Millisecond,
			Timestamp:  time.Now(),
			Turn:       frame.Turn,
			Gen:        frame.Gen,
			UnitID:     frame.UnitID,
			IsFinal:    true,
		})
	}
	return e.PushDirection(f, dir)
}

func TestScenarioC_MixedProviderInterchangeability(t *testing.T) {
	// Scenario C - Interchangeability (spec §41 Scenario C):
	// Google STT -> UserContext -> xAI LLM -> AssistantContext -> Google TTS.
	// Verifies that swapping Google LLM with xAI LLM works seamlessly with zero core changes.

	xaiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		// Stream response from grok-2-1212
		_, _ = fmt.Fprint(w, "data: {\"choices\": [{\"delta\": {\"content\": \"Hello! How can I \"}}]}\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(10 * time.Millisecond)

		_, _ = fmt.Fprint(w, "data: {\"choices\": [{\"delta\": {\"content\": \"assist you today?\"}}], \"usage\": {\"prompt_tokens\": 15, \"completion_tokens\": 8, \"total_tokens\": 23}}\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(10 * time.Millisecond)

		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer xaiServer.Close()

	// 1. Google STT Mock
	stt := &mockGoogleSTT{
		transcripts: []string{"Hello xAI, this is Scenario C."},
	}

	// 2. Core Conversation Context & User Context Processor
	ctxPool := core.NewConversationContext(core.ContextConfig{MaxMessages: 20, MaxTokens: 4000})
	userCtxProc := core.NewUserContextProcessor(ctxPool, core.UserContextConfig{TurnPrefix: "turn"})

	// 3. xAI LLM Processor
	xaiCfg := Config{
		Endpoint:   xaiServer.URL,
		Model:      "grok-2-1212",
		APIKey:     "xai-scenario-c-key",
		HTTPClient: xaiServer.Client(),
		ContextProvider: func() []core.Message {
			return ctxPool.Messages()
		},
	}
	xaiProc, err := NewProcessor(xaiCfg)
	if err != nil {
		t.Fatalf("new xai processor: %v", err)
	}

	// 4. Core Assistant Context Processor
	asstCtxProc := core.NewAssistantContextProcessor(ctxPool, core.DefaultAssistantContextConfig())

	// 5. Google TTS Mock
	tts := &mockGoogleTTS{}
	tts.synthesizedWg.Add(1)

	// 6. Transport Output Mock
	transportOutput := coretest.NewTransportOutput(coretest.TransportConfig{WriteDelay: 0})

	// Assemble mixed pipeline: Google STT -> UserContext -> xAI LLM -> AsstContext -> Google TTS -> Output
	pipe, err := core.NewPipeline(stt, userCtxProc, xaiProc, asstCtxProc, tts, transportOutput)
	if err != nil {
		t.Fatalf("assemble mixed pipeline: %v", err)
	}

	budget := core.DefaultResourceBudget()
	session, err := core.NewSession(1001, pipe, budget)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	if err := session.Start(); err != nil {
		t.Fatalf("start session: %v", err)
	}
	defer session.Close()

	// Feed audio into session (triggers STT -> UserContext -> xAI LLM -> AsstContext -> TTS -> Output)
	inputAudio := &core.InputAudioFrame{
		Samples:    make([]byte, 640),
		Encoding:   core.EncodingPCM16LE,
		SampleRate: 16000,
		Channels:   1,
		Duration:   20 * time.Millisecond,
		Timestamp:  time.Now(),
		Seq:        1,
		Gen:        session.CurrentGeneration(),
	}

	if err := session.Post(inputAudio); err != nil {
		t.Fatalf("post input audio: %v", err)
	}

	// Wait for synthesis to complete
	doneCh := make(chan struct{})
	go func() {
		tts.synthesizedWg.Wait()
		close(doneCh)
	}()

	select {
	case <-doneCh:
		// Success
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Scenario C synthesis")
	}

	time.Sleep(50 * time.Millisecond)

	// Verify synthesized text was received from xAI and passed to Google TTS
	tts.mu.Lock()
	synthesizedUnits := tts.synthesized
	tts.mu.Unlock()

	if len(synthesizedUnits) == 0 {
		t.Fatal("no synthesis units delivered to Google TTS in Scenario C")
	}

	t.Logf("Scenario C succeeded: synthesized units=%v", synthesizedUnits)

	if len(transportOutput.GetWrittenFrames()) == 0 {
		t.Fatal("no output audio delivered to transport output")
	}

	// Verify assistant turn was committed to conversation context
	msgs := ctxPool.Messages()
	var hasAssistantMsg bool
	for _, m := range msgs {
		if m.Role == core.RoleAssistant && m.Content != "" {
			hasAssistantMsg = true
			if m.TurnID == "" {
				t.Fatalf("assistant message committed with empty TurnID: %+v", m)
			}
		}
	}
	if !hasAssistantMsg {
		t.Fatalf("assistant message was not committed to conversation context in Scenario C; msgs=%+v", msgs)
	}
}
