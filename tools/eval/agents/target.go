package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"govoice/server/agent"
	"govoice/server/api"
	"govoice/server/eval"
	"govoice/server/storage"
	"govoice/tools/internal/stack"
)

const (
	maxAgentPages = 50
	agentPageSize = 100
)

// httpTarget runs eval calls against a server reached over its management API and public listener.
type httpTarget struct{ srv *stack.Server }

func (t httpTarget) PublicAddr() string { return t.srv.Public }

// DraftToken mints a voice token for a call on the rewritten draft of the resolved version. The
// version's own tool selection is the draft's, so an older version runs with its own tools.
func (t httpTarget) DraftToken(_ context.Context, a eval.Agent) (string, error) {
	tools := a.Tools
	if tools == nil {
		tools = []agent.ToolSelection{}
	}
	d, err := t.srv.Call("POST", "voice-tokens", api.VoiceTokenRequest{
		Principal: eval.Principal, AgentID: a.ID, Draft: &api.DraftRequest{Config: a.Draft, MCPTools: &tools}})
	if err != nil {
		return "", fmt.Errorf("voice token: %w", err)
	}
	var tok api.VoiceTokenResponse
	if err := json.Unmarshal(d, &tok); err != nil || tok.Token == "" {
		return "", errors.New("voice token: empty response")
	}
	return tok.Token, nil
}

// Stored reads the call's session, turns and traces from the management API.
func (t httpTarget) Stored(_ context.Context, id string) (eval.Stored, error) {
	var (
		sess  api.SessionDTO
		turns []api.TurnDTO
		tr    api.SessionTracesDTO
	)
	for _, g := range []struct {
		path string
		into any
	}{{"sessions/" + id, &sess}, {"sessions/" + id + "/turns", &turns}, {"sessions/" + id + "/traces", &tr}} {
		d, err := t.srv.Call("GET", g.path, nil)
		if err == nil {
			err = json.Unmarshal(d, g.into)
		}
		if err != nil {
			return eval.Stored{}, err
		}
	}
	return toStored(sess, turns, tr), nil
}

// toStored converts the API's shapes back into the storage records the engine reads.
func toStored(sess api.SessionDTO, turns []api.TurnDTO, tr api.SessionTracesDTO) eval.Stored {
	st := eval.Stored{Session: storage.SessionRecord{ID: sess.ID, AgentID: sess.AgentID, AgentVersionID: sess.AgentVersionID,
		ConfigHash: sess.ConfigHash, Status: sess.Status, SafeSummary: sess.SafeSummary, Draft: sess.Draft, Caller: sess.Caller,
		EndReason: sess.EndReason, StartedAt: sess.StartedAt, TurnCount: sess.TurnCount}}
	if sess.EndedAt != nil {
		st.Session.EndedAt = *sess.EndedAt
	}
	end := func(t *time.Time) (z time.Time) {
		if t != nil {
			return *t
		}
		return z
	}
	for _, t := range turns {
		st.Turns = append(st.Turns, storage.TurnRecord{ID: t.ID, SessionID: t.SessionID, Ordinal: t.Ordinal, Transcript: t.Transcript,
			AssistantText: t.AssistantText, StartedAt: t.StartedAt, EndedAt: end(t.EndedAt), Interrupted: t.Interrupted})
	}
	for _, sp := range tr.Spans {
		st.Spans = append(st.Spans, storage.TraceSpan{ID: sp.ID, SessionID: sp.SessionID, TurnID: sp.TurnID, ParentID: sp.ParentID,
			Name: sp.Name, AttributesJSON: string(sp.Attributes), StartedAt: sp.StartedAt, EndedAt: end(sp.EndedAt)})
	}
	for _, c := range tr.ToolCalls {
		st.Tools = append(st.Tools, storage.ToolCallRecord{ID: c.ID, SessionID: c.SessionID, TurnID: c.TurnID, MCPServerID: c.MCPServerID,
			RemoteName: c.RemoteName, SchemaHash: c.SchemaHash, Status: c.Status, ErrorCategory: c.ErrorCategory,
			StartedAt: c.StartedAt, EndedAt: end(c.EndedAt)})
	}
	return st
}

// resolveAgent finds an agent by id or exact name and the version to test (0: the current one),
// and rewrites that version's config for mode.
func resolveAgent(srv *stack.Server, key string, version int, mode string) (eval.Agent, error) {
	var found []agent.Agent
	for page, more := 0, true; more && page < maxAgentPages; page++ {
		d, err := srv.Call("GET", fmt.Sprintf("agents?limit=%d&offset=%d", agentPageSize, page*agentPageSize), nil)
		if err != nil {
			return eval.Agent{}, fmt.Errorf("list agents: %w", err)
		}
		var items []agent.Agent
		if err := json.Unmarshal(d, &items); err != nil {
			return eval.Agent{}, fmt.Errorf("list agents: %w", err)
		}
		for _, a := range items {
			if !a.Archived && (a.ID == key || a.Name == key) {
				found = append(found, a)
			}
		}
		more = len(items) == agentPageSize
	}
	switch len(found) {
	case 0:
		return eval.Agent{}, fmt.Errorf("agent %q: no such agent (match by id or exact name)", key)
	case 1:
	default:
		return eval.Agent{}, fmt.Errorf("agent %q: %d agents match, use the id", key, len(found))
	}
	a := found[0]
	ver := a.Version
	if version > 0 {
		d, err := srv.Call("GET", fmt.Sprintf("agents/%s/versions/%d", a.ID, version), nil)
		if err != nil {
			return eval.Agent{}, fmt.Errorf("agent %q version %d: %w", key, version, err)
		}
		ver = new(agent.Version)
		if err := json.Unmarshal(d, ver); err != nil {
			return eval.Agent{}, fmt.Errorf("agent %q version %d: %w", key, version, err)
		}
	}
	if ver == nil {
		return eval.Agent{}, fmt.Errorf("agent %q has no published version", key)
	}
	def, err := agent.Decode(ver.Config)
	if err != nil {
		return eval.Agent{}, fmt.Errorf("agent %q: %w", key, err)
	}
	draft, err := eval.Rewrite(ver.Config, mode)
	if err != nil {
		return eval.Agent{}, fmt.Errorf("agent %q: %w", key, err)
	}
	ref := eval.Agent{ID: a.ID, Name: a.Name, VersionID: ver.ID, Version: ver.Number, Start: def.Start, Draft: draft, Tools: ver.MCPTools,
		StepNames: map[string]string{}, Prompts: map[string]string{}}
	for _, s := range def.Steps {
		ref.StepNames[s.ID] = s.Name
		ref.Prompts[s.ID] = s.Config.LLM.SystemPrompt
	}
	return ref, nil
}
