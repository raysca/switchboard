package core

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	ErrContextLimitExceeded = errors.New("core: conversation context exceeds budget limits even after complete turn-pair eviction")
	ErrInvalidRole          = errors.New("core: invalid message role")
)

// Role represents the conversational participant role.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is a provider-neutral representation of a conversation turn or instruction.
type Message struct {
	Role      Role           `json:"role"`
	Content   string         `json:"content"`
	TurnID    string         `json:"turn_id,omitempty"`
	Partial   bool           `json:"partial,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// ContextConfig specifies the bounded message and token limits for conversation history.
type ContextConfig struct {
	MaxMessages int // Maximum total messages preserved in history (must be >= 2 if set)
	MaxTokens   int // Maximum estimated tokens across active messages
}

// EstimateTokens provides a deterministic, provider-neutral token count estimate.
// A standard heuristic of 4 characters per token is used with a 4-token framing overhead per message.
func EstimateTokens(text string) int {
	if len(text) == 0 {
		return 0
	}
	toks := (len(text) + 3) / 4
	if toks < 1 {
		toks = 1
	}
	return toks
}

// MessageTokens returns the estimated token count for a message including envelope overhead.
func MessageTokens(m Message) int {
	return EstimateTokens(m.Content) + 4
}

// ConversationContext maintains a thread-safe, bounded, multi-turn conversation history.
// When message or token budgets are exceeded, it deterministically evicts the oldest complete
// turn-pairs while strictly preserving system instruction(s), the current user turn, and the newest turns.
type ConversationContext struct {
	mu           sync.RWMutex
	cfg          ContextConfig
	messages     []Message
	evictions    int
	evictedPairs []string
}

// NewConversationContext creates an initialized conversation context with the given budget.
func NewConversationContext(cfg ContextConfig) *ConversationContext {
	return &ConversationContext{
		cfg:          cfg,
		messages:     make([]Message, 0, 16),
		evictedPairs: make([]string, 0),
	}
}

// SetSystemInstruction sets or replaces the initial system instruction message.
func (c *ConversationContext) SetSystemInstruction(instruction string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	sysMsg := Message{
		Role:      RoleSystem,
		Content:   instruction,
		CreatedAt: time.Now(),
	}

	if len(c.messages) > 0 && c.messages[0].Role == RoleSystem {
		c.messages[0] = sysMsg
		return
	}

	// Prepend system instruction
	c.messages = append([]Message{sysMsg}, c.messages...)
}

// Messages returns a copy of all current messages in context.
func (c *ConversationContext) Messages() []Message {
	c.mu.RLock()
	defer c.mu.RUnlock()

	res := make([]Message, len(c.messages))
	copy(res, c.messages)
	return res
}

// EstimatedTokens returns the total estimated token count across all messages in context.
func (c *ConversationContext) EstimatedTokens() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.estimatedTokensLocked()
}

func (c *ConversationContext) estimatedTokensLocked() int {
	total := 0
	for _, m := range c.messages {
		total += MessageTokens(m)
	}
	return total
}

// EvictionCount returns the total number of turn-pair evictions performed.
func (c *ConversationContext) EvictionCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.evictions
}

// EvictedTurnPairs returns the TurnIDs of all turn pairs that have been evicted.
func (c *ConversationContext) EvictedTurnPairs() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	res := make([]string, len(c.evictedPairs))
	copy(res, c.evictedPairs)
	return res
}

// AddMessage appends a new message and applies deterministic eviction if limits are breached.
// If limits cannot be satisfied without evicting the system prompt or the new message itself,
// the addition is rejected and context remains unchanged.
func (c *ConversationContext) AddMessage(msg Message) error {
	if msg.Role != RoleSystem && msg.Role != RoleUser && msg.Role != RoleAssistant {
		return fmt.Errorf("%w: %q", ErrInvalidRole, msg.Role)
	}
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now()
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Tentatively append message
	originalMessages := c.messages
	c.messages = append(c.messages, msg)

	// Apply eviction if needed
	err := c.enforceBudgetLocked()
	if err != nil {
		// Rollback on rejection
		c.messages = originalMessages
		return err
	}

	return nil
}

// enforceBudgetLocked evicts the oldest complete turn-pairs until budget is satisfied.
// Invariant: System instruction and the latest user turn are NEVER evicted.
func (c *ConversationContext) enforceBudgetLocked() error {
	for c.isOverBudgetLocked() {
		// Find the oldest complete turn pair to evict
		turnID, found := c.findOldestCompleteTurnPairLocked()
		if !found {
			// No complete turn pairs available to evict, but still over budget!
			return ErrContextLimitExceeded
		}

		// Evict all messages belonging to this turnID
		newMsgs := make([]Message, 0, len(c.messages))
		for _, m := range c.messages {
			if m.TurnID != turnID || m.Role == RoleSystem {
				newMsgs = append(newMsgs, m)
			}
		}
		c.messages = newMsgs
		c.evictions++
		c.evictedPairs = append(c.evictedPairs, turnID)
	}

	return nil
}

func (c *ConversationContext) isOverBudgetLocked() bool {
	if c.cfg.MaxMessages > 0 && len(c.messages) > c.cfg.MaxMessages {
		return true
	}
	if c.cfg.MaxTokens > 0 && c.estimatedTokensLocked() > c.cfg.MaxTokens {
		return true
	}
	return false
}

// findOldestCompleteTurnPairLocked identifies the TurnID of the oldest completed turn pair.
// A turn pair is complete if it has an Assistant message. The active (latest) user turn is excluded.
func (c *ConversationContext) findOldestCompleteTurnPairLocked() (string, bool) {
	// Identify current user turn ID to protect it
	var activeUserTurnID string
	for i := len(c.messages) - 1; i >= 0; i-- {
		if c.messages[i].Role == RoleUser {
			activeUserTurnID = c.messages[i].TurnID
			break
		}
	}

	// Map turns to their completeness
	hasAssistant := make(map[string]bool)
	var orderedTurns []string

	for _, m := range c.messages {
		if m.Role == RoleSystem || m.TurnID == "" {
			continue
		}
		if _, seen := hasAssistant[m.TurnID]; !seen {
			orderedTurns = append(orderedTurns, m.TurnID)
			hasAssistant[m.TurnID] = false
		}
		if m.Role == RoleAssistant {
			hasAssistant[m.TurnID] = true
		}
	}

	// Find the oldest turn that has completed (has an Assistant message) and is not active
	for _, turnID := range orderedTurns {
		if turnID == activeUserTurnID {
			continue
		}
		if hasAssistant[turnID] {
			return turnID, true
		}
	}

	return "", false
}
