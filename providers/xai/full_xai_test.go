//go:build xaispeech

package xai

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"govoice/core"
	coretranscript "govoice/core/transcript"
)

// fullXAISink captures frames emitted at the end of the full xAI pipeline.
type fullXAISink struct {
	mu           sync.Mutex
	session      core.SessionController
	userTurns    []string
	userMessages []string
	llmDeltas    string
	outputAudio  []*core.OutputAudioFrame
	totalPCM     int
	doneChan     chan struct{}
	closeOnce    sync.Once
}

func newFullXAISink() *fullXAISink {
	return &fullXAISink{
		doneChan: make(chan struct{}),
	}
}

func (s *fullXAISink) signalDone() {
	s.closeOnce.Do(func() {
		close(s.doneChan)
	})
}

func (s *fullXAISink) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch frame := f.(type) {
	case *core.FinalTranscriptionFrame:
		s.userTurns = append(s.userTurns, frame.Text)
	case *core.UserMessageFrame:
		s.userMessages = append(s.userMessages, frame.Content)
	case *core.LLMTextDeltaFrame:
		s.llmDeltas += frame.Delta
	case *core.OutputAudioFrame:
		s.outputAudio = append(s.outputAudio, frame)
		s.totalPCM += len(frame.Samples)
		if frame.IsFinal && s.session != nil {
			_ = s.session.Reenter(&core.WriteCompletionFrame{
				Turn:    frame.Turn,
				Gen:     frame.Gen,
				UnitID:  frame.UnitID,
				IsFinal: true,
			}, core.Downstream, 0, frame.Gen)
		}
	case *core.AssistantMessageFrame:
		go s.signalDone()
	}
	return e.PushDirection(f, dir)
}

func (s *fullXAISink) Attach(sess core.SessionController, index int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.session = sess
	return nil
}

func TestFullXAI_EndToEndPipeline(t *testing.T) {
	// Mock xAI LLM Server (SSE)
	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		_, _ = fmt.Fprint(w, "data: {\"choices\": [{\"delta\": {\"content\": \"Full xAI pipeline \"}}]}\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(10 * time.Millisecond)

		_, _ = fmt.Fprint(w, "data: {\"choices\": [{\"delta\": {\"content\": \"is operational.\"}}], \"usage\": {\"prompt_tokens\": 10, \"completion_tokens\": 6, \"total_tokens\": 16}}\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(10 * time.Millisecond)

		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer llmServer.Close()

	// Mock xAI TTS Server (REST)
	ttsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/pcm")
		w.WriteHeader(http.StatusOK)
		// Return 4800 bytes of raw 24kHz 16-bit mono PCM (100ms)
		pcm := make([]byte, 4800)
		for i := range pcm {
			pcm[i] = byte(i % 256)
		}
		_, _ = w.Write(pcm)
	}))
	defer ttsServer.Close()

	// 1. Mock STT Stream & Processor
	mockStream := &mockSTTStream{
		messages: []*STTMessage{
			{Type: "transcript.created"},
			{Type: "transcript.partial", Text: "Hello Grok, can you hear me?"},
			{Type: "transcript.partial", Text: "Hello Grok, can you hear me?", SpeechFinal: true},
		},
	}
	mockOpener := &mockSTTOpener{stream: mockStream}

	sttCfg := DefaultSTTConfig()
	sttCfg.APIKey = "test-key"
	sttCfg.StreamOpener = mockOpener
	sttProc, err := NewSTTProcessor(sttCfg)
	if err != nil {
		t.Fatalf("new stt processor: %v", err)
	}
	defer sttProc.Close()

	// 2. User Context Processor
	convCtx := core.NewConversationContext(core.ContextConfig{MaxMessages: 20, MaxTokens: 4000})
	finalCfg := coretranscript.DefaultConfig()
	finalCfg.GracePeriod = 0
	transcriptProc, _ := coretranscript.NewProcessor(finalCfg)
	userCtx := core.NewUserContextProcessor(convCtx, core.UserContextConfig{TurnPrefix: "turn-full-xai", RequireCommittedTranscript: true})

	// 3. xAI LLM Processor
	llmCfg := Config{
		Model:        "grok-2-1212",
		Endpoint:     llmServer.URL,
		APIKey:       "test-key",
		SystemPrompt: "You are a full xAI assistant.",
	}
	llmProc, err := NewProcessor(llmCfg)
	if err != nil {
		t.Fatalf("new llm processor: %v", err)
	}

	// 4. Assistant Context Processor
	asstCtx := core.NewAssistantContextProcessor(convCtx, core.DefaultAssistantContextConfig())

	// 5. xAI TTS Processor
	ttsCfg := DefaultTTSConfig()
	ttsCfg.Endpoint = ttsServer.URL
	ttsCfg.APIKey = "test-key"
	ttsProc, err := NewTTSProcessor(ttsCfg)
	if err != nil {
		t.Fatalf("new tts processor: %v", err)
	}
	defer ttsProc.Close()

	// 6. Sink
	sink := newFullXAISink()

	// Assemble Pipeline: STT -> Transcript finalizer -> UserCtx -> LLM -> AsstCtx -> TTS -> Sink
	pipe, err := core.NewPipeline(sttProc, transcriptProc, userCtx, llmProc, asstCtx, ttsProc, sink)
	if err != nil {
		t.Fatalf("new pipeline: %v", err)
	}

	session, err := core.NewSession(1, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	if err := session.Start(); err != nil {
		t.Fatalf("start session: %v", err)
	}
	defer session.Close()

	// Supply the stable turn identity before audio opens the STT stream.
	if err := session.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1}); err != nil {
		t.Fatalf("post speech start: %v", err)
	}
	// Send an InputAudioFrame to trigger STT stream opening
	err = session.Post(&core.InputAudioFrame{
		Samples:    make([]byte, 640),
		Encoding:   core.EncodingPCM16LE,
		SampleRate: 16000,
		Channels:   1,
		Duration:   20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("post audio frame: %v", err)
	}

	// Wait for pipeline execution to complete
	select {
	case <-sink.doneChan:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for full xAI pipeline synthesis to complete")
	}

	sink.mu.Lock()
	defer sink.mu.Unlock()

	// Verify User Context received transcription
	if len(sink.userTurns) != 1 || sink.userTurns[0] != "Hello Grok, can you hear me?" {
		t.Errorf("unexpected user transcription: %v", sink.userTurns)
	}
	if len(sink.userMessages) != 1 || sink.userMessages[0] != "Hello Grok, can you hear me?" {
		t.Errorf("expected exactly one xAI UserMessage, got %v", sink.userMessages)
	}

	// Verify LLM generated text deltas
	expectedText := "Full xAI pipeline is operational."
	if sink.llmDeltas != expectedText {
		t.Errorf("expected LLM deltas %q, got %q", expectedText, sink.llmDeltas)
	}

	// Verify TTS produced OutputAudioFrames at 24000 Hz PCM16LE
	if len(sink.outputAudio) == 0 {
		t.Fatalf("expected output audio frames, got none")
	}
	for i, frame := range sink.outputAudio {
		if frame.SampleRate != 24000 {
			t.Errorf("frame %d: expected 24000 Hz, got %d", i, frame.SampleRate)
		}
		if frame.Encoding != core.EncodingPCM16LE {
			t.Errorf("frame %d: expected PCM16LE, got %v", i, frame.Encoding)
		}
		if frame.Channels != 1 {
			t.Errorf("frame %d: expected mono, got %d channels", i, frame.Channels)
		}
		if len(frame.Samples) != 960 { // 20ms @ 24kHz * 2 bytes
			t.Errorf("frame %d: expected 960 bytes (20ms), got %d", i, len(frame.Samples))
		}
	}

	if sink.totalPCM != 4800 {
		t.Errorf("expected 4800 total PCM bytes, got %d", sink.totalPCM)
	}

	// Allow session loop to deliver WriteCompletionFrame
	time.Sleep(100 * time.Millisecond)

	// Verify conversation context history
	history := convCtx.Messages()
	for i, m := range history {
		t.Logf("history[%d]: Role=%s, TurnID=%s, Content=%q", i, m.Role, m.TurnID, m.Content)
	}
	if len(history) < 2 {
		t.Fatalf("expected at least 2 messages in context history, got %d", len(history))
	}
	if history[0].Role != core.RoleUser || history[0].Content != "Hello Grok, can you hear me?" {
		t.Errorf("unexpected user context message: %+v", history[0])
	}
	if history[1].Role != core.RoleAssistant || history[1].Content != expectedText {
		t.Errorf("unexpected assistant context message: %+v", history[1])
	}
}

func TestFullXAI_LiveEndToEnd(t *testing.T) {
	if os.Getenv("RUN_LIVE_XAI_TESTS") != "1" {
		t.Skip("skipping live xAI end-to-end test; set RUN_LIVE_XAI_TESTS=1 to run")
	}
	apiKey := os.Getenv("XAI_API_KEY")
	if apiKey == "" {
		t.Skip("skipping live xAI end-to-end test; XAI_API_KEY not set")
	}

	// Test real TTS call against api.x.ai
	ttsCfg := DefaultTTSConfig()
	ttsCfg.APIKey = apiKey
	ttsCfg.Voice = "eve"
	ttsCfg.Timeout = 10 * time.Second

	client := NewRealXAITTSClient()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pcm, err := client.Synthesize(ctx, "Hello, this is a live test of the xAI TTS engine.", ttsCfg)
	if err != nil {
		t.Fatalf("live xAI TTS failed: %v", err)
	}
	if len(pcm) == 0 {
		t.Fatal("live xAI TTS returned empty PCM data")
	}
	if len(pcm)%2 != 0 {
		t.Errorf("live xAI TTS data length %d is not a multiple of 2 (16-bit samples)", len(pcm))
	}
	t.Logf("Live xAI TTS synthesis succeeded: %d bytes of 24kHz PCM audio received", len(pcm))

	// Test real STT stream against wss://api.x.ai/v1/stt
	sttCfg := DefaultSTTConfig()
	sttCfg.APIKey = apiKey
	opener := &RealXAISTTOpener{}
	sttCtx, sttCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer sttCancel()

	stream, err := opener.OpenStream(sttCtx, sttCfg)
	if err != nil {
		t.Logf("live xAI STT WebSocket OpenStream returned: %v (WebSocket STT may require specialized tier or REST endpoint)", err)
		return
	}
	defer stream.Close()
	t.Logf("Live xAI STT WebSocket connected successfully!")

	// Send silence / test audio
	sendErr := stream.SendAudio(sttCtx, make([]byte, 6400))
	if sendErr != nil {
		t.Logf("SendAudio error: %v", sendErr)
	}

	// Try reading initial message
	msg, recvErr := stream.Recv(sttCtx)
	if recvErr != nil {
		t.Logf("Recv error: %v", recvErr)
	} else if msg != nil {
		t.Logf("Received STT event: type=%s, text=%s", msg.Type, msg.Text)
	}
}
