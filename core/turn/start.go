package turn

import (
	"regexp"
	"strings"
	"time"
	"unicode"
)

// startVerdict is one strategy's answer, mirroring Pipecat's CONTINUE/STOP
// plus an explicit "start".
type startVerdict uint8

const (
	startContinue startVerdict = iota // no opinion, ask the next strategy
	startYes                          // user turn starts; first yes wins
	startBlock                        // veto: stop the chain without starting (wake phrase idle)
)

// startInput is what the manager shows a strategy. It is a value snapshot;
// strategies never see the manager.
type startInput struct {
	At time.Time
	// VADCandidate is true when speech activity opened the candidate.
	// SpeakingFor is the candidate's age and Debounce the configured
	// start debounce (max of StartDebounce and MinimumSpeechDuration).
	VADCandidate bool
	SpeakingFor  time.Duration
	Debounce     time.Duration
	// Text is a transcript delivered since the last evaluation (HasText false
	// when none). Final is false for interim results.
	Text    string
	HasText bool
	Final   bool
	// BotSpeaking is the latest bot-output state reported by the caller.
	BotSpeaking bool
	// External is a start signal from another component (one-shot).
	External bool
}

// UserTurnStartStrategy decides whether a user turn (and so a barge-in) starts.
// The interface is sealed: the manager calls strategies while holding its
// state lock, so only the in-package, non-blocking, callback-free built-ins
// below may implement it. A strategy instance belongs to one Manager.
type UserTurnStartStrategy interface {
	evaluate(startInput) startVerdict
	// activity reports user/bot activity at every observation, including
	// while a turn is active, so timeouts see time outside evaluations.
	activity(at time.Time)
	reset()
}

type stateless struct{}

func (stateless) reset()             {}
func (stateless) activity(time.Time) {}

type vadStart struct{ stateless }

// VADStart starts a turn after sustained speech activity (the default).
func VADStart() UserTurnStartStrategy { return vadStart{} }

func (vadStart) evaluate(in startInput) startVerdict {
	if in.VADCandidate && in.SpeakingFor >= in.Debounce {
		return startYes
	}
	return startContinue
}

type transcriptionStart struct {
	stateless
	useInterim bool
}

// TranscriptionStart starts a turn on any transcript; a fallback for speech
// VAD misses. Interim results count only when useInterim is set.
func TranscriptionStart(useInterim bool) UserTurnStartStrategy {
	return transcriptionStart{useInterim: useInterim}
}

func (s transcriptionStart) evaluate(in startInput) startVerdict {
	if in.HasText && (in.Final || s.useInterim) {
		return startYes
	}
	return startContinue
}

type minWordsStart struct {
	stateless
	min        int
	useInterim bool
}

// MinWordsStart starts a turn once a transcript has at least min words while
// the bot is speaking (one word when it is not), so "uh-huh" does not barge in.
func MinWordsStart(min int, useInterim bool) UserTurnStartStrategy {
	return minWordsStart{min: min, useInterim: useInterim}
}

func (s minWordsStart) evaluate(in startInput) startVerdict {
	if !in.HasText || !(in.Final || s.useInterim) {
		return startContinue
	}
	need := 1
	if in.BotSpeaking {
		need = s.min
	}
	if len(strings.Fields(in.Text)) >= need {
		return startYes
	}
	return startContinue
}

type externalStart struct{ stateless }

// ExternalStart starts a turn only when the caller signals it
// (Observation.ExternalStart).
func ExternalStart() UserTurnStartStrategy { return externalStart{} }

func (externalStart) evaluate(in startInput) startVerdict {
	if in.External {
		return startYes
	}
	return startContinue
}

type wakePhraseStart struct {
	patterns []*regexp.Regexp
	timeout  time.Duration
	awake    bool
	deadline time.Time
	acc      string
}

// WakePhraseStart blocks the chain until a final transcript contains one of
// phrases, then lets later strategies run until timeout of inactivity passes.
// Place it first. ponytail: no single-activation mode; add if needed.
func WakePhraseStart(phrases []string, timeout time.Duration) UserTurnStartStrategy {
	s := &wakePhraseStart{timeout: timeout}
	for _, p := range phrases {
		words := strings.Fields(stripPunct(p))
		for i := range words {
			words[i] = regexp.QuoteMeta(words[i])
		}
		if len(words) > 0 {
			// Go's \b is ASCII-only; text is punctuation-stripped, so a
			// whitespace/end boundary is the Unicode-correct equivalent.
			s.patterns = append(s.patterns, regexp.MustCompile(`(?i)(?:^|\s)`+strings.Join(words, `\s*`)+`(?:\s|$)`))
		}
	}
	return s
}

func stripPunct(text string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsMark(r) || unicode.IsDigit(r) || unicode.IsSpace(r) || r == '_' {
			return r
		}
		return -1
	}, text)
}

func (s *wakePhraseStart) reset() { s.awake, s.acc = false, "" }

func (s *wakePhraseStart) activity(at time.Time) {
	if s.awake && at.After(s.deadline) {
		s.reset()
	} else if s.awake {
		s.deadline = at.Add(s.timeout)
	}
}

func (s *wakePhraseStart) evaluate(in startInput) startVerdict {
	if s.awake && in.At.After(s.deadline) {
		s.reset()
	}
	if s.awake {
		return startContinue
	}
	if in.HasText && in.Final {
		s.acc += " " + stripPunct(in.Text)
		if len(s.acc) > 250 {
			s.acc = s.acc[len(s.acc)-250:]
		}
		for _, p := range s.patterns {
			if p.MatchString(s.acc) {
				s.awake, s.acc, s.deadline = true, "", in.At.Add(s.timeout)
				return startYes
			}
		}
	}
	return startBlock
}
