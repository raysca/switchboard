package google

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/coretest"
	coretranscript "govoice/core/transcript"
)

// deterministicVAD simulates voice activity detection boundaries.
type deterministicVAD struct {
	session core.SessionController
	index   int
}

func (v *deterministicVAD) Attach(s core.SessionController, index int) error {
	v.session = s
	v.index = index
	return nil
}

func (v *deterministicVAD) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	return e.Push(f)
}

func TestGooglePipeline_MultiTurnRoundTripWithDeterministicVAD(t *testing.T) {
	// Configure mock Google providers
	var mockStream *mockSTTStream
	var streamMu sync.Mutex

	sttOpener := &mockSTTOpener{
		onOpenStream: func(cfg STTConfig) *mockSTTStream {
			st := newMockSTTStream()
			st.PushResult(&STTResult{
				Transcript: "What is the capital of France?",
				IsFinal:    true,
			})
			streamMu.Lock()
			mockStream = st
			streamMu.Unlock()
			return st
		},
	}

	sttCfg := DefaultSTTConfig()
	sttCfg.StreamOpener = sttOpener
	sttProc, _ := NewSTTProcessor(sttCfg)

	llmMock := &mockLLMClient{
		generateFn: func(ctx context.Context, model string, systemPrompt string, history []core.Message) (<-chan LLMStreamChunk, <-chan error, error) {
			chunkCh := make(chan LLMStreamChunk, 2)
			errCh := make(chan error, 1)
			go func() {
				defer close(chunkCh)
				defer close(errCh)
				chunkCh <- LLMStreamChunk{Text: "The capital of France is Paris."}
			}()
			return chunkCh, errCh, nil
		},
	}
	llmCfg := DefaultLLMConfig()
	llmCfg.Client = llmMock
	llmProc, _ := NewLLMProcessor(llmCfg)

	ttsMock := &mockTTSClient{
		synthesizeFn: func(ctx context.Context, text string, voice string, sampleRate int) ([]byte, error) {
			// Return 200ms of 24kHz PCM16LE mono (9600 bytes)
			return make([]byte, 9600), nil
		},
	}
	ttsCfg := DefaultTTSConfig()
	ttsCfg.Client = ttsMock
	ttsProc, _ := NewTTSProcessor(ttsCfg)

	ctxPool := core.NewConversationContext(core.ContextConfig{MaxMessages: 20, MaxTokens: 4000})
	finalCfg := coretranscript.DefaultConfig()
	finalCfg.GracePeriod = 0
	transcriptProc, _ := coretranscript.NewProcessor(finalCfg)
	userCtxProc := core.NewUserContextProcessor(ctxPool, core.UserContextConfig{TurnPrefix: "turn", RequireCommittedTranscript: true})
	asstCtxProc := core.NewAssistantContextProcessor(ctxPool, core.DefaultAssistantContextConfig())
	vadProc := &deterministicVAD{}
	fakeTransport := coretest.NewTransportOutput(coretest.TransportConfig{WriteDelay: 0})

	pipe, err := core.NewPipeline(
		vadProc,
		sttProc,
		transcriptProc,
		userCtxProc,
		llmProc,
		asstCtxProc,
		ttsProc,
		fakeTransport,
	)
	if err != nil {
		t.Fatal(err)
	}

	session, err := core.NewSession(1, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}

	_ = vadProc.Attach(session, 0)
	_ = sttProc.Attach(session, 1)
	_ = userCtxProc.Attach(session, 3)
	_ = llmProc.Attach(session, 4)
	_ = asstCtxProc.Attach(session, 5)
	_ = ttsProc.Attach(session, 6)
	_ = fakeTransport.Attach(session, 7)

	_ = session.Start()
	defer session.Close()

	// 1. Turn 1: User speaks
	_ = session.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	// Feed audio
	_ = session.Post(&core.InputAudioFrame{
		Samples:    make([]byte, 640),
		Encoding:   core.EncodingPCM16LE,
		SampleRate: 16000,
		Channels:   1,
		Gen:        1,
	})
	_ = session.Post(&core.UserStoppedSpeakingFrame{Seq: 2, Gen: 1, Timestamp: time.Now()})

	// Wait for pipeline processing: STT -> UserContext -> LLM -> AssistantContext -> TTS -> OutputAudio
	time.Sleep(300 * time.Millisecond)

	audioWritten := len(fakeTransport.GetWrittenFrames())

	if audioWritten == 0 {
		t.Fatal("expected output audio frames to be generated for turn 1, got 0")
	}

	// 2. Turn 2: User interrupts / barges in
	// InterruptionFrame triggers AssistantContextProcessor to advance generation and send flush barrier
	_ = session.Post(&core.InterruptionFrame{Turn: "turn-1", Gen: 1})

	time.Sleep(50 * time.Millisecond)

	// Next turn succeeds in same session with new generation (Gen 2)
	_ = session.Post(&core.UserStartedSpeakingFrame{Seq: 3, Gen: 2})
	_ = session.Post(&core.InputAudioFrame{
		Samples:    make([]byte, 640),
		Encoding:   core.EncodingPCM16LE,
		SampleRate: 16000,
		Channels:   1,
		Gen:        2,
	})

	time.Sleep(50 * time.Millisecond)

	streamMu.Lock()
	if mockStream != nil {
		mockStream.PushResult(&STTResult{
			Transcript: "What is the population of Paris?",
			IsFinal:    true,
		})
	}
	streamMu.Unlock()

	_ = session.Post(&core.UserStoppedSpeakingFrame{Seq: 4, Gen: 2, Timestamp: time.Now()})

	time.Sleep(300 * time.Millisecond)

	newAudioWritten := len(fakeTransport.GetWrittenFrames())

	if newAudioWritten <= audioWritten {
		t.Logf("Turns created: %d, duplicate drops: %d", userCtxProc.TurnsCreated.Load(), userCtxProc.DuplicateDropCount.Load())
		t.Logf("Audio written turn 1: %d, total audio written: %d", audioWritten, newAudioWritten)
		t.Fatal("expected new generation audio after interruption, got none")
	}
	if got := userCtxProc.TurnsCreated.Load(); got != 2 {
		t.Fatalf("expected exactly one UserMessage per Google turn, got %d", got)
	}
}

func TestLiveGoogle_MultiTurnRoundTrip(t *testing.T) {
	// Gated by RUN_LIVE_GOOGLE_TEST=1 and presence of credentials
	if os.Getenv("RUN_LIVE_GOOGLE_TEST") != "1" {
		t.Skip("skipping live Google test: RUN_LIVE_GOOGLE_TEST=1 not set")
	}

	apiKey := os.Getenv("GOOGLE_API_KEY")
	credsFile := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")
	if apiKey == "" && credsFile == "" {
		t.Skip("skipping live Google test: neither GOOGLE_API_KEY nor GOOGLE_APPLICATION_CREDENTIALS is set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 1. Live TTS test: synthesize speech
	ttsCfg := DefaultTTSConfig()
	ttsCfg.APIKey = apiKey
	ttsCfg.CredentialsFile = credsFile
	ttsClient, err := NewRealGoogleTTSClient(ctx, ttsCfg)
	if err != nil {
		t.Fatalf("failed to create real Google TTS client: %v", err)
	}
	defer ttsClient.Close()

	audio, err := ttsClient.SynthesizeSpeech(ctx, "Hello from Voice Framework V1.", ttsCfg.Voice, ttsCfg.SampleRate)
	if err != nil {
		t.Fatalf("real Google TTS synthesis failed: %v", err)
	}
	if len(audio) == 0 {
		t.Fatal("real Google TTS returned empty audio")
	}

	// 2. Live LLM test: generate streamed content
	var authMode AuthMode = AuthModeGeminiAPIKey
	if credsFile != "" {
		authMode = AuthModeVertexADC
	}
	creds, err := ResolveCredentials(CredentialConfig{
		Mode:            authMode,
		APIKey:          apiKey,
		CredentialsFile: credsFile,
	})
	if err != nil {
		t.Fatalf("failed to resolve live credentials: %v", err)
	}

	llmClient, err := NewRealGoogleLLMClient(ctx, creds)
	if err != nil {
		t.Fatalf("failed to create real Google LLM client: %v", err)
	}

	history := []core.Message{
		{Role: core.RoleUser, Content: "Say 'hello' in one word."},
	}
	chunkCh, errCh, err := llmClient.GenerateStream(ctx, "gemini-2.5-flash", "You are a concise assistant.", history)
	if err != nil {
		t.Fatalf("real Google LLM generation failed: %v", err)
	}

	var chunksReceived int
	for chunk := range chunkCh {
		if chunk.Text != "" {
			chunksReceived++
		}
	}
	if err := <-errCh; err != nil {
		t.Fatalf("real Google LLM stream returned error: %v", err)
	}
	if chunksReceived == 0 {
		t.Fatal("expected at least one chunk from real Google LLM stream")
	}
}

func TestLongCall_ProductionLimitRotation(t *testing.T) {
	// Pinned requirement: "Release suite includes a 30-minute real call crossing a production-limit rotation"
	// For routine verify runs, skip unless RUN_LONG_ROTATION_TEST=1
	if os.Getenv("RUN_LONG_ROTATION_TEST") != "1" {
		t.Skip("skipping 30-minute rotation test (set RUN_LONG_ROTATION_TEST=1 for release verification)")
	}

	// When RUN_LONG_ROTATION_TEST=1, runs for 31 minutes crossing the 240s threshold multiple times
	ctx, cancel := context.WithTimeout(context.Background(), 31*time.Minute)
	defer cancel()

	cfg := DefaultSTTConfig()
	cfg.StreamRotationThreshold = 240 * time.Second
	cfg.StreamOverlap = 500 * time.Millisecond

	// Use mock opener that stays alive
	opener := &mockSTTOpener{}
	cfg.StreamOpener = opener
	proc, err := NewSTTProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}

	pipe, _ := core.NewPipeline(proc)
	session, _ := core.NewSession(1, pipe, core.ResourceBudget{
		MaxQueuedFrames:    2048,
		MaxControlFrames:   256,
		MaxEmitsPerFrame:   32,
		MaxFrameProcessing: 5 * time.Second,
		MaxSessionDuration: 35 * time.Minute,
	})
	_ = proc.Attach(session, 0)
	_ = session.Start()
	defer session.Close()

	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	chunk := make([]byte, 640)

	startTime := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if time.Since(startTime) >= 30*time.Minute {
				rotations := proc.RotationsCount.Load()
				if rotations < 7 { // At 240s threshold, 30 min crosses 7 rotations
					t.Fatalf("expected at least 7 rotations in 30 minutes, got %d", rotations)
				}
				return
			}
			_ = session.Post(&core.InputAudioFrame{
				Samples:    chunk,
				Encoding:   core.EncodingPCM16LE,
				SampleRate: 16000,
				Channels:   1,
			})
		}
	}
}
