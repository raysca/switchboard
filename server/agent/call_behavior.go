package agent

import (
	"fmt"
	"math"
	"slices"
	"strings"

	coreturn "govoice/core/turn"
)

// IdleSettings is an agent's quiet-caller behavior (call-level: the start step). Present, it replaces
// the server's user-idle settings as one unit; members it leaves out take the built-in defaults.
// Pointers tell "absent" from an explicit 0: a timeout of 0 turns the handling off for the agent.
type IdleSettings struct {
	// UserTimeoutMS is the silence before each question: 0 (off) or 1000..300000.
	UserTimeoutMS *int `json:"user_timeout_ms,omitempty"`
	// MaxPrompts is how many times to ask before ending the call, 1..10 (default 2).
	MaxPrompts *int `json:"max_prompts,omitempty"`
	// Prompt is the nudge text; PromptMode is "llm" (the model words it, default) or "say" (verbatim).
	Prompt     string `json:"prompt,omitempty"`
	PromptMode string `json:"prompt_mode,omitempty"`
	// Goodbye is spoken, with no model, before hanging up.
	Goodbye string `json:"goodbye,omitempty"`
}

// TurnSettings is an agent's interruption rules and end-of-turn choice (call-level: the start step). Each
// member overrides the server's turn setting only when present; absent means the server's.
type TurnSettings struct {
	// MinWords is how many words of caller speech cut the agent off: 0 (off, any speech does) or 1..10.
	MinWords *int `json:"min_words,omitempty"`
	// Mute replaces the server's mute list; [] is no mute. Names are core's four strategies.
	Mute *[]string `json:"mute,omitempty"`
	// MaxMuteMS caps one mute, 1000..60000 and (at publish) at most the server's own cap.
	MaxMuteMS *int `json:"max_mute_ms,omitempty"`
	// SpeechTimeoutMS is how long the caller may pause before the turn ends: 200..3000.
	SpeechTimeoutMS *int `json:"speech_timeout_ms,omitempty"`
	// EndOfTurn picks the detector: default (wait for silence), smart (a model judges) or eager (end on a
	// full sentence). Every choice keeps the speech timeout as its fallback.
	EndOfTurn string `json:"end_of_turn,omitempty"`
	// SmartHoldMS is how long a turn the model judges unfinished is held: 500..5000, only with smart.
	SmartHoldMS *int `json:"smart_hold_ms,omitempty"`
}

// AudioSettings is an agent's call audio (call-level). Each member overrides the server's setting
// only when present.
type AudioSettings struct {
	// Denoise present turns noise reduction on for the call.
	Denoise *DenoiseSettings `json:"denoise,omitempty"`
	// OutputGainDB scales the bot's voice, -30..+6; an explicit 0 overrides a server-wide gain.
	OutputGainDB *float64 `json:"output_gain_db,omitempty"`
	// Background plays a server-configured sound under the voice.
	Background *BackgroundSettings `json:"background,omitempty"`
	// ToolCue plays a server cue (audio.cues) while a slow synchronous tool runs: once the tool has run
	// after_ms (0: 1000 ms) with the call quiet.
	ToolCue *HandoffCue `json:"tool_cue,omitempty"`
}

type DenoiseSettings struct {
	// Strength is above 0 and at most 1 (default 0.5).
	Strength *float64 `json:"strength,omitempty"`
}

// BackgroundSettings names a sound from the server's audio.backgrounds catalogue, never a path. An empty
// name means no background for this agent, even when the server has a default one.
type BackgroundSettings struct {
	Name   string   `json:"name"`              // present even when empty: {"name":""} is "none"
	GainDB *float64 `json:"gain_db,omitempty"` // -60..0, default -30
	Loop   *bool    `json:"loop,omitempty"`    // default true
}

// The end-of-turn choices an agent may name (turn.end_of_turn).
const (
	EndOfTurnDefault = "default"
	EndOfTurnSmart   = "smart"
	EndOfTurnEager   = "eager"
)

const (
	maxIdleTextBytes = 200
	minIdleTimeoutMS = 1000
	maxIdleTimeoutMS = 300000
	maxIdlePrompts   = 10

	MinCallDurationSeconds = 60 // the shortest call limit an agent may set (the editor shows it too)
	maxCallDurationSeconds = 86400

	maxTurnMinWords = 10
	minMaxMuteMS    = 1000
	maxMaxMuteMS    = 60000

	minSpeechTimeoutMS = 200
	maxSpeechTimeoutMS = 3000
	minSmartHoldMS     = 500
	maxSmartHoldMS     = 5000
)

var muteStrategies = []string{coreturn.MuteAlways, coreturn.MuteFirstSpeech, coreturn.MuteUntilFirstBot, coreturn.MuteFunctionCall}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

func (s *IdleSettings) validate() error {
	if *s == (IdleSettings{}) {
		return fmt.Errorf("%w: idle must set at least one member", ErrInvalidConfig)
	}
	if t := s.UserTimeoutMS; t != nil && *t != 0 && (*t < minIdleTimeoutMS || *t > maxIdleTimeoutMS) {
		return fmt.Errorf("%w: idle.user_timeout_ms must be 0 (off) or between %d and %d", ErrInvalidConfig, minIdleTimeoutMS, maxIdleTimeoutMS)
	}
	if p := s.MaxPrompts; p != nil && (*p < 1 || *p > maxIdlePrompts) {
		return fmt.Errorf("%w: idle.max_prompts must be between 1 and %d", ErrInvalidConfig, maxIdlePrompts)
	}
	switch s.PromptMode {
	case "", "llm", "say":
	default:
		return fmt.Errorf("%w: idle.prompt_mode must be llm or say", ErrInvalidConfig)
	}
	if err := validateSpokenText("idle.prompt", s.Prompt, maxIdleTextBytes); err != nil {
		return err
	}
	return validateSpokenText("idle.goodbye", s.Goodbye, maxIdleTextBytes)
}

func (a *AudioSettings) validate() error {
	if *a == (AudioSettings{}) {
		return fmt.Errorf("%w: audio must set at least one member", ErrInvalidConfig)
	}
	if d := a.Denoise; d != nil && d.Strength != nil && (!finite(*d.Strength) || *d.Strength <= 0 || *d.Strength > 1) {
		return fmt.Errorf("%w: audio.denoise.strength must be above 0 and at most 1 (leave it out for 0.5)", ErrInvalidConfig)
	}
	if g := a.OutputGainDB; g != nil && (!finite(*g) || *g < -30 || *g > 6) {
		return fmt.Errorf("%w: audio.output_gain_db must be between -30 and 6", ErrInvalidConfig)
	}
	if c := a.ToolCue; c != nil && (!cueName.MatchString(c.Name) || c.AfterMS < 0 || c.AfterMS > maxCueAfterMS) {
		return fmt.Errorf("%w: audio.tool_cue.name must name a server cue (lowercase letters, digits, - and _, up to 32) and after_ms must be between 0 and %d", ErrInvalidConfig, maxCueAfterMS)
	}
	if b := a.Background; b != nil {
		switch {
		case b.Name == "": // "no background for this agent", even when the server has a default
			if b.GainDB != nil || b.Loop != nil {
				return fmt.Errorf("%w: audio.background with no name (none) takes no gain_db or loop", ErrInvalidConfig)
			}
			return nil
		case !cueName.MatchString(b.Name):
			return fmt.Errorf("%w: audio.background.name must name a server sound (lowercase letters, digits, - and _, up to 32) or be empty for none", ErrInvalidConfig)
		}
		if g := b.GainDB; g != nil && (!finite(*g) || *g < -60 || *g > 0) {
			return fmt.Errorf("%w: audio.background.gain_db must be between -60 and 0", ErrInvalidConfig)
		}
	}
	return nil
}

// validate checks the turn settings; greeting is the start step's, which until_first_bot_complete needs
// (without one the cap is the only release).
func (s *TurnSettings) validate(greeting string) error {
	if *s == (TurnSettings{}) {
		return fmt.Errorf("%w: turn must set at least one member", ErrInvalidConfig)
	}
	if w := s.MinWords; w != nil && (*w < 0 || *w > maxTurnMinWords) {
		return fmt.Errorf("%w: turn.min_words must be between 0 (off) and %d", ErrInvalidConfig, maxTurnMinWords)
	}
	if s.Mute != nil {
		for i, name := range *s.Mute {
			switch {
			case !slices.Contains(muteStrategies, name):
				return fmt.Errorf("%w: turn.mute[%d] %q is not one of %s", ErrInvalidConfig, i, name, strings.Join(muteStrategies, ", "))
			case slices.Contains((*s.Mute)[:i], name):
				return fmt.Errorf("%w: turn.mute[%d] repeats %q", ErrInvalidConfig, i, name)
			case name == coreturn.MuteUntilFirstBot && strings.TrimSpace(greeting) == "":
				return fmt.Errorf("%w: turn.mute[%d] %s needs a greeting on the start step", ErrInvalidConfig, i, name)
			}
		}
	}
	if m := s.MaxMuteMS; m != nil && (*m < minMaxMuteMS || *m > maxMaxMuteMS) {
		return fmt.Errorf("%w: turn.max_mute_ms must be between %d and %d", ErrInvalidConfig, minMaxMuteMS, maxMaxMuteMS)
	}
	if t := s.SpeechTimeoutMS; t != nil && (*t < minSpeechTimeoutMS || *t > maxSpeechTimeoutMS) {
		return fmt.Errorf("%w: turn.speech_timeout_ms must be between %d and %d", ErrInvalidConfig, minSpeechTimeoutMS, maxSpeechTimeoutMS)
	}
	if !slices.Contains([]string{"", EndOfTurnDefault, EndOfTurnSmart, EndOfTurnEager}, s.EndOfTurn) {
		return fmt.Errorf("%w: turn.end_of_turn must be %s, %s or %s", ErrInvalidConfig, EndOfTurnDefault, EndOfTurnSmart, EndOfTurnEager)
	}
	if h := s.SmartHoldMS; h != nil {
		switch {
		case s.EndOfTurn != EndOfTurnSmart:
			return fmt.Errorf("%w: turn.smart_hold_ms only applies with turn.end_of_turn %q", ErrInvalidConfig, EndOfTurnSmart)
		case *h < minSmartHoldMS || *h > maxSmartHoldMS:
			return fmt.Errorf("%w: turn.smart_hold_ms must be between %d and %d", ErrInvalidConfig, minSmartHoldMS, maxSmartHoldMS)
		}
	}
	return nil
}
