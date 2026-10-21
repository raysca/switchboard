package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"govoice/core"
	coretranscript "govoice/core/transcript"
	"govoice/examples/envload"
	"govoice/providers/google"
	"govoice/providers/xai"
	"govoice/vad"
)

// main demonstrates Scenario C from the specification: interchangeable LLM substitution
// using xAI Grok within an otherwise Google-based pipeline (Google STT + Google TTS)
// with zero changes to the core runtime.
// forwardProcessor passes every frame through: a stand-in for the transport at either end of the
// example pipeline.
type forwardProcessor struct{}

func (forwardProcessor) ProcessFrame(_ context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	return e.PushDirection(f, dir)
}

func main() {
	_ = envload.Load()

	fmt.Println("=== Switchboard: Scenario C xAI LLM Interchangeability Example ===")

	// 1. Initialize In-Process VAD Executor
	vadCfg := vad.Config{
		Mode:            "sharded",
		Workers:         1,
		Batch:           1,
		QueueCap:        128,
		IntraOp:         1,
		PerSessionLimit: 1,
		ParentDeadline:  200 * time.Millisecond,
		ParentGrace:     200 * time.Millisecond,
	}
	vadExec, err := vad.New(vadCfg)
	if err != nil {
		log.Fatalf("Failed to initialize VAD executor: %v", err)
	}
	defer vadExec.Close()

	vadProc := vad.NewProcessor(vadExec, vad.DefaultProcessorConfig())

	// 2. Google STT
	sttCfg := google.DefaultSTTConfig()
	if opener, err := google.NewRealGoogleSTTOpener(context.Background(), sttCfg); err == nil {
		sttCfg.StreamOpener = opener
	}
	sttProc, err := google.NewSTTProcessor(sttCfg)
	if err != nil {
		log.Fatalf("Failed to create Google STT processor: %v", err)
	}

	// 3. Conversation Context & Bounded Context Processor
	convCtx := core.NewConversationContext(core.ContextConfig{
		MaxMessages: 20,
		MaxTokens:   4096,
	})
	transcriptCfg := coretranscript.DefaultConfig()
	transcriptProc, _ := coretranscript.NewProcessor(transcriptCfg)
	userCtxProc := core.NewUserContextProcessor(convCtx, core.UserContextConfig{TurnPrefix: "turn", RequireCommittedTranscript: true})

	// 4. xAI LLM Processor (Drop-in substitution for Google LLM)
	model := "grok-4.20-0309-non-reasoning"
	if envModel := os.Getenv("XAI_MODEL"); envModel != "" {
		model = envModel
	}
	endpoint := "https://api.x.ai/v1/responses"
	if envEndpoint := os.Getenv("XAI_ENDPOINT"); envEndpoint != "" {
		endpoint = envEndpoint
	}
	xaiProc, err := xai.NewProcessor(xai.Config{
		Model:        model,
		Endpoint:     endpoint,
		APIKey:       os.Getenv("XAI_API_KEY"),
		SystemPrompt: "You are a concise voice assistant.",
	})
	if err != nil {
		log.Fatalf("Failed to create xAI LLM processor: %v", err)
	}

	// 5. Assistant Context Processor & Google TTS
	asstCtxProc := core.NewAssistantContextProcessor(convCtx, core.DefaultAssistantContextConfig())
	ttsCfg := google.DefaultTTSConfig()
	if client, err := google.NewRealGoogleTTSClient(context.Background(), ttsCfg); err == nil {
		ttsCfg.Client = client
	}
	ttsProc, err := google.NewTTSProcessor(ttsCfg)
	if err != nil {
		log.Fatalf("Failed to create Google TTS processor: %v", err)
	}

	// 6. Sink Processor
	sinkProc := forwardProcessor{}

	// 7. Assemble Mixed Pipeline (Google STT -> UserCtx -> xAI LLM -> AsstCtx -> Google TTS)
	pipeline, err := core.NewPipeline(
		vadProc,
		sttProc,
		transcriptProc,
		userCtxProc,
		xaiProc,
		asstCtxProc,
		ttsProc,
		sinkProc,
	)
	if err != nil {
		log.Fatalf("Failed to assemble mixed pipeline: %v", err)
	}

	// 8. Session Runner
	session, err := core.NewSession(2, pipeline, core.DefaultResourceBudget())
	if err != nil {
		log.Fatalf("Failed to create session: %v", err)
	}
	if err := session.Start(); err != nil {
		log.Fatalf("Failed to start session: %v", err)
	}

	fmt.Printf("Scenario C Session %d running with %d processors (STT: Google, LLM: xAI, TTS: Google).\n",
		session.ID(), len(pipeline.Processors()))

	// Emit synthetic frame
	audioPayload := make([]byte, 640)
	_ = session.Post(&core.InputAudioFrame{
		Samples:    audioPayload,
		Encoding:   core.EncodingPCM16LE,
		SampleRate: 16000,
		Channels:   1,
		Duration:   20 * time.Millisecond,
	})

	if err := session.Close(); err != nil {
		log.Printf("Session close error: %v", err)
	}
	<-session.Done()
	fmt.Println("Scenario C session completed cleanly.")
}
