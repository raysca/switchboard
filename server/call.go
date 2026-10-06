package server

import (
	"context"
	"errors"
	"fmt"
	"time"

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
func (s *Server) resolveCall(ctx context.Context, agentID string) (CallSpec, func(), error) {
	if s.agents == nil || s.mcpService == nil || s.credentials == nil || s.store == nil {
		return CallSpec{}, nil, errAgentsUnavailable
	}
	if agentID == "" {
		return CallSpec{}, nil, errors.New("token does not name an agent")
	}
	rs, err := runtime.StartSession(ctx, s.agents, s.mcpService, agentID, storage.NewID())
	if err != nil {
		return CallSpec{}, nil, fmt.Errorf("start agent session: %w", err)
	}
	release := func() {
		// In-flight MCP tool calls are waited for, but never past the session
		// teardown budget: a stuck remote tool must not pin the call forever.
		ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
		defer cancel()
		if err := rs.Close(ctx); err != nil {
			s.logger.Warn("agent session did not end cleanly", "session", rs.Pin.ID, "err", err.Error())
		}
	}
	call, err := s.callSpec(ctx, rs)
	if err != nil {
		release()
		return CallSpec{}, nil, err
	}
	return call, release, nil
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
	for _, stage := range cfg.Stages() {
		if !s.registry.Supports(stage.Provider, stage.Name) {
			return CallSpec{}, fmt.Errorf("%s provider %q is not available on this server", stage.Name, stage.Provider)
		}
		id, reason := agent.ResolveCredential(stage, s.providerDefs, creds)
		if reason != "" {
			return CallSpec{}, fmt.Errorf("agent not callable: %s", reason)
		}
		if id == "" {
			continue // provider needs no credential
		}
		secret, err := s.credentials.ProviderSecret(ctx, id, stage.Provider)
		if err != nil {
			return CallSpec{}, fmt.Errorf("%s credential: %w", stage.Name, err)
		}
		secrets[stage.Name] = secret
	}
	maxMessages, maxTokens := cfg.Context.Limits()
	return CallSpec{
		STT:             StageSpec{Provider: cfg.STT.Provider, Model: cfg.STT.Model, Language: cfg.STT.Language, Secret: secrets["stt"]},
		LLM:             StageSpec{Provider: cfg.LLM.Provider, Model: cfg.LLM.Model, SystemPrompt: cfg.LLM.SystemPrompt, Secret: secrets["llm"]},
		TTS:             StageSpec{Provider: cfg.TTS.Provider, Voice: cfg.TTS.Voice, Secret: secrets["tts"]},
		MaxMessages:     maxMessages,
		MaxTokens:       maxTokens,
		ToolDefinitions: rs.Definitions,
		ToolExecutor:    rs.Executor,
	}, nil
}

// wipeSecrets zeroes the decrypted credentials once provider clients are built;
// adapters copy what they need (API key strings, parsed service accounts).
func (c *CallSpec) wipeSecrets() {
	for _, b := range [][]byte{c.STT.Secret, c.LLM.Secret, c.TTS.Secret} {
		clear(b)
	}
	c.STT.Secret, c.LLM.Secret, c.TTS.Secret = nil, nil, nil
}
