package flow

import (
	"fmt"
	"math"
)

// IdleSettings is a flow's quiet-caller behavior (call-level: the start step). Present, it replaces
// the server's user-idle settings as one unit; members it leaves out take the built-in defaults.
// Pointers tell "absent" from an explicit 0: a timeout of 0 turns the handling off for the flow.
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

// AudioSettings is a flow's call audio (call-level). Each member overrides the server's setting
// only when present.
type AudioSettings struct {
	// Denoise present turns noise reduction on for the call.
	Denoise *DenoiseSettings `json:"denoise,omitempty"`
	// OutputGainDB scales the bot's voice, -30..+6; an explicit 0 overrides a server-wide gain.
	OutputGainDB *float64 `json:"output_gain_db,omitempty"`
	// Background plays a server-configured sound under the voice.
	Background *BackgroundSettings `json:"background,omitempty"`
}

type DenoiseSettings struct {
	// Strength is above 0 and at most 1 (default 0.5).
	Strength *float64 `json:"strength,omitempty"`
}

// BackgroundSettings names a sound from the server's audio.backgrounds catalogue, never a path. An empty
// name means no background for this flow, even when the server has a default one.
type BackgroundSettings struct {
	Name   string   `json:"name"`              // present even when empty: {"name":""} is "none"
	GainDB *float64 `json:"gain_db,omitempty"` // -60..0, default -30
	Loop   *bool    `json:"loop,omitempty"`    // default true
}

const (
	maxIdleTextBytes = 200
	minIdleTimeoutMS = 1000
	maxIdleTimeoutMS = 300000
	maxIdlePrompts   = 10
)

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
	if a.Denoise == nil && a.OutputGainDB == nil && a.Background == nil {
		return fmt.Errorf("%w: audio must set at least one member", ErrInvalidConfig)
	}
	if d := a.Denoise; d != nil && d.Strength != nil && (!finite(*d.Strength) || *d.Strength <= 0 || *d.Strength > 1) {
		return fmt.Errorf("%w: audio.denoise.strength must be above 0 and at most 1 (leave it out for 0.5)", ErrInvalidConfig)
	}
	if g := a.OutputGainDB; g != nil && (!finite(*g) || *g < -30 || *g > 6) {
		return fmt.Errorf("%w: audio.output_gain_db must be between -30 and 6", ErrInvalidConfig)
	}
	if b := a.Background; b != nil {
		switch {
		case b.Name == "": // "no background for this flow", even when the server has a default
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
