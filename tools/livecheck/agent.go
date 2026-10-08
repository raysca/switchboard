package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// systemPrompt is the ONE standard agent prompt, identical across profiles. The long-answer
// clause gives the barge-in scenarios a bot that keeps talking; the tool clause drives S6.
const systemPrompt = "You are a voice assistant. Answer in one or two short sentences, except when asked to tell about a topic: then give a detailed answer of at least six sentences. If asked what time it is, call the get_time tool and say what it returns."

// toolName is the one trivial tool the standard agent can call (S6). It is served by an MCP
// server the operator supplies with -mcp-url: the server only accepts public HTTPS MCP endpoints.
const toolName = "get_time"

// agentConfig is the stored agent config for a resolved profile and its credential ids per
// stage. Stage structure is the only thing that differs between profiles.
func agentConfig(p Profile, cred map[string]string) map[string]any {
	stage := func(s Stage, name string, extra map[string]any) map[string]any {
		m := map[string]any{"provider": s.Provider, "credential_id": cred[name]}
		for k, v := range extra {
			if v != "" {
				m[k] = v
			}
		}
		return m
	}
	return map[string]any{
		"stt": stage(p.Stages.STT, "stt", map[string]any{"model": p.Stages.STT.Model, "language": p.Stages.STT.Language}),
		"llm": stage(p.Stages.LLM, "llm", map[string]any{"model": p.Stages.LLM.Model, "system_prompt": systemPrompt}),
		"tts": stage(p.Stages.TTS, "tts", map[string]any{"voice": p.Stages.TTS.Voice}),
	}
}

// provision stores one credential per distinct provider/role the profile needs (a mixed-vendor
// profile therefore gets several), creates the standard agent referencing them per stage and,
// with mcpURL set, attaches the get_time tool. It returns the agent id.
func (s *server) provision(p Profile, env func(string) string, mcpURL string) (string, error) {
	secrets, stageKey, err := credPlan(p, env)
	if err != nil {
		return "", err
	}
	var keys []string
	for k := range secrets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ids := map[string]string{}
	for _, k := range keys {
		theRedactor.add(secrets[k])
		prov, _, _ := strings.Cut(k, "/")
		if ids[k], err = s.id("providers/credentials", map[string]string{"provider": prov, "secret": secrets[k]}); err != nil {
			return "", fmt.Errorf("store %s credential: %w", prov, err)
		}
	}
	cred := map[string]string{}
	for stage, k := range stageKey {
		cred[stage] = ids[k]
	}
	agent, err := s.id("agents", map[string]any{"name": "Live Check", "description": "livecheck " + p.Name, "config": agentConfig(p, cred)})
	if err != nil {
		return "", err
	}
	if mcpURL != "" {
		if err := s.attachTool(agent, mcpURL); err != nil {
			return "", fmt.Errorf("attach %s: %w", toolName, err)
		}
	}
	return agent, nil
}

// attachTool registers the MCP server, discovers its tools and selects get_time for the agent.
func (s *server) attachTool(agent, url string) error {
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
			_, err = s.call("PUT", "agents/"+agent+"/mcp-tools", map[string]any{"tools": []map[string]any{
				{"mcp_server_id": srv, "remote_name": toolName, "schema_hash": t.SchemaHash, "required": true}}})
			return err
		}
	}
	return fmt.Errorf("the MCP server does not list a %s tool", toolName)
}
