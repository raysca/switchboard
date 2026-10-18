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
// (smartturn.Stop may wait on a model). A strategy that panics or returns an
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

// Defaults for SpeechTimeoutConfig zero values (Pipecat user_speech_timeout and the aggregator's
// user_turn_stop_timeout).
const (
	DefaultUserSpeechTimeout   = 600 * time.Millisecond
	DefaultNoTranscriptTimeout = 5 * time.Second
)

// SpeechTimeoutConfig sets the speech-timeout stop for one call. All durations compare with
// DetectionInput.SilenceFor, which counts from silence onset.
type SpeechTimeoutConfig struct {
	VADStop             time.Duration // the call's VAD stop: onset to the VAD stop event
	UserSpeechTimeout   time.Duration // resume window after the VAD stop (0: 600ms)
	STTP99              time.Duration // primary STT p99, end of speech to final (0: UnknownSTTP99)
	NoTranscriptTimeout time.Duration // end an untranscribed turn after this silence (0: 5s)
}

// SpeechTimeout is Pipecat's speech-timeout stop: the turn ends once the user has been silent
// for the VAD stop plus the resume window, a transcript exists, and it is final or the STT's p99
// has passed. A turn with no transcript ends after NoTranscriptTimeout (dropped downstream), so
// a dead STT cannot wedge it.
func SpeechTimeout(cfg SpeechTimeoutConfig) UserTurnStopStrategy {
	if cfg.UserSpeechTimeout <= 0 {
		cfg.UserSpeechTimeout = DefaultUserSpeechTimeout
	}
	if cfg.STTP99 <= 0 {
		cfg.STTP99 = UnknownSTTP99
	}
	if cfg.NoTranscriptTimeout <= 0 {
		cfg.NoTranscriptTimeout = DefaultNoTranscriptTimeout
	}
	return speechTimeout(cfg)
}

type speechTimeout SpeechTimeoutConfig

func (s speechTimeout) Detect(in DetectionInput) (DetectionDecision, bool) {
	stop := DetectionDecision{Reason: core.TurnReasonSilence}
	if in.SilenceFor >= s.NoTranscriptTimeout {
		return stop, true
	}
	if in.SilenceFor < s.VADStop+s.UserSpeechTimeout || strings.TrimSpace(in.Text) == "" {
		return DetectionDecision{}, false
	}
	if in.TextFinal || in.SilenceFor >= s.STTP99 {
		return stop, true
	}
	return DetectionDecision{}, false
}

type AnalyzerResult struct {
	Complete   bool
	Confidence float64
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
// (Observation.ExternalStop; the turn processor sets it when a
// core.PushToTalkFrame reports the button released).
func ExternalStop() UserTurnStopStrategy { return externalStop{} }

type externalStop struct{}

func (externalStop) Detect(in DetectionInput) (DetectionDecision, bool) {
	if in.ExternalStop {
		return DetectionDecision{Reason: core.TurnReasonSilence}, true
	}
	return DetectionDecision{}, false
}
