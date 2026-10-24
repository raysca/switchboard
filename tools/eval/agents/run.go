package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"govoice/server/agent"
	"govoice/server/api"
	"govoice/tools/internal/stack"
)

const (
	evalPrincipal  = "agent-eval" // the Calls page shows this as the caller
	maxAgentPages  = 50
	agentPageSize  = 100
	sessionEndWait = 15 * time.Second
	rowsSettleWait = 500 * time.Millisecond // tool rows are written by a per-call goroutine
	tracesWait     = 5 * time.Second        // handoff spans arrive through the async trace queue
)

// opts bounds one call.
type opts struct {
	TurnTimeout time.Duration // per typed turn: reply plus settle
	Quiet       time.Duration // no agent activity for this long ends a turn
}

// agentRef is a stored agent resolved for one case: the version that will run, and the
// rewritten (text-only) config the call runs as a draft.
type agentRef struct {
	ID, Name  string
	Version   agent.Version
	Start     string
	StepNames map[string]string // step id -> name
	Draft     json.RawMessage
}

// resolveAgent finds an agent by id or exact name and the version to test (0: the current one),
// and rewrites that version's config for mode.
func resolveAgent(srv *stack.Server, key string, version int, mode string) (agentRef, error) {
	var found []agent.Agent
	for page, more := 0, true; more && page < maxAgentPages; page++ {
		d, err := srv.Call("GET", fmt.Sprintf("agents?limit=%d&offset=%d", agentPageSize, page*agentPageSize), nil)
		if err != nil {
			return agentRef{}, fmt.Errorf("list agents: %w", err)
		}
		var items []agent.Agent
		if err := json.Unmarshal(d, &items); err != nil {
			return agentRef{}, fmt.Errorf("list agents: %w", err)
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
		return agentRef{}, fmt.Errorf("agent %q: no such agent (match by id or exact name)", key)
	case 1:
	default:
		return agentRef{}, fmt.Errorf("agent %q: %d agents match, use the id", key, len(found))
	}
	a := found[0]
	ver := a.Version
	if version > 0 {
		d, err := srv.Call("GET", fmt.Sprintf("agents/%s/versions/%d", a.ID, version), nil)
		if err != nil {
			return agentRef{}, fmt.Errorf("agent %q version %d: %w", key, version, err)
		}
		ver = new(agent.Version)
		if err := json.Unmarshal(d, ver); err != nil {
			return agentRef{}, fmt.Errorf("agent %q version %d: %w", key, version, err)
		}
	}
	if ver == nil {
		return agentRef{}, fmt.Errorf("agent %q has no published version", key)
	}
	def, err := agent.Decode(ver.Config)
	if err != nil {
		return agentRef{}, fmt.Errorf("agent %q: %w", key, err)
	}
	draft, err := Rewrite(ver.Config, mode)
	if err != nil {
		return agentRef{}, fmt.Errorf("agent %q: %w", key, err)
	}
	ref := agentRef{ID: a.ID, Name: a.Name, Version: *ver, Start: def.Start, Draft: draft, StepNames: map[string]string{}}
	for _, s := range def.Steps {
		ref.StepNames[s.ID] = s.Name
	}
	return ref, nil
}

// draftToken mints a voice token for a call on the rewritten draft of the resolved version. The
// version's own tool selection is the draft's, so an older version runs with its own tools.
func draftToken(srv *stack.Server, a agentRef) (string, error) {
	tools := a.Version.MCPTools
	if tools == nil {
		tools = []agent.ToolSelection{}
	}
	d, err := srv.Call("POST", "voice-tokens", api.VoiceTokenRequest{
		Principal: evalPrincipal, AgentID: a.ID, Draft: &api.DraftRequest{Config: a.Draft, MCPTools: &tools}})
	if err != nil {
		return "", fmt.Errorf("voice token: %w", err)
	}
	var t api.VoiceTokenResponse
	if err := json.Unmarshal(d, &t); err != nil || t.Token == "" {
		return "", errors.New("voice token: empty response")
	}
	return t.Token, nil
}

// jti reads the token id (the stored session id) from a JWT's payload. The signature is not
// checked: the server issued the token a moment ago.
func jti(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", errors.New("voice token is not a JWT")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("voice token payload: %w", err)
	}
	var p struct {
		JTI string `json:"jti"`
	}
	if err := json.Unmarshal(raw, &p); err != nil || p.JTI == "" {
		return "", errors.New("voice token has no jti")
	}
	return p.JTI, nil
}

// activity reports whether an event shows the agent working (text, a tool call, a new step's greeting, an error).
func activity(kind string) bool {
	return kind == "audio" || kind == "error" || strings.HasPrefix(kind, "bot-") || strings.HasPrefix(kind, "llm-function-call")
}

// turnState reads the events after mark: when the agent last showed activity, whether it has said
// anything, and whether a reply is still open. A reply is bracketed by bot-llm-started (sent with its
// first text piece, or, for a reply with no text, just before its stop) and bot-llm-stopped, and a slow
// model or tool leaves gaps inside it: it is open from its start to its stop, while a tool call is in
// flight, and after a tool call ends until the reply that follows it (a transfer's greeting included)
// stops. An RTVI error after a tool call ends closes that reply too: a model call that failed sends no
// stop, and the turn must not wait out -turn-timeout for it.
func turnState(ev []stack.Event, mark time.Time) (last time.Time, text, open bool) {
	last = mark
	reply, calls, afterCall := false, 0, false
	for _, e := range ev {
		if !e.At.After(mark) || !activity(e.Kind) {
			continue
		}
		last = e.At
		switch e.Kind {
		case "bot-llm-text":
			text = true
		case "bot-llm-started":
			reply = true
		case "bot-llm-stopped":
			reply, afterCall = false, false
		case "llm-function-call-started":
			calls++
		case "llm-function-call-stopped":
			calls = max(calls-1, 0)
			afterCall = true
		case "error":
			if calls == 0 {
				reply, afterCall = false, false
			}
		}
	}
	return last, text, reply || calls > 0 || afterCall
}

// waitSettled returns once the agent has shown no activity for o.Quiet after mark, has no reply
// open (see turnState) and, when needText, has said something since mark. It returns early (nil)
// when the connection ends, e.g. after end_call, and fails at o.TurnTimeout.
func waitSettled(ctx context.Context, cl *stack.Client, mark time.Time, needText bool, o opts) error {
	deadline := time.Now().Add(o.TurnTimeout)
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-cl.Closed():
			return nil
		case <-tick.C:
		}
		last, text, open := turnState(cl.Snap(), mark)
		if (text || !needText) && !open && time.Since(last) >= o.Quiet {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("no settled reply within %s", o.TurnTimeout)
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// runOnce makes one call: draft token, dial, caller turns, hang-up (unless the agent ended it),
// then the stored session and traces. A harness problem (timeout, API error) comes back as err
// together with whatever was observed.
func runOnce(ctx context.Context, srv *stack.Server, a agentRef, next NextFunc, o opts) (Observed, error) {
	tok, err := draftToken(srv, a)
	if err != nil {
		return Observed{}, err
	}
	id, err := jti(tok)
	if err != nil {
		return Observed{}, err
	}
	cl, err := stack.Dial(ctx, srv.Public, tok, false)
	if err != nil {
		return Observed{}, fmt.Errorf("call %s: %w", id, err)
	}
	defer cl.Close()

	var (
		errs []error
		hist []Line
		sent []Sent
	)
	if err := waitSettled(ctx, cl, cl.T0, false, o); err != nil { // the greeting, if any
		errs = append(errs, fmt.Errorf("greeting: %w", err))
	}
turns:
	for turn := 1; len(errs) == 0; turn++ {
		select {
		case <-cl.Closed(): // the agent hung up
			break turns
		default:
		}
		line, done, err := next(ctx, hist)
		if err != nil {
			errs = append(errs, fmt.Errorf("caller: %w", err))
			break
		}
		if done {
			break
		}
		if turn > maxPersonaTurn {
			errs = append(errs, fmt.Errorf("more than %d caller turns", maxPersonaTurn))
			break
		}
		if _, _, open := turnState(cl.Snap(), time.Time{}); open { // the harness gave up on a slow reply: not the agent's fault
			errs = append(errs, fmt.Errorf("turn %d: reply still open when the next line was sent", turn))
			break
		}
		mark := time.Now()
		sent = append(sent, Sent{At: mark, Text: line})
		if err := cl.SendText(line); err != nil {
			select {
			case <-cl.Closed():
				break turns
			default:
				errs = append(errs, fmt.Errorf("turn %d: %w", turn, err))
			}
			break
		}
		hist = append(hist, Line{"caller", line})
		if err := waitSettled(ctx, cl, mark, true, o); err != nil {
			errs = append(errs, fmt.Errorf("turn %d: %w", turn, err))
			break
		}
		hist = append(hist, Line{"agent", cl.Texts(mark, "bot-llm-text", false)})
	}
	cl.Close() // the caller hangs up; a no-op when the agent already did

	sess, tr, err := fetchCall(ctx, srv, id)
	if err != nil {
		errs = append(errs, err)
	}
	obs := Observe(sess, tr, a.Start, cl.Snap(), sent)
	obs.SessionID, obs.Dropped = id, cl.Dropped()
	return obs, errors.Join(errs...)
}

// fetchCall waits for the stored session to end, lets the asynchronous rows land, and reads the
// session and its traces.
func fetchCall(ctx context.Context, srv *stack.Server, id string) (api.SessionDTO, api.SessionTracesDTO, error) {
	var sess api.SessionDTO
	var tr api.SessionTracesDTO
	ended := false
	for end := time.Now().Add(sessionEndWait); time.Now().Before(end) && !ended; {
		if d, err := srv.Call("GET", "sessions/"+id, nil); err == nil && json.Unmarshal(d, &sess) == nil {
			ended = sess.EndedAt != nil
		}
		if !ended {
			if err := sleepCtx(ctx, 200*time.Millisecond); err != nil {
				return sess, tr, err
			}
		}
	}
	if !ended {
		return sess, tr, fmt.Errorf("session %s did not end within %s", id, sessionEndWait)
	}
	if err := sleepCtx(ctx, rowsSettleWait); err != nil {
		return sess, tr, err
	}
	// Handoff spans go through the asynchronous trace queue and may land after the tool rows: poll
	// until every transfer tool has its span, or give up loudly.
	for end := time.Now().Add(tracesWait); ; {
		d, err := srv.Call("GET", "sessions/"+id+"/traces", nil)
		if err == nil {
			tr = api.SessionTracesDTO{}
			err = json.Unmarshal(d, &tr)
		}
		if err != nil {
			return sess, tr, fmt.Errorf("session %s traces: %w", id, err)
		}
		if tracesComplete(tr) {
			return sess, tr, nil
		}
		if time.Now().After(end) {
			return sess, tr, fmt.Errorf("session %s: traces incomplete: a transfer tool has no handoff span after %s", id, tracesWait)
		}
		if err := sleepCtx(ctx, 200*time.Millisecond); err != nil {
			return sess, tr, err
		}
	}
}

// tracesComplete reports whether every transfer_to_* tool call has its handoff span (active or failed).
func tracesComplete(tr api.SessionTracesDTO) bool {
	transfers, spans := 0, 0
	for _, c := range tr.ToolCalls {
		if strings.HasPrefix(c.RemoteName, "transfer_to_") {
			transfers++
		}
	}
	for _, sp := range tr.Spans {
		if sp.Name == "handoff.active" || sp.Name == "handoff.failed" {
			spans++
		}
	}
	return spans >= transfers
}
