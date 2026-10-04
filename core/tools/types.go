// Package tools defines provider-neutral tool contracts. Registration and
// execution policy are supplied by later framework and server integrations.
package tools

import (
	"context"
	"encoding/json"
	"time"

	"govoice/core"
)

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
	SessionID  core.SessionID
	TurnID     core.TurnID
	Generation core.Generation
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
	Execute(ctx context.Context, call ToolCall) (ToolResult, error)
}

type ToolExecutorFunc func(context.Context, ToolCall) (ToolResult, error)

func (fn ToolExecutorFunc) Execute(ctx context.Context, call ToolCall) (ToolResult, error) {
	return fn(ctx, call)
}
