package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"govoice/core"
	"govoice/server/agent"
	"govoice/server/runtime"
	"govoice/server/storage"
)

var errAgentsUnavailable = errors.New("agent storage is not enabled")

// releaseTimeout bounds ending a call's agent session (MCP drain + DB write).
const releaseTimeout = 5 * time.Second

// resolveTimeout bounds pinning an agent at connect (MCP capture + DB writes).
const resolveTimeout = 10 * time.Second

// resolveCall pins the token's agent for a new call: it captures the agent's
// MCP tools, persists the session row, and resolves each stage's provider and
// secret. release ends the persisted session and must run exactly once.
// The session row takes the token's single-use jti as its id, so a client that
// holds the token can find its session (the admin voice test links to it).
// resolveCall pins the token's agent, records the session (caller is the token's principal) and returns the
// call's spec and the function that ends it. ending is the call's end-reason holder: release records what it holds.
func (s *Server) resolveCall(ctx context.Context, agentID, tokenID, caller string, ending *endReason) (CallSpec, func(), error) {
	if s.agents == nil || s.mcpService == nil || s.resolver == nil || s.store == nil {
		return CallSpec{}, nil, errAgentsUnavailable
	}
	if agentID == "" {
		return CallSpec{}, nil, errors.New("token does not name an agent")
	}
	sessionID := tokenID
	if !storage.IsID(sessionID) { // legacy tokens carry a random UUIDv4 or no jti
		sessionID = storage.NewID()
	}
	var rs *runtime.Session
	var err error
	if draft, found := s.drafts.take(sessionID); found {
		// The agent may have been archived, disabled or left unready since the token was issued.
		if _, err = s.agents.ResolveCurrent(ctx, agentID); err == nil {
			rs, err = runtime.StartSnapshotSession(ctx, s.agents, s.mcpService, draft.snapshot, sessionID, caller)
		}
	} else {
		rs, err = runtime.StartSession(ctx, s.agents, s.mcpService, agentID, sessionID, caller)
	}
	if err != nil {
		return CallSpec{}, nil, fmt.Errorf("start agent session: %w", err)
	}
	rs.EndReason = ending.get
	lat := newTurnLatencies()
	var history *turnRecorder // set below; release runs for a panic before then too
	release := func() {
		// In-flight MCP tool calls are waited for, but never past the session
		// teardown budget: a stuck remote tool must not pin the call forever.
		ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
		defer cancel()
		// Turn rows land before the session is ended, on their own budget so a
		// slow drain cannot leave the session row active.
		if history != nil {
			historyCtx, cancelHistory := context.WithTimeout(context.Background(), 2*time.Second)
			history.close(historyCtx)
			cancelHistory()
		}
		if err := rs.Close(ctx); err != nil {
			s.logger.Warn("agent session did not end cleanly", "session", rs.Pin.ID, "err", s.redactor.Redact(err.Error()))
		}
	}
	handedBack := false
	defer func() { // an error or a panic before the call is handed back ends the row and drops the pins
		if !handedBack {
			ending.set(storage.EndSetupFailed)
			release()
		}
	}()
	history = newTurnRecorder(func(ctx context.Context, row storage.TurnRecord) error {
		s.metrics.TurnsTotal.Add(1)
		row = s.redactTurn(row)
		return s.redactErr(s.store.Sessions().AddTurn(ctx, row))
	}, func(ctx context.Context, rec storage.ToolCallRecord) error {
		return s.redactErr(s.store.Traces().AddToolCall(ctx, rec))
	}, rs.MCPTool, rs.Pin.ID, s.logger, lat)
	call, err := s.callSpec(ctx, rs)
	if err != nil {
		return CallSpec{}, nil, err
	}
	call.History = history
	call.TurnLatencies = lat
	if s.adjustCall != nil {
		s.adjustCall(&call)
	}
	handedBack = true
	return call, release, nil
}

// redactTurn masks sensitive content in a row headed for storage. The live LLM
// context is never touched.
func (s *Server) redactTurn(row storage.TurnRecord) storage.TurnRecord {
	row.Transcript, row.AssistantText = s.redactor.Redact(row.Transcript), s.redactor.Redact(row.AssistantText)
	return row
}

// turnRecorder stores one turn row per assistant reply, complete or
// interrupted (a reply cut off before any of it was heard keeps a row with no
// reply text), pairing it with the user message that started it, and one
// tool_calls row per tool call the loop announced: its name, status and
// timing, never its arguments or result. Rows are written by one goroutine per
// call so the session run loop never waits on the database; a turn row is
// written before the tool calls that reference it.
type turnRecorder struct {
	mu       sync.Mutex // the run loop and close; never held across a write
	users    map[string]*core.UserMessageFrame
	calls    map[string]pendingTool              // announced calls by tool call id, until their result
	finished map[string][]storage.ToolCallRecord // settled calls by turn, until the turn's row is queued
	rowIDs   map[string]string                   // turn -> its queued row id, for calls settled after it
	ordinal  int
	closed   bool
	rows     chan turnWrite
	done     chan struct{}
	lat      *turnLatencies
	mcpTool  func(name string) (agent.MCPToolSnapshot, bool)
}

// pendingTool is an announced call waiting for its result.
type pendingTool struct {
	turn string
	rec  storage.ToolCallRecord
}

// turnWrite is one ordered write: a turn row (nil for none), then the tool
// calls that belong with it.
type turnWrite struct {
	turn  *storage.TurnRecord
	tools []storage.ToolCallRecord
}

// The recorder forgets what it can no longer pair. A tool round runs at most 16
// calls and a reply settles its turn, so these bounds are reached only when
// results or replies never come.
const (
	maxRecordedTurns = 32
	maxPendingTools  = 64
)

// redactErr masks PII in a store error (it can echo a failing row's values)
// before the turn recorder logs it.
func (s *Server) redactErr(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(s.redactor.Redact(err.Error()))
}

func newTurnRecorder(addTurn func(context.Context, storage.TurnRecord) error, addToolCall func(context.Context, storage.ToolCallRecord) error,
	mcpTool func(name string) (agent.MCPToolSnapshot, bool), sessionID string, logger *slog.Logger, lat *turnLatencies) *turnRecorder {
	r := &turnRecorder{lat: lat, mcpTool: mcpTool, users: map[string]*core.UserMessageFrame{}, calls: map[string]pendingTool{},
		finished: map[string][]storage.ToolCallRecord{}, rowIDs: map[string]string{}, rows: make(chan turnWrite, 64), done: make(chan struct{})}
	go func() {
		defer close(r.done)
		for w := range r.rows {
			func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				defer func() { // a store bug loses this row, not the process or the rows behind it
					if recover() != nil {
						logger.Error("turn recorder panicked", "session", sessionID)
					}
				}()
				stored := true
				if w.turn != nil {
					w.turn.SessionID = sessionID
					if err := addTurn(ctx, *w.turn); err != nil {
						stored = false
						logger.Warn("turn not recorded", "session", sessionID, "err", err.Error())
					}
				}
				for _, tc := range w.tools {
					tc.SessionID = sessionID
					if !stored {
						tc.TurnID = "" // its turn row is missing: keep the call, unattached
					}
					if addToolCall == nil {
						continue
					}
					if err := addToolCall(ctx, tc); err != nil {
						logger.Warn("tool call not recorded", "session", sessionID, "err", err.Error())
					}
				}
			}()
		}
	}()
	return r
}

func (r *turnRecorder) ProcessFrame(_ context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	switch f.(type) {
	case *core.UserMessageFrame, *core.ToolCallFrame, *core.ToolResultFrame, *core.AssistantMessageFrame, *core.InterruptionFrame:
	default: // audio and the rest pass without the lock
		return e.PushDirection(f, dir)
	}
	r.mu.Lock()
	switch m := f.(type) {
	case *core.UserMessageFrame:
		if len(r.users) >= maxRecordedTurns { // ponytail: replies never came for these; forget them
			clear(r.users)
		}
		r.users[m.Turn] = m
	case *core.ToolCallFrame:
		if len(r.calls) >= maxPendingTools { // ponytail: results never came for these; forget them
			clear(r.calls)
		}
		r.calls[m.Call.ID] = pendingTool{turn: string(m.Call.TurnID), rec: r.toolRecord(m.Call.Name, m.Timestamp)}
	case *core.ToolResultFrame:
		r.settleLocked(m.Result)
	case *core.AssistantMessageFrame:
		r.recordTurnLocked(m.Turn, m.Timestamp, m.Content, m.Partial)
	case *core.InterruptionFrame:
		// A reply cut off before any of it was heard emits no assistant message (nothing to put in the
		// context), but the turn happened: it keeps its row, with no reply text.
		// ponytail: the session fence drops this frame when a second interruption bumped the generation
		// first; that turn then has no row (its entry leaves with the maxRecordedTurns clear).
		if _, open := r.users[m.Turn]; open && m.Turn != "" {
			r.recordTurnLocked(m.Turn, time.Now(), "", true)
		}
	}
	r.mu.Unlock()
	return e.PushDirection(f, dir)
}

// recordTurnLocked queues turn's row, pairing it with the user message that started it and the tool
// calls already settled for it.
func (r *turnRecorder) recordTurnLocked(turn string, at time.Time, reply string, interrupted bool) {
	user := r.users[turn]
	delete(r.users, turn)
	row := storage.TurnRecord{ID: storage.NewID(), Ordinal: r.ordinal + 1, StartedAt: at, EndedAt: at, AssistantText: reply, Interrupted: interrupted, LatenciesJSON: r.lat.take(turn)}
	if user != nil {
		row.Transcript, row.StartedAt = user.Content, user.Timestamp
	}
	tools := r.finished[turn]
	delete(r.finished, turn)
	for i := range tools {
		tools[i].TurnID = row.ID
	}
	if r.sendLocked(turnWrite{turn: &row, tools: tools}) {
		r.ordinal++
		if len(r.rowIDs) >= maxRecordedTurns {
			clear(r.rowIDs)
		}
		r.rowIDs[turn] = row.ID
	}
}

// toolRecord starts a call's row: an MCP tool by its server, remote name and
// schema hash, a built-in tool (a handoff's transfer, end_call) by its own name.
// It reads cancelled until a result settles it.
func (r *turnRecorder) toolRecord(name string, at time.Time) storage.ToolCallRecord {
	rec := storage.ToolCallRecord{ID: storage.NewID(), RemoteName: name, StartedAt: at, Status: string(core.ToolCancelled)}
	if r.mcpTool != nil {
		if t, ok := r.mcpTool(name); ok {
			rec.MCPServerID, rec.RemoteName, rec.SchemaHash = t.ServerID, t.RemoteName, t.SchemaHash
		}
	}
	return rec
}

// settleLocked completes an announced call's row with its result; a result for
// a call that was never announced (a stale generation) is ignored.
func (r *turnRecorder) settleLocked(res core.ToolResult) {
	p, ok := r.calls[res.CallID]
	if !ok {
		return
	}
	delete(r.calls, res.CallID)
	p.rec.Status, p.rec.EndedAt, p.rec.ErrorCategory = string(res.Status), res.EndedAt, res.ErrorCategory
	if c := res.ErrorCategory; (c == "unknown_tool" || c == "invalid_tool_name") && p.rec.RemoteName == "unknown_tool" && p.rec.MCPServerID == "" {
		// The loop announces a name it has no definition for as "unknown_tool" (the model wrote it and it can
		// hold the caller's words; remote_name is never purged). A defined tool whose executor itself returns
		// unknown_tool keeps its own name.
		p.rec.RemoteName, p.rec.MCPServerID, p.rec.SchemaHash = c, "", ""
	}
	if !res.StartedAt.IsZero() {
		p.rec.StartedAt = res.StartedAt
	}
	if id, ok := r.rowIDs[p.turn]; ok { // settled after its turn's row: an async tool, a call cut off by barge-in
		p.rec.TurnID = id
		r.sendLocked(turnWrite{tools: []storage.ToolCallRecord{p.rec}})
		return
	}
	if _, ok := r.finished[p.turn]; !ok && len(r.finished) >= maxRecordedTurns {
		r.flushWaitingLocked() // ponytail: replies never came for these turns
	}
	r.finished[p.turn] = append(r.finished[p.turn], p.rec)
}

// flushWaitingLocked writes the settled calls still waiting for a turn row, as
// they are (unattached unless their turn's row id is known).
func (r *turnRecorder) flushWaitingLocked() {
	var rest []storage.ToolCallRecord
	for _, tools := range r.finished {
		rest = append(rest, tools...)
	}
	clear(r.finished)
	if len(rest) > 0 {
		r.sendLocked(turnWrite{tools: rest})
	}
}

// sendLocked queues w without waiting: history is best effort and never stalls the call on the database.
func (r *turnRecorder) sendLocked(w turnWrite) bool {
	if r.closed {
		return false
	}
	select {
	case r.rows <- w:
		return true
	default:
		return false
	}
}

// close records the calls still running (cancelled), stops accepting rows and
// waits (bounded by ctx) for queued ones.
func (r *turnRecorder) close(ctx context.Context) {
	r.mu.Lock()
	if !r.closed {
		for _, p := range r.calls {
			if id, ok := r.rowIDs[p.turn]; ok {
				p.rec.TurnID = id
			}
			r.finished[p.turn] = append(r.finished[p.turn], p.rec)
		}
		clear(r.calls)
		r.flushWaitingLocked()
		r.closed = true
		close(r.rows)
	}
	r.mu.Unlock()
	select {
	case <-r.done:
	case <-ctx.Done():
	}
}

func (s *Server) callSpec(ctx context.Context, rs *runtime.Session) (CallSpec, error) {
	g, err := agent.Decode(rs.Snapshot.Version.Config)
	if err != nil {
		return CallSpec{}, err
	}
	entry := g.StartStep()
	cfg := entry.Config
	if missing := cfg.Missing(); len(missing) > 0 {
		return CallSpec{}, fmt.Errorf("agent config incomplete: %v", missing)
	}
	secrets, err := s.resolveStageSecrets(ctx, cfg, "stt", "llm", "tts")
	if err != nil {
		return CallSpec{}, err
	}
	maxMessages, maxTokens := cfg.Context.Limits()
	maxParallel := s.maxParallel(cfg)
	// A one-step agent is a plain agent: every captured tool, no handoffs.
	tools, exec := rs.Definitions, rs.Executor
	var graph *CallAgent
	var ender *callEnder
	if anyEndCall(g) {
		ender = &callEnder{}
	}
	if len(g.Steps) > 1 {
		graph = &CallAgent{Start: g.Start, Cue: g.HandoffCue, Backgrounds: stepBackgrounds(g)}
		graph.Resolve = func(ctx context.Context, id string) (StepSpec, error) {
			return s.stepSpec(ctx, rs, g, graph, ender, id)
		}
		tools, exec = s.stepToolset(rs, g, entry, graph, ender)
	} else if entry.EndCall {
		// One step with end_call: every captured tool plus the hang-up tool, no router.
		allowed := make(map[string]bool, len(tools))
		for _, d := range tools {
			allowed[d.Name] = true
		}
		tools = append(tools[:len(tools):len(tools)], endCallDefinition())
		exec = stepExecutor(rs.Executor, allowed, nil, nil, ender.hangUp(entry.ID))
	}
	fallbacks := s.resolveFallbacks(ctx, rs, "llm", cfg, &s.metrics.LLMFallbacksSkippedTotal, func(c agent.Candidate, secret []byte) StageSpec {
		return StageSpec{Provider: c.Provider, Model: c.Model, SystemPrompt: cfg.LLM.SystemPrompt, Sampling: cfg.LLM.LLMSampling, Secret: secret, Tools: tools, ToolExecutor: exec, MaxParallel: maxParallel}
	})
	ttsFallbacks := s.resolveFallbacks(ctx, rs, "tts", cfg, &s.metrics.TTSFallbacksSkippedTotal, func(c agent.Candidate, secret []byte) StageSpec {
		return StageSpec{Provider: c.Provider, Voice: c.Voice, Secret: secret}
	})
	var disableTextCleaning bool
	if cfg.TTS.CleanText != nil && !*cfg.TTS.CleanText {
		disableTextCleaning = true
	}
	return CallSpec{
		LLMFallbacks: fallbacks, TTSFallbacks: ttsFallbacks, Failover: cfg.Failover,
		STTFallbacks: s.sttFallbacks(ctx, rs, cfg),
		STT:          StageSpec{Provider: cfg.STT.Provider, Model: cfg.STT.Model, Language: cfg.STT.Language, Secret: secrets["stt"]},
		LLM: StageSpec{Provider: cfg.LLM.Provider, Model: cfg.LLM.Model, SystemPrompt: cfg.LLM.SystemPrompt, Sampling: cfg.LLM.LLMSampling, Secret: secrets["llm"],
			Tools: tools, ToolExecutor: exec, MaxParallel: maxParallel},
		TTS:                 StageSpec{Provider: cfg.TTS.Provider, Voice: cfg.TTS.Voice, VoiceSettings: cfg.TTS.VoiceSettings, Secret: secrets["tts"]},
		DisableTextCleaning: disableTextCleaning,
		MaxMessages:         maxMessages,
		MaxTokens:           maxTokens,
		Summary:             cfg.Context.Summarization,
		Greeting:            cfg.GreetingText(),
		AgentVersionID:      rs.Snapshot.Version.ID,
		StoredSessionID:     rs.Pin.ID,
		Recording:           cfg.Recording.Active(),
		VADStopMS:           cfg.VADStopMS,
		Idle:                cfg.Idle,
		Turn:                cfg.Turn,
		Audio:               cfg.Audio,
		STTP99MS:            cfg.STTP99MS,
		Agent:               graph,
		Ender:               ender,
		MaxCallDuration:     callLimit(s.cfg.Limits.MaxCallDuration, cfg.MaxCallDurationSeconds),
	}, nil
}

// stepBackgrounds is the background each step of g names, by step id.
func stepBackgrounds(g agent.Definition) map[string]*agent.BackgroundSettings {
	var out map[string]*agent.BackgroundSettings
	for _, n := range g.Steps {
		if a := n.Config.Audio; a != nil && a.Background != nil {
			if out == nil {
				out = map[string]*agent.BackgroundSettings{}
			}
			out[n.ID] = a.Background
		}
	}
	return out
}

func (s *Server) maxParallel(cfg agent.Config) int {
	if mp := cfg.LLM.MaxParallel; mp > 0 {
		return mp
	}
	return max(s.cfg.Tools.MaxParallel, 0)
}

// resolveStageSecrets reads the secret of each named stage from the
// environment, now. On error the secrets gathered so far are wiped.
func (s *Server) resolveStageSecrets(ctx context.Context, cfg agent.Config, names ...string) (map[string][]byte, error) {
	secrets := map[string][]byte{}
	ok := false
	defer func() {
		if !ok {
			for _, b := range secrets {
				clear(b)
			}
		}
	}()
	for _, stage := range cfg.Stages() {
		if !slices.Contains(names, stage.Name) {
			continue
		}
		if !s.registry.Supports(stage.Provider, stage.Name) {
			return nil, fmt.Errorf("%s provider %q is not available on this server", stage.Name, stage.Provider)
		}
		secret, err := s.resolver.Secret(stage.Provider, stage.Name)
		if err != nil {
			return nil, fmt.Errorf("agent not callable: %s credential: %w", stage.Name, err)
		}
		secrets[stage.Name] = secret
	}
	ok = true
	return secrets, nil
}

// stepToolset is the tools and executor of one step: its listed MCP tools plus
// a transfer tool per outgoing handoff.
func (s *Server) stepToolset(rs *runtime.Session, g agent.Definition, n agent.Step, cg *CallAgent, ender *callEnder) ([]core.ToolDefinition, core.ToolExecutor) {
	mcp := stepDefinitions(rs.Definitions, n.Tools)
	allowed := make(map[string]bool, len(mcp))
	for _, d := range mcp {
		allowed[d.Name] = true
	}
	defs := append(mcp[:len(mcp):len(mcp)], transferTools(g, n.ID)...)
	var end func() error
	if n.EndCall {
		defs = append(defs, endCallDefinition())
		end = ender.hangUp(n.ID)
	}
	return defs, stepExecutor(rs.Executor, allowed, transferTargets(g, n.ID), func(target, note string) error {
		return cg.commitWithNote(target, note)
	}, end)
}

// stepSpec resolves a non-start step's LLM and TTS when the step is activated:
// secrets are read from the environment now, not held for the whole call.
func (s *Server) stepSpec(ctx context.Context, rs *runtime.Session, g agent.Definition, cg *CallAgent, ender *callEnder, id string) (StepSpec, error) {
	n, ok := g.Step(id)
	if !ok {
		return StepSpec{}, fmt.Errorf("unknown step %q", id)
	}
	cfg := n.Config
	secrets, err := s.resolveStageSecrets(ctx, cfg, "llm", "tts")
	if err != nil {
		return StepSpec{}, err
	}
	tools, exec := s.stepToolset(rs, g, n, cg, ender)
	maxParallel := s.maxParallel(cfg)
	return StepSpec{
		Name: n.Name, Greeting: cfg.GreetingText(),
		LLM: StageSpec{Provider: cfg.LLM.Provider, Model: cfg.LLM.Model, SystemPrompt: cfg.LLM.SystemPrompt, Sampling: cfg.LLM.LLMSampling, Secret: secrets["llm"],
			Tools: tools, ToolExecutor: exec, MaxParallel: maxParallel},
		LLMFallbacks: s.resolveFallbacks(ctx, rs, "llm", cfg, &s.metrics.LLMFallbacksSkippedTotal, func(c agent.Candidate, secret []byte) StageSpec {
			return StageSpec{Provider: c.Provider, Model: c.Model, SystemPrompt: cfg.LLM.SystemPrompt, Sampling: cfg.LLM.LLMSampling, Secret: secret, Tools: tools, ToolExecutor: exec, MaxParallel: maxParallel}
		}),
		TTS: StageSpec{Provider: cfg.TTS.Provider, Voice: cfg.TTS.Voice, VoiceSettings: cfg.TTS.VoiceSettings, Secret: secrets["tts"]},
		TTSFallbacks: s.resolveFallbacks(ctx, rs, "tts", cfg, &s.metrics.TTSFallbacksSkippedTotal, func(c agent.Candidate, secret []byte) StageSpec {
			return StageSpec{Provider: c.Provider, Voice: c.Voice, Secret: secret}
		}),
	}, nil
}

// resolveFallbacks resolves the chain after the primary for one stage. A
// fallback that cannot be resolved now is skipped (logged and counted), not fatal.
func (s *Server) resolveFallbacks(ctx context.Context, rs *runtime.Session, stage string, cfg agent.Config, skipped *atomic.Uint64, build func(agent.Candidate, []byte) StageSpec) []StageSpec {
	var out []StageSpec
	for _, cand := range cfg.Chain(stage)[1:] {
		skip := func(why string) {
			skipped.Add(1)
			s.logger.Warn(stage+" fallback skipped", "session", rs.Pin.ID, "provider", cand.Provider, "reason", why)
		}
		if !s.registry.Supports(cand.Provider, stage) {
			skip("provider not available")
			continue
		}
		secret, err := s.resolver.Secret(cand.Provider, stage)
		if err != nil {
			skip("credential unavailable")
			continue
		}
		out = append(out, build(cand, secret))
	}
	return out
}

// wipeSecrets zeroes the secrets once provider clients are built;
// adapters copy what they need (API key strings, parsed service accounts).
func (c *CallSpec) wipeSecrets() {
	for _, b := range [][]byte{c.STT.Secret, c.LLM.Secret, c.TTS.Secret} {
		clear(b)
	}
	for _, f := range c.LLMFallbacks {
		clear(f.Secret)
	}
	for _, f := range c.TTSFallbacks {
		clear(f.Secret)
	}
	for _, f := range c.STTFallbacks {
		clear(f.Secret)
	}
	c.LLMFallbacks, c.TTSFallbacks, c.STTFallbacks = nil, nil, nil
	c.STT.Secret, c.LLM.Secret, c.TTS.Secret = nil, nil, nil
}

// callLimit is the shorter of the server's limit and the agent's (0 = the agent sets none).
func callLimit(server time.Duration, agentSeconds int) time.Duration {
	if agentSeconds > 0 {
		return min(server, time.Duration(agentSeconds)*time.Second)
	}
	return server
}
