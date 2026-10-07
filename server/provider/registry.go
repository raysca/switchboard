// Package provider owns the server's explicit provider capability catalog and
// bounded model/voice discovery boundary. It never guesses capabilities from keys.
package provider

import (
	"errors"
	"sort"
	"strings"
)

type Capability string

const (
	CapabilityLLM Capability = "llm"
	CapabilitySTT Capability = "stt"
	CapabilityTTS Capability = "tts"
	CapabilityMCP Capability = "mcp"

	// MCPProviderID owns credentials usable as remote MCP bearer tokens.
	MCPProviderID = "mcp"
)

var (
	ErrUnknownProvider       = errors.New("unknown provider")
	ErrUnsupportedCapability = errors.New("unsupported provider capability")
)

type Definition struct {
	ID           string       `json:"id"`
	DisplayName  string       `json:"display_name"`
	Capabilities []Capability `json:"capabilities"`
	AuthRequired bool         `json:"auth_required"`
}

type Registry struct{ definitions map[string]Definition }

// xaiCapabilities lists what the xAI adapters in this build provide. xAI STT
// and TTS are experimental and compiled only with the xaispeech build tag.
var xaiCapabilities = []Capability{CapabilityLLM}

func Definitions() []Definition {
	return []Definition{
		{ID: "google", DisplayName: "Google", AuthRequired: true, Capabilities: []Capability{CapabilityLLM, CapabilitySTT, CapabilityTTS}},
		{ID: "cartesia", DisplayName: "Cartesia", AuthRequired: true, Capabilities: []Capability{CapabilityTTS}},
		{ID: "elevenlabs", DisplayName: "ElevenLabs", AuthRequired: true, Capabilities: []Capability{CapabilityTTS}},
		{ID: "deepgram", DisplayName: "Deepgram", AuthRequired: true, Capabilities: []Capability{CapabilitySTT}},
		{ID: "openai", DisplayName: "OpenAI-compatible", AuthRequired: true, Capabilities: []Capability{CapabilityLLM}},
		{ID: "xai", DisplayName: "xAI", AuthRequired: true, Capabilities: xaiCapabilities},
		// Bearer tokens for remote MCP servers. The only credentials whose
		// plaintext may leave the server (to the MCP endpoint they belong to).
		{ID: MCPProviderID, DisplayName: "MCP server token", AuthRequired: true, Capabilities: []Capability{CapabilityMCP}},
	}
}

func NewRegistry(definitions []Definition) *Registry {
	items := make(map[string]Definition, len(definitions))
	for _, def := range definitions {
		def.ID = strings.ToLower(strings.TrimSpace(def.ID))
		def.DisplayName = strings.TrimSpace(def.DisplayName)
		def.Capabilities = append([]Capability(nil), def.Capabilities...)
		sort.Slice(def.Capabilities, func(i, j int) bool { return def.Capabilities[i] < def.Capabilities[j] })
		items[def.ID] = def
	}
	return &Registry{definitions: items}
}

func (r *Registry) List() []Definition {
	if r == nil {
		return nil
	}
	ids := make([]string, 0, len(r.definitions))
	for id := range r.definitions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]Definition, 0, len(ids))
	for _, id := range ids {
		def := r.definitions[id]
		def.Capabilities = append([]Capability(nil), def.Capabilities...)
		out = append(out, def)
	}
	return out
}

func (r *Registry) Provider(id string) (Definition, bool) {
	if r == nil {
		return Definition{}, false
	}
	def, ok := r.definitions[strings.ToLower(strings.TrimSpace(id))]
	def.Capabilities = append([]Capability(nil), def.Capabilities...)
	return def, ok
}

func (r *Registry) Supports(id string, capability Capability) bool {
	def, ok := r.Provider(id)
	if !ok {
		return false
	}
	for _, item := range def.Capabilities {
		if item == capability {
			return true
		}
	}
	return false
}
