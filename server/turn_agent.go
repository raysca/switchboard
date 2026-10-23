package server

import (
	"cmp"
	"slices"
	"time"

	coreturn "govoice/core/turn"
	"govoice/server/agent"
)

// effectiveMaxMute is the cap on one continuous mute: turn.max_mute, or the library's default.
func (t TurnConfig) effectiveMaxMute() time.Duration {
	return cmp.Or(t.MaxMute, coreturn.DefaultMaxMute)
}

// effectiveSpeechTimeout is the resume window after the VAD stop: turn.speech_timeout, or the library's default.
func (t TurnConfig) effectiveSpeechTimeout() time.Duration {
	return cmp.Or(t.SpeechTimeout, coreturn.DefaultUserSpeechTimeout)
}

// endOfTurn names the detector the server's own chain uses, in the words an agent chooses it by.
func (t TurnConfig) endOfTurn() string {
	switch {
	case slices.Contains(t.Stop, "turn-analyzer"):
		return agent.EndOfTurnSmart
	case slices.Contains(t.Stop, "eager"):
		return agent.EndOfTurnEager
	}
	return agent.EndOfTurnDefault
}

// forCall is the server's turn settings with an agent's choices laid over them, member by member. The
// server's config is shared by every call, so the result never aliases its slices, and an agent can only
// shorten the mute cap.
func (t TurnConfig) forCall(f *agent.TurnSettings) TurnConfig {
	t.Mute = slices.Clone(t.Mute)
	t.Stop = slices.Clone(t.Stop)
	if f == nil {
		return t
	}
	if f.MinWords != nil {
		t.MinWords = *f.MinWords
	}
	if f.Mute != nil {
		t.Mute = slices.Clone(*f.Mute)
	}
	if f.MaxMuteMS != nil {
		t.MaxMute = min(t.effectiveMaxMute(), time.Duration(*f.MaxMuteMS)*time.Millisecond)
	}
	if f.SpeechTimeoutMS != nil {
		t.SpeechTimeout = time.Duration(*f.SpeechTimeoutMS) * time.Millisecond
	}
	if f.SmartHoldMS != nil {
		t.Analyzer.Hold = time.Duration(*f.SmartHoldMS) * time.Millisecond
	}
	if f.EndOfTurn != "" {
		t.Stop = agentStop(f.EndOfTurn, t.Stop)
	}
	return t
}

// agentStop is the stop chain for an agent's end_of_turn. It replaces only the detector entries: an
// llm-completion entry is the prompt protocol, which stays the server's. Every chain keeps speech-timeout
// (a chain of llm-completion alone is refused by turnStopDetector), so default is [] only without it.
func agentStop(endOfTurn string, server []string) []string {
	var stop []string
	switch endOfTurn {
	case agent.EndOfTurnSmart:
		stop = []string{"turn-analyzer", "speech-timeout"}
	case agent.EndOfTurnEager:
		stop = []string{"eager", "speech-timeout"}
	}
	if slices.Contains(server, "llm-completion") {
		if stop == nil {
			stop = []string{"speech-timeout"}
		}
		stop = append(stop, "llm-completion")
	}
	return stop
}
