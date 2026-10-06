package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"govoice/server/provider"
	"govoice/server/storage"
)

// Config is the typed agent configuration (schema v2). It is the only source
// of what a call runs. Every stage is attached to a provider and to one of that
// provider's stored credentials by ID.
type Config struct {
	STT     STTConfig     `json:"stt"`
	LLM     LLMConfig     `json:"llm"`
	TTS     TTSConfig     `json:"tts"`
	Context ContextLimits `json:"context,omitempty"`
}

type STTConfig struct {
	Provider     string `json:"provider"`
	Model        string `json:"model,omitempty"`
	Language     string `json:"language,omitempty"`
	CredentialID string `json:"credential_id,omitempty"`
}

type LLMConfig struct {
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	SystemPrompt string `json:"system_prompt,omitempty"`
	CredentialID string `json:"credential_id,omitempty"`
}

type TTSConfig struct {
	Provider     string `json:"provider"`
	Voice        string `json:"voice,omitempty"`
	CredentialID string `json:"credential_id,omitempty"`
}

type ContextLimits struct {
	MaxMessages int `json:"max_messages,omitempty"`
	MaxTokens   int `json:"max_tokens,omitempty"`
}

// Default context limits, applied when an agent leaves them unset (0) so a
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

// DecodeConfig strictly decodes an agent config: unknown fields and wrong
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
	if len(c.LLM.SystemPrompt) > maxSystemPromptBytes {
		return c, fmt.Errorf("%w: llm.system_prompt exceeds %d bytes", ErrInvalidConfig, maxSystemPromptBytes)
	}
	if c.Context.MaxMessages < 0 || c.Context.MaxTokens < 0 || c.Context.MaxMessages == 1 {
		return c, fmt.Errorf("%w: context.max_messages must be 0 (default) or at least 2, max_tokens non-negative", ErrInvalidConfig)
	}
	return c, nil
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
	return reasons
}

// Stage names one provider slot of an agent.
type Stage struct {
	Name         string // "stt" | "llm" | "tts"
	Provider     string
	CredentialID string
}

func (c Config) Stages() []Stage {
	return []Stage{
		{Name: "stt", Provider: c.STT.Provider, CredentialID: c.STT.CredentialID},
		{Name: "llm", Provider: c.LLM.Provider, CredentialID: c.LLM.CredentialID},
		{Name: "tts", Provider: c.TTS.Provider, CredentialID: c.TTS.CredentialID},
	}
}

// ResolveCredential checks the credential a stage is attached to: it must be
// named explicitly, exist, belong to the stage's provider and not be marked
// invalid. It returns a readiness reason (e.g. "missing_credential_stt") otherwise.
func ResolveCredential(stage Stage, registry *provider.Registry, creds []storage.Credential) (string, string) {
	if _, ok := registry.Provider(stage.Provider); !ok || stage.Provider == provider.MCPProviderID {
		return "", "unknown_provider_" + stage.Name
	}
	if !registry.Supports(stage.Provider, provider.Capability(stage.Name)) {
		return "", "unsupported_provider_" + stage.Name
	}
	if stage.CredentialID == "" {
		return "", "missing_credential_" + stage.Name
	}
	for _, c := range creds {
		if c.ID == stage.CredentialID {
			if c.Provider != stage.Provider || c.ValidationState == "invalid" {
				return "", "invalid_credential_" + stage.Name
			}
			return c.ID, ""
		}
	}
	return "", "missing_credential_" + stage.Name
}

// AttachmentProblems lists why a config is not attached to a usable set of
// providers: missing fields, or stages whose provider/credential cannot run.
func AttachmentProblems(c Config, registry *provider.Registry, creds []storage.Credential) []string {
	problems := c.Missing()
	if len(problems) > 0 {
		return problems
	}
	for _, stage := range c.Stages() {
		if _, reason := ResolveCredential(stage, registry, creds); reason != "" {
			problems = append(problems, reason)
		}
	}
	return problems
}
