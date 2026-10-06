package core

import "errors"

// FailurePolicy defines how the pipeline and session run-loop respond
// when a processor returns an error from ProcessFrame.
type FailurePolicy uint8

const (
	// PolicyFailSession terminates the session immediately upon processor error.
	// This is the default failure policy for processors.
	PolicyFailSession FailurePolicy = iota

	// PolicyDisable permanently disables the processor for subsequent frames.
	// Emits a non-fatal ErrorFrame and forwards the frame downstream.
	PolicyDisable

	// PolicyDegrade temporarily bypasses the processor for the failing frame.
	// Emits a non-fatal ErrorFrame and forwards the frame downstream. The processor
	// remains active for subsequent frames.
	PolicyDegrade

	// PolicySafeRetry retries processing the frame once before failing the session.
	// If the processor has already emitted output for the frame before erroring,
	// retry is considered unsafe and the session fails immediately to prevent duplicate side effects.
	PolicySafeRetry
)

var (
	// ErrRetryUnsafe is returned/recorded when a processor configured with PolicySafeRetry
	// fails after already emitting output frames during the current attempt.
	ErrRetryUnsafe = errors.New("core: safe retry aborted because processor already emitted output")
)

// FailurePolicyProvider is an optional interface implemented by processors
// to declare their failure handling policy. If not implemented, the processor
// defaults to PolicyFailSession.
type FailurePolicyProvider interface {
	FailurePolicy() FailurePolicy
}
