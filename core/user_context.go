package core

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"
)

var ErrInvalidCommittedTranscript = errors.New("core: committed transcript requires turn, generation, text, and timestamp")

// UserContextConfig configures the user context processor.
type UserContextConfig struct {
	TurnPrefix                 string // Prefix for auto-generated turn IDs (default: "turn")
	RequireCommittedTranscript bool   // V2: only finalizer-marked finals may enter context
	MaxCommittedTurns          int    // V2 idempotence window: recent committed turns remembered (default: 128)
}

// UserContextProcessor receives final transcription frames, assigns stable turn IDs,
// records user turns into the conversation context, and protects against duplicate or
// late-arriving final transcripts.
type UserContextProcessor struct {
	session SessionController
	index   int

	ctxPool *ConversationContext
	cfg     UserContextConfig

	turnCounter        uint64
	lastFinalTurn      string
	lastFinalText      string
	lastFinalGen       uint64
	committedTurns     map[committedTurnKey]struct{}
	committedOrder     []committedTurnKey // ring of committedTurns keys, oldest at committedNext
	committedNext      int
	DuplicateDropCount atomic.Uint64
	TurnsCreated       atomic.Uint64
}

type committedTurnKey struct {
	turn string
	gen  uint64
}

// EvictionCount reports turn pairs dropped from this session's context store.
func (p *UserContextProcessor) EvictionCount() int {
	if p.ctxPool == nil {
		return 0
	}
	return p.ctxPool.EvictionCount()
}

// NewUserContextProcessor creates a new UserContextProcessor bound to the given context.
func NewUserContextProcessor(ctxPool *ConversationContext, cfg UserContextConfig) *UserContextProcessor {
	if cfg.TurnPrefix == "" {
		cfg.TurnPrefix = "turn"
	}
	if cfg.MaxCommittedTurns <= 0 {
		cfg.MaxCommittedTurns = 128
	}
	return &UserContextProcessor{
		ctxPool:        ctxPool,
		cfg:            cfg,
		committedTurns: make(map[committedTurnKey]struct{}, cfg.MaxCommittedTurns),
		committedOrder: make([]committedTurnKey, cfg.MaxCommittedTurns),
	}
}

// Attach registers the processor with the session pipeline and supervisor.
func (p *UserContextProcessor) Attach(s SessionController, index int) error {
	p.session = s
	p.index = index
	return nil
}

// ProcessFrame processes downstream frames, specifically FinalTranscriptionFrame.
func (p *UserContextProcessor) ProcessFrame(ctx context.Context, f Frame, dir Direction, e Emitter) error {
	if dir != Downstream || f.Type() != FrameFinalTranscription {
		return e.PushDirection(f, dir)
	}

	ft, ok := f.(*FinalTranscriptionFrame)
	if !ok {
		return e.PushDirection(f, dir)
	}
	if p.cfg.RequireCommittedTranscript && !ft.Committed {
		return e.PushDirection(f, dir)
	}
	if p.cfg.RequireCommittedTranscript && (ft.Turn == "" || ft.Gen == 0 || ft.Text == "" || ft.Timestamp.IsZero()) {
		return ErrInvalidCommittedTranscript
	}
	// 1. Stale generation check
	if ft.Gen != 0 && p.session != nil && ft.Gen < p.session.CurrentGeneration() {
		p.DuplicateDropCount.Add(1)
		return nil
	}
	var committedKey committedTurnKey
	if ft.Committed {
		committedKey = committedTurnKey{turn: ft.Turn, gen: ft.Gen}
		if _, exists := p.committedTurns[committedKey]; exists {
			p.DuplicateDropCount.Add(1)
			return nil
		}
	}

	// 2. Duplicate / late final transcript protection:
	// A closed turn or identical transcript text for the active/closed turn must not create another user turn.
	if !ft.Committed && ((ft.Turn != "" && ft.Turn == p.lastFinalTurn) ||
		(ft.Text != "" && ft.Text == p.lastFinalText && (ft.Turn == "" || ft.Turn == p.lastFinalTurn))) {
		p.DuplicateDropCount.Add(1)
		return nil
	}

	// 3. Assign or preserve stable TurnID
	turnID := ft.Turn
	if turnID == "" {
		p.turnCounter++
		turnID = fmt.Sprintf("%s-%d", p.cfg.TurnPrefix, p.turnCounter)
	}

	createdAt := ft.Timestamp
	if createdAt.IsZero() {
		// Explicit V1 compatibility: historical direct-final frames did not
		// require a timestamp. V2 committed frames always carry policy time.
		createdAt = time.Now()
	}
	userMsg := Message{
		Role:      RoleUser,
		Content:   ft.Text,
		TurnID:    turnID,
		CreatedAt: createdAt,
	}

	if err := p.ctxPool.AddMessage(userMsg); err != nil {
		return err
	}
	if ft.Committed {
		p.rememberCommitted(committedKey)
	}
	p.lastFinalTurn = turnID
	p.lastFinalText = ft.Text
	p.lastFinalGen = ft.Gen

	p.TurnsCreated.Add(1)

	// A new user turn supersedes any reply still generating or playing. Voice
	// turns usually interrupted at speech start; typed turns have no speech
	// start, so run the barrier here, before the turn reaches the LLM. The
	// assistant ignores it when no reply is active.
	gen := ft.Gen
	if ft.Committed && p.session != nil {
		_ = e.Push(&InterruptionFrame{Gen: p.session.CurrentGeneration()})
		gen = p.session.CurrentGeneration() // advanced if a reply was interrupted
	}

	// Forward FinalTranscriptionFrame downstream toward transport/listeners
	_ = e.PushDirection(f, dir)

	// Forward UserMessageFrame downstream toward LLM
	return e.Push(&UserMessageFrame{
		Turn:      turnID,
		Content:   ft.Text,
		Gen:       gen,
		Timestamp: userMsg.CreatedAt,
	})
}

// rememberCommitted records a committed turn for duplicate suppression. Only the
// most recent MaxCommittedTurns are kept, so long calls never hit a limit.
// ponytail: a re-delivery older than the window would be accepted again; such a
// late duplicate is already dropped by the stale-generation check once the call
// has moved on to a newer generation.
func (p *UserContextProcessor) rememberCommitted(key committedTurnKey) {
	if len(p.committedTurns) >= len(p.committedOrder) {
		delete(p.committedTurns, p.committedOrder[p.committedNext])
	}
	p.committedTurns[key] = struct{}{}
	p.committedOrder[p.committedNext] = key
	p.committedNext = (p.committedNext + 1) % len(p.committedOrder)
}
