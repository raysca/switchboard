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
	Name string
	// RemoteName is the tool's own name where it runs, when that differs from Name (an MCP tool's
	// name on its server; Name is the model-facing one). Empty for built-in tools.
	RemoteName  string
	Description string
	InputSchema json.RawMessage
	Policy      ToolPolicy
}
type ToolPolicy struct {
	Timeout  time.Duration
	Required bool
	// Async makes the loop acknowledge the call immediately and deliver the real
	// result later as an injected context message. Async calls survive barge-in.
	Async bool
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

// AsyncToolResult is a finished background (ToolPolicy.Async) call handed to
// the loop's AsyncSink. Message is the labelled envelope for the model; the
// sink owns when it enters history (Inject, which honours the loop's round
// lock so it never splits a tool call from its result).
type AsyncToolResult struct {
	Call    ToolCall
	Result  ToolResult
	Message Message
	Inject  func() error
	// NoTurn: inject the message when the call is idle but do not, on its own, start a model
	// turn (an operator note without run). A batch starts one turn if any item lacks it.
	NoTurn bool
	// Speaks: Inject makes the assistant say a fixed line (a greeting). It implies NoTurn, and the other queued
	// items wait until the assistant is idle again, so a model turn cannot cut the line off.
	Speaks bool
	// Operator marks an operator note or run request (AsyncTrigger.DeliverOperator): it has its
	// own queue cap, is never injected early on overflow, and rides a caller turn that starts
	// before the call is idle (a note joins that turn's history; a run is that turn).
	Operator bool
}
