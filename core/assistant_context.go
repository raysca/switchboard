package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// AssistantContextConfig configures synthesis segmentation and flush timeouts.
type AssistantContextConfig struct {
	MinUnitLength int           // Minimum character count before splitting on punctuation (default: 20)
	MaxUnitLength int           // Maximum character count per synthesis unit (default: 150)
	FlushTimeout  time.Duration // Timeout waiting for transport flush acknowledgement (default: 200ms)
	// BargeInOnTurnEvent moves barge-in from raw VAD speech start to the turn
	// manager's TurnEventStarted. Set only when a turn.Processor is in the pipeline.
	BargeInOnTurnEvent bool
	// FirstClauseMinLength lets the first unit of a turn end at a clause
	// boundary (comma, semicolon, colon, dash) once it has this many characters,
	// so audio starts before the first full sentence is generated. 0 disables.
	FirstClauseMinLength int
	// Completion enables the LLM turn-completion marker protocol (see
	// turn_completion.go). Requires BargeInOnTurnEvent's turn manager; nil = off.
	Completion *TurnCompletionConfig
	// Summary, when it has a Summarizer, summarizes older history after each
	// completed assistant turn (see ContextSummarizer). Nil = off.
	Summary *SummaryConfig
	// SlackMerge, when set, merges sentences for unary TTS providers (see
	// SlackMergeConfig). Nil = off, one unit per sentence.
	SlackMerge *SlackMergeConfig
	// DisableTextCleaning skips stripping markdown formatting, emojis, and tags from
	// synthesis units. When false (default), CleanTTSText sanitizes text before TTS.
	DisableTextCleaning bool
}

// DefaultAssistantContextConfig returns a safe, production-tuned default configuration.
func DefaultAssistantContextConfig() AssistantContextConfig {
	return AssistantContextConfig{
		MinUnitLength: 10,
		MaxUnitLength: 150,
		FlushTimeout:  200 * time.Millisecond,
		// Off, like Pipecat: a clause cut changes TTS prosody, and there is no
		// latency measurement yet showing it pays. Opt in via first_clause_chars.
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

	activeTurn      string
	active          atomic.Bool // mirrors activeTurn != "" for other goroutines
	activeGen       uint64
	textBuf         strings.Builder
	totalTurnText   strings.Builder
	unitCounter     uint64 // session-global; never reused within this processor
	firstUnitID     uint64 // first unit in the active turn
	finalUnitID     uint64 // 0 until final unit is emitted
	finalSignalSent bool
	llmComplete     bool // true once complete LLMText has been received
	pendingUnits    map[uint64]string
	deliveredUnits  map[uint64]string
	heard           *HeardText
	summ            *ContextSummarizer
	slack           *slackMerger // nil = off

	// Turn-completion protocol state (nil gate = protocol off for this turn).
	gate      *completionGate
	holdShort bool
	holdMu    sync.Mutex
	holdStop  chan struct{}
	held      atomic.Bool
	nudgeN    atomic.Uint64
	sayN      atomic.Uint64 // fixed lines queued through Say

	// Flush barrier coordination
	pendingFlushGen   atomic.Uint64
	ackedFlushGen     atomic.Uint64
	FlushTimeouts     atomic.Uint64
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

	if cfg.Completion != nil {
		c := *cfg.Completion
		c.defaults()
		cfg.Completion = &c
	}

	var slack *slackMerger
	if cfg.SlackMerge != nil {
		slack = newSlackMerger(*cfg.SlackMerge)
	}
	return &AssistantContextProcessor{
		slack:          slack,
		ctxPool:        ctxPool,
		cfg:            cfg,
		pendingUnits:   make(map[uint64]string),
		deliveredUnits: make(map[uint64]string),
		heard:          NewHeardText(),
	}
}

// Attach registers the processor with the session pipeline and supervisor.
func (p *AssistantContextProcessor) Attach(s SessionController, index int) error {
	p.session = s
	p.index = index
	if sc := p.cfg.Summary; sc != nil && sc.Summarizer != nil {
		var emit func(DebugEvent)
		if d, ok := s.(interface{ EmitDebug(DebugEvent) }); ok {
			emit = d.EmitDebug
		}
		p.summ = NewContextSummarizer(p.ctxPool, *sc, s.Go, emit)
	}
	return nil
}

// sayTurnPrefix marks the turns Say opens: not model replies (no completion gate, no latency events).
const sayTurnPrefix = "say-"

// ErrSayEmpty is returned by Say for text with nothing to speak.
var ErrSayEmpty = errors.New("core: nothing to say")

// Say speaks text as the assistant's next turn: the same units, delivery tracking, history commit and
// interruption rules as a model reply, without a model (a greeting). It enters at this processor, so
// the line never reaches the LLM stage. It only queues; it fails if the text is empty or the session is closing.
func (p *AssistantContextProcessor) Say(text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return ErrSayEmpty
	}
	if p.session == nil {
		return errors.New("core: assistant context is not attached to a session")
	}
	turn := fmt.Sprintf("%s%d", sayTurnPrefix, p.sayN.Add(1))
	gen := p.session.CurrentGeneration()
	now := time.Now()
	if err := p.session.Reenter(&UserMessageFrame{Turn: turn, Gen: gen, Timestamp: now}, Downstream, p.index, gen); err != nil {
		return fmt.Errorf("say: %w", err)
	}
	// Like a model reply: the text as a delta (the transport streams it to the client), then the complete text.
	if err := p.session.Reenter(&LLMTextDeltaFrame{Turn: turn, Gen: gen, Delta: text}, Downstream, p.index, gen); err != nil {
		return fmt.Errorf("say: %w", err)
	}
	if err := p.session.Reenter(&LLMTextFrame{Turn: turn, Gen: gen, Text: text}, Downstream, p.index, gen); err != nil {
		return fmt.Errorf("say: %w", err)
	}
	return nil
}

// ProcessFrame handles text streaming, synthesis units, write completions, and interruption barriers.
func (p *AssistantContextProcessor) ProcessFrame(ctx context.Context, f Frame, dir Direction, e Emitter) error {
	switch f.Type() {
	case FrameUserMessage:
		// New user turn received: reset assistant turn state
		um := f.(*UserMessageFrame)
		p.activeTurn = um.Turn
		p.active.Store(true)
		p.activeGen = um.Gen
		if p.activeGen == 0 && p.session != nil {
			p.activeGen = p.session.CurrentGeneration()
		}
		p.textBuf.Reset()
		p.totalTurnText.Reset()
		p.firstUnitID = 0
		p.finalUnitID = 0
		p.finalSignalSent = false
		p.llmComplete = false
		p.pendingUnits = make(map[uint64]string)
		p.deliveredUnits = make(map[uint64]string)
		p.heard.Reset()
		p.slackReset()
		p.cancelHold()
		p.gate = nil
		if p.cfg.Completion != nil {
			p.gate = &completionGate{lenient: isSyntheticTurn(um.Turn)}
		}
		return e.PushDirection(f, dir)

	case FrameToolCall:
		if p.gate != nil && !p.gate.decided {
			p.gate.lenient = true // a tool call before any text: the turn is complete
		}
		return e.PushDirection(f, dir)

	case FrameLLMTextDelta:
		delta := f.(*LLMTextDeltaFrame)
		if p.isStaleTurn(delta.Turn, delta.Gen) {
			return nil
		}
		if p.gate != nil {
			out := p.gateFeed(delta.Delta)
			if out == "" {
				return nil
			}
			if out != delta.Delta {
				delta = &LLMTextDeltaFrame{Delta: out, Turn: delta.Turn, Gen: delta.Gen}
				f = delta
			}
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
		if p.gate != nil {
			text, held := p.gateFinal(lt.Text)
			if held { // incomplete user turn: nothing is spoken or recorded; wait for the user
				short := p.holdShort
				err := e.PushDirection(&LLMTextFrame{Turn: lt.Turn, Gen: lt.Gen}, dir)
				p.resetTurn()
				p.armHold(short)
				return err
			}
			lt = &LLMTextFrame{Text: text, Turn: lt.Turn, Gen: lt.Gen}
			f = lt
		}
		p.llmComplete = true
		// Providers emit deltas and then a final frame containing the full
		// accumulated response. Append only text that is not already buffered.
		p.appendFinalText(lt.Text)
		if err := e.PushDirection(f, dir); err != nil {
			return err
		}
		if err := p.segmentBuffer(e, true); err != nil {
			return err
		}
		if p.firstUnitID == 0 { // nothing to speak: the reply is over (Active must not stick)
			p.resetTurn()
		}
		return nil

	case FrameWriteCompletion:
		wc := f.(*WriteCompletionFrame)
		if p.isStaleTurn(wc.Turn, wc.Gen) {
			return nil
		}
		if wc.Failed {
			// A unit that produced no audio: drop it from the reply and, if it
			// was the last one outstanding, close the reply out.
			delete(p.pendingUnits, wc.UnitID)
			return p.finishIfDelivered(e)
		}
		if wc.Err == nil {
			if p.slack != nil {
				p.slack.observe(wc.UnitID, p.firstUnitID, wc.Media, wc.IsFinal)
				// Playback writes are the clock tick: held text still goes at the
				// last safe moment when the LLM stalls (slow tokens, a tool call).
				if err := p.emitUnits(e, p.slack.due(false)); err != nil {
					return err
				}
			}
			p.heard.Played(wc.UnitID, wc.Media, wc.WordTimings, wc.IsFinal)
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
			if p.llmComplete && p.finalUnitID > 0 && p.allPendingDelivered() {
				return p.commitTurn(e, false)
			}
		}
		return nil

	case FrameUserStartedSpeaking:
		p.cancelHold()
		// If assistant is active (generating or playing audio), user speech
		// triggers barge-in. The speech event still reaches the transport after
		// the barrier so the client sees user-started-speaking.
		if p.activeTurn != "" && !p.cfg.BargeInOnTurnEvent {
			if err := p.handleInterruption(e); err != nil {
				return err
			}
		}
		return e.PushDirection(f, dir)

	case FrameTurnEvent:
		if f.(*TurnEventFrame).Kind == TurnEventStarted {
			p.cancelHold()
		}
		if p.cfg.BargeInOnTurnEvent && p.activeTurn != "" && f.(*TurnEventFrame).Kind == TurnEventStarted {
			if err := p.handleInterruption(e); err != nil {
				return err
			}
		}
		return e.PushDirection(f, dir)

	case FrameInterruption:
		intr := f.(*InterruptionFrame)
		p.cancelHold()
		if p.activeTurn != "" && (intr.Turn == "" || intr.Turn == p.activeTurn) && (intr.Gen == 0 || intr.Gen == p.activeGen) {
			return p.handleInterruption(e)
		}
		return e.PushDirection(f, dir)

	case FrameCancel:
		cancel := f.(*CancelFrame)
		p.cancelHold()
		if p.activeTurn != "" && (cancel.Gen == 0 || cancel.Gen == p.activeGen) {
			p.resetTurn()
		}
		return e.PushDirection(f, dir)

	case FrameFlushProcessed:
		fp := f.(*FlushProcessedFrame)
		if p.pendingFlushGen.Load() == fp.Gen {
			p.ackedFlushGen.Store(fp.Gen)
		}
		return e.PushDirection(f, dir)

	default:
		return e.PushDirection(f, dir)
	}
}

// finishIfDelivered ends the reply once the model is done and every unit still
// pending has played; a reply with nothing heard is dropped, not committed.
func (p *AssistantContextProcessor) finishIfDelivered(e Emitter) error {
	if !p.llmComplete || p.finalUnitID == 0 {
		return nil
	}
	for id := range p.pendingUnits {
		if _, ok := p.deliveredUnits[id]; !ok {
			return nil
		}
	}
	if len(p.deliveredUnits) == 0 {
		p.resetTurn()
		return nil
	}
	return p.commitTurn(e, false)
}

func (p *AssistantContextProcessor) allPendingDelivered() bool {
	if len(p.pendingUnits) == 0 || len(p.deliveredUnits) != len(p.pendingUnits) {
		return false
	}
	for id := range p.pendingUnits {
		if _, ok := p.deliveredUnits[id]; !ok {
			return false
		}
	}
	return true
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
	if p.activeTurn == "" {
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

		splitIdx := nextSplit(buf, splitRules{
			min: p.cfg.MinUnitLength, max: p.cfg.MaxUnitLength, clause: p.cfg.FirstClauseMinLength,
			first: p.firstUnitID == 0, last: isLastChunk,
		})
		if splitIdx == -1 {
			break
		}
		unitText := strings.TrimSpace(buf[:splitIdx])
		buf = buf[splitIdx:]

		if !p.cfg.DisableTextCleaning {
			unitText = CleanTTSText(unitText)
		}

		if speakable(unitText) { // a stray closing quote or dash is not worth a TTS call
			restSpeakable := false
			if isLastChunk && len(buf) > 0 {
				rest := buf
				if !p.cfg.DisableTextCleaning {
					rest = CleanTTSText(rest)
				}
				restSpeakable = speakable(rest)
			}
			isFinalUnit := isLastChunk && !restSpeakable
			outs := []slackOut{{unitText, isFinalUnit}}
			if p.slack != nil {
				outs = p.slack.admit(unitText, p.firstUnitID == 0, isFinalUnit)
			}
			if err := p.emitUnits(e, outs); err != nil {
				return err
			}
		}
	}
	if p.slack != nil { // held text goes at the last safe moment (checked per delta) or at reply end
		if err := p.emitUnits(e, p.slack.due(isLastChunk && len(buf) == 0)); err != nil {
			return err
		}
	}

	p.textBuf.Reset()
	p.textBuf.WriteString(buf)

	if isLastChunk && len(buf) == 0 && p.firstUnitID > 0 {
		p.finalUnitID = p.unitCounter
		if !p.finalSignalSent {
			gen := p.activeGen
			if p.session != nil {
				gen = p.session.CurrentGeneration()
			}
			if err := e.Push(&OutputTurnFinalFrame{Turn: p.activeTurn, Gen: gen, UnitID: p.finalUnitID}); err != nil {
				return err
			}
			p.finalSignalSent = true
		}
		if p.allPendingDelivered() {
			return p.commitTurn(e, false)
		}
	}
	return nil
}

func (p *AssistantContextProcessor) slackReset() {
	if p.slack != nil {
		p.slack.reset()
	}
}

// emitUnits sends one synthesis unit per out (a merged unit is still one unit).
func (p *AssistantContextProcessor) emitUnits(e Emitter, outs []slackOut) error {
	for _, o := range outs {
		unitID := p.unitCounter + 1
		if u, ok := p.session.(interface{ NextUnitID() uint64 }); ok {
			unitID = u.NextUnitID() // shared with idle speech, so ids may skip
		}
		p.unitCounter = unitID
		if p.firstUnitID == 0 {
			p.firstUnitID = unitID
		}
		p.pendingUnits[unitID] = o.text
		p.heard.AddUnit(unitID, o.text)
		if o.final {
			p.finalUnitID = unitID
			p.finalSignalSent = true
		}
		if p.slack != nil {
			p.slack.sent(unitID, utf8.RuneCountInString(o.text))
		}
		gen := uint64(0)
		if p.session != nil {
			gen = p.session.CurrentGeneration()
		}
		if err := e.Push(&SynthesisUnitFrame{Turn: p.activeTurn, Gen: gen, UnitID: unitID, Text: o.text, IsFinal: o.final}); err != nil {
			return err
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
	for id := p.firstUnitID; id != 0 && id <= p.unitCounter; id++ {
		if text, exists := p.deliveredUnits[id]; exists {
			parts = append(parts, text)
		} else if _, ours := p.pendingUnits[id]; ours {
			break
		}
	}

	content := strings.Join(parts, " ")
	gen := p.activeGen

	asstMsg := Message{
		Role:      RoleAssistant,
		Content:   content,
		TurnID:    p.activeTurn,
		Partial:   partial,
		CreatedAt: time.Now(),
	}

	recorded := true
	if len(content) > 0 {
		var err error
		if recorded, err = p.addAssistantMessage(asstMsg); err != nil {
			return err
		}
		if recorded && p.summ != nil && !partial {
			p.summ.Maybe() // between turns, off the hot path; skips open tool rounds itself
		}
	}

	var emitErr error
	if recorded { // never announce a message that is absent from history
		emitErr = e.Push(&AssistantMessageFrame{
			Turn:      p.activeTurn,
			Content:   content,
			Gen:       gen,
			Partial:   partial,
			Timestamp: asstMsg.CreatedAt,
		})
	}

	// Reset turn state
	p.resetTurn()

	return emitErr
}

// DebugContextReplyDropped: a spoken reply did not fit the context budget and
// is missing from history (attr chars: its length).
const DebugContextReplyDropped = "context.reply_dropped"

// addAssistantMessage records a spoken reply. One that cannot fit the context
// budget (the active turn is never evicted) is skipped: losing the history
// entry is better than ending the call that just spoke it. recorded reports
// whether the message made it into history.
func (p *AssistantContextProcessor) addAssistantMessage(m Message) (recorded bool, err error) {
	err = p.ctxPool.AddMessage(m)
	if errors.Is(err, ErrContextLimitExceeded) {
		if d, ok := p.session.(interface{ EmitDebug(DebugEvent) }); ok {
			d.EmitDebug(NewDebugEvent(DebugContextReplyDropped, time.Now(), "", TurnID(m.TurnID), 0, map[string]any{"chars": len(m.Content)}))
		}
		return false, nil
	}
	return err == nil, err
}

// failUndeliveredTurn refuses to commit assistant text when the final audio write failed.
func (p *AssistantContextProcessor) failUndeliveredTurn(e Emitter, writeErr error) error {
	p.resetTurn()
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
	// Units whose final audio frame was written before the barrier are included
	// in full; the unit being spoken contributes only its heard prefix.
	var delivered []string
	id := p.firstUnitID
	for ; id != 0 && id <= p.unitCounter; id++ {
		if text, exists := p.deliveredUnits[id]; exists {
			delivered = append(delivered, text)
		} else if _, ours := p.pendingUnits[id]; ours {
			break
		}
	}
	// The unit being spoken at the barrier contributes only what was heard of
	// it (by word timings, else a proportional estimate), never the whole unit.
	if id != 0 && id <= p.unitCounter {
		if h := p.heard.Heard(id); h != "" {
			delivered = append(delivered, h)
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
		recorded, err := p.addAssistantMessage(asstMsg)
		if err != nil {
			return err
		}
		if recorded {
			if err := e.Push(&AssistantMessageFrame{
				Turn:      interruptedTurn,
				Content:   partialText,
				Gen:       oldGen,
				Partial:   true,
				Timestamp: asstMsg.CreatedAt,
			}); err != nil {
				p.resetTurn()
				return err
			}
		}
	}
	// If len(delivered) == 0, no assistant text is committed to context.

	// Clear active assistant state
	p.resetTurn()

	// 2. Emit InterruptionFrame downstream and upstream
	intrFrame := &InterruptionFrame{Turn: interruptedTurn, Gen: oldGen, committed: true}
	_ = e.Push(intrFrame)
	_ = e.PushDirection(intrFrame, Upstream)

	// Store the expected generation before the flush is emitted. The transport
	// acknowledgement is synchronous and would otherwise be discarded.
	p.pendingFlushGen.Store(oldGen)

	// 3. Emit FlushFrame downstream
	flushFrame := &FlushFrame{Gen: oldGen, committed: true}
	_ = e.Push(flushFrame)

	if p.session != nil {
		p.startFlushBarrierTimeout(oldGen)
	}

	return nil
}

// Active reports whether a reply is generating or playing (safe from any goroutine).
func (p *AssistantContextProcessor) Active() bool { return p.active.Load() }

func (p *AssistantContextProcessor) resetTurn() {
	p.activeTurn = ""
	p.active.Store(false)
	p.activeGen = 0
	p.textBuf.Reset()
	p.totalTurnText.Reset()
	p.firstUnitID = 0
	p.finalUnitID = 0
	p.finalSignalSent = false
	p.llmComplete = false
	p.pendingUnits = make(map[uint64]string)
	p.deliveredUnits = make(map[uint64]string)
	p.heard.Reset()
	p.slackReset()
	p.gate = nil
}

func (p *AssistantContextProcessor) startFlushBarrierTimeout(gen uint64) {
	_ = p.session.Go(func(ctx context.Context) {
		timer := time.NewTimer(p.cfg.FlushTimeout)
		defer timer.Stop()

		select {
		case <-timer.C:
			// Acknowledged in time, or superseded by a newer barrier.
			if p.ackedFlushGen.Load() == gen || p.pendingFlushGen.Load() != gen {
				return
			}
			p.FlushTimeouts.Add(1)
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
