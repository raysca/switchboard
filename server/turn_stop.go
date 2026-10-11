package server

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"govoice/core"
	coreturn "govoice/core/turn"
)

// turnStopDetector builds the user-turn stop chain from turn.stop. Empty
// means the gated speech timeout (core/turn SpeechTimeout: resume window after
// the VAD stop, a transcript, final or past the STT p99). A chain
// must contain speech-timeout: eager alone never ends an unpunctuated turn.
// external is rejected: push-to-talk is chosen per call by the client
// (client-ready push_to_talk), not by server config.
// llm-completion is not a detector: it enables the marker protocol in the
// assistant stage (core.TurnCompletionConfig) and adds nothing to the chain.
//
// turn-analyzer (Smart Turn model) builds its strategy with analyzer(base),
// which already contains the speech-timeout fallback (base when the model
// cannot answer, stop_secs when it says incomplete); any speech-timeout entry
// in such a chain is therefore subsumed by it, since a plain timeout would end
// every turn the model wanted to hold. Its base is analyzerBase(st).
func turnStopDetector(stop []string, st coreturn.SpeechTimeoutConfig, eagerGrace time.Duration, analyzer func(base time.Duration) coreturn.UserTurnStopStrategy) (coreturn.TurnDetector, error) {
	if len(stop) == 0 {
		return coreturn.SpeechTimeout(st), nil
	}
	if !slices.Contains(stop, "speech-timeout") {
		return nil, errors.New("turn.stop must include speech-timeout as the fallback")
	}
	if eagerGrace <= 0 {
		eagerGrace = 200 * time.Millisecond
	}
	var chain []coreturn.UserTurnStopStrategy
	withModel := slices.Contains(stop, "turn-analyzer")
	for _, name := range stop {
		switch name {
		case "speech-timeout":
			if !withModel {
				chain = append(chain, coreturn.SpeechTimeout(st))
			}
		case "turn-analyzer":
			if analyzer == nil {
				return nil, errors.New("turn.stop turn-analyzer: smart-turn model is not loaded")
			}
			chain = append(chain, analyzer(analyzerBase(st)))
		case "eager":
			chain = append(chain, coreturn.EagerStop(eagerGrace))
		case "llm-completion":
			// protocol, not a stop strategy; see completionConfig
		case "external":
			return nil, errors.New(`turn.stop "external" is not a server setting; clients opt in with client-ready push_to_talk`)
		default:
			return nil, fmt.Errorf("turn.stop: unknown strategy %q (speech-timeout, eager, turn-analyzer, llm-completion)", name)
		}
	}
	return coreturn.StopChain(chain...), nil
}

// analyzerBase is the smart-turn fallback silence: the gated speech timeout's resume window
// from onset (VAD stop + user speech timeout).
func analyzerBase(st coreturn.SpeechTimeoutConfig) time.Duration {
	us := st.UserSpeechTimeout
	if us <= 0 {
		us = coreturn.DefaultUserSpeechTimeout
	}
	return st.VADStop + us
}

// CompletionConfig is turn.completion.
type CompletionConfig struct {
	ShortWait time.Duration `yaml:"short_wait"`
	LongWait  time.Duration `yaml:"long_wait"`
	Prompt    string        `yaml:"prompt"`
	Nudge     string        `yaml:"nudge"`
}

// completionConfig returns the LLM turn-completion protocol settings, or nil
// when turn.stop does not list llm-completion.
func completionConfig(stop []string, c CompletionConfig, onVerdict func(string)) *core.TurnCompletionConfig {
	if !slices.Contains(stop, "llm-completion") {
		return nil
	}
	return &core.TurnCompletionConfig{ShortWait: c.ShortWait, LongWait: c.LongWait, Nudge: c.Nudge, OnVerdict: onVerdict}
}

// completionPrompt appends the protocol instructions to a system prompt.
func completionPrompt(system string, c CompletionConfig) string {
	p := c.Prompt
	if p == "" {
		p = core.CompletionInstructions
	}
	return system + "\n" + p
}
