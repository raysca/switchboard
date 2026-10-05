// Package tools defines provider-neutral tool contracts. Registration and
// execution policy are supplied by later framework and server integrations.
package tools

import "govoice/core"

type ToolDefinition = core.ToolDefinition
type ToolPolicy = core.ToolPolicy
type ToolCall = core.ToolCall
type ToolStatus = core.ToolStatus

const (
	ToolSucceeded = core.ToolSucceeded
	ToolFailed    = core.ToolFailed
	ToolCancelled = core.ToolCancelled
)

type ToolResult = core.ToolResult
type ToolExecutor = core.ToolExecutor
type ToolExecutorFunc = core.ToolExecutorFunc
