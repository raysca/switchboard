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
	ErrContextLimitExceeded = errors.New("core: conversation context exceeds budget limits even after turn eviction")
	ErrInvalidRole          = errors.New("core: invalid message role")
	ErrInvalidToolMessage   = errors.New("core: invalid tool context message")
	ErrUnmatchedToolResult  = errors.New("core: tool result has no matching call")
	ErrToolCorrelation      = errors.New("core: conflicting tool call correlation")
	ErrInvalidMetadata      = errors.New("core: message metadata must be bounded JSON-safe data")
	ErrToolCallPending      = errors.New("core: tool call is already in progress")
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
	MaxToolIdentities int // Recent tool replay identities remembered (default: 256)
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
// When message or token budgets are exceeded, it deterministically evicts the oldest turns
// while strictly preserving system instruction(s), the current user turn, and the newest turns.
type ConversationContext struct {
	mu         sync.RWMutex
	cfg        ContextConfig
	messages   []Message
	evictions  int
	toolLedger map[string]toolReplayIdentity
	toolOrder  []string // FIFO of toolLedger keys, oldest first

	// While openRounds > 0 a tool round is in flight: plain messages are queued
	// (flushed when the last round ends) so they never land between a tool call
	// and its result. ponytail: one global queue, no per-round attribution.
	openRounds int
	queued     []Message
}

// maxQueuedMessages bounds messages held while a tool round is open (a round
// is itself bounded by the loop's round timeout).
const maxQueuedMessages = 64

// BeginToolRound marks a tool round in flight; pair with EndToolRound. Rounds
// from several loops sharing this context nest.
func (c *ConversationContext) BeginToolRound() {
	c.mu.Lock()
	c.openRounds++
	c.mu.Unlock()
}

// EndToolRound closes a round; when none remain open, queued messages are
// appended in arrival order. A message the budget now rejects is dropped.
func (c *ConversationContext) EndToolRound() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.openRounds > 0 {
		c.openRounds--
	}
	if c.openRounds > 0 {
		return
	}
	for _, m := range c.queued {
		_ = c.addMessageLocked(m)
	}
	c.queued = nil
}

type toolReplayIdentity struct {
	turn, name, arguments string
	gen                   uint64
	hasResult             bool
	status                ToolStatus
	result, safeError     string
	startedAt, endedAt    time.Time
}

// NewConversationContext creates an initialized conversation context with the given budget.
func NewConversationContext(cfg ContextConfig) *ConversationContext {
	if cfg.MaxToolIdentities <= 0 {
		cfg.MaxToolIdentities = 256
	}
	return &ConversationContext{
		cfg:        cfg,
		messages:   make([]Message, 0, 16),
		toolLedger: make(map[string]toolReplayIdentity, cfg.MaxToolIdentities),
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
	if len(c.messages) > 0 && c.messages[0].Role == RoleSystem {
		c.messages[0] = sysMsg
	} else {
		c.messages = append([]Message{sysMsg}, c.messages...)
	}
	if err := c.enforceBudgetLocked(); err != nil {
		c.messages = original
		c.evictions = originalEvictions
		return err
	}
	return nil
}

// Messages returns a copy of all current messages in context.
func (c *ConversationContext) Messages() []Message {
	c.mu.RLock()
	defer c.mu.RUnlock()

	// Mid-round, a call still without its result is hidden: a provider
	// snapshot taken now (a barge-in's next generation) must stay valid. The
	// ledger is not used: it forgets old identities whose calls stay in history.
	var resolved map[string]bool
	if c.openRounds > 0 {
		resolved = map[string]bool{}
		for _, m := range c.messages {
			if m.Kind == MessageToolResult {
				resolved[m.ToolCallID] = true
			}
		}
	}
	res := make([]Message, 0, len(c.messages)+len(c.queued))
	for _, m := range c.messages {
		if resolved != nil && m.Kind == MessageToolCall && !resolved[m.ToolCallID] {
			continue
		}
		cm, _ := cloneMessage(m)
		res = append(res, cm)
	}
	for _, m := range c.queued {
		cm, _ := cloneMessage(m)
		res = append(res, cm)
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
	if c.openRounds > 0 {
		cloned, err := cloneMessage(msg)
		if err != nil {
			return err
		}
		if len(c.queued) >= maxQueuedMessages {
			return ErrContextLimitExceeded
		}
		c.queued = append(c.queued, cloned)
		return nil
	}
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
	c.messages = append(c.messages, cloned)

	// Apply eviction if needed
	err = c.enforceBudgetLocked()
	if err != nil {
		// Rollback on rejection
		c.messages = originalMessages
		c.evictions = originalEvictions
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
	if at.IsZero() {
		at = time.Now()
	}
	originalMessages := append([]Message(nil), c.messages...)
	originalEvictions := c.evictions
	if err := c.addMessageLocked(Message{Role: RoleAssistant, Kind: MessageToolCall, Content: string(append([]byte(nil), arguments...)), TurnID: turn, ToolCallID: callID, ToolName: name, Generation: gen, CreatedAt: at}); err != nil {
		return err
	}
	if !c.hasToolMessageLocked(MessageToolCall, turn, gen, callID) {
		c.messages = originalMessages
		c.evictions = originalEvictions
		return ErrContextLimitExceeded
	}
	c.rememberToolLocked(callID, toolReplayIdentity{turn: turn, gen: gen, name: name, arguments: string(arguments), startedAt: at})
	return nil
}

// ClaimToolCall atomically records a first-seen tool call. It returns claimed
// true only to the caller allowed to execute it. A completed exact replay
// returns its copied prior result; an in-flight replay fails closed.
func (c *ConversationContext) ClaimToolCall(call ToolCall, at time.Time) (ToolResult, bool, error) {
	if call.TurnID == "" || call.Generation == 0 || call.ID == "" || call.Name == "" {
		return ToolResult{}, false, ErrInvalidToolMessage
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if identity, exists := c.toolLedger[call.ID]; exists {
		if identity.turn != string(call.TurnID) || identity.gen != uint64(call.Generation) || identity.name != call.Name || identity.arguments != string(call.Arguments) {
			return ToolResult{}, false, ErrToolCorrelation
		}
		if !identity.hasResult {
			return ToolResult{}, false, ErrToolCallPending
		}
		return ToolResult{CallID: call.ID, Status: identity.status, Result: json.RawMessage(append([]byte(nil), identity.result...)), ErrorCategory: identity.safeError, StartedAt: identity.startedAt, EndedAt: identity.endedAt}, false, nil
	}
	if at.IsZero() {
		at = time.Now()
	}
	originalMessages := append([]Message(nil), c.messages...)
	originalEvictions := c.evictions
	if err := c.addMessageLocked(Message{Role: RoleAssistant, Kind: MessageToolCall, Content: string(append([]byte(nil), call.Arguments...)), TurnID: string(call.TurnID), ToolCallID: call.ID, ToolName: call.Name, Generation: uint64(call.Generation), CreatedAt: at}); err != nil {
		return ToolResult{}, false, err
	}
	if !c.hasToolMessageLocked(MessageToolCall, string(call.TurnID), uint64(call.Generation), call.ID) {
		c.messages = originalMessages
		c.evictions = originalEvictions
		return ToolResult{}, false, ErrContextLimitExceeded
	}
	c.rememberToolLocked(call.ID, toolReplayIdentity{turn: string(call.TurnID), gen: uint64(call.Generation), name: call.Name, arguments: string(call.Arguments), startedAt: at})
	return ToolResult{}, true, nil
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
	if !c.hasToolMessageLocked(MessageToolCall, turn, gen, callID) {
		return ErrUnmatchedToolResult
	}
	content := string(append([]byte(nil), result...))
	if at.IsZero() {
		at = time.Now()
	}
	originalMessages := append([]Message(nil), c.messages...)
	originalEvictions := c.evictions
	if err := c.addMessageLocked(Message{Role: RoleTool, Kind: MessageToolResult, Content: content, TurnID: turn, ToolCallID: callID, Generation: gen, ToolStatus: status, ToolErrorCategory: safeError, CreatedAt: at}); err != nil {
		return err
	}
	if !c.hasToolMessageLocked(MessageToolCall, turn, gen, callID) || !c.hasToolMessageLocked(MessageToolResult, turn, gen, callID) {
		c.messages = originalMessages
		c.evictions = originalEvictions
		return ErrContextLimitExceeded
	}
	identity.hasResult = true
	identity.status = status
	identity.result = string(result)
	identity.safeError = safeError
	identity.endedAt = at
	c.toolLedger[callID] = identity
	return nil
}

// rememberToolLocked records a new tool identity, forgetting the oldest once
// MaxToolIdentities are held, so long calls never hit a tool-call limit.
// ponytail: a replay older than the window re-executes; a result for a call
// that old (MaxToolIdentities calls still in flight) is rejected as unmatched.
func (c *ConversationContext) rememberToolLocked(callID string, identity toolReplayIdentity) {
	if len(c.toolOrder) >= c.cfg.MaxToolIdentities {
		delete(c.toolLedger, c.toolOrder[0])
		c.toolOrder = c.toolOrder[1:]
	}
	c.toolLedger[callID] = identity
	c.toolOrder = append(c.toolOrder, callID)
}

func (c *ConversationContext) hasToolMessageLocked(kind MessageKind, turn string, gen uint64, callID string) bool {
	for _, message := range c.messages {
		if message.Kind == kind && message.TurnID == turn && message.Generation == gen && message.ToolCallID == callID {
			return true
		}
	}
	return false
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

// enforceBudgetLocked evicts the oldest turns until budget is satisfied.
// Invariant: System instruction and the latest user turn are NEVER evicted.
func (c *ConversationContext) enforceBudgetLocked() error {
	for c.isOverBudgetLocked() {
		turnID, found := c.findOldestTurnLocked()
		if !found {
			// Nothing but the system instruction and active turn left.
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

// findOldestTurnLocked identifies the TurnID of the oldest non-system turn
// other than the active (latest) user turn. Incomplete turns (no assistant
// reply: interrupted, failed, superseded) are evictable too, otherwise enough
// of them would wedge the context. A turn is evicted whole, so its tool calls
// and results always leave together.
func (c *ConversationContext) findOldestTurnLocked() (string, bool) {
	var activeUserTurnID string
	for i := len(c.messages) - 1; i >= 0; i-- {
		if c.messages[i].Role == RoleUser {
			activeUserTurnID = c.messages[i].TurnID
			break
		}
	}
	for _, m := range c.messages {
		if m.Role != RoleSystem && m.TurnID != "" && m.TurnID != activeUserTurnID {
			return m.TurnID, true
		}
	}
	return "", false
}
