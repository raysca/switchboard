package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"govoice/server/catalog"
)

// systemPrompt is the ONE standard flow prompt, identical across profiles. The long-answer
// clause gives the barge-in scenarios a bot that keeps talking; the tool clause drives S6.
const systemPrompt = "You are a voice assistant. Answer in one or two short sentences, except when asked to tell about a topic: then give a detailed answer of at least six sentences. If asked what time it is, call the get_time tool and say what it returns."

// toolName is the one trivial tool the standard flow can call (S6). It is served by an MCP
// server the operator supplies with -mcp-url: the server only accepts public HTTPS MCP endpoints.
const toolName = "get_time"

// flowConfig is the stored flow config for a resolved profile. Stage structure is the only
// thing that differs between profiles; the server reads every secret from its environment.
func flowConfig(p Profile) map[string]any {
	stage := func(s Stage, extra map[string]any) map[string]any {
		m := map[string]any{"provider": s.Provider}
		for k, v := range extra {
			if v != "" {
				m[k] = v
			}
		}
		return m
	}
	return map[string]any{
		"stt": stage(p.Stages.STT, map[string]any{"model": p.Stages.STT.Model, "language": p.Stages.STT.Language}),
		"llm": stage(p.Stages.LLM, map[string]any{"model": p.Stages.LLM.Model, "system_prompt": systemPrompt}),
		"tts": stage(p.Stages.TTS, map[string]any{"voice": p.Stages.TTS.Voice}),
	}
}

// provision creates the standard flow and, with mcpURL set, attaches the get_time tool.
// It returns the flow id.
func (s *server) provision(p Profile, mcpURL string) (string, error) {
	fl, err := s.id("agents", map[string]any{"name": "Live Check", "description": "livecheck " + p.Name, "config": flowConfig(p)})
	if err != nil {
		return "", err
	}
	if mcpURL != "" {
		if err := s.attachTool(fl, mcpURL); err != nil {
			return "", fmt.Errorf("attach %s: %w", toolName, err)
		}
	}
	return fl, nil
}

// failoverPeer is the provider S13 falls back to: another stage's provider that also serves
// llm, on its catalog default model. The primary's variables are broken for the whole server
// child, so the primary provider must serve the llm stage only. na says why S13 cannot run.
func failoverPeer(p Profile) (provider, model, na string) {
	prim := normProvider(p.Stages.LLM.Provider)
	if normProvider(p.Stages.STT.Provider) == prim || normProvider(p.Stages.TTS.Provider) == prim {
		return "", "", "the broken " + prim + " key would also break STT/TTS: S13 needs two providers, the LLM's on the llm stage only"
	}
	for _, c := range []string{p.Stages.STT.Provider, p.Stages.TTS.Provider} {
		pr, ok := providers.Provider(normProvider(c))
		if !ok || pr.Services["llm"] == nil {
			continue
		}
		if e, ok := catalog.DefaultEntry(pr.Services["llm"].Models); ok {
			return pr.ID, e.ID, ""
		}
	}
	return "", "", "S13 needs two providers: no STT/TTS provider of this profile also serves llm as the fallback"
}

// failoverConfig is the standard flow with one LLM fallback on another provider. The primary's
// key is broken in the server's environment (see brokenEnv). Every failure class may fail over,
// client_error included (a rejected key), so a call must be answered by the fallback.
func failoverConfig(p Profile, fbProvider, fbModel string) map[string]any {
	cfg := flowConfig(p)
	cfg["llm"].(map[string]any)["fallbacks"] = []map[string]any{{"provider": fbProvider, "model": fbModel}}
	cfg["failover"] = map[string]any{"on": []string{"client_error", "server_error", "timeout", "circuit_open"}}
	return cfg
}

// brokenEnv replaces the values of the variables the profile's LLM stage reads with keys the
// provider rejects (env holds "K=V" entries).
func brokenEnv(p Profile, env []string) []string {
	vars := credSpecs[normProvider(p.Stages.LLM.Provider)]("llm")
	out := slices.Clone(env)
	for i, kv := range out {
		if k, v, _ := strings.Cut(kv, "="); slices.Contains(vars, k) {
			bad := brokenSecret(v)
			theRedactor.add(bad)
			out[i] = k + "=" + bad
		}
	}
	return out
}

// flowMarker is the word the Sales step must say in every reply, so S14 can tell which flow
// answered without trusting the model to announce itself.
const flowMarker = "catalogue"

// flowConfig is the two-step flow for S14: Front hands buyers to Sales. Both steps use the
// profile's providers; only Front (the entry) has STT, as the server requires.
func routedFlowConfig(p Profile) map[string]any {
	front := flowConfig(p)
	front["llm"].(map[string]any)["system_prompt"] = "You are the front desk of a car dealership. Answer in one short sentence. " +
		"If the caller wants to buy a car or asks about prices, call the transfer_to_sales tool, then say one short sentence that you are connecting them."
	sales := flowConfig(p)
	delete(sales, "stt")
	sales["llm"].(map[string]any)["system_prompt"] = "You are the sales agent of a car dealership. Answer in one short sentence and always use the word '" + flowMarker + "'."
	return map[string]any{
		"start": "front",
		"steps": []map[string]any{
			{"id": "front", "name": "Front desk", "config": front},
			{"id": "sales", "name": "Sales", "config": sales},
		},
		"routes": []map[string]any{{"id": "e1", "from": "front", "to": "sales", "condition": "The caller wants to buy a car or asks about prices."}},
	}
}

// provisionFlow creates the two-step flow for S14 and returns its id.
func (s *server) provisionFlow(p Profile) (string, error) {
	return s.id("agents", map[string]any{"name": "Live Check Graph", "description": "livecheck graph " + p.Name, "config": routedFlowConfig(p)})
}

// brokenSecret keeps a key's length and prefix but changes its last characters, so the provider
// rejects it as invalid rather than the server reading it as unset.
func brokenSecret(s string) string {
	if len(s) < 8 {
		return s + "-invalid"
	}
	return s[:len(s)-4] + "zzzz"
}

// provisionFailover creates the failover flow for S13 and returns its id.
func (s *server) provisionFailover(p Profile) (string, error) {
	prov, model, na := failoverPeer(p)
	if na != "" {
		return "", errors.New(na)
	}
	return s.id("agents", map[string]any{"name": "Live Check Failover", "description": "livecheck failover " + p.Name, "config": failoverConfig(p, prov, model)})
}

// attachTool registers the MCP server, discovers its tools and selects get_time for the flow.
func (s *server) attachTool(fl, url string) error {
	srv, err := s.id("mcp-servers", map[string]any{"name": "livecheck-tools", "endpoint": url, "enabled": true})
	if err != nil {
		return err
	}
	if _, err := s.api("mcp-servers/"+srv+"/validate", struct{}{}); err != nil {
		return err
	}
	d, err := s.call("GET", "mcp-servers/"+srv, nil)
	if err != nil {
		return err
	}
	var detail struct {
		Discovery struct {
			Tools []struct {
				RemoteName string `json:"remote_name"`
				SchemaHash string `json:"schema_hash"`
			}
		}
	}
	if err := json.Unmarshal(d, &detail); err != nil {
		return err
	}
	for _, t := range detail.Discovery.Tools {
		if t.RemoteName == toolName {
			_, err = s.call("PUT", "agents/"+fl+"/mcp-tools", map[string]any{"tools": []map[string]any{
				{"mcp_server_id": srv, "remote_name": toolName, "schema_hash": t.SchemaHash, "required": true}}})
			return err
		}
	}
	return fmt.Errorf("the MCP server does not list a %s tool", toolName)
}
