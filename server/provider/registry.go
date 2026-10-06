// Package provider owns the server's explicit provider capability catalog and
// bounded model/voice discovery boundary. It never guesses capabilities from keys.
package provider

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"
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
	ErrDiscoveryLimit        = errors.New("provider discovery result limit exceeded")
	ErrDiscoveryFailed       = errors.New("provider discovery failed")
)

const (
	MaxDiscoveryResults = 100
	MaxOptionFieldBytes = 256
	DiscoveryTimeout    = 5 * time.Second
)

type Definition struct {
	ID           string       `json:"id"`
	DisplayName  string       `json:"display_name"`
	Capabilities []Capability `json:"capabilities"`
	AuthRequired bool         `json:"auth_required"`
}

type Option struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Language    string `json:"language,omitempty"`
}

type DiscoveryResult struct {
	Models []Option `json:"models"`
	Voices []Option `json:"voices"`
}

type DiscoveryAdapter interface {
	Discover(context.Context, Capability) (DiscoveryResult, error)
}

type DiscoveryFunc func(context.Context, Capability) (DiscoveryResult, error)

func (f DiscoveryFunc) Discover(ctx context.Context, c Capability) (DiscoveryResult, error) {
	return f(ctx, c)
}

type Registry struct{ definitions map[string]Definition }

func Definitions() []Definition {
	return []Definition{
		{ID: "google", DisplayName: "Google", AuthRequired: true, Capabilities: []Capability{CapabilityLLM, CapabilitySTT, CapabilityTTS}},
		{ID: "xai", DisplayName: "xAI", AuthRequired: true, Capabilities: []Capability{CapabilityLLM, CapabilitySTT, CapabilityTTS}},
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

func (r *Registry) Discover(parent context.Context, providerID string, capability Capability, adapter DiscoveryAdapter) (DiscoveryResult, error) {
	if _, ok := r.Provider(providerID); !ok {
		return DiscoveryResult{}, ErrUnknownProvider
	}
	if !r.Supports(providerID, capability) {
		return DiscoveryResult{}, ErrUnsupportedCapability
	}
	if adapter == nil {
		return DiscoveryResult{}, ErrDiscoveryFailed
	}
	ctx, cancel := context.WithTimeout(parent, DiscoveryTimeout)
	defer cancel()
	result, err := invokeDiscovery(ctx, adapter, capability)
	if err != nil {
		return DiscoveryResult{}, ErrDiscoveryFailed
	}
	if len(result.Models) > MaxDiscoveryResults || len(result.Voices) > MaxDiscoveryResults {
		return DiscoveryResult{}, ErrDiscoveryLimit
	}
	models, err := sanitizeOptions(result.Models)
	if err != nil {
		return DiscoveryResult{}, err
	}
	voices, err := sanitizeOptions(result.Voices)
	if err != nil {
		return DiscoveryResult{}, err
	}
	return DiscoveryResult{Models: models, Voices: voices}, nil
}

func invokeDiscovery(ctx context.Context, adapter DiscoveryAdapter, capability Capability) (result DiscoveryResult, err error) {
	defer func() {
		if recover() != nil {
			result = DiscoveryResult{}
			err = ErrDiscoveryFailed
		}
	}()
	return adapter.Discover(ctx, capability)
}

func sanitizeOptions(options []Option) ([]Option, error) {
	out := append([]Option(nil), options...)
	seen := make(map[string]struct{}, len(out))
	for i := range out {
		out[i].ID = strings.TrimSpace(out[i].ID)
		out[i].DisplayName = strings.TrimSpace(out[i].DisplayName)
		out[i].Language = strings.TrimSpace(out[i].Language)
		if out[i].ID == "" || len(out[i].ID) > MaxOptionFieldBytes || len(out[i].DisplayName) > MaxOptionFieldBytes || len(out[i].Language) > MaxOptionFieldBytes || hasControl(out[i].ID) || hasControl(out[i].DisplayName) || hasControl(out[i].Language) {
			return nil, ErrDiscoveryFailed
		}
		if _, ok := seen[out[i].ID]; ok {
			return nil, ErrDiscoveryFailed
		}
		seen[out[i].ID] = struct{}{}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
