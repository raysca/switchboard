package core

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

// AssistantContextConfig configures synthesis segmentation and flush timeouts.
type AssistantContextConfig struct {
	MinUnitLength int           // Minimum character count before splitting on punctuation (default: 20)
	MaxUnitLength int           // Maximum character count per synthesis unit (default: 150)
	FlushTimeout  time.Duration // Timeout waiting for transport flush acknowledgement (default: 200ms)
}

// DefaultAssistantContextConfig returns a safe, production-tuned default configuration.
func DefaultAssistantContextConfig() AssistantContextConfig {
	return AssistantContextConfig{
		MinUnitLength: 10,
		MaxUnitLength: 150,
		FlushTimeout:  200 * time.Millisecond,
	}
}

// AssistantContextProcessor segments LLM text into explicit synthesis units, tracks
// transport write completions, commits delivered units to conversation context,
// coordinates the interruption barrier, and enforces partial-turn semantics upon barge-in.
type AssistantContextProcessor struct {
	session SessionController
	index   int

	ctxPool *ConversationContext
	cfg     AssistantContextConfig

	activeTurn     string
	activeGen      uint64
	textBuf        strings.Builder
	totalTurnText  strings.Builder
	unitCounter    uint64
	finalUnitID    uint64 // 0 until final unit is emitted
	llmComplete    bool   // true once complete LLMText has been received
	ttsStopped     bool
	pendingUnits   map[uint64]string
	deliveredUnits map[uint64]string

	// Flush barrier coordination
	flushAckCh        chan uint64
	pendingFlushGen   atomic.Uint64
	FlushAcks         atomic.Uint64
	FlushTimeouts     atomic.Uint64
	Degraded          atomic.Uint64
	InterruptionCount atomic.Uint64
}

// NewAssistantContextProcessor creates an initialized AssistantContextProcessor.
func NewAssistantContextProcessor(ctxPool *ConversationContext, cfg AssistantContextConfig) *AssistantContextProcessor {
	if cfg.MinUnitLength <= 0 {
		cfg.MinUnitLength = 10
	}
	if cfg.MaxUnitLength <= 0 {
		cfg.MaxUnitLength = 150
	}
	if cfg.FlushTimeout <= 0 {
		cfg.FlushTimeout = 200 * time.Millisecond
	}

	return &AssistantContextProcessor{
		ctxPool:        ctxPool,
		cfg:            cfg,
		pendingUnits:   make(map[uint64]string),
		deliveredUnits: make(map[uint64]string),
		flushAckCh:     make(chan uint64, 8),
	}
}

// Attach registers the processor with the session pipeline and supervisor.
func (p *AssistantContextProcessor) Attach(s SessionController, index int) error {
	p.session = s
	p.index = index
	return nil
}

// ProcessFrame handles text streaming, synthesis units, write completions, and interruption barriers.
func (p *AssistantContextProcessor) ProcessFrame(ctx context.Context, f Frame, dir Direction, e Emitter) error {
	switch f.Type() {
	case FrameUserMessage:
		// New user turn received: reset assistant turn state
		um := f.(*UserMessageFrame)
		p.activeTurn = um.Turn
		p.activeGen = um.Gen
		p.textBuf.Reset()
		p.totalTurnText.Reset()
		p.unitCounter = 0
		p.finalUnitID = 0
		p.llmComplete = false
		p.ttsStopped = false
		p.pendingUnits = make(map[uint64]string)
		p.deliveredUnits = make(map[uint64]string)
		return e.PushDirection(f, dir)

	case FrameLLMTextDelta:
		delta := f.(*LLMTextDeltaFrame)
		if p.isStaleTurn(delta.Turn, delta.Gen) {
			return nil
		}
		p.textBuf.WriteString(delta.Delta)
		p.totalTurnText.WriteString(delta.Delta)
		if err := e.PushDirection(f, dir); err != nil {
			return err
		}
		return p.segmentBuffer(e, false)

	case FrameLLMText:
		lt := f.(*LLMTextFrame)
		if p.isStaleTurn(lt.Turn, lt.Gen) {
			return nil
		}
		p.llmComplete = true
		// Providers emit deltas and then a final frame containing the full
		// accumulated response. Append only text that is not already buffered.
		p.appendFinalText(lt.Text)
		if err := e.PushDirection(f, dir); err != nil {
			return err
		}
		return p.segmentBuffer(e, true)

	case FrameTTSStopped:
		ts := f.(*TTSStoppedFrame)
		if p.isStaleTurn(ts.Turn, ts.Gen) {
			return nil
		}
		p.ttsStopped = true
		if p.llmComplete && p.finalUnitID > 0 && len(p.deliveredUnits) >= len(p.pendingUnits) {
			return p.commitTurn(e, false)
		}
		return e.PushDirection(f, dir)

	case FrameWriteCompletion:
		wc := f.(*WriteCompletionFrame)
		if p.isStaleTurn(wc.Turn, wc.Gen) {
			return nil
		}
		if wc.IsFinal {
			if wc.Err != nil {
				return p.failUndeliveredTurn(e, wc.Err)
			}
			// Final audio frame of unit wc.UnitID was written to the client.
			if unitText, exists := p.pendingUnits[wc.UnitID]; exists {
				p.deliveredUnits[wc.UnitID] = unitText
			}
			// If this was the final unit of the turn and LLM has completed, commit complete assistant turn
			if p.llmComplete && p.finalUnitID > 0 && wc.UnitID == p.finalUnitID {
				return p.commitTurn(e, false)
			}
		}
		return nil

	case FrameUserStartedSpeaking:
		// If assistant is active (generating or playing audio), user speech triggers barge-in
		if p.activeTurn != "" {
			return p.handleInterruption(e)
		}
		return e.PushDirection(f, dir)

	case FrameInterruption:
		intr := f.(*InterruptionFrame)
		if p.activeTurn != "" && (intr.Turn == "" || intr.Turn == p.activeTurn) {
			return p.handleInterruption(e)
		}
		return e.PushDirection(f, dir)

	case FrameFlushProcessed:
		fp := f.(*FlushProcessedFrame)
		if p.pendingFlushGen.Load() == fp.Gen {
			p.FlushAcks.Add(1)
			select {
			case p.flushAckCh <- fp.Gen:
			default:
			}
		}
		return e.PushDirection(f, dir)

	default:
		return e.PushDirection(f, dir)
	}
}

func (p *AssistantContextProcessor) appendFinalText(text string) {
	if text == "" {
		return
	}
	total := p.totalTurnText.String()
	if total == "" {
		// Non-streaming LLM: no deltas were previously ingested
		p.textBuf.WriteString(text)
		p.totalTurnText.WriteString(text)
		return
	}
	if strings.HasPrefix(text, total) {
		// Only append remainder not covered by streamed deltas
		tail := text[len(total):]
		p.textBuf.WriteString(tail)
		p.totalTurnText.WriteString(tail)
		return
	}
	if text == total || strings.Contains(total, text) || strings.TrimSpace(text) == strings.TrimSpace(total) {
		return
	}
	accumulated := p.textBuf.String()
	if strings.HasPrefix(text, accumulated) {
		tail := text[len(accumulated):]
		p.textBuf.WriteString(tail)
		p.totalTurnText.WriteString(tail)
		return
	}
	if !strings.Contains(accumulated, text) {
		p.textBuf.WriteString(text)
		p.totalTurnText.WriteString(text)
	}
}

func (p *AssistantContextProcessor) isStaleTurn(turn string, gen uint64) bool {
	if gen != 0 && p.session != nil && gen < p.session.CurrentGeneration() {
		return true
	}
	if p.activeTurn != "" && turn != "" && turn != p.activeTurn {
		return true
	}
	return false
}

// segmentBuffer splits buffered text into explicit synthesis units.
func (p *AssistantContextProcessor) segmentBuffer(e Emitter, isLastChunk bool) error {
	buf := p.textBuf.String()

	for {
		if len(buf) == 0 {
			break
		}

		splitIdx := -1
		// Check sentence boundaries: '.', '!', '?', '\n'
		for i, r := range buf {
			if i >= p.cfg.MinUnitLength {
				if r == '.' || r == '!' || r == '?' || r == '\n' {
					// Split after punctuation and trailing space
					splitIdx = i + 1
					for splitIdx < len(buf) && (buf[splitIdx] == ' ' || buf[splitIdx] == '\t') {
						splitIdx++
					}
					break
				}
			}
			if i >= p.cfg.MaxUnitLength && (r == ' ' || r == ',') {
				splitIdx = i + 1
				break
			}
		}

		if splitIdx == -1 {
			if isLastChunk && len(buf) > 0 {
				splitIdx = len(buf)
			} else {
				break
			}
		}

		unitText := strings.TrimSpace(buf[:splitIdx])
		buf = buf[splitIdx:]

		if len(unitText) > 0 {
			p.unitCounter++
			unitID := p.unitCounter
			p.pendingUnits[unitID] = unitText

			isFinalUnit := isLastChunk && len(buf) == 0
			if isFinalUnit {
				p.finalUnitID = unitID
			}

			gen := uint64(0)
			if p.session != nil {
				gen = p.session.CurrentGeneration()
			}

			unitFrame := &SynthesisUnitFrame{
				Turn:    p.activeTurn,
				Gen:     gen,
				UnitID:  unitID,
				Text:    unitText,
				IsFinal: isFinalUnit,
			}
			if err := e.Push(unitFrame); err != nil {
				return err
			}
		}
	}

	p.textBuf.Reset()
	p.textBuf.WriteString(buf)

	if isLastChunk && len(buf) == 0 && p.unitCounter > 0 {
		p.finalUnitID = p.unitCounter
		if _, delivered := p.deliveredUnits[p.finalUnitID]; delivered {
			return p.commitTurn(e, false)
		}
	}
	return nil
}

// commitTurn records the fully delivered assistant turn to conversation context.
func (p *AssistantContextProcessor) commitTurn(e Emitter, partial bool) error {
	if p.activeTurn == "" {
		return nil
	}

	// Reconstruct delivered content in sequential unit order
	var parts []string
	for id := uint64(1); id <= p.unitCounter; id++ {
		if text, exists := p.deliveredUnits[id]; exists {
			parts = append(parts, text)
		} else {
			break
		}
	}

	content := strings.Join(parts, " ")
	gen := p.activeGen
	if p.session != nil {
		gen = p.session.CurrentGeneration()
	}

	asstMsg := Message{
		Role:      RoleAssistant,
		Content:   content,
		TurnID:    p.activeTurn,
		Partial:   partial,
		CreatedAt: time.Now(),
	}

	if len(content) > 0 {
		if err := p.ctxPool.AddMessage(asstMsg); err != nil {
			return err
		}
	}

	_ = e.Push(&AssistantMessageFrame{
		Turn:      p.activeTurn,
		Content:   content,
		Gen:       gen,
		Partial:   partial,
		Timestamp: asstMsg.CreatedAt,
	})

	// Reset turn state
	p.activeTurn = ""
	p.textBuf.Reset()
	p.unitCounter = 0
	p.finalUnitID = 0
	p.llmComplete = false
	p.ttsStopped = false
	p.pendingUnits = make(map[uint64]string)
	p.deliveredUnits = make(map[uint64]string)

	return nil
}

// failUndeliveredTurn refuses to commit assistant text when the final audio write failed.
func (p *AssistantContextProcessor) failUndeliveredTurn(e Emitter, writeErr error) error {
	p.activeTurn = ""
	p.textBuf.Reset()
	p.unitCounter = 0
	p.finalUnitID = 0
	p.llmComplete = false
	p.ttsStopped = false
	p.pendingUnits = make(map[uint64]string)
	p.deliveredUnits = make(map[uint64]string)
	return e.Push(&ErrorFrame{
		Err:   fmt.Errorf("assistant audio was not delivered: %w", writeErr),
		Fatal: true,
	})
}

// handleInterruption executes the server-side interruption barrier and partial context rule.
func (p *AssistantContextProcessor) handleInterruption(e Emitter) error {
	p.InterruptionCount.Add(1)
	interruptedTurn := p.activeTurn
	oldGen := uint64(0)
	if p.session != nil {
		oldGen = p.session.CurrentGeneration()
		// Advance generation immediately to fence all late old-generation work
		p.session.IncrementGeneration()
	}

	// 1. Partial context rule:
	// Only complete synthesis units whose final audio frame was successfully written
	// before the barrier are included. A partly written unit is excluded in full.
	var delivered []string
	for id := uint64(1); id <= p.unitCounter; id++ {
		if text, exists := p.deliveredUnits[id]; exists {
			delivered = append(delivered, text)
		} else {
			break
		}
	}

	if len(delivered) > 0 {
		partialText := strings.Join(delivered, " ")
		asstMsg := Message{
			Role:      RoleAssistant,
			Content:   partialText,
			TurnID:    interruptedTurn,
			Partial:   true,
			CreatedAt: time.Now(),
		}
		_ = p.ctxPool.AddMessage(asstMsg)
		_ = e.Push(&AssistantMessageFrame{
			Turn:      interruptedTurn,
			Content:   partialText,
			Gen:       oldGen,
			Partial:   true,
			Timestamp: asstMsg.CreatedAt,
		})
	}
	// If len(delivered) == 0, no assistant text is committed to context.

	// Clear active assistant state
	p.activeTurn = ""
	p.textBuf.Reset()
	p.unitCounter = 0
	p.finalUnitID = 0
	p.llmComplete = false
	p.ttsStopped = false
	p.pendingUnits = make(map[uint64]string)
	p.deliveredUnits = make(map[uint64]string)

	// 2. Emit InterruptionFrame downstream and upstream
	intrFrame := &InterruptionFrame{Turn: interruptedTurn, Gen: oldGen}
	_ = e.Push(intrFrame)
	_ = e.PushDirection(intrFrame, Upstream)

	// Store the expected generation before the flush is emitted. The transport
	// acknowledgement is synchronous and would otherwise be discarded.
	p.pendingFlushGen.Store(oldGen)

	// 3. Emit FlushFrame downstream
	flushFrame := &FlushFrame{Gen: oldGen}
	_ = e.Push(flushFrame)

	if p.session != nil {
		p.startFlushBarrierTimeout(oldGen)
	}

	return nil
}

func (p *AssistantContextProcessor) startFlushBarrierTimeout(gen uint64) {
	_ = p.session.Go(func(ctx context.Context) {
		timer := time.NewTimer(p.cfg.FlushTimeout)
		defer timer.Stop()

		select {
		case ackGen := <-p.flushAckCh:
			if ackGen == gen {
				// Flush barrier acknowledged in time
				return
			}
		case <-timer.C:
			if p.pendingFlushGen.Load() != gen {
				return
			}
			p.FlushTimeouts.Add(1)
			p.Degraded.Add(1)
			// Non-fatal, generation 0 so the fence does not drop the degradation.
			_ = p.session.Reenter(&ErrorFrame{
				Err:   fmt.Errorf("flush barrier timed out for generation %d; session degraded", gen),
				Fatal: false,
			}, Downstream, p.index, 0)
		case <-ctx.Done():
			return
		}
	})
}
