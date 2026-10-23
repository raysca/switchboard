package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
)

// Rewrite modes: keep the agent's own model, or swap it for the free offline or echo one.
// Speech is always replaced by the text-only silence provider.
const (
	modeKeep    = "keep"
	modeOffline = "offline"
	modeEcho    = "echo"
)

// Rewrite turns a stored agent config into the config an eval call runs as a draft. It works on
// the generic JSON (the stored config is the contract) and changes only provider, model, voice
// and fallbacks of the stages; prompts, sampling, tools, end_call and handoffs are untouched.
// Flat configs carry the stages at the top level, multi-step ones under steps[].config.
func Rewrite(config json.RawMessage, mode string) (json.RawMessage, error) {
	llm := map[string]any{}
	switch mode {
	case modeKeep:
		llm = nil
	case modeOffline:
		llm["provider"], llm["model"] = "offline", "govoice-test"
	case modeEcho:
		llm["provider"], llm["model"] = "echo", "fake"
	default:
		return nil, fmt.Errorf("unknown providers mode %q, want keep, offline or echo", mode)
	}
	root, err := decode(config)
	if err != nil {
		return nil, err
	}
	rewriteStages(root, llm)
	for _, s := range stepConfigs(root) {
		rewriteStages(s, llm)
	}
	return json.Marshal(root)
}

// NeedsLive reports whether running the config calls a paid model: any step's LLM, or one of its
// fallbacks, that is not echo, offline or silence.
func NeedsLive(config json.RawMessage) bool {
	root, err := decode(config)
	if err != nil {
		return true // cannot tell: treat as paid
	}
	for _, s := range append([]map[string]any{root}, stepConfigs(root)...) {
		l, _ := s["llm"].(map[string]any)
		if l == nil {
			continue
		}
		if paid(l) {
			return true
		}
		fbs, _ := l["fallbacks"].([]any)
		for _, f := range fbs {
			if fm, _ := f.(map[string]any); fm != nil && paid(fm) {
				return true
			}
		}
	}
	return false
}

func paid(block map[string]any) bool {
	p, _ := block["provider"].(string)
	return !slices.Contains([]string{"echo", "offline", "silence"}, p)
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

func stepConfigs(root map[string]any) (out []map[string]any) {
	steps, _ := root["steps"].([]any)
	for _, s := range steps {
		sm, _ := s.(map[string]any)
		if c, _ := sm["config"].(map[string]any); c != nil {
			out = append(out, c)
		}
	}
	return out
}

// rewriteStages edits one stage holder (the config or a step's config); llm is nil to keep the model.
func rewriteStages(h map[string]any, llm map[string]any) {
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
