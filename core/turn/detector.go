// Package turn contains provider-neutral turn detection and per-session
// deterministic turn management.
package turn

import (
	"time"

	"govoice/core"
)

// DetectionInput is a snapshot supplied by the manager to a detector. Every
// timestamp and duration is derived from caller observations, never a clock.
type DetectionInput struct {
	At             time.Time
	TurnStartedAt  time.Time
	TurnID         core.TurnID
	Generation     core.Generation
	SilenceFor     time.Duration
	SilenceAfter   time.Duration
	SemanticMaxAge time.Duration
	Semantic       *SemanticEvidence
	// Latest transcript of the open turn (joined across segments) and when it
	// arrived; TextFinal is false for interim results.
	Text         string
	TextFinal    bool
	TextAt       time.Time
	ExternalStop bool
}

// DetectionDecision is a detector's one-shot completion suggestion.
type DetectionDecision struct {
	Reason        core.TurnReason
	Confidence    float64
	HasConfidence bool
}

// TurnDetector decides whether the current user turn has ended.
// Implementations should be deterministic, fast, and side-effect free. The
// manager invokes Detect without holding its state lock and permits only one
// callback in flight; concurrent or reentrant advances update state but do
// not wait for or start another detector call. The callback owner drains one
// coalesced newest snapshot with a bounded plugin-call budget, then uses the
// built-in silence fallback if adversarial reentry keeps invalidating results.
type TurnDetector interface {
	Detect(DetectionInput) (DetectionDecision, bool)
}

// SilenceDetector completes a turn when the configured silence duration has
// elapsed. The manager owns timing and passes elapsed duration explicitly.
type SilenceDetector struct{}

var _ TurnDetector = SilenceDetector{}

func (SilenceDetector) Detect(input DetectionInput) (DetectionDecision, bool) {
	if input.SilenceAfter > 0 && input.SilenceFor >= input.SilenceAfter {
		return DetectionDecision{Reason: core.TurnReasonSilence}, true
	}
	return DetectionDecision{}, false
}

// SemanticEvidence is provider-neutral output from an optional semantic end
// of turn scorer. It contains no transcript or provider-specific payload.
type SemanticEvidence struct {
	TurnID     core.TurnID
	Generation core.Generation
	At         time.Time
	Confidence float64
}
