package agent

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"govoice/core/resilience"
)

// MaxChainLength bounds primary + fallbacks per stage.
const MaxChainLength = 3

// STTFallback, LLMFallback and TTSFallback are the per-stage fields a fallback
// may set. The LLM system prompt and context limits are shared with the primary.
type STTFallback struct {
	Provider string `json:"provider"`
	Model    string `json:"model,omitempty"`
	Language string `json:"language,omitempty"`
}

type LLMFallback struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

type TTSFallback struct {
	Provider string `json:"provider"`
	Voice    string `json:"voice,omitempty"`
}

// Failover classes: the kinds of primary failure that may move a stage to its
// next candidate. They are what ClassOf can read from a provider error and the
// provider's resilience.Verdict; providers do not tell rate limits, 5xx and
// dropped connections apart, so those are all "server_error". "client_error"
// (bad key, bad request, unknown model) is opt-in: it is usually a config bug.
const (
	ClassTimeout = "timeout"
	ClassServer  = "server_error"
	ClassCircuit = "circuit_open"
	ClassClient  = "client_error"
)

var (
	knownClasses   = []string{ClassTimeout, ClassServer, ClassCircuit, ClassClient}
	defaultClasses = []string{ClassTimeout, ClassServer, ClassCircuit}
)

// ClassOf maps a failed provider call to a failover class. "" means never fail
// over: the caller cancelled (barge-in, hang-up) or its own deadline expired.
func ClassOf(err error, v resilience.Verdict) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, resilience.ErrCircuitOpen):
		return ClassCircuit
	case errors.Is(err, resilience.ErrTimeout):
		return ClassTimeout
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return ""
	case v.ClientError:
		return ClassClient
	}
	return ClassServer
}

// FailoverPolicy lists the error classes that allow failover. Empty = default.
type FailoverPolicy struct {
	On []string `json:"on,omitempty"`
}

// Allows reports whether an error of the given class may fail over.
func (p FailoverPolicy) Allows(class string) bool {
	set := p.On
	if len(set) == 0 {
		set = defaultClasses
	}
	for _, c := range set {
		if c == class {
			return true
		}
	}
	return false
}

// Candidate is one entry of a stage chain; Index 0 is the primary.
type Candidate struct {
	Stage    string
	Index    int
	Provider string
	Model    string
	Voice    string
	Language string
}

func (c Candidate) key() string {
	return strings.Join([]string{c.Provider, c.Model, c.Voice, c.Language}, "\x00")
}

// Chain returns the full ordered chain (primary first) for "stt", "llm" or "tts".
func (c Config) Chain(stage string) []Candidate {
	var out []Candidate
	switch stage {
	case "stt":
		out = append(out, Candidate{Provider: c.STT.Provider, Model: c.STT.Model, Language: c.STT.Language})
		for _, f := range c.STT.Fallbacks {
			out = append(out, Candidate{Provider: f.Provider, Model: f.Model, Language: f.Language})
		}
	case "llm":
		out = append(out, Candidate{Provider: c.LLM.Provider, Model: c.LLM.Model})
		for _, f := range c.LLM.Fallbacks {
			out = append(out, Candidate{Provider: f.Provider, Model: f.Model})
		}
	case "tts":
		out = append(out, Candidate{Provider: c.TTS.Provider, Voice: c.TTS.Voice})
		for _, f := range c.TTS.Fallbacks {
			out = append(out, Candidate{Provider: f.Provider, Voice: f.Voice})
		}
	}
	for i := range out {
		out[i].Stage, out[i].Index = stage, i
	}
	return out
}

// SilenceProvider is the text-only speech provider (docs/decisions/silence-provider.md). It never fails, so it
// has nothing to fail over from, and as a fallback it would swallow a real voice's audio or never transcribe.
const SilenceProvider = "silence"

// validateChains is the structural check (no registry needed): length,
// duplicates, required fields, policy classes.
func (c *Config) validateChains() error {
	for _, stage := range []string{"stt", "llm", "tts"} {
		chain := c.Chain(stage)
		if len(chain) > MaxChainLength {
			return fmt.Errorf("%w: %s chain has %d entries, max %d", ErrInvalidConfig, stage, len(chain), MaxChainLength)
		}
		if len(chain) == 1 {
			continue
		}
		seen := map[string]bool{}
		for _, cand := range chain {
			if cand.Provider == SilenceProvider {
				return fmt.Errorf("%w: %s %q takes no fallbacks and cannot be one", ErrInvalidConfig, stage, SilenceProvider)
			}
			if cand.Index > 0 && (cand.Provider == "" || (stage == "llm" && strings.TrimSpace(cand.Model) == "")) {
				return fmt.Errorf("%w: %s fallback %d needs a provider%s", ErrInvalidConfig, stage, cand.Index, map[bool]string{true: " and model"}[stage == "llm"])
			}
			if seen[cand.key()] {
				return fmt.Errorf("%w: %s chain has a duplicate entry", ErrInvalidConfig, stage)
			}
			seen[cand.key()] = true
		}
	}
	for i, class := range c.Failover.On {
		known := false
		for _, k := range knownClasses {
			known = known || k == class
		}
		if !known {
			return fmt.Errorf("%w: unknown failover class %q", ErrInvalidConfig, class)
		}
		for _, prev := range c.Failover.On[:i] {
			if prev == class {
				return fmt.Errorf("%w: duplicate failover class %q", ErrInvalidConfig, class)
			}
		}
	}
	return nil
}

func (c *Config) normalizeFallbacks() {
	for i := range c.STT.Fallbacks {
		c.STT.Fallbacks[i].Provider = strings.ToLower(strings.TrimSpace(c.STT.Fallbacks[i].Provider))
	}
	for i := range c.LLM.Fallbacks {
		c.LLM.Fallbacks[i].Provider = strings.ToLower(strings.TrimSpace(c.LLM.Fallbacks[i].Provider))
	}
	for i := range c.TTS.Fallbacks {
		c.TTS.Fallbacks[i].Provider = strings.ToLower(strings.TrimSpace(c.TTS.Fallbacks[i].Provider))
	}
}

// FallbackProblems checks every fallback like a primary. Reasons look like
// "missing_credential_llm_fallback_1". Used at publish time only.
func FallbackProblems(c Config, av Availability) (reasons, missingEnv []string) {
	for _, stage := range []string{"stt", "llm", "tts"} {
		for _, cand := range c.Chain(stage)[1:] {
			if u := av.Check(cand.Provider, stage); u.Reason != "" {
				reasons = append(reasons, u.Reason+"_"+stage+"_fallback_"+strconv.Itoa(cand.Index))
				missingEnv = append(missingEnv, u.MissingEnv...)
			}
		}
	}
	return reasons, uniq(missingEnv)
}
