package eval

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"govoice/server/agent"
)

// Rewrite modes: keep the agent's own model, or swap it for the free offline or echo one.
// Speech is always replaced by the text-only silence provider.
const (
	ModeKeep    = "keep"
	ModeOffline = "offline"
	ModeEcho    = "echo"
)

// Rewrite turns a stored agent config into the config an eval call runs as a draft. It works on
// the generic JSON (the stored config is the contract) and changes only provider, model, voice,
// fallbacks and idle of the stages; prompts, sampling, tools, end_call and handoffs are untouched.
// Flat configs carry the stages at the top level, multi-step ones under steps[].config. The server
// decodes keys case-insensitively, so keys are matched that way too, and the result is decoded
// with the server's own decoder: Rewrite fails rather than return a config that is not text-only.
func Rewrite(config json.RawMessage, mode string) (json.RawMessage, error) {
	llm := map[string]any{}
	switch mode {
	case ModeKeep:
		llm = nil
	case ModeOffline:
		llm["provider"], llm["model"] = "offline", "govoice-test"
	case ModeEcho:
		llm["provider"], llm["model"] = "echo", "fake"
	default:
		return nil, fmt.Errorf("unknown providers mode %q, want keep, offline or echo", mode)
	}
	if _, err := agent.Decode(config); err != nil {
		return nil, fmt.Errorf("agent config: %w", err)
	}
	root, err := decode(config)
	if err != nil {
		return nil, err
	}
	for _, h := range stageHolders(root) {
		rewriteStages(h, llm)
	}
	out, err := json.Marshal(root)
	if err != nil {
		return nil, err
	}
	def, err := agent.Decode(out)
	if err != nil {
		return nil, fmt.Errorf("rewritten agent config: %w", err)
	}
	for _, s := range def.Steps {
		c := s.Config
		if p := c.STT.Provider; p != "" && p != agent.SilenceProvider || c.TTS.Provider != agent.SilenceProvider || c.Idle != nil {
			return nil, fmt.Errorf("rewritten step %q is not text-only (stt %q, tts %q)", s.ID, c.STT.Provider, c.TTS.Provider)
		}
		if ch := c.Chain("llm"); llm != nil && (len(ch) != 1 || ch[0].Provider != mode) {
			return nil, fmt.Errorf("rewritten step %q keeps llm chain %v, want only %s", s.ID, ch, mode)
		}
	}
	return out, nil
}

// NeedsLive reports whether running the config calls a paid model: any step's LLM, or one of its
// fallbacks, that is not echo, offline or silence. A config the server's decoder refuses counts as paid.
func NeedsLive(config json.RawMessage) bool {
	def, err := agent.Decode(config)
	if err != nil {
		return true // cannot tell: treat as paid
	}
	for _, s := range def.Steps {
		for _, c := range s.Config.Chain("llm") {
			if !slices.Contains([]string{"echo", "offline", "silence"}, c.Provider) {
				return true
			}
		}
	}
	return false
}

func decode(config json.RawMessage) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(config))
	dec.UseNumber() // keep numbers as written
	var root map[string]any
	if err := dec.Decode(&root); err != nil {
		return nil, fmt.Errorf("agent config: %w", err)
	}
	if root == nil {
		return nil, fmt.Errorf("agent config: not an object")
	}
	return root, nil
}

// canon renames the keys of m that match one of names case-insensitively (as the server's decoder
// does) to the lowercase name, so the rest of the code can use exact keys.
func canon(m map[string]any, names ...string) {
	for k, v := range m {
		for _, n := range names {
			if k != n && strings.EqualFold(k, n) {
				delete(m, k)
				if _, exact := m[n]; !exact {
					m[n] = v
				}
			}
		}
	}
}

// stageHolders returns the maps that carry stages: the config itself and every step's config.
func stageHolders(root map[string]any) []map[string]any {
	out := []map[string]any{root}
	steps, _ := root["steps"].([]any)
	for _, s := range steps {
		sm, _ := s.(map[string]any)
		if sm == nil {
			continue
		}
		canon(sm, "config")
		if c, _ := sm["config"].(map[string]any); c != nil {
			out = append(out, c)
		}
	}
	for _, h := range out {
		canon(h, "stt", "llm", "tts", "idle")
		for _, stage := range []string{"stt", "llm", "tts"} {
			if b, _ := h[stage].(map[string]any); b != nil {
				canon(b, "provider", "model", "language", "voice", "fallbacks")
			}
		}
	}
	return out
}

// rewriteStages edits one stage holder (the config or a step's config); llm is nil to keep the model.
func rewriteStages(h map[string]any, llm map[string]any) {
	delete(h, "idle") // with idle unset, a silence call is never nudged for typing slowly
	if stt, ok := h["stt"].(map[string]any); ok {
		lang, _ := stt["language"].(string)
		if lang == "" {
			lang = "en-US"
		}
		h["stt"] = map[string]any{"provider": "silence", "model": "text", "language": lang}
	}
	if _, ok := h["tts"].(map[string]any); ok {
		h["tts"] = map[string]any{"provider": "silence", "voice": "none"}
	}
	if l, ok := h["llm"].(map[string]any); ok && llm != nil {
		delete(l, "fallbacks") // silence takes none, and a swapped model has no chain to keep
		for k, v := range llm {
			l[k] = v
		}
	}
}
