package core

import (
	"context"
	"encoding/json"
	"time"
)

// These provider-neutral values live in core so frames and context can carry
// them without translation. The core/tools package preserves the original V2
// public import path through type aliases.
type ToolDefinition struct {
	Name        string
	Description string
	InputSchema json.RawMessage
	Policy      ToolPolicy
}
type ToolPolicy struct {
	Timeout  time.Duration
	Required bool
}
type ToolCall struct {
	ID         string
	Name       string
	Arguments  json.RawMessage
	SessionID  SessionID
	TurnID     TurnID
	Generation Generation
}
type ToolStatus string

const (
	ToolSucceeded ToolStatus = "succeeded"
	ToolFailed    ToolStatus = "failed"
	ToolCancelled ToolStatus = "cancelled"
)

type ToolResult struct {
	CallID        string
	Status        ToolStatus
	Result        json.RawMessage
	ErrorCategory string
	StartedAt     time.Time
	EndedAt       time.Time
}
type ToolExecutor interface {
	Execute(context.Context, ToolCall) (ToolResult, error)
}
type ToolExecutorFunc func(context.Context, ToolCall) (ToolResult, error)

func (fn ToolExecutorFunc) Execute(ctx context.Context, call ToolCall) (ToolResult, error) {
	return fn(ctx, call)
}
