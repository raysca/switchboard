package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync/atomic"
	"time"

	"govoice/core"
	"govoice/server/flow"
	"govoice/server/runtime"
	"govoice/server/storage"
)

var errFlowsUnavailable = errors.New("flow storage is not enabled")

// releaseTimeout bounds ending a call's flow session (MCP drain + DB write).
const releaseTimeout = 5 * time.Second

// resolveTimeout bounds pinning a flow at connect (MCP capture + DB writes).
const resolveTimeout = 10 * time.Second

// resolveCall pins the token's flow for a new call: it captures the flow's
// MCP tools, persists the session row, and resolves each stage's provider and
// secret. release ends the persisted session and must run exactly once.
// The session row takes the token's single-use jti as its id, so a client that
// holds the token can find its session (the admin voice test links to it).
func (s *Server) resolveCall(ctx context.Context, flowID, tokenID string) (CallSpec, func(), error) {
	if s.flows == nil || s.mcpService == nil || s.resolver == nil || s.store == nil {
		return CallSpec{}, nil, errFlowsUnavailable
	}
	if flowID == "" {
		return CallSpec{}, nil, errors.New("token does not name a flow")
	}
	sessionID := tokenID
	if !storage.IsID(sessionID) { // legacy tokens carry a random UUIDv4 or no jti
		sessionID = storage.NewID()
	}
	rs, err := runtime.StartSession(ctx, s.flows, s.mcpService, flowID, sessionID)
	if err != nil {
		return CallSpec{}, nil, fmt.Errorf("start flow session: %w", err)
	}
	history := newTurnRecorder(func(ctx context.Context, row storage.TurnRecord) error {
		s.metrics.TurnsTotal.Add(1)
		row = s.redactTurn(row)
		return s.store.Sessions().AddTurn(ctx, row)
	}, rs.Pin.ID, s.logger)
	release := func() {
		// In-flight MCP tool calls are waited for, but never past the session
		// teardown budget: a stuck remote tool must not pin the call forever.
		ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
		defer cancel()
		// Turn rows land before the session is ended, on their own budget so a
		// slow drain cannot leave the session row active.
		historyCtx, cancelHistory := context.WithTimeout(context.Background(), 2*time.Second)
		history.close(historyCtx)
		cancelHistory()
		if err := rs.Close(ctx); err != nil {
			s.logger.Warn("flow session did not end cleanly", "session", rs.Pin.ID, "err", s.redactor.Redact(err.Error()))
		}
	}
	call, err := s.callSpec(ctx, rs)
	if err != nil {
		release()
		return CallSpec{}, nil, err
	}
	call.History = history
	return call, release, nil
}

// redactTurn masks sensitive content in a row headed for storage. The live LLM
// context is never touched.
func (s *Server) redactTurn(row storage.TurnRecord) storage.TurnRecord {
	row.Transcript, row.AssistantText = s.redactor.Redact(row.Transcript), s.redactor.Redact(row.AssistantText)
	return row
}

// turnRecorder stores one turn row per assistant reply, complete or
// interrupted, pairing it with the user message that started it. Rows are
// written by one goroutine per call so the session run loop never waits on
// the database.
type turnRecorder struct {
	users   map[string]*core.UserMessageFrame // run-loop only
	ordinal int
	rows    chan storage.TurnRecord
	done    chan struct{}
}

func newTurnRecorder(addTurn func(context.Context, storage.TurnRecord) error, sessionID string, logger *slog.Logger) *turnRecorder {
	r := &turnRecorder{users: map[string]*core.UserMessageFrame{}, rows: make(chan storage.TurnRecord, 64), done: make(chan struct{})}
	go func() {
		defer close(r.done)
		for row := range r.rows {
			row.SessionID = sessionID
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := addTurn(ctx, row); err != nil {
				logger.Warn("turn not recorded", "session", sessionID, "err", err.Error())
			}
			cancel()
		}
	}()
	return r
}

func (r *turnRecorder) ProcessFrame(_ context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	switch m := f.(type) {
	case *core.UserMessageFrame:
		if len(r.users) >= 32 { // ponytail: replies never came for these; forget them
			clear(r.users)
		}
		r.users[m.Turn] = m
	case *core.AssistantMessageFrame:
		user := r.users[m.Turn]
		delete(r.users, m.Turn)
		row := storage.TurnRecord{ID: storage.NewID(), Ordinal: r.ordinal + 1, StartedAt: m.Timestamp, EndedAt: m.Timestamp, AssistantText: m.Content, Interrupted: m.Partial}
		if user != nil {
			row.Transcript, row.StartedAt = user.Content, user.Timestamp
		}
		select {
		case r.rows <- row:
			r.ordinal++
		default: // ponytail: history is best effort; never stall the call on the DB
		}
	}
	return e.PushDirection(f, dir)
}

// close stops accepting rows and waits (bounded by ctx) for queued ones.
func (r *turnRecorder) close(ctx context.Context) {
	close(r.rows)
	select {
	case <-r.done:
	case <-ctx.Done():
	}
}

func (s *Server) callSpec(ctx context.Context, rs *runtime.Session) (CallSpec, error) {
	g, err := flow.Decode(rs.Snapshot.Version.Config)
	if err != nil {
		return CallSpec{}, err
	}
	entry := g.StartStep()
	cfg := entry.Config
	if missing := cfg.Missing(); len(missing) > 0 {
		return CallSpec{}, fmt.Errorf("flow config incomplete: %v", missing)
	}
	secrets, err := s.resolveStageSecrets(ctx, cfg, "stt", "llm", "tts")
	if err != nil {
		return CallSpec{}, err
	}
	maxMessages, maxTokens := cfg.Context.Limits()
	maxParallel := s.maxParallel(cfg)
	// A one-step flow is a plain flow: every captured tool, no handoffs.
	tools, exec := rs.Definitions, rs.Executor
	var graph *CallFlow
	if len(g.Steps) > 1 {
		graph = &CallFlow{Start: g.Start, Cue: g.RouteCue, CanEnd: anyEndCall(g)}
		graph.Resolve = func(ctx context.Context, id string) (StepSpec, error) { return s.stepSpec(ctx, rs, g, graph, id) }
		tools, exec = s.stepToolset(rs, g, entry, graph)
	}
	fallbacks := s.resolveFallbacks(ctx, rs, "llm", cfg, &s.metrics.LLMFallbacksSkippedTotal, func(c flow.Candidate, secret []byte) StageSpec {
		return StageSpec{Provider: c.Provider, Model: c.Model, SystemPrompt: cfg.LLM.SystemPrompt, Secret: secret, Tools: tools, ToolExecutor: exec, MaxParallel: maxParallel}
	})
	ttsFallbacks := s.resolveFallbacks(ctx, rs, "tts", cfg, &s.metrics.TTSFallbacksSkippedTotal, func(c flow.Candidate, secret []byte) StageSpec {
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
		LLM: StageSpec{Provider: cfg.LLM.Provider, Model: cfg.LLM.Model, SystemPrompt: cfg.LLM.SystemPrompt, Secret: secrets["llm"],
			Tools: tools, ToolExecutor: exec, MaxParallel: maxParallel},
		TTS:                 StageSpec{Provider: cfg.TTS.Provider, Voice: cfg.TTS.Voice, Secret: secrets["tts"]},
		DisableTextCleaning: disableTextCleaning,
		MaxMessages:         maxMessages,
		MaxTokens:           maxTokens,
		Summary:             cfg.Context.Summarization,
		FlowVersionID:       rs.Snapshot.Version.ID,
		StoredSessionID:     rs.Pin.ID,
		Recording:           cfg.Recording.Active(),
		VADStopMS:           cfg.VADStopMS,
		STTP99MS:            cfg.STTP99MS,
		Flow:                graph,
	}, nil
}

func (s *Server) maxParallel(cfg flow.Config) int {
	if mp := cfg.LLM.MaxParallel; mp > 0 {
		return mp
	}
	return max(s.cfg.Tools.MaxParallel, 0)
}

// resolveStageSecrets reads the secret of each named stage from the
// environment, now. On error the secrets gathered so far are wiped.
func (s *Server) resolveStageSecrets(ctx context.Context, cfg flow.Config, names ...string) (map[string][]byte, error) {
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
			return nil, fmt.Errorf("flow not callable: %s credential: %w", stage.Name, err)
		}
		secrets[stage.Name] = secret
	}
	ok = true
	return secrets, nil
}

// stepToolset is the tools and executor of one step: its listed MCP tools plus
// a transfer tool per outgoing route.
func (s *Server) stepToolset(rs *runtime.Session, g flow.Definition, n flow.Step, cg *CallFlow) ([]core.ToolDefinition, core.ToolExecutor) {
	mcp := stepDefinitions(rs.Definitions, n.Tools)
	allowed := make(map[string]bool, len(mcp))
	for _, d := range mcp {
		allowed[d.Name] = true
	}
	defs := append(mcp[:len(mcp):len(mcp)], transferTools(g, n.ID)...)
	var end func() error
	if n.EndCall {
		defs = append(defs, endCallDefinition())
		end = func() error {
			if cg.End == nil {
				return errors.New("end not ready")
			}
			return cg.End(n.ID)
		}
	}
	return defs, stepExecutor(rs.Executor, allowed, transferTargets(g, n.ID), func(target, note string) error {
		if cg.Commit == nil {
			return errors.New("route not ready")
		}
		cg.setNote(note) // every transfer overwrites the previous note
		if err := cg.Commit(target); err != nil {
			cg.setNote("")
			return err
		}
		return nil
	}, end)
}

// stepSpec resolves a non-start step's LLM and TTS when the step is activated:
// secrets are read from the environment now, not held for the whole call.
func (s *Server) stepSpec(ctx context.Context, rs *runtime.Session, g flow.Definition, cg *CallFlow, id string) (StepSpec, error) {
	n, ok := g.Step(id)
	if !ok {
		return StepSpec{}, fmt.Errorf("unknown step %q", id)
	}
	cfg := n.Config
	secrets, err := s.resolveStageSecrets(ctx, cfg, "llm", "tts")
	if err != nil {
		return StepSpec{}, err
	}
	tools, exec := s.stepToolset(rs, g, n, cg)
	maxParallel := s.maxParallel(cfg)
	return StepSpec{
		Name: n.Name,
		LLM: StageSpec{Provider: cfg.LLM.Provider, Model: cfg.LLM.Model, SystemPrompt: cfg.LLM.SystemPrompt, Secret: secrets["llm"],
			Tools: tools, ToolExecutor: exec, MaxParallel: maxParallel},
		LLMFallbacks: s.resolveFallbacks(ctx, rs, "llm", cfg, &s.metrics.LLMFallbacksSkippedTotal, func(c flow.Candidate, secret []byte) StageSpec {
			return StageSpec{Provider: c.Provider, Model: c.Model, SystemPrompt: cfg.LLM.SystemPrompt, Secret: secret, Tools: tools, ToolExecutor: exec, MaxParallel: maxParallel}
		}),
		TTS: StageSpec{Provider: cfg.TTS.Provider, Voice: cfg.TTS.Voice, Secret: secrets["tts"]},
		TTSFallbacks: s.resolveFallbacks(ctx, rs, "tts", cfg, &s.metrics.TTSFallbacksSkippedTotal, func(c flow.Candidate, secret []byte) StageSpec {
			return StageSpec{Provider: c.Provider, Voice: c.Voice, Secret: secret}
		}),
	}, nil
}

// resolveFallbacks resolves the chain after the primary for one stage. A
// fallback that cannot be resolved now is skipped (logged and counted), not fatal.
func (s *Server) resolveFallbacks(ctx context.Context, rs *runtime.Session, stage string, cfg flow.Config, skipped *atomic.Uint64, build func(flow.Candidate, []byte) StageSpec) []StageSpec {
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
