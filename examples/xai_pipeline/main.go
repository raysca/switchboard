package main

import (
	"fmt"
	"log"
	"time"

	"govoice/core"
	"govoice/providers/google"
	"govoice/providers/xai"
	"govoice/vad"
)

// main demonstrates Scenario C from the specification: interchangeable LLM substitution
// using xAI Grok (grok-2-1212) within an otherwise Google-based pipeline (Google STT + Google TTS)
// with zero changes to the core runtime.
func main() {
	fmt.Println("=== Voice Framework V1: Scenario C xAI LLM Interchangeability Example ===")

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
	sttProc, err := google.NewSTTProcessor(google.DefaultSTTConfig())
	if err != nil {
		log.Fatalf("Failed to create Google STT processor: %v", err)
	}

	// 3. Conversation Context & Bounded Context Processor
	convCtx := core.NewConversationContext(core.ContextConfig{
		MaxMessages: 20,
		MaxTokens:   4096,
	})
	userCtxProc := core.NewUserContextProcessor(convCtx, core.UserContextConfig{TurnPrefix: "turn"})

	// 4. xAI LLM Processor (Drop-in substitution for Google LLM)
	xaiProc, err := xai.NewProcessor(xai.Config{
		Model:        "grok-2-1212",
		Endpoint:     "https://api.x.ai/v1/responses",
		SystemPrompt: "You are a concise voice assistant.",
	})
	if err != nil {
		log.Fatalf("Failed to create xAI LLM processor: %v", err)
	}

	// 5. Assistant Context Processor & Google TTS
	asstCtxProc := core.NewAssistantContextProcessor(convCtx, core.DefaultAssistantContextConfig())
	ttsProc, err := google.NewTTSProcessor(google.DefaultTTSConfig())
	if err != nil {
		log.Fatalf("Failed to create Google TTS processor: %v", err)
	}

	// 6. Sink Processor
	sinkProc := &core.PassthroughProcessor{}

	// 7. Assemble Mixed Pipeline (Google STT -> UserCtx -> xAI LLM -> AsstCtx -> Google TTS)
	pipeline, err := core.NewPipeline(
		vadProc,
		sttProc,
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
