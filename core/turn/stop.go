package turn

import (
	"strings"
	"time"
	"unicode/utf8"

	"govoice/core"
)

// UserTurnStopStrategy decides whether the open user turn has ended. It is
// the TurnDetector contract: the manager calls it outside its lock with a
// value snapshot (caller timestamps only), with at most one call in flight,
// so strategies must be deterministic, side-effect free, and may be slow
// (a TurnAnalyzer may run a model). A strategy that panics or returns an
// invalid decision is replaced by the built-in silence fallback.
//
// Strategies are evaluated only while the user is silent (after StopDebounce)
// or after an external stop signal.
type UserTurnStopStrategy = TurnDetector

// StopChain is Pipecat's any-stop chain: strategies are asked in order and
// the first one that ends the turn wins. Nil entries are skipped. An empty
// chain never ends a turn (MaxTurnDuration still applies).
func StopChain(strategies ...UserTurnStopStrategy) TurnDetector {
	var kept []UserTurnStopStrategy
	for _, s := range strategies {
		if s != nil {
			kept = append(kept, s)
		}
	}
	return stopChain(kept)
}

type stopChain []UserTurnStopStrategy

func (c stopChain) Detect(in DetectionInput) (DetectionDecision, bool) {
	for _, s := range c {
		if d, ok := s.Detect(in); ok {
			return d, true
		}
	}
	return DetectionDecision{}, false
}

// SpeechTimeout ends the turn after the user has been silent for after
// (zero: the manager's Config.SilenceTimeout). With the default it is exactly
// SilenceDetector, the historical behavior. ponytail: no wait-for-transcript
// gate; add when a provider needs it.
func SpeechTimeout(after time.Duration) UserTurnStopStrategy { return speechTimeout{after} }

type speechTimeout struct{ after time.Duration }

func (s speechTimeout) Detect(in DetectionInput) (DetectionDecision, bool) {
	if s.after > 0 {
		in.SilenceAfter = s.after
	}
	return SilenceDetector{}.Detect(in)
}

// TurnAnalyzer is the audio-based end-of-turn model seam (the ONNX smart-turn
// worker implements it). It sees the same snapshot as any strategy; model
// evidence arrives in in.Semantic. Confidence must be in [0,1].
type TurnAnalyzer interface {
	Analyze(DetectionInput) AnalyzerResult
}

type AnalyzerResult struct {
	Complete   bool
	Confidence float64
}

// AnalyzerStop ends the turn when the analyzer says complete (reason
// semantic, with its confidence). While it says incomplete the turn is held
// open until the user has been silent for fallback (zero: the manager's
// SilenceTimeout), then ends with reason silence, so a wrong or dead model can
// delay but never wedge a turn. A panic or out-of-range confidence falls back
// to the silence detector for that call.
//
// ponytail: Analyze runs synchronously on the session loop on every tick
// (~20ms) of silence while it says incomplete. The P1-3 worker must bound its
// own latency and cache its verdict per (TurnID, Generation, silence start).
func AnalyzerStop(a TurnAnalyzer, fallback time.Duration) UserTurnStopStrategy {
	return analyzerStop{a, fallback}
}

type analyzerStop struct {
	a        TurnAnalyzer
	fallback time.Duration
}

func (s analyzerStop) Detect(in DetectionInput) (DetectionDecision, bool) {
	fallback := s.fallback
	if fallback <= 0 {
		fallback = in.SilenceAfter
	}
	if fallback > 0 && in.SilenceFor >= fallback {
		return DetectionDecision{Reason: core.TurnReasonSilence}, true
	}
	if r := s.a.Analyze(in); r.Complete {
		return DetectionDecision{Reason: core.TurnReasonSemantic, Confidence: r.Confidence, HasConfidence: true}, true
	}
	return DetectionDecision{}, false
}

// EagerStop ends the turn early, after only grace of silence, when the latest
// transcript is final and ends a sentence. If the user resumes inside grace
// the silence clock resets and nothing was emitted. After grace the end is
// final: the LLM answers the utterance so far, and a continuation ("...for
// two") is a new user turn that barges in on that reply. Unlike Pipecat's
// eager strategy there is no speculation gate holding the reply until the
// STT commits (that needs STT eager-end-of-turn signals this runtime lacks),
// so this trades false cuts on mid-thought sentence ends for latency. It
// never ends an unpunctuated turn; pair it with SpeechTimeout.
func EagerStop(grace time.Duration) UserTurnStopStrategy { return eagerStop{grace} }

type eagerStop struct{ grace time.Duration }

func (s eagerStop) Detect(in DetectionInput) (DetectionDecision, bool) {
	if in.TextFinal && in.SilenceFor >= s.grace && endsSentence(in.Text) {
		return DetectionDecision{Reason: core.TurnReasonSilence}, true
	}
	return DetectionDecision{}, false
}

func endsSentence(text string) bool {
	text = strings.TrimRight(text, " \t\n\"'”’)」』")
	r, _ := utf8.DecodeLastRuneInString(text)
	return strings.ContainsRune(".!?…。！？", r)
}

// ExternalStop ends the turn only when the caller signals it
// (Observation.ExternalStop / Processor.SignalStop). ponytail: nothing in the
// server calls SignalStop yet, so turn.stop rejects "external"; wire a source
// (e.g. an RTVI client message or an STT that owns endpointing) first.
func ExternalStop() UserTurnStopStrategy { return externalStop{} }

type externalStop struct{}

func (externalStop) Detect(in DetectionInput) (DetectionDecision, bool) {
	if in.ExternalStop {
		return DetectionDecision{Reason: core.TurnReasonSilence}, true
	}
	return DetectionDecision{}, false
}
