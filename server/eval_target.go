package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"govoice/core"
	"govoice/server/agent"
	"govoice/server/api"
	"govoice/server/catalog"
	"govoice/server/eval"
)

// errVoiceToken marks a failure to issue the draft token: the worker records it as category token.
var errVoiceToken = errors.New("voice token")

// localTarget runs eval calls against this server itself: the draft token is issued in process (no
// HTTP hop, no management key), the call dials this instance's internal eval listener, and the stored
// rows are read straight from the store.
type localTarget struct{ s *Server }

// DraftToken issues a draft token for the rewritten config of the resolved version. The version's own
// tool selection is the draft's, so an older version runs with its own tools.
func (t localTarget) DraftToken(ctx context.Context, a eval.Agent) (string, error) {
	tools := a.Tools
	if tools == nil {
		tools = []agent.ToolSelection{}
	}
	resp, _, err := t.s.issueVoiceToken(ctx, api.VoiceTokenRequest{
		Principal: eval.Principal, AgentID: a.ID, Draft: &api.DraftRequest{Config: a.Draft, MCPTools: &tools}}, true)
	if err != nil {
		return "", fmt.Errorf("%w: %w", errVoiceToken, err)
	}
	return resp.Token, nil
}

// PublicAddr is the internal loopback listener (eval_listener.go), not the public one: the public /ws
// applies browser rules a server-side client has no way to meet.
func (t localTarget) PublicAddr() string { return t.s.evalAddr }

// Stored reads the call's session, turns, spans and tool calls from the store.
func (t localTarget) Stored(ctx context.Context, id string) (eval.Stored, error) {
	if t.s.store == nil {
		return eval.Stored{}, errors.New("stored rows: no store")
	}
	var (
		st  eval.Stored
		err error
	)
	if st.Session, err = t.s.store.Sessions().Get(ctx, id); err != nil {
		return eval.Stored{}, fmt.Errorf("session %s: %w", id, err)
	}
	if st.Turns, err = t.s.store.Sessions().Turns(ctx, id); err != nil {
		return eval.Stored{}, fmt.Errorf("turns of %s: %w", id, err)
	}
	if st.Spans, err = t.s.store.Traces().Spans(ctx, id, ""); err != nil {
		return eval.Stored{}, fmt.Errorf("spans of %s: %w", id, err)
	}
	if st.Tools, err = t.s.store.Traces().ToolCalls(ctx, id, ""); err != nil {
		return eval.Stored{}, fmt.Errorf("tool calls of %s: %w", id, err)
	}
	return st, nil
}

// completer builds one plain-text completion function on a configured LLM provider, for the judge
// and the persona caller. An empty model is the catalog default. The key comes from the environment
// only; every error text is redacted of it. The returned func releases the processor.
func (s *Server) completer(ctx context.Context, provider, model string) (eval.AskFunc, func(), error) {
	p, ok := s.catalog.Provider(provider)
	if !ok || p.Services[catalog.ServiceLLM] == nil {
		return nil, nil, fmt.Errorf("provider %q has no llm service", provider)
	}
	svc := p.Services[catalog.ServiceLLM]
	if model == "" {
		e, found := catalog.DefaultEntry(svc.Models)
		if !found {
			return nil, nil, fmt.Errorf("provider %q has no default llm model", provider)
		}
		model = e.ID
	}
	secret, err := s.resolver.Secret(provider, catalog.ServiceLLM)
	if err != nil {
		return nil, nil, fmt.Errorf("provider %q credential: %w", provider, err)
	}
	defer clear(secret)
	literals := secretLiterals(secret)
	redact := func(m string) string {
		for _, l := range literals {
			m = strings.ReplaceAll(m, l, "[redacted]")
		}
		return s.redactor.Redact(m)
	}
	proc, err := s.registry.BuildStage(s.cfg, catalog.ServiceLLM, StageSpec{Provider: provider, Model: model, Secret: secret})
	if err != nil {
		return nil, nil, errors.New(redact(err.Error()))
	}
	closeProc := func() {
		if c, ok := proc.(core.Closer); ok {
			_ = c.Close()
		}
	}
	c, ok := proc.(textCompleter)
	if !ok {
		closeProc()
		return nil, nil, fmt.Errorf("provider %q cannot complete plain text", provider)
	}
	ask := func(ctx context.Context, system, user string) (string, error) {
		out, err := c.SummarizeText(ctx, system, user)
		if err != nil {
			return "", errors.New(redact(err.Error()))
		}
		return out, nil
	}
	return ask, closeProc, nil
}

// secretLiterals are the exact credential strings an error text must not carry: a bare key, or each
// value of a JSON credential ({"api_key": ..., "base_url": ...}).
func secretLiterals(secret []byte) []string {
	trimmed := strings.TrimSpace(string(secret))
	var fields map[string]string
	if !strings.HasPrefix(trimmed, "{") || json.Unmarshal([]byte(trimmed), &fields) != nil {
		if trimmed == "" { // a keyless provider: replacing "" would corrupt every message
			return nil
		}
		return []string{trimmed}
	}
	out := make([]string, 0, len(fields))
	for _, v := range fields {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}
