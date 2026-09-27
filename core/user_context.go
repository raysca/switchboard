package core

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"
)

// UserContextConfig configures the user context processor.
type UserContextConfig struct {
	TurnPrefix string // Prefix for auto-generated turn IDs (default: "turn")
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
	DuplicateDropCount atomic.Uint64
	TurnsCreated       atomic.Uint64
}

// NewUserContextProcessor creates a new UserContextProcessor bound to the given context.
func NewUserContextProcessor(ctxPool *ConversationContext, cfg UserContextConfig) *UserContextProcessor {
	if cfg.TurnPrefix == "" {
		cfg.TurnPrefix = "turn"
	}
	return &UserContextProcessor{
		ctxPool: ctxPool,
		cfg:     cfg,
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

	// 1. Stale generation check
	if ft.Gen != 0 && p.session != nil && ft.Gen < p.session.CurrentGeneration() {
		p.DuplicateDropCount.Add(1)
		return nil
	}

	// 2. Duplicate / late final transcript protection:
	// A closed turn or identical transcript text for the active/closed turn must not create another user turn.
	if (ft.Turn != "" && ft.Turn == p.lastFinalTurn) ||
		(ft.Text != "" && ft.Text == p.lastFinalText && (ft.Turn == "" || ft.Turn == p.lastFinalTurn)) {
		p.DuplicateDropCount.Add(1)
		return nil
	}

	// 3. Assign or preserve stable TurnID
	turnID := ft.Turn
	if turnID == "" {
		p.turnCounter++
		turnID = fmt.Sprintf("%s-%d", p.cfg.TurnPrefix, p.turnCounter)
	}

	p.lastFinalTurn = turnID
	p.lastFinalText = ft.Text
	p.lastFinalGen = ft.Gen

	userMsg := Message{
		Role:      RoleUser,
		Content:   ft.Text,
		TurnID:    turnID,
		CreatedAt: time.Now(),
	}

	if err := p.ctxPool.AddMessage(userMsg); err != nil {
		return err
	}

	p.TurnsCreated.Add(1)

	// Forward FinalTranscriptionFrame downstream toward transport/listeners
	_ = e.PushDirection(f, dir)

	// Forward UserMessageFrame downstream toward LLM
	return e.Push(&UserMessageFrame{
		Turn:      turnID,
		Content:   ft.Text,
		Gen:       ft.Gen,
		Timestamp: userMsg.CreatedAt,
	})
}
