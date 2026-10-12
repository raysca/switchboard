package flow

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Config is the typed flow configuration (schema v2). It is the only source
// of what a call runs. Every stage is attached to a provider; credentials come
// from the catalog and environment, never from the config.
type Config struct {
	STT     STTConfig     `json:"stt"`
	LLM     LLMConfig     `json:"llm"`
	TTS     TTSConfig     `json:"tts"`
	Context ContextLimits `json:"context,omitempty"`
	// VADStopMS overrides the server VAD stop hangover (200..2000 ms). 0 uses the server default.
	VADStopMS int `json:"vad_stop_ms,omitempty"`
	// STTP99MS overrides the STT p99 the speech timeout waits for (100..10000 ms). 0 uses the
	// server's turn.stt_p99 or the built-in table.
	STTP99MS int `json:"stt_p99_ms,omitempty"`
	// AsyncTools lists selected MCP tools ("<mcp_server_id>/<remote_name>") that
	// run in the background: the model gets an immediate ack and is told the
	// result when it arrives. Default none.
	AsyncTools []string `json:"async_tools,omitempty"`
	// Failover says which error classes let a stage move to its next
	// fallback. Fallbacks are optional; none = single provider (legacy configs).
	Failover FailoverPolicy `json:"failover,omitempty"`
	// Recording opts this flow's calls into audio recording. It only takes
	// effect when the server enables recording too.
	Recording RecordingConfig `json:"recording,omitzero"`
}

// RecordingConfig is the per-flow recording opt-in. Recording captures caller
// audio, so the operator must confirm that callers are told (disclosure).
type RecordingConfig struct {
	Enabled             bool `json:"enabled"`
	DisclosureConfirmed bool `json:"disclosure_confirmed"`
}

// Active reports whether calls to this flow are recorded (when the server allows it).
func (r RecordingConfig) Active() bool { return r.Enabled && r.DisclosureConfirmed }

type STTConfig struct {
	Provider  string        `json:"provider"`
	Model     string        `json:"model,omitempty"`
	Language  string        `json:"language,omitempty"`
	Fallbacks []STTFallback `json:"fallbacks,omitempty"`
}

type LLMConfig struct {
	Provider     string        `json:"provider"`
	Model        string        `json:"model"`
	SystemPrompt string        `json:"system_prompt,omitempty"`
	Fallbacks    []LLMFallback `json:"fallbacks,omitempty"`
	// MaxParallel bounds concurrently executing tool calls within one round (1..16, 0 keeps default 1).
	MaxParallel int `json:"max_parallel,omitempty"`
}

type TTSConfig struct {
	Provider  string        `json:"provider"`
	Voice     string        `json:"voice,omitempty"`
	Fallbacks []TTSFallback `json:"fallbacks,omitempty"`
	// CleanText controls whether markdown, emojis, and formatting markers are stripped
	// before synthesis. When nil or true, text cleaning is active. Set false to disable.
	CleanText *bool `json:"clean_text,omitempty"`
}

type ContextLimits struct {
	MaxMessages   int                 `json:"max_messages,omitempty"`
	MaxTokens     int                 `json:"max_tokens,omitempty"`
	Summarization SummarizationConfig `json:"summarization,omitzero"`
}

// SummarizationConfig condenses older turns into one summary (by the primary
// LLM) before the token budget starts evicting them. Off by default. Summaries
// are derived from call content and live only in the call's history.
type SummarizationConfig struct {
	Enabled bool `json:"enabled"`
	// TriggerTokens: summarize when history exceeds this many estimated tokens.
	// 0 = three quarters of the effective max_tokens; otherwise it must be below it.
	TriggerTokens int `json:"trigger_tokens,omitempty"`
	// KeepTurns: newest turns kept verbatim. 0 = 4.
	KeepTurns int `json:"keep_turns,omitempty"`
	// Prompt overrides the summarization system prompt (max 2 KiB). The
	// transcript is always sent as delimited, untrusted data.
	Prompt string `json:"prompt,omitempty"`
}

const maxSummaryPromptBytes = 2 << 10

// SummarizingProviders are the LLM providers whose adapters implement text
// completion for summaries.
var SummarizingProviders = map[string]bool{"google": true, "nvidia": true, "openai": true, "xai": true}

// DefaultSummaryTrigger is the trigger when none is set: three quarters of maxTokens.
func DefaultSummaryTrigger(maxTokens int) int { return maxTokens * 3 / 4 }

func (s SummarizationConfig) validate(llmProvider string, maxTokens int) error {
	if s == (SummarizationConfig{}) {
		return nil
	}
	_, effMax := ContextLimits{MaxTokens: maxTokens}.Limits()
	switch {
	case s.TriggerTokens < 0 || s.KeepTurns < 0:
		return fmt.Errorf("%w: context.summarization trigger_tokens and keep_turns must not be negative", ErrInvalidConfig)
	case s.TriggerTokens >= effMax:
		return fmt.Errorf("%w: context.summarization.trigger_tokens must be below context.max_tokens (%d)", ErrInvalidConfig, effMax)
	case len(s.Prompt) > maxSummaryPromptBytes:
		return fmt.Errorf("%w: context.summarization.prompt exceeds %d bytes", ErrInvalidConfig, maxSummaryPromptBytes)
	case s.Enabled && llmProvider != "" && !SummarizingProviders[llmProvider]:
		return fmt.Errorf("%w: llm provider %q does not support context summarization", ErrInvalidConfig, llmProvider)
	}
	return nil
}

// Default context limits, applied when a flow leaves them unset (0) so a
// call's conversation history, prompt size and LLM cost stay bounded.
const (
	DefaultMaxMessages = 20
	DefaultMaxTokens   = 4000
)

// Limits returns the context limits a call uses, with defaults applied.
func (l ContextLimits) Limits() (maxMessages, maxTokens int) {
	maxMessages, maxTokens = l.MaxMessages, l.MaxTokens
	if maxMessages == 0 {
		maxMessages = DefaultMaxMessages
	}
	if maxTokens == 0 {
		maxTokens = DefaultMaxTokens
	}
	return maxMessages, maxTokens
}

const maxSystemPromptBytes = 32 << 10

var ErrInvalidConfig = errors.New("invalid agent config")

// DecodeConfig strictly decodes a flow config: unknown fields and wrong
// types are rejected. Completeness is checked by AttachmentProblems.
func DecodeConfig(raw json.RawMessage) (Config, error) {
	var c Config
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}
	c.STT.Provider = strings.ToLower(strings.TrimSpace(c.STT.Provider))
	c.LLM.Provider = strings.ToLower(strings.TrimSpace(c.LLM.Provider))
	c.TTS.Provider = strings.ToLower(strings.TrimSpace(c.TTS.Provider))
	c.normalizeFallbacks()
	if err := c.validateChains(); err != nil {
		return c, err
	}
	if len(c.LLM.SystemPrompt) > maxSystemPromptBytes {
		return c, fmt.Errorf("%w: llm.system_prompt exceeds %d bytes", ErrInvalidConfig, maxSystemPromptBytes)
	}
	if c.Context.MaxMessages < 0 || c.Context.MaxTokens < 0 || c.Context.MaxMessages == 1 {
		return c, fmt.Errorf("%w: context.max_messages must be 0 (default) or at least 2, max_tokens non-negative", ErrInvalidConfig)
	}
	if c.LLM.MaxParallel < 0 || c.LLM.MaxParallel > 16 {
		return c, fmt.Errorf("%w: llm.max_parallel must be between 0 and 16", ErrInvalidConfig)
	}
	if c.VADStopMS < 0 || (c.VADStopMS > 0 && (c.VADStopMS < 200 || c.VADStopMS > 2000)) {
		return c, fmt.Errorf("%w: vad_stop_ms must be 0 (default) or between 200 and 2000", ErrInvalidConfig)
	}
	if c.STTP99MS < 0 || (c.STTP99MS > 0 && (c.STTP99MS < 100 || c.STTP99MS > 10000)) {
		return c, fmt.Errorf("%w: stt_p99_ms must be 0 (default) or between 100 and 10000", ErrInvalidConfig)
	}
	if err := c.Context.Summarization.validate(c.LLM.Provider, c.Context.MaxTokens); err != nil {
		return c, err
	}
	if len(c.AsyncTools) > maxAsyncTools {
		return c, fmt.Errorf("%w: async_tools lists more than %d tools", ErrInvalidConfig, maxAsyncTools)
	}
	seen := map[string]bool{}
	for _, name := range c.AsyncTools {
		server, remote, ok := strings.Cut(name, "/")
		if !ok || server == "" || remote == "" || len(name) > 256 || seen[name] {
			return c, fmt.Errorf("%w: async_tools entries must be unique \"<mcp_server_id>/<remote_name>\"", ErrInvalidConfig)
		}
		seen[name] = true
	}
	return c, nil
}

const maxAsyncTools = 64

// IsAsyncTool reports whether the selected MCP tool runs in the background.
func (c Config) IsAsyncTool(serverID, remoteName string) bool {
	return slices.Contains(c.AsyncTools, serverID+"/"+remoteName)
}

// Missing lists required fields the config lacks.
func (c Config) Missing() []string {
	var reasons []string
	if c.STT.Provider == "" {
		reasons = append(reasons, "missing_stt")
	}
	if c.LLM.Provider == "" || strings.TrimSpace(c.LLM.Model) == "" {
		reasons = append(reasons, "missing_llm")
	}
	if c.TTS.Provider == "" {
		reasons = append(reasons, "missing_tts")
	}
	if c.Recording.Enabled && !c.Recording.DisclosureConfirmed {
		reasons = append(reasons, "recording_disclosure_unconfirmed")
	}
	return reasons
}

// Availability says whether a build and environment can run provider/service.
type Availability interface {
	Check(provider, service string) Unavailable
}

// Unavailable says why a provider service cannot run; the zero value means it can.
type Unavailable struct {
	Reason     string
	MissingEnv []string
}

// Stage names one provider slot of a flow.
type Stage struct {
	Name     string // "stt" | "llm" | "tts"
	Provider string
}

func (c Config) Stages() []Stage {
	return []Stage{
		{Name: "stt", Provider: c.STT.Provider},
		{Name: "llm", Provider: c.LLM.Provider},
		{Name: "tts", Provider: c.TTS.Provider},
	}
}

// AttachmentProblems lists why a config cannot run: missing fields, or stages
// whose provider is unknown, unsupported or lacks credentials in the environment.
func AttachmentProblems(c Config, av Availability) (reasons, missingEnv []string) {
	if m := c.Missing(); len(m) > 0 {
		return m, nil
	}
	for _, st := range c.Stages() {
		if u := av.Check(st.Provider, st.Name); u.Reason != "" {
			reasons = append(reasons, u.Reason+"_"+st.Name)
			missingEnv = append(missingEnv, u.MissingEnv...)
		}
	}
	return reasons, uniq(missingEnv)
}

// uniq sorts and de-duplicates ss.
func uniq(ss []string) []string {
	slices.Sort(ss)
	return slices.Compact(ss)
}
