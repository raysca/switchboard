package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
// credential. release ends the persisted session and must run exactly once.
// The session row takes the token's single-use jti as its id, so a client that
// holds the token can find its session (the admin voice test links to it).
func (s *Server) resolveCall(ctx context.Context, agentID, tokenID string) (CallSpec, func(), error) {
	if s.agents == nil || s.mcpService == nil || s.credentials == nil || s.store == nil {
		return CallSpec{}, nil, errAgentsUnavailable
	}
	if agentID == "" {
		return CallSpec{}, nil, errors.New("token does not name an agent")
	}
	sessionID := tokenID
	if !storage.IsID(sessionID) { // legacy tokens carry a random UUIDv4 or no jti
		sessionID = storage.NewID()
	}
	rs, err := runtime.StartSession(ctx, s.agents, s.mcpService, agentID, sessionID)
	if err != nil {
		return CallSpec{}, nil, fmt.Errorf("start agent session: %w", err)
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
			s.logger.Warn("agent session did not end cleanly", "session", rs.Pin.ID, "err", s.redactor.Redact(err.Error()))
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
	cfg, err := agent.DecodeConfig(rs.Snapshot.Version.Config)
	if err != nil {
		return CallSpec{}, err
	}
	if missing := cfg.Missing(); len(missing) > 0 {
		return CallSpec{}, fmt.Errorf("agent config incomplete: %v", missing)
	}
	creds, err := s.store.Credentials().List(ctx)
	if err != nil {
		return CallSpec{}, fmt.Errorf("list credentials: %w", err)
	}
	secrets := map[string][]byte{}
	resolved := false
	defer func() { // an error return must not leave decrypted primaries behind
		if !resolved {
			for _, b := range secrets {
				clear(b)
			}
		}
	}()
	for _, stage := range cfg.Stages() {
		if !s.registry.Supports(stage.Provider, stage.Name) {
			return CallSpec{}, fmt.Errorf("%s provider %q is not available on this server", stage.Name, stage.Provider)
		}
		id, reason := agent.ResolveCredential(stage, s.providerDefs, creds)
		if reason != "" {
			return CallSpec{}, fmt.Errorf("agent not callable: %s", reason)
		}
		secret, err := s.credentials.ProviderSecret(ctx, id, stage.Provider)
		if err != nil {
			return CallSpec{}, fmt.Errorf("%s credential: %w", stage.Name, err)
		}
		secrets[stage.Name] = secret
	}
	resolved = true
	maxMessages, maxTokens := cfg.Context.Limits()
	fallbacks := s.resolveFallbacks(ctx, rs, creds, "llm", cfg, &s.metrics.LLMFallbacksSkippedTotal, func(c agent.Candidate, secret []byte) StageSpec {
		return StageSpec{Provider: c.Provider, Model: c.Model, SystemPrompt: cfg.LLM.SystemPrompt, Secret: secret, Tools: rs.Definitions, ToolExecutor: rs.Executor}
	})
	ttsFallbacks := s.resolveFallbacks(ctx, rs, creds, "tts", cfg, &s.metrics.TTSFallbacksSkippedTotal, func(c agent.Candidate, secret []byte) StageSpec {
		return StageSpec{Provider: c.Provider, Voice: c.Voice, Secret: secret}
	})
	return CallSpec{
		LLMFallbacks: fallbacks, TTSFallbacks: ttsFallbacks, Failover: cfg.Failover,
		STTFallbacks: s.sttFallbacks(ctx, rs, cfg, creds),
		STT:          StageSpec{Provider: cfg.STT.Provider, Model: cfg.STT.Model, Language: cfg.STT.Language, Secret: secrets["stt"]},
		LLM: StageSpec{Provider: cfg.LLM.Provider, Model: cfg.LLM.Model, SystemPrompt: cfg.LLM.SystemPrompt, Secret: secrets["llm"],
			Tools: rs.Definitions, ToolExecutor: rs.Executor},
		TTS:            StageSpec{Provider: cfg.TTS.Provider, Voice: cfg.TTS.Voice, Secret: secrets["tts"]},
		MaxMessages:    maxMessages,
		MaxTokens:      maxTokens,
		Summary:        cfg.Context.Summarization,
		AgentVersionID: rs.Snapshot.Version.ID,
		Recording:      cfg.Recording.Active(),
	}, nil
}

// resolveFallbacks resolves the chain after the primary for one stage. A
// fallback that cannot be resolved now is skipped (logged and counted), not fatal.
func (s *Server) resolveFallbacks(ctx context.Context, rs *runtime.Session, creds []storage.Credential, stage string, cfg agent.Config, skipped *atomic.Uint64, build func(agent.Candidate, []byte) StageSpec) []StageSpec {
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
		id, reason := agent.ResolveCredential(agent.Stage{Name: stage, Provider: cand.Provider, CredentialID: cand.CredentialID}, s.providerDefs, creds)
		if reason != "" {
			skip(reason)
			continue
		}
		secret, err := s.credentials.ProviderSecret(ctx, id, cand.Provider)
		if err != nil {
			skip("credential unavailable")
			continue
		}
		out = append(out, build(cand, secret))
	}
	return out
}

// wipeSecrets zeroes the decrypted credentials once provider clients are built;
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
