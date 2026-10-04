// Package turn contains provider-neutral turn detection and per-session
// deterministic turn management.
package turn

import (
	"math"
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
// coalesced newest evaluation before returning when the input changed.
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

// HybridDetector accepts valid high-confidence semantic evidence only after a
// minimum silence guard; every other case delegates to the silence fallback.
type HybridDetector struct {
	fallback  TurnDetector
	guard     time.Duration
	threshold float64
}

var _ TurnDetector = HybridDetector{}

// NewHybridDetector composes a semantic confidence guard with a deterministic
// fallback. A nil fallback, negative guard, or invalid threshold is rejected.
func NewHybridDetector(fallback TurnDetector, guard time.Duration, threshold float64) (HybridDetector, error) {
	if fallback == nil || guard < 0 || math.IsNaN(threshold) || math.IsInf(threshold, 0) || threshold <= 0 || threshold > 1 {
		return HybridDetector{}, ErrInvalidConfig
	}
	return HybridDetector{fallback: fallback, guard: guard, threshold: threshold}, nil
}

func (d HybridDetector) Detect(input DetectionInput) (DetectionDecision, bool) {
	if evidence := input.Semantic; evidence != nil &&
		evidence.TurnID == input.TurnID && evidence.Generation == input.Generation &&
		!evidence.At.Before(input.TurnStartedAt) && !evidence.At.After(input.At) &&
		input.SemanticMaxAge > 0 && input.At.Sub(evidence.At) <= input.SemanticMaxAge &&
		!math.IsNaN(evidence.Confidence) && !math.IsInf(evidence.Confidence, 0) &&
		evidence.Confidence >= d.threshold && evidence.Confidence <= 1 && input.SilenceFor >= d.guard {
		return DetectionDecision{Reason: core.TurnReasonSemantic, Confidence: evidence.Confidence, HasConfidence: true}, true
	}
	fallback := d.fallback
	if fallback == nil {
		fallback = SilenceDetector{}
	}
	return fallback.Detect(input)
}
