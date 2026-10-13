package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"govoice/core"
	"govoice/core/coretest"
)

// ProfanityMaskProcessor is a custom application-level core.Processor that inspects
// LLMTextFrame instances and masks prohibited words before passing them downstream.
// This proves that framework consumers can extend pipeline behavior with custom logic
// without modifying any core runtime code.
type ProfanityMaskProcessor struct {
	blockedWords []string
	session      core.SessionController
	index        int
	mu           sync.Mutex
	maskedCount  int
}

// NewProfanityMaskProcessor creates a new custom filter processor.
func NewProfanityMaskProcessor(blockedWords []string) *ProfanityMaskProcessor {
	return &ProfanityMaskProcessor{
		blockedWords: blockedWords,
	}
}

// Attach registers the session controller and pipeline index.
func (p *ProfanityMaskProcessor) Attach(s core.SessionController, index int) error {
	p.session = s
	p.index = index
	return nil
}

// ProcessFrame inspects and filters frames flowing through the pipeline.
func (p *ProfanityMaskProcessor) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	switch frame := f.(type) {
	case *core.LLMTextFrame:
		// Check for blocked words
		text := frame.Text
		masked := text
		modified := false
		for _, w := range p.blockedWords {
			if strings.Contains(strings.ToLower(masked), strings.ToLower(w)) {
				replacement := strings.Repeat("*", len(w))
				lowerM := strings.ToLower(masked)
				lowerW := strings.ToLower(w)
				idx := strings.Index(lowerM, lowerW)
				for idx != -1 {
					masked = masked[:idx] + replacement + masked[idx+len(w):]
					lowerM = strings.ToLower(masked)
					idx = strings.Index(lowerM, lowerW)
				}
				modified = true
			}
		}

		if modified {
			p.mu.Lock()
			p.maskedCount++
			p.mu.Unlock()
			fmt.Printf("[CustomProcessor] Filtered text: %q -> %q\n", text, masked)
			// Emit sanitized frame
			sanitizedFrame := &core.LLMTextFrame{
				Text: masked,
				Turn: frame.Turn,
				Gen:  frame.Gen,
			}
			return e.PushDirection(sanitizedFrame, dir)
		}
		return e.PushDirection(f, dir)

	default:
		// Pass all other frames (audio, control, system) untouched
		return e.PushDirection(f, dir)
	}
}

// MaskedCount returns how many text items were sanitized.
func (p *ProfanityMaskProcessor) MaskedCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.maskedCount
}

func main() {
	fmt.Println("=== Voice Framework V1: Custom Processor Example ===")

	// 1. Initialize custom processor
	customFilter := NewProfanityMaskProcessor([]string{"secret", "classified", "forbidden"})

	// 2. Build pipeline with custom processor inserted between upstream and sink
	sourceProc := &coretest.PassthroughProcessor{}
	sinkProc := &coretest.PassthroughProcessor{}

	pipeline, err := core.NewPipeline(
		sourceProc,
		customFilter,
		sinkProc,
	)
	if err != nil {
		log.Fatalf("Failed to assemble pipeline with custom processor: %v", err)
	}

	session, err := core.NewSession(3, pipeline, core.DefaultResourceBudget())
	if err != nil {
		log.Fatalf("Failed to create session: %v", err)
	}
	if err := session.Start(); err != nil {
		log.Fatalf("Failed to start session: %v", err)
	}

	// Send normal frame
	_ = session.Post(&core.LLMTextFrame{
		Text: "Hello, this is public information.",
		Turn: "turn-1",
		Gen:  1,
	})

	// Send frame containing sensitive word
	_ = session.Post(&core.LLMTextFrame{
		Text: "Please do not reveal the classified credentials.",
		Turn: "turn-1",
		Gen:  1,
	})

	// Wait briefly for frames to process through pipeline
	time.Sleep(100 * time.Millisecond)

	if err := session.Close(); err != nil {
		log.Printf("Session close error: %v", err)
	}
	<-session.Done()

	fmt.Printf("Custom processor executed cleanly. Total masked frames: %d\n", customFilter.MaskedCount())
}
