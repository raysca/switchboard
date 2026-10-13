package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"govoice/core"
	"govoice/core/coretest"
	coretranscript "govoice/core/transcript"
	"govoice/examples/envload"
	"govoice/providers/google"
	"govoice/vad"
)

// main demonstrates how to assemble and run a complete Google Voice pipeline
// programmatically using standard govoice components without the HTTP/WebSocket server.
func main() {
	_ = envload.Load()

	fmt.Println("=== Voice Framework V1: Programmatic Google Pipeline Example ===")

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

	// 2. Initialize Google Providers
	sttCfg := google.DefaultSTTConfig()
	if opener, err := google.NewRealGoogleSTTOpener(context.Background(), sttCfg); err == nil {
		sttCfg.StreamOpener = opener
	}
	sttProc, err := google.NewSTTProcessor(sttCfg)
	if err != nil {
		log.Fatalf("Failed to create Google STT processor: %v", err)
	}

	llmCfg := google.DefaultLLMConfig()
	if apiKey := os.Getenv("GOOGLE_API_KEY"); apiKey != "" {
		creds, err := google.ResolveCredentials(google.CredentialConfig{
			Mode:   google.AuthModeGeminiAPIKey,
			APIKey: apiKey,
		})
		if err == nil && creds != nil {
			if client, err := google.NewRealGoogleLLMClient(context.Background(), creds); err == nil {
				llmCfg.Client = client
			}
		}
	} else if saFile := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"); saFile != "" {
		creds, err := google.ResolveCredentials(google.CredentialConfig{
			Mode:            google.AuthModeVertexADC,
			CredentialsFile: saFile,
		})
		if err == nil && creds != nil {
			if client, err := google.NewRealGoogleLLMClient(context.Background(), creds); err == nil {
				llmCfg.Client = client
			}
		}
	}
	llmProc, err := google.NewLLMProcessor(llmCfg)
	if err != nil {
		log.Fatalf("Failed to create Google LLM processor: %v", err)
	}

	ttsCfg := google.DefaultTTSConfig()
	if client, err := google.NewRealGoogleTTSClient(context.Background(), ttsCfg); err == nil {
		ttsCfg.Client = client
	}
	ttsProc, err := google.NewTTSProcessor(ttsCfg)
	if err != nil {
		log.Fatalf("Failed to create Google TTS processor: %v", err)
	}

	// 3. Conversation Context & Turn Budget
	convCtx := core.NewConversationContext(core.ContextConfig{
		MaxMessages: 20,
		MaxTokens:   4096,
	})
	transcriptCfg := coretranscript.DefaultConfig()
	transcriptProc, _ := coretranscript.NewProcessor(transcriptCfg)
	userCtxProc := core.NewUserContextProcessor(convCtx, core.UserContextConfig{TurnPrefix: "turn", RequireCommittedTranscript: true})
	asstCtxProc := core.NewAssistantContextProcessor(convCtx, core.DefaultAssistantContextConfig())

	// 4. Sink Processor (e.g. recording or output playback)
	sinkProc := &coretest.PassthroughProcessor{}

	// 5. Assemble Programmatic Pipeline
	pipeline, err := core.NewPipeline(
		vadProc,
		sttProc,
		transcriptProc,
		userCtxProc,
		llmProc,
		asstCtxProc,
		ttsProc,
		sinkProc,
	)
	if err != nil {
		log.Fatalf("Failed to assemble pipeline: %v", err)
	}

	// 6. Create Session Runner
	session, err := core.NewSession(1, pipeline, core.DefaultResourceBudget())
	if err != nil {
		log.Fatalf("Failed to create session: %v", err)
	}
	if err := session.Start(); err != nil {
		log.Fatalf("Failed to start session: %v", err)
	}

	fmt.Printf("Session %d started successfully with %d pipeline processors.\n", session.ID(), len(pipeline.Processors()))

	// Emit synthetic 20ms silence frame (16kHz 16-bit mono = 320 samples = 640 bytes)
	audioPayload := make([]byte, 640)
	err = session.Post(&core.InputAudioFrame{
		Samples:    audioPayload,
		Encoding:   core.EncodingPCM16LE,
		SampleRate: 16000,
		Channels:   1,
		Duration:   20 * time.Millisecond,
	})
	if err != nil {
		log.Printf("Post frame error: %v", err)
	}

	// Clean graceful close
	if err := session.Close(); err != nil {
		log.Printf("Session close error: %v", err)
	}
	<-session.Done()
	fmt.Println("Session terminated gracefully.")
}
