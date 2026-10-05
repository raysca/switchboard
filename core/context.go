package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
)

var (
	ErrContextLimitExceeded = errors.New("core: conversation context exceeds budget limits even after complete turn-pair eviction")
	ErrInvalidRole          = errors.New("core: invalid message role")
	ErrInvalidToolMessage   = errors.New("core: invalid tool context message")
	ErrUnmatchedToolResult  = errors.New("core: tool result has no matching call")
	ErrToolCorrelation      = errors.New("core: conflicting tool call correlation")
	ErrInvalidMetadata      = errors.New("core: message metadata must be bounded JSON-safe data")
	ErrToolIdentityLimit    = errors.New("core: tool identity replay ledger is full")
)

// Role represents the conversational participant role.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

type MessageKind string

const (
	MessageText       MessageKind = "text"
	MessageToolCall   MessageKind = "tool_call"
	MessageToolResult MessageKind = "tool_result"
)

// Message is a provider-neutral representation of a conversation turn or instruction.
type Message struct {
	Role              Role           `json:"role"`
	Content           string         `json:"content"`
	TurnID            string         `json:"turn_id,omitempty"`
	Partial           bool           `json:"partial,omitempty"`
	CreatedAt         time.Time      `json:"created_at"`
	Metadata          map[string]any `json:"metadata,omitempty"`
	Kind              MessageKind    `json:"kind,omitempty"`
	ToolCallID        string         `json:"tool_call_id,omitempty"`
	ToolName          string         `json:"tool_name,omitempty"`
	Generation        uint64         `json:"generation,omitempty"`
	ToolStatus        ToolStatus     `json:"tool_status,omitempty"`
	ToolErrorCategory string         `json:"tool_error_category,omitempty"`
}

// ContextConfig specifies the bounded message and token limits for conversation history.
type ContextConfig struct {
	MaxMessages       int // Maximum total messages preserved in history (must be >= 2 if set)
	MaxTokens         int // Maximum estimated tokens across active messages
	MaxToolIdentities int // Maximum retained tool replay identities (default: 256)
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
	toolLedger   map[string]toolReplayIdentity
}

type toolReplayIdentity struct {
	turn, name, arguments string
	gen                   uint64
	hasResult             bool
	status                ToolStatus
	result, safeError     string
}

// NewConversationContext creates an initialized conversation context with the given budget.
func NewConversationContext(cfg ContextConfig) *ConversationContext {
	if cfg.MaxToolIdentities <= 0 {
		cfg.MaxToolIdentities = 256
	}
	return &ConversationContext{
		cfg:          cfg,
		messages:     make([]Message, 0, 16),
		evictedPairs: make([]string, 0),
		toolLedger:   make(map[string]toolReplayIdentity, cfg.MaxToolIdentities),
	}
}

// SetSystemInstruction sets or replaces the initial system instruction message.
func (c *ConversationContext) SetSystemInstruction(instruction string) {
	_ = c.SetSystemInstructionChecked(instruction, time.Time{})
}

// SetSystemInstructionChecked replaces the system instruction atomically and
// rolls back when the configured context budget cannot retain it.
func (c *ConversationContext) SetSystemInstructionChecked(instruction string, at time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if at.IsZero() {
		at = time.Now()
	}

	sysMsg := Message{
		Role:      RoleSystem,
		Content:   instruction,
		CreatedAt: at,
	}

	original := append([]Message(nil), c.messages...)
	originalEvictions := c.evictions
	originalEvictedPairs := append([]string(nil), c.evictedPairs...)
	if len(c.messages) > 0 && c.messages[0].Role == RoleSystem {
		c.messages[0] = sysMsg
	} else {
		c.messages = append([]Message{sysMsg}, c.messages...)
	}
	if err := c.enforceBudgetLocked(); err != nil {
		c.messages = original
		c.evictions = originalEvictions
		c.evictedPairs = originalEvictedPairs
		return err
	}
	return nil
}

// Messages returns a copy of all current messages in context.
func (c *ConversationContext) Messages() []Message {
	c.mu.RLock()
	defer c.mu.RUnlock()

	res := make([]Message, len(c.messages))
	for i := range c.messages {
		res[i], _ = cloneMessage(c.messages[i])
	}
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
	if msg.Role != RoleSystem && msg.Role != RoleUser && msg.Role != RoleAssistant && msg.Role != RoleTool {
		return fmt.Errorf("%w: %q", ErrInvalidRole, msg.Role)
	}
	if msg.Kind != "" && msg.Kind != MessageText && msg.Kind != MessageToolCall && msg.Kind != MessageToolResult {
		return ErrInvalidToolMessage
	}
	// Tool messages must use AddToolCall/AddToolResult so correlation and replay
	// validation has one authoritative path.
	if msg.Kind == MessageToolCall || msg.Kind == MessageToolResult || msg.Role == RoleTool {
		return ErrInvalidToolMessage
	}
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now()
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	return c.addMessageLocked(msg)
}

func (c *ConversationContext) addMessageLocked(msg Message) error {
	cloned, err := cloneMessage(msg)
	if err != nil {
		return err
	}
	// Tentatively append message
	originalMessages := append([]Message(nil), c.messages...)
	originalEvictions := c.evictions
	originalEvictedPairs := append([]string(nil), c.evictedPairs...)
	c.messages = append(c.messages, cloned)

	// Apply eviction if needed
	err = c.enforceBudgetLocked()
	if err != nil {
		// Rollback on rejection
		c.messages = originalMessages
		c.evictions = originalEvictions
		c.evictedPairs = originalEvictedPairs
		return err
	}

	return nil
}

// AddToolCall appends the provider-neutral context projection of a tool request.
// Replays with the same turn/generation/call identity are idempotent; conflicting
// reuse fails closed.
func (c *ConversationContext) AddToolCall(turn string, gen uint64, callID, name string, arguments []byte, at time.Time) error {
	if turn == "" || gen == 0 || callID == "" || name == "" {
		return ErrInvalidToolMessage
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if identity, exists := c.toolLedger[callID]; exists {
		if identity.turn == turn && identity.gen == gen && identity.name == name && identity.arguments == string(arguments) {
			return nil
		}
		return ErrToolCorrelation
	}
	if len(c.toolLedger) >= c.cfg.MaxToolIdentities {
		return ErrToolIdentityLimit
	}
	if at.IsZero() {
		at = time.Now()
	}
	if err := c.addMessageLocked(Message{Role: RoleAssistant, Kind: MessageToolCall, Content: string(append([]byte(nil), arguments...)), TurnID: turn, ToolCallID: callID, ToolName: name, Generation: gen, CreatedAt: at}); err != nil {
		return err
	}
	c.toolLedger[callID] = toolReplayIdentity{turn: turn, gen: gen, name: name, arguments: string(arguments)}
	return nil
}

// AddToolResult appends a result only after its matching call and at most once.
func (c *ConversationContext) AddToolResult(turn string, gen uint64, callID string, result []byte, safeError string, at time.Time) error {
	status := ToolSucceeded
	if safeError != "" {
		status = ToolFailed
	}
	return c.AddToolResultStatus(turn, gen, callID, status, result, safeError, at)
}

func (c *ConversationContext) AddToolResultStatus(turn string, gen uint64, callID string, status ToolStatus, result []byte, safeError string, at time.Time) error {
	if turn == "" || gen == 0 || callID == "" || (len(result) == 0 && safeError == "") {
		return ErrInvalidToolMessage
	}
	if status != ToolSucceeded && status != ToolFailed && status != ToolCancelled {
		return ErrInvalidToolMessage
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	identity, found := c.toolLedger[callID]
	if !found {
		return ErrUnmatchedToolResult
	}
	if identity.turn != turn || identity.gen != gen {
		return ErrToolCorrelation
	}
	if identity.hasResult {
		if identity.status == status && identity.result == string(result) && identity.safeError == safeError {
			return nil
		}
		return ErrToolCorrelation
	}
	content := string(append([]byte(nil), result...))
	if at.IsZero() {
		at = time.Now()
	}
	if err := c.addMessageLocked(Message{Role: RoleTool, Kind: MessageToolResult, Content: content, TurnID: turn, ToolCallID: callID, Generation: gen, ToolStatus: status, ToolErrorCategory: safeError, CreatedAt: at}); err != nil {
		return err
	}
	identity.hasResult = true
	identity.status = status
	identity.result = string(result)
	identity.safeError = safeError
	c.toolLedger[callID] = identity
	return nil
}

func cloneMessage(m Message) (Message, error) {
	metadata, err := cloneMetadata(m.Metadata, 0, new(int))
	if err != nil {
		return Message{}, err
	}
	m.Metadata = metadata
	return m, nil
}

const maxMetadataNodes = 4096
const maxMetadataDepth = 16

func cloneMetadata(src map[string]any, depth int, nodes *int) (map[string]any, error) {
	if src == nil {
		return nil, nil
	}
	if depth > maxMetadataDepth || *nodes+len(src) > maxMetadataNodes {
		return nil, ErrInvalidMetadata
	}
	*nodes += len(src)
	dst := make(map[string]any, len(src))
	for k, v := range src {
		cloned, err := cloneMetadataValue(v, depth+1, nodes)
		if err != nil {
			return nil, err
		}
		dst[k] = cloned
	}
	return dst, nil
}

func cloneMetadataValue(v any, depth int, nodes *int) (any, error) {
	if depth > maxMetadataDepth || *nodes >= maxMetadataNodes {
		return nil, ErrInvalidMetadata
	}
	*nodes = *nodes + 1
	switch x := v.(type) {
	case map[string]any:
		return cloneMetadata(x, depth, nodes)
	case []any:
		if *nodes+len(x) > maxMetadataNodes {
			return nil, ErrInvalidMetadata
		}
		*nodes += len(x)
		out := make([]any, len(x))
		for i := range x {
			var err error
			out[i], err = cloneMetadataValue(x[i], depth+1, nodes)
			if err != nil {
				return nil, err
			}
		}
		return out, nil
	case []string:
		if *nodes+len(x) > maxMetadataNodes {
			return nil, ErrInvalidMetadata
		}
		*nodes += len(x)
		return append([]string(nil), x...), nil
	case []byte:
		if *nodes+len(x) > maxMetadataNodes {
			return nil, ErrInvalidMetadata
		}
		*nodes += len(x)
		return append([]byte(nil), x...), nil
	case json.RawMessage:
		if !json.Valid(x) {
			return nil, ErrInvalidMetadata
		}
		if *nodes+len(x) > maxMetadataNodes {
			return nil, ErrInvalidMetadata
		}
		*nodes += len(x)
		return append(json.RawMessage(nil), x...), nil
	case []float64:
		if *nodes+len(x) > maxMetadataNodes {
			return nil, ErrInvalidMetadata
		}
		*nodes += len(x)
		for _, n := range x {
			if math.IsNaN(n) || math.IsInf(n, 0) {
				return nil, ErrInvalidMetadata
			}
		}
		return append([]float64(nil), x...), nil
	case map[string]string:
		if *nodes+len(x) > maxMetadataNodes {
			return nil, ErrInvalidMetadata
		}
		*nodes += len(x)
		out := make(map[string]string, len(x))
		for k, v := range x {
			out[k] = v
		}
		return out, nil
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return nil, ErrInvalidMetadata
		}
		return x, nil
	case float32:
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			return nil, ErrInvalidMetadata
		}
		return x, nil
	case nil, string, bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return x, nil
	default:
		return nil, ErrInvalidMetadata
	}
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
		if m.Role == RoleAssistant && m.Kind != MessageToolCall {
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
