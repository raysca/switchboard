//go:build xaispeech

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"time"

	"govoice/core"
	coretranscript "govoice/core/transcript"
	"govoice/examples/envload"
	"govoice/providers/xai"
	"govoice/vad"
)

// audioCollector collects synthesized output audio and events.
type audioCollector struct {
	mu         sync.Mutex
	session    core.SessionController
	audioBytes int
	chunks     int
	doneCh     chan struct{}
}

func newAudioCollector() *audioCollector {
	return &audioCollector{
		doneCh: make(chan struct{}),
	}
}

func (c *audioCollector) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	switch frame := f.(type) {
	case *core.UserStartedSpeakingFrame:
		fmt.Printf(" [VAD] User started speaking (seq=%d)\n", frame.Seq)
	case *core.UserStoppedSpeakingFrame:
		fmt.Printf(" [VAD] User stopped speaking (seq=%d)\n", frame.Seq)
	case *core.InterimTranscriptionFrame:
		fmt.Printf(" [STT Interim] %s\n", frame.Text)
	case *core.FinalTranscriptionFrame:
		fmt.Printf("\n [User Transcription] %s\n\n [xAI Grok Thinking/Responding]: ", frame.Text)
	case *core.LLMTextDeltaFrame:
		fmt.Printf("%s", frame.Delta)
	case *core.SynthesisUnitFrame:
		fmt.Printf("\n [Synthesis Unit] %s (Gen=%d)\n", frame.Text, frame.Gen)
	case *core.OutputAudioFrame:
		c.mu.Lock()
		c.audioBytes += len(frame.Samples)
		c.chunks++
		c.mu.Unlock()
		if frame.IsFinal && c.session != nil {
			_ = c.session.Reenter(&core.WriteCompletionFrame{
				Turn:    frame.Turn,
				Gen:     frame.Gen,
				UnitID:  frame.UnitID,
				IsFinal: true,
			}, core.Downstream, 0, frame.Gen)
		}
	case *core.AssistantMessageFrame:
		fmt.Printf("\n [Assistant Turn Committed] %s\n", frame.Content)
		c.mu.Lock()
		select {
		case <-c.doneCh:
		default:
			close(c.doneCh)
		}
		c.mu.Unlock()
	case *core.TTSStoppedFrame:
		c.mu.Lock()
		if c.chunks > 0 {
			select {
			case <-c.doneCh:
			default:
				close(c.doneCh)
			}
		}
		c.mu.Unlock()
	}
	return e.PushDirection(f, dir)
}

func (c *audioCollector) Attach(s core.SessionController, index int) error {
	c.session = s
	return nil
}

func main() {
	_ = envload.Load()

	fmt.Println("=== Voice Framework V1: 100% Native xAI End-to-End Pipeline Example ===")
	apiKey := os.Getenv("XAI_API_KEY")
	if apiKey == "" {
		log.Println("WARNING: XAI_API_KEY is not set. Real provider calls will fail without a valid key.")
	}

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

	// 2. Initialize Native xAI STT Processor (WebSocket streaming)
	sttCfg := xai.DefaultSTTConfig()
	sttCfg.APIKey = apiKey
	sttCfg.Language = "en"
	sttProc, err := xai.NewSTTProcessor(sttCfg)
	if err != nil {
		log.Fatalf("Failed to create xAI STT processor: %v", err)
	}
	defer sttProc.Close()

	// 3. Bounded Conversation Context & User Context Processor
	convCtx := core.NewConversationContext(core.ContextConfig{
		MaxMessages: 20,
		MaxTokens:   4096,
	})
	transcriptCfg := coretranscript.DefaultConfig()
	transcriptProc, _ := coretranscript.NewProcessor(transcriptCfg)
	userCtxProc := core.NewUserContextProcessor(convCtx, core.UserContextConfig{TurnPrefix: "turn", RequireCommittedTranscript: true})

	// 4. Initialize Native xAI LLM Processor (Grok-2 streaming)
	model := "grok-2-1212"
	if envModel := os.Getenv("XAI_MODEL"); envModel != "" {
		model = envModel
	}
	llmCfg := xai.Config{
		Model:        model,
		Endpoint:     "https://api.x.ai/v1/responses",
		APIKey:       apiKey,
		SystemPrompt: "You are a concise voice assistant powered entirely by xAI. Keep your answers brief, friendly, and natural for speech.",
	}
	xaiLLMProc, err := xai.NewProcessor(llmCfg)
	if err != nil {
		log.Fatalf("Failed to create xAI LLM processor: %v", err)
	}

	// 5. Assistant Context Processor
	asstCtxProc := core.NewAssistantContextProcessor(convCtx, core.DefaultAssistantContextConfig())

	// 6. Initialize Native xAI TTS Processor (REST PCM 24kHz)
	ttsCfg := xai.DefaultTTSConfig()
	ttsCfg.APIKey = apiKey
	ttsCfg.Voice = "eve"
	ttsCfg.Language = "en"
	ttsProc, err := xai.NewTTSProcessor(ttsCfg)
	if err != nil {
		log.Fatalf("Failed to create xAI TTS processor: %v", err)
	}
	defer ttsProc.Close()

	// 7. Output Audio Collector Sink
	collector := newAudioCollector()

	// 8. Assemble Full xAI Pipeline:
	// VAD -> xAI STT -> UserCtx -> xAI LLM -> AsstCtx -> xAI TTS -> AudioCollector
	pipeline, err := core.NewPipeline(
		vadProc,
		sttProc,
		transcriptProc,
		userCtxProc,
		xaiLLMProc,
		asstCtxProc,
		ttsProc,
		collector,
	)
	if err != nil {
		log.Fatalf("Failed to assemble full xAI pipeline: %v", err)
	}

	// 9. Create and Start Session
	session, err := core.NewSession(1, pipeline, core.DefaultResourceBudget())
	if err != nil {
		log.Fatalf("Failed to create session: %v", err)
	}
	if err := session.Start(); err != nil {
		log.Fatalf("Failed to start session: %v", err)
	}
	defer session.Close()

	fmt.Printf("Full xAI Session started with %d processors:\n", len(pipeline.Processors()))
	fmt.Println("  [0] Silero VAD")
	fmt.Println("  [1] xAI STT (WebSocket)")
	fmt.Println("  [2] UserContext")
	fmt.Println("  [3] xAI LLM (Grok-2)")
	fmt.Println("  [4] AssistantContext")
	fmt.Println("  [5] xAI TTS (Eve 24kHz PCM)")
	fmt.Println("  [6] Output Audio Sink")

	// 10. Feed input audio from clip1.wav or synthetic 16kHz audio
	audioData := loadAudioClip("assets/speech/clip1.wav")
	fmt.Printf("\nStreaming %d bytes of 16kHz audio (approx %.2f seconds)...\n",
		len(audioData), float64(len(audioData))/(16000.0*2.0))

	// Feed in 20ms chunks (640 bytes for 16kHz 16-bit mono)
	chunkSize := 640
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for offset := 0; offset < len(audioData); offset += chunkSize {
		end := offset + chunkSize
		if end > len(audioData) {
			end = len(audioData)
		}
		chunk := audioData[offset:end]
		if len(chunk) < chunkSize {
			// Pad chunk with zeroes
			padded := make([]byte, chunkSize)
			copy(padded, chunk)
			chunk = padded
		}

		err := session.Post(&core.InputAudioFrame{
			Samples:    chunk,
			Encoding:   core.EncodingPCM16LE,
			SampleRate: 16000,
			Channels:   1,
			Duration:   20 * time.Millisecond,
		})
		if err != nil {
			log.Printf("Error posting audio frame: %v", err)
			break
		}
		<-ticker.C
	}

	fmt.Println("\nFinished streaming input audio. Waiting for synthesis response...")

	// Wait up to 15 seconds for synthesis to complete
	select {
	case <-collector.doneCh:
		collector.mu.Lock()
		fmt.Printf("\nSUCCESS: Synthesized %d chunks (%d bytes of 24kHz PCM audio)\n",
			collector.chunks, collector.audioBytes)
		collector.mu.Unlock()
	case <-time.After(15 * time.Second):
		collector.mu.Lock()
		if collector.chunks > 0 {
			fmt.Printf("\nSynthesized %d chunks (%d bytes of 24kHz PCM audio)\n",
				collector.chunks, collector.audioBytes)
		} else {
			fmt.Println("\nTimed out waiting for synthesis response.")
		}
		collector.mu.Unlock()
	}

	_ = session.Close()
	<-session.Done()
	fmt.Println("Session terminated cleanly.")
}

// loadAudioClip loads a 16kHz mono 16-bit WAV file and strips standard 44-byte WAV header.
func loadAudioClip(path string) []byte {
	f, err := os.Open(path)
	if err != nil {
		log.Printf("Could not open audio file %s (%v); using 1 second of silence", path, err)
		return make([]byte, 32000)
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		log.Printf("Could not read audio file %s; using silence", path)
		return make([]byte, 32000)
	}

	pcmData := data
	// Check if WAV header is present (RIFF .... WAVE)
	if len(data) > 44 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WAVE" {
		// Parse fmt chunk to find data chunk offset
		offset := 12
		for offset+8 <= len(data) {
			chunkID := string(data[offset : offset+4])
			chunkSize := binary.LittleEndian.Uint32(data[offset+4 : offset+8])
			if chunkID == "data" {
				dataStart := offset + 8
				dataEnd := dataStart + int(chunkSize)
				if dataEnd > len(data) {
					dataEnd = len(data)
				}
				pcmData = bytes.Clone(data[dataStart:dataEnd])
				break
			}
			offset += 8 + int(chunkSize)
		}
		if pcmData == nil || len(pcmData) == len(data) {
			pcmData = bytes.Clone(data[44:])
		}
	}

	// Append 1.5s of trailing silence (48,000 bytes) so VAD detects end of user speech cleanly
	silence := make([]byte, 48000)
	return append(pcmData, silence...)
}
