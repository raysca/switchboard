package turn

import (
	"strings"
	"time"
)

// startVerdict is one strategy's answer, mirroring Pipecat's CONTINUE plus an
// explicit "start".
type startVerdict uint8

const (
	startContinue startVerdict = iota // no opinion, ask the next strategy
	startYes                          // user turn starts; first yes wins
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
