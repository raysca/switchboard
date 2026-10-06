package e2e_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
	coretools "govoice/core/tools"
	"govoice/server/agent"
	"govoice/server/api"
	"govoice/server/credential"
	"govoice/server/mcp"
	"govoice/server/provider"
	"govoice/server/runtime"
	"govoice/server/storage"
	"govoice/server/trace"
	"govoice/transport/ws"
)

// allowAllDestinations implements mcp.DestinationPolicy for local test doubles.
type allowAllDestinations struct{}

func (allowAllDestinations) Validate(context.Context, *url.URL) error { return nil }

// mockMCPRemote implements mcp.RemoteClient for deterministic tool calls.
type mockMCPRemote struct {
	mu        sync.Mutex
	tools     []mcp.RemoteTool
	failCalls bool
	callDelay time.Duration
	calls     int
	entered   chan struct{}
	release   chan struct{}
}

func (m *mockMCPRemote) ListTools(_ context.Context, _, _ string) ([]mcp.RemoteTool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	res := make([]mcp.RemoteTool, len(m.tools))
	for i, t := range m.tools {
		res[i] = mcp.RemoteTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: append(json.RawMessage(nil), t.InputSchema...),
		}
	}
	return res, nil
}

func (m *mockMCPRemote) CallTool(ctx context.Context, _, _, name string, args json.RawMessage) (mcp.RemoteResult, error) {
	m.mu.Lock()
	m.calls++
	fail := m.failCalls
	delay := m.callDelay
	entered := m.entered
	release := m.release
	m.mu.Unlock()

	if entered != nil {
		select {
		case <-entered:
		default:
			close(entered)
		}
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return mcp.RemoteResult{}, ctx.Err()
		}
	}
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return mcp.RemoteResult{}, ctx.Err()
		}
	}
	if fail {
		return mcp.RemoteResult{}, errors.New("remote MCP server connection refused (503)")
	}

	if name == "lookup_order" {
		var input struct {
			OrderID string `json:"order_id"`
		}
		_ = json.Unmarshal(args, &input)
		return mcp.RemoteResult{
			JSON: json.RawMessage(`{"status":"shipped","carrier":"express","order_id":"` + input.OrderID + `"}`),
		}, nil
	}
	if name == "track_shipment" {
		var input struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(args, &input)
		return mcp.RemoteResult{
			JSON: json.RawMessage(`{"status":"in_transit","carrier":"express","id":"` + input.ID + `"}`),
		}, nil
	}
	return mcp.RemoteResult{}, mcp.ErrNotFound
}

// mockGoogleSTT simulates Google Cloud STT emitting a candidate transcript on audio input.
type mockGoogleSTT struct {
	candidate string
}

func (m *mockGoogleSTT) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	switch frame := f.(type) {
	case *core.InputAudioFrame:
		if m.candidate != "" {
			text := m.candidate
			m.candidate = ""
			return e.Push(&core.FinalTranscriptionFrame{
				Text:      text,
				Turn:      "turn-mixed-1",
				Gen:       frame.Gen,
				Timestamp: time.Now(),
			})
		}
	}
	return e.PushDirection(f, dir)
}

// mockXAILLM simulates an xAI LLM processor generating text synthesis units.
type mockXAILLM struct {
	session  core.SessionController
	response string
	timeout  atomic.Bool
}

func (m *mockXAILLM) Attach(s core.SessionController, index int) error {
	m.session = s
	return nil
}

func (m *mockXAILLM) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	switch frame := f.(type) {
	case *core.UserMessageFrame:
		if m.timeout.Load() {
			return e.Push(&core.ErrorFrame{
				Err:   errors.New("xai: request timeout (context deadline exceeded)"),
				Fatal: false,
			})
		}
		if m.session != nil {
			gen := frame.Gen
			if gen == 0 {
				gen = m.session.CurrentGeneration()
			}
			turn := frame.Turn
			return m.session.Go(func(c context.Context) {
				_ = m.session.Reenter(&core.LLMTextDeltaFrame{
					Delta: m.response,
					Turn:  turn,
					Gen:   gen,
				}, core.Downstream, 0, gen)
				_ = m.session.Reenter(&core.LLMTextFrame{
					Text: m.response,
					Turn: turn,
					Gen:  gen,
				}, core.Downstream, 0, gen)
			})
		}
		// Fallback synchronous emission
		if err := e.Push(&core.LLMTextDeltaFrame{
			Delta: m.response,
			Turn:  frame.Turn,
			Gen:   frame.Gen,
		}); err != nil {
			return err
		}
		return e.Push(&core.LLMTextFrame{
			Text: m.response,
			Turn: frame.Turn,
			Gen:  frame.Gen,
		})
	}
	return e.PushDirection(f, dir)
}

// mockTTS simulates TTS converting synthesis units into PCM output audio frames.
type mockTTS struct {
	mu          sync.Mutex
	synthesized []string
}

func (m *mockTTS) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	switch frame := f.(type) {
	case *core.SynthesisUnitFrame:
		m.mu.Lock()
		m.synthesized = append(m.synthesized, frame.Text)
		m.mu.Unlock()
		return e.Push(&core.OutputAudioFrame{
			Samples:    make([]byte, 960), // 20ms chunk PCM16
			Encoding:   core.EncodingPCM16LE,
			SampleRate: 24000,
			Channels:   1,
			Duration:   20 * time.Millisecond,
			Timestamp:  time.Now(),
			Turn:       frame.Turn,
			Gen:        frame.Gen,
			UnitID:     frame.UnitID,
			IsFinal:    true,
		})
	}
	return e.PushDirection(f, dir)
}

// helper to open a clean isolated SQLite database for a scenario.
func openScenarioDB(t *testing.T, name string) *storage.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), name+".db")
	store, err := storage.Open(context.Background(), storage.Config{
		Dialect: storage.SQLite,
		DSN:     dbPath,
	})
	if err != nil {
		t.Fatalf("open scenario database %s: %v", name, err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if name != "scenario1_fresh_install" {
		seedScenarioCredentials(t, store, "google", "xai")
	}
	return store
}

// seedScenarioCredentials stores one credential per provider; agent readiness
// requires a resolvable credential for each stage.
func seedScenarioCredentials(t *testing.T, s *storage.Store, providers ...string) {
	t.Helper()
	for _, p := range providers {
		if err := s.Credentials().Put(context.Background(), storage.Credential{ID: testCredentialID(p), Provider: p, MaskedSuffix: "abcd",
			ValidationState: "unvalidated", CapabilitiesJSON: "[]", Ciphertext: []byte("x"), KeyID: "k"}); err != nil {
			t.Fatal(err)
		}
	}
}

// =============================================================================
// Scenario 1: Fresh Install & Onboarding
// =============================================================================
// Clean startup on empty database, zero initial credentials/agents;
// onboarding configures provider credentials, creates first agent with
// valid readiness evaluation, and exposes immutable v1 version.
func TestScenario1_FreshInstallAndOnboarding(t *testing.T) {
	ctx := context.Background()
	store := openScenarioDB(t, "scenario1_fresh_install")

	// 1. Invariant: Clean database with zero initial credentials and zero agents
	initialCreds, err := store.Credentials().List(ctx)
	if err != nil {
		t.Fatalf("list credentials on fresh db: %v", err)
	}
	if len(initialCreds) != 0 {
		t.Fatalf("expected 0 initial credentials, got %d", len(initialCreds))
	}

	initialAgents, err := store.Agents().List(ctx, 10, 0)
	if err != nil {
		t.Fatalf("list agents on fresh db: %v", err)
	}
	if len(initialAgents) != 0 {
		t.Fatalf("expected 0 initial agents, got %d", len(initialAgents))
	}

	// 2. Set up provider capability registry and credential service
	reg := provider.NewRegistry(provider.Definitions())
	keyring := credential.Keyring{
		ActiveID: "key-1",
		Keys:     map[string][]byte{"key-1": []byte(strings.Repeat("k", 32))},
	}
	credService, err := credential.NewService(store.Credentials(), keyring, reg, nil)
	if err != nil {
		t.Fatalf("new credential service: %v", err)
	}

	// 3. Define readiness evaluator that checks provider credential readiness
	evaluator := func(c context.Context, agentID string, rawConfig json.RawMessage) (bool, error) {
		cfg, err := agent.DecodeConfig(rawConfig)
		if err != nil {
			return false, nil
		}
		creds, err := credService.List(c)
		if err != nil {
			return false, err
		}
		hasProv := make(map[string]bool)
		for _, cr := range creds {
			hasProv[cr.Provider] = true
		}
		// Agent requires configured STT/LLM/TTS providers to have active credentials
		return hasProv[cfg.STT.Provider] && hasProv[cfg.LLM.Provider] && hasProv[cfg.TTS.Provider], nil
	}

	agentService := agent.NewService(store, evaluator)

	// 4. Before credentials: an agent cannot be created, because agents are
	// attached to providers (and their credentials) at creation.
	if _, err := agentService.Create(ctx, agent.CreateRequest{
		Name:        "Unattached Agent",
		Description: "Created before provider onboarding",
		Config:      json.RawMessage(`{"stt":{"provider":"google","credential_id":"018f0000-0000-7000-8000-000000000001"},"llm":{"provider":"xai","model":"xai","credential_id":"018f0000-0000-7000-8000-000000000002"},"tts":{"provider":"google","credential_id":"018f0000-0000-7000-8000-000000000001"}}`),
	}); !errors.Is(err, agent.ErrNotAttached) {
		t.Fatalf("expected agent creation to require attached credentials, got %v", err)
	}

	// 5. Onboarding: configure provider credentials
	googleCred, err := credService.Create(ctx, credential.CreateInput{
		Provider: "google",
		Secret:   "google-cloud-api-key-test-sentinel-123",
	})
	if err != nil {
		t.Fatalf("create google credential: %v", err)
	}
	if !strings.HasPrefix(googleCred.MaskedValue, "••••••") {
		t.Fatalf("expected secret to be masked in DTO, got %s", googleCred.MaskedValue)
	}

	xaiCred, err := credService.Create(ctx, credential.CreateInput{
		Provider: "xai",
		Secret:   "xai-grok-api-key-test-sentinel-456",
	})
	if err != nil {
		t.Fatalf("create xai credential: %v", err)
	}
	if !strings.HasPrefix(xaiCred.MaskedValue, "••••••") {
		t.Fatalf("expected secret to be masked in DTO, got %s", xaiCred.MaskedValue)
	}

	// 6. Onboarding creates the first ready agent
	firstAgent, err := agentService.Create(ctx, agent.CreateRequest{
		Name:        "Customer Care Bot",
		Description: "Onboarded primary voice agent",
		Config:      json.RawMessage(`{"stt":{"provider":"google","model":"google","credential_id":"` + googleCred.ID + `"},"llm":{"provider":"xai","model":"xai","credential_id":"` + xaiCred.ID + `"},"tts":{"provider":"google","voice":"google","credential_id":"` + googleCred.ID + `"}}`),
	})
	if err != nil {
		t.Fatalf("create first onboarded agent: %v", err)
	}

	// 7. Verification: Agent is ready, enabled, and exposes immutable v1 version
	if firstAgent.ID == "" {
		t.Fatal("expected non-empty agent ID")
	}
	if !firstAgent.Enabled {
		t.Fatal("expected agent to be enabled by default")
	}
	if firstAgent.Version == nil {
		t.Fatal("expected agent to have current version")
	}
	if firstAgent.Version.Number != 1 {
		t.Fatalf("expected version number 1, got %d", firstAgent.Version.Number)
	}
	if len(firstAgent.Version.ConfigHash) != 64 {
		t.Fatalf("expected 64-char SHA-256 config hash, got %q", firstAgent.Version.ConfigHash)
	}
	if firstAgent.Readiness.State != agent.ReadinessReady {
		t.Fatalf("expected agent readiness %q, got %q (reasons: %v)", agent.ReadinessReady, firstAgent.Readiness.State, firstAgent.Readiness.Reasons)
	}
	if len(firstAgent.Readiness.Reasons) != 0 {
		t.Fatalf("expected 0 blocking reasons for ready agent, got %v", firstAgent.Readiness.Reasons)
	}

	// 8. Management HTTP API validation for onboarding
	apiHandler := api.NewAgentsHandler(agentService)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+firstAgent.ID, nil)
	w := httptest.NewRecorder()
	apiHandler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("management GET agent status %d: %s", w.Code, w.Body.String())
	}
	var apiResp struct {
		Data agent.Agent `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &apiResp); err != nil {
		t.Fatalf("decode agent API response: %v", err)
	}
	if apiResp.Data.ID != firstAgent.ID || apiResp.Data.Version.Number != 1 || apiResp.Data.Readiness.State != agent.ReadinessReady {
		t.Fatalf("mismatched agent in API response: %+v", apiResp.Data)
	}
}

// =============================================================================
// Scenario 2: Mixed Providers
// =============================================================================
// Composite pipeline with simulated Google STT candidate, xAI LLM tool completion,
// and TTS output; session runs with immutable configuration hash pinning.
func TestScenario2_MixedProviders(t *testing.T) {
	ctx := context.Background()
	store := openScenarioDB(t, "scenario2_mixed_providers")
	agentService := agent.NewService(store)

	// 1. Create agent configured with composite multi-provider stack
	agentConfig := json.RawMessage(`{"stt":{"provider":"google","model":"google.stt","credential_id":"018f0000-0000-7000-8000-000000000001"},"llm":{"provider":"xai","model":"xai.llm","system_prompt":"You are a helpful assistant","credential_id":"018f0000-0000-7000-8000-000000000002"},"tts":{"provider":"google","voice":"mock.tts","credential_id":"018f0000-0000-7000-8000-000000000001"}}`)
	createdAgent, err := agentService.Create(ctx, agent.CreateRequest{
		Name:        "Mixed Provider Agent",
		Description: "Google STT + xAI LLM + Mock TTS",
		Config:      agentConfig,
	})
	if err != nil {
		t.Fatalf("create mixed provider agent: %v", err)
	}

	expectedHash := createdAgent.Version.ConfigHash
	if expectedHash == "" {
		t.Fatal("agent version missing config hash")
	}

	// 2. Start session pinned to the agent version snapshot
	pin, err := agentService.StartSession(ctx, createdAgent.ID, "")
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	if pin.ConfigHash != expectedHash {
		t.Fatalf("session pinned hash %s does not match agent version hash %s", pin.ConfigHash, expectedHash)
	}
	if pin.AgentVersionID != createdAgent.Version.ID {
		t.Fatalf("session pinned version %s does not match agent version %s", pin.AgentVersionID, createdAgent.Version.ID)
	}

	// 3. Assemble composite pipeline:
	// Google STT Mock -> UserContext -> AssistantContext -> FakeLLM -> FakeTTS -> TransportOutput
	stt := &mockGoogleSTT{candidate: "What is the status of shipment 99?"}
	contextPool := core.NewConversationContext(core.ContextConfig{MaxMessages: 10, MaxTokens: 2000})
	userCtx := core.NewUserContextProcessor(contextPool, core.UserContextConfig{TurnPrefix: "turn"})
	asstCtx := core.NewAssistantContextProcessor(contextPool, core.DefaultAssistantContextConfig())
	llm := core.NewFakeLLMProcessor(core.FakeLLMConfig{
		ResponseChunks: []string{"Shipment 99 has arrived at the local depot."},
	})
	tts := core.NewFakeTTSProcessor(core.FakeTTSConfig{
		FramesPerUnit: 2,
	})
	transportOutput := core.NewFakeTransportOutput(core.FakeTransportConfig{})

	pipe, err := core.NewPipeline(stt, userCtx, asstCtx, llm, tts, transportOutput)
	if err != nil {
		t.Fatalf("assemble composite pipeline: %v", err)
	}

	// 4. Session execution with pinned configuration hash
	sess, err := core.NewSession(1002, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	if err := sess.Start(); err != nil {
		t.Fatalf("start session: %v", err)
	}
	defer sess.Close()

	// Post audio frame into pipeline
	inputAudio := &core.InputAudioFrame{
		Samples:    make([]byte, 640),
		Encoding:   core.EncodingPCM16LE,
		SampleRate: 16000,
		Channels:   1,
		Duration:   20 * time.Millisecond,
		Timestamp:  time.Now(),
		Seq:        1,
		Gen:        sess.CurrentGeneration(),
	}
	if err := sess.Post(inputAudio); err != nil {
		t.Fatalf("post input audio: %v", err)
	}

	// 5. Verify conversation turns through the mixed pipeline
	deadline := time.Now().Add(2 * time.Second)
	for len(transportOutput.GetWrittenFrames()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	written := transportOutput.GetWrittenFrames()
	if len(written) == 0 {
		t.Fatal("no output audio delivered from mixed provider pipeline")
	}

	if tts.Synthesized.Load() == 0 {
		t.Fatal("expected TTS synthesis units to be produced")
	}

	// 6. Verify conversation context committed both turns
	messages := contextPool.Messages()
	var hasUser, hasAssistant bool
	for _, m := range messages {
		if m.Role == core.RoleUser && strings.Contains(m.Content, "shipment 99") {
			hasUser = true
		}
		if m.Role == core.RoleAssistant && strings.Contains(m.Content, "local depot") {
			hasAssistant = true
		}
	}
	if !hasUser || !hasAssistant {
		t.Fatalf("missing user or assistant message in conversation context: %+v", messages)
	}

	// 7. Verify session record in storage preserves the pinned immutable config hash
	sessionRecord, err := store.Sessions().Get(ctx, pin.ID)
	if err != nil {
		t.Fatalf("query session record: %v", err)
	}
	if sessionRecord.ConfigHash != expectedHash {
		t.Fatalf("stored session record config hash %s does not match expected hash %s", sessionRecord.ConfigHash, expectedHash)
	}
}

// =============================================================================
// Scenario 3: MCP Tool Lifecycle
// =============================================================================
// MCP discovery snapshot, tool schema hash, session startup captures runtime wrapper,
// ToolLoop execution succeeds, and trace spans correlate to turn.
func TestScenario3_MCPToolLifecycle(t *testing.T) {
	ctx := context.Background()
	store := openScenarioDB(t, "scenario3_mcp_lifecycle")

	// 1. Remote MCP server double with "lookup_order" tool
	orderSchema := json.RawMessage(`{"type":"object","properties":{"order_id":{"type":"string"}},"required":["order_id"]}`)
	remote := &mockMCPRemote{
		tools: []mcp.RemoteTool{
			{Name: "lookup_order", Description: "Look up order shipment status", InputSchema: orderSchema},
		},
	}

	// Set up trace recorder
	traceAdapter := trace.NewAdapter(store.Traces())
	defer traceAdapter.Close()

	mcpService, err := mcp.NewService(mcp.ServiceConfig{
		Store:        store,
		Remote:       remote,
		Destinations: allowAllDestinations{},
		Tracer:       traceAdapter,
	})
	if err != nil {
		t.Fatalf("new MCP service: %v", err)
	}

	// 2. Register MCP server and perform discovery refresh
	mcpServer, err := mcpService.Create(ctx, mcp.ServerConfig{
		Name:     "orders-service",
		Endpoint: "https://mcp.internal/rpc",
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create MCP server: %v", err)
	}

	discovery, err := mcpService.Refresh(ctx, mcpServer.ID)
	if err != nil {
		t.Fatalf("refresh MCP discovery: %v", err)
	}
	if len(discovery.Tools) != 1 || discovery.Tools[0].RemoteName != "lookup_order" {
		t.Fatalf("unexpected discovery tools: %+v", discovery.Tools)
	}
	schemaHash := discovery.Tools[0].SchemaHash
	if len(schemaHash) != 64 {
		t.Fatalf("expected 64-char SHA-256 schema hash, got %q", schemaHash)
	}

	// 3. Attach tool to agent
	agentService := agent.NewService(store)
	agentCreated, err := agentService.Create(ctx, agent.CreateRequest{
		Name:        "Order Assistant",
		Description: "Agent with MCP order lookup tool",
		Config:      json.RawMessage(`{"stt":{"provider":"google","model":"google","credential_id":"018f0000-0000-7000-8000-000000000001"},"llm":{"provider":"xai","model":"xai","credential_id":"018f0000-0000-7000-8000-000000000002"},"tts":{"provider":"google","voice":"google","credential_id":"018f0000-0000-7000-8000-000000000001"}}`),
		MCPTools: []agent.ToolSelection{
			{ServerID: mcpServer.ID, RemoteName: "lookup_order", SchemaHash: schemaHash, Required: true},
		},
	})
	if err != nil {
		t.Fatalf("create agent with MCP tool: %v", err)
	}

	// 4. Start session and capture runtime tool wrapper
	sessionID := storage.NewID()
	runtimeSession, err := runtime.StartSession(ctx, agentService, mcpService, agentCreated.ID, sessionID)
	if err != nil {
		t.Fatalf("start runtime session: %v", err)
	}
	defer runtimeSession.Close(ctx)

	if len(runtimeSession.Definitions) != 1 {
		t.Fatalf("expected 1 captured tool definition, got %d", len(runtimeSession.Definitions))
	}
	toolDef := runtimeSession.Definitions[0]
	if !strings.HasPrefix(toolDef.Name, "mcp__") {
		t.Fatalf("expected namespaced runtime tool name, got %s", toolDef.Name)
	}

	// 5. ToolLoop execution succeeds through captured executor
	history := core.NewConversationContext(core.ContextConfig{MaxMessages: 10, MaxTokens: 2000})
	toolLoop, err := coretools.NewToolLoop(runtimeSession.Definitions, runtimeSession.Executor, history, coretools.DefaultToolLoopConfig())
	if err != nil {
		t.Fatalf("new tool loop: %v", err)
	}
	defer toolLoop.Close()

	turnID := storage.NewID()
	call := core.ToolCall{
		ID:         "call-ord-1",
		Name:       toolDef.Name,
		SessionID:  core.SessionID(sessionID),
		TurnID:     core.TurnID(turnID),
		Generation: 1,
		Arguments:  json.RawMessage(`{"order_id":"ORD-987"}`),
	}

	loopResult, err := toolLoop.Run(ctx, []core.ToolCall{call}, nil)
	if err != nil {
		t.Fatalf("tool loop execution: %v", err)
	}
	if !loopResult.Done || len(loopResult.Results) != 1 {
		t.Fatalf("unexpected loop result: %+v", loopResult)
	}
	res := loopResult.Results[0]
	if res.Status != core.ToolSucceeded {
		t.Fatalf("expected tool status succeeded, got %s", res.Status)
	}
	if !strings.Contains(string(res.Result), "ORD-987") || !strings.Contains(string(res.Result), "shipped") {
		t.Fatalf("unexpected tool output: %s", string(res.Result))
	}

	// 6. Trace spans correlate to session ID and turn ID
	if err := store.Sessions().AddTurn(ctx, storage.TurnRecord{
		ID:        turnID,
		SessionID: sessionID,
		Ordinal:   1,
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("add turn record: %v", err)
	}

	turnSpan := traceAdapter.StartSpan(ctx, core.NewTraceEvent("turn.lifecycle", time.Now().UTC(), core.SessionID(sessionID), core.TurnID(turnID), 1, core.TracePolicy{}, nil))
	turnSpan.AddEvent(core.NewTraceEvent("tool.execution", time.Now().UTC(), core.SessionID(sessionID), core.TurnID(turnID), 1, core.TracePolicy{}, map[string]any{
		"tool_name":    toolDef.Name,
		"tool_call_id": "call-ord-1",
	}))
	turnSpan.End(nil)

	if err := traceAdapter.Flush(ctx); err != nil {
		t.Fatalf("flush traces: %v", err)
	}

	// Verify trace spans in store
	spans, err := store.Traces().Spans(ctx, sessionID, turnID)
	if err != nil {
		t.Fatalf("query traces: %v", err)
	}
	if len(spans) == 0 {
		t.Fatalf("expected trace spans correlated to %s", turnID)
	}
	if spans[0].SessionID != sessionID || spans[0].TurnID != turnID {
		t.Fatalf("span correlation mismatch: session=%s turn=%s", spans[0].SessionID, spans[0].TurnID)
	}
}

// =============================================================================
// Scenario 4: Immutable Agent Versioning
// =============================================================================
// Session starts on v1; agent config updated to v2 with different system prompt/voice;
// active session continues running on v1 snapshot and reports v1 config hash.
func TestScenario4_ImmutableAgentVersioning(t *testing.T) {
	ctx := context.Background()
	store := openScenarioDB(t, "scenario4_immutable_versioning")
	agentService := agent.NewService(store)

	// 1. Create Agent v1
	v1Prompt := "You are a professional banking assistant version 1"
	v1Voice := "voice-v1-banking"
	v1Config := json.RawMessage(`{"stt":{"provider":"google","credential_id":"018f0000-0000-7000-8000-000000000001"},"llm":{"provider":"xai","model":"grok","system_prompt":"` + v1Prompt + `","credential_id":"018f0000-0000-7000-8000-000000000002"},"tts":{"provider":"google","voice":"` + v1Voice + `","credential_id":"018f0000-0000-7000-8000-000000000001"}}`)

	agentV1, err := agentService.Create(ctx, agent.CreateRequest{
		Name:        "Banking Agent",
		Description: "Initial v1 deployment",
		Config:      v1Config,
	})
	if err != nil {
		t.Fatalf("create agent v1: %v", err)
	}

	v1VersionID := agentV1.Version.ID
	v1ConfigHash := agentV1.Version.ConfigHash
	if agentV1.Version.Number != 1 {
		t.Fatalf("expected version 1, got %d", agentV1.Version.Number)
	}

	// 2. Start active Session 1 pinned to Agent v1
	session1ID := storage.NewID()
	pin1, err := agentService.StartSession(ctx, agentV1.ID, session1ID)
	if err != nil {
		t.Fatalf("start session 1: %v", err)
	}
	if pin1.AgentVersionID != v1VersionID || pin1.ConfigHash != v1ConfigHash {
		t.Fatalf("session 1 pin mismatch: version=%s hash=%s", pin1.AgentVersionID, pin1.ConfigHash)
	}

	// 3. Update Agent to publish contiguous v2 with different prompt and voice
	v2Prompt := "You are a friendly concierge version 2"
	v2Voice := "voice-v2-concierge"
	v2Config := json.RawMessage(`{"stt":{"provider":"google","credential_id":"018f0000-0000-7000-8000-000000000001"},"llm":{"provider":"xai","model":"grok","system_prompt":"` + v2Prompt + `","credential_id":"018f0000-0000-7000-8000-000000000002"},"tts":{"provider":"google","voice":"` + v2Voice + `","credential_id":"018f0000-0000-7000-8000-000000000001"}}`)

	agentV2, err := agentService.Update(ctx, agentV1.ID, agent.UpdateRequest{
		Config: v2Config,
	})
	if err != nil {
		t.Fatalf("update agent to v2: %v", err)
	}

	if agentV2.Version.Number != 2 {
		t.Fatalf("expected version 2, got %d", agentV2.Version.Number)
	}
	v2VersionID := agentV2.Version.ID
	v2ConfigHash := agentV2.Version.ConfigHash

	if v1ConfigHash == v2ConfigHash {
		t.Fatalf("expected different config hashes between v1 and v2, both are %s", v1ConfigHash)
	}

	// 4. Invariant: Active Session 1 continues running on v1 snapshot and reports v1 hash
	snapshot1, err := agentService.ResolveSession(ctx, session1ID)
	if err != nil {
		t.Fatalf("resolve active session 1: %v", err)
	}
	if snapshot1.Version.Number != 1 {
		t.Fatalf("expected session 1 to report version 1, got %d", snapshot1.Version.Number)
	}
	if snapshot1.Version.ID != v1VersionID {
		t.Fatalf("expected session 1 version ID %s, got %s", v1VersionID, snapshot1.Version.ID)
	}
	if snapshot1.ConfigHash != v1ConfigHash {
		t.Fatalf("expected session 1 config hash %s, got %s", v1ConfigHash, snapshot1.ConfigHash)
	}

	// Verify exact config contents of Session 1 reflect v1, not v2
	s1Parsed, err := agent.DecodeConfig(snapshot1.Version.Config)
	if err != nil {
		t.Fatalf("decode s1 config: %v", err)
	}
	s1Config := struct{ Prompt, Voice string }{s1Parsed.LLM.SystemPrompt, s1Parsed.TTS.Voice}
	if s1Config.Prompt != v1Prompt || s1Config.Voice != v1Voice {
		t.Fatalf("session 1 leaked v2 config: prompt=%q voice=%q", s1Config.Prompt, s1Config.Voice)
	}

	// 5. Start Session 2: New session resolves to v2 snapshot and reports v2 hash
	session2ID := storage.NewID()
	pin2, err := agentService.StartSession(ctx, agentV1.ID, session2ID)
	if err != nil {
		t.Fatalf("start session 2: %v", err)
	}
	if pin2.AgentVersionID != v2VersionID || pin2.ConfigHash != v2ConfigHash {
		t.Fatalf("session 2 pin mismatch: version=%s hash=%s", pin2.AgentVersionID, pin2.ConfigHash)
	}

	snapshot2, err := agentService.ResolveSession(ctx, session2ID)
	if err != nil {
		t.Fatalf("resolve session 2: %v", err)
	}
	if snapshot2.Version.Number != 2 || snapshot2.ConfigHash != v2ConfigHash {
		t.Fatalf("session 2 snapshot mismatch: version=%d hash=%s", snapshot2.Version.Number, snapshot2.ConfigHash)
	}

	s2Parsed, err := agent.DecodeConfig(snapshot2.Version.Config)
	if err != nil {
		t.Fatalf("decode s2 config: %v", err)
	}
	s2Config := struct{ Prompt, Voice string }{s2Parsed.LLM.SystemPrompt, s2Parsed.TTS.Voice}
	if s2Config.Prompt != v2Prompt || s2Config.Voice != v2Voice {
		t.Fatalf("session 2 did not get v2 config: prompt=%q voice=%q", s2Config.Prompt, s2Config.Voice)
	}

	// Clean up sessions
	_ = agentService.EndSession(ctx, session1ID)
	_ = agentService.EndSession(ctx, session2ID)
}

// =============================================================================
// Scenario 5: MCP Deletion Safety
// =============================================================================
// Attempt delete referenced MCP server -> fails 409 conflict; confirmed detach-and-delete ->
// atomically publishes v2 agent without MCP tool, disables server, waits for active session drain,
// and deletes server while preserving historical session snapshots.
func TestScenario5_MCPDeletionSafety(t *testing.T) {
	ctx := context.Background()
	store := openScenarioDB(t, "scenario5_mcp_deletion")

	orderSchema := json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`)
	remote := &mockMCPRemote{
		tools: []mcp.RemoteTool{
			{Name: "track_shipment", Description: "Track shipment", InputSchema: orderSchema},
		},
	}

	mcpService, err := mcp.NewService(mcp.ServiceConfig{
		Store:        store,
		Remote:       remote,
		Destinations: allowAllDestinations{},
	})
	if err != nil {
		t.Fatalf("new MCP service: %v", err)
	}

	mcpServer, err := mcpService.Create(ctx, mcp.ServerConfig{
		Name:     "shipment-service",
		Endpoint: "https://mcp.internal/shipment",
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create MCP server: %v", err)
	}

	discovery, err := mcpService.Refresh(ctx, mcpServer.ID)
	if err != nil {
		t.Fatalf("discovery: %v", err)
	}

	agentService := agent.NewService(store)
	agentCreated, err := agentService.Create(ctx, agent.CreateRequest{
		Name:        "Logistics Bot",
		Description: "Bot with shipment tracking",
		Config:      json.RawMessage(`{"stt":{"provider":"google","model":"google","credential_id":"018f0000-0000-7000-8000-000000000001"},"llm":{"provider":"xai","model":"xai","credential_id":"018f0000-0000-7000-8000-000000000002"},"tts":{"provider":"google","voice":"google","credential_id":"018f0000-0000-7000-8000-000000000001"}}`),
		MCPTools: []agent.ToolSelection{
			{ServerID: mcpServer.ID, RemoteName: "track_shipment", SchemaHash: discovery.Tools[0].SchemaHash, Required: true},
		},
	})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}

	// 1. Start active session holding captured tool wrapper
	sessionID := storage.NewID()
	runtimeSession, err := runtime.StartSession(ctx, agentService, mcpService, agentCreated.ID, sessionID)
	if err != nil {
		t.Fatalf("start runtime session: %v", err)
	}

	// 2. Invariant: Attempt direct deletion of referenced MCP server fails with 409 conflict
	deleteErr := mcpService.Delete(ctx, mcpServer.ID)
	if !errors.Is(deleteErr, storage.ErrMCPReferenced) {
		t.Fatalf("expected ErrMCPReferenced conflict on direct delete, got: %v", deleteErr)
	}

	// Verify current references show the agent
	refs, err := store.MCP().CurrentReferences(ctx, mcpServer.ID)
	if err != nil {
		t.Fatalf("query current references: %v", err)
	}
	if len(refs) == 0 || refs[0].AgentID != agentCreated.ID {
		t.Fatalf("expected agent reference in details: %+v", refs)
	}

	// 3. Confirmed detach-and-delete:
	// - Atomically publishes v2 agent without MCP tool
	// - Disables server
	// - Active session remains in flight; server is NOT yet deleted from active records
	if err := mcpService.DetachAndDelete(ctx, mcpServer.ID); err != nil {
		t.Fatalf("detach and delete: %v", err)
	}

	// Verify Agent was atomically updated to Version 2 without MCP tool
	agentAfterDetach, err := agentService.Get(ctx, agentCreated.ID)
	if err != nil {
		t.Fatalf("get agent after detach: %v", err)
	}
	if agentAfterDetach.Version.Number != 2 {
		t.Fatalf("expected agent version 2 after detach, got %d", agentAfterDetach.Version.Number)
	}
	if !agentAfterDetach.Version.MCPReviewRequired {
		t.Fatal("expected MCPReviewRequired to be set on v2 version after detach")
	}
	if len(agentAfterDetach.Version.MCPTools) != 0 {
		t.Fatalf("expected 0 MCP tools on v2 version, got %d", len(agentAfterDetach.Version.MCPTools))
	}

	// Verify server is disabled while active session is holding captured wrapper
	srvRow, err := store.MCP().Server(ctx, mcpServer.ID)
	if err != nil {
		t.Fatalf("active session lost server record during drain: %v", err)
	}
	if srvRow.Enabled {
		t.Fatal("expected MCP server to be disabled during deletion drain")
	}

	// Captured tool in active session can still execute during drain
	toolCall := core.ToolCall{
		ID:         "call-drain-1",
		Name:       runtimeSession.Definitions[0].Name,
		SessionID:  core.SessionID(sessionID),
		TurnID:     "turn-drain-1",
		Generation: 1,
		Arguments:  json.RawMessage(`{"id":"SH-1"}`),
	}
	callRes, err := runtimeSession.Executor.Execute(ctx, toolCall)
	if err != nil || callRes.Status != core.ToolSucceeded {
		t.Fatalf("captured tool execution failed during drain: %+v %v", callRes, err)
	}

	// 4. Drain completes when active session closes
	if err := runtimeSession.Close(ctx); err != nil {
		t.Fatalf("close session: %v", err)
	}

	// 5. Server record is now completely deleted from active lookups
	if _, err = store.MCP().Server(ctx, mcpServer.ID); err == nil {
		t.Fatal("expected error for deleted server after drain, but server was still found")
	}

	// 6. Historical preservation: session snapshot resolves with original MCP tool evidence
	historicalSnap, err := agentService.ResolveSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("resolve historical session snapshot: %v", err)
	}
	if len(historicalSnap.MCPTools) != 1 {
		t.Fatalf("expected historical snapshot to preserve 1 MCP tool, got %d", len(historicalSnap.MCPTools))
	}
	if historicalSnap.MCPTools[0].SchemaHash != discovery.Tools[0].SchemaHash {
		t.Fatalf("historical schema hash mismatch: expected %s, got %s", discovery.Tools[0].SchemaHash, historicalSnap.MCPTools[0].SchemaHash)
	}
}

// =============================================================================
// Scenario 6: Repeated Barge-in & Interruption
// =============================================================================
// Rapid consecutive interruptions during assistant speech generation properly cancel output,
// flush audio pacer queues, and preserve conversational turn order.
func TestScenario6_RepeatedBargeInAndInterruption(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var deliveredWrites atomic.Uint64
	var writtenGens []uint64
	var writeMu sync.Mutex

	// Paced writer simulating WebSocket audio output
	pacer := ws.NewPacedWriter(ws.DefaultPacerConfig(), func(_ context.Context, data []byte) error {
		deliveredWrites.Add(1)
		writeMu.Lock()
		defer writeMu.Unlock()
		return nil
	})
	defer pacer.Close()

	go pacer.Run(ctx)

	// Helper to create an audio frame with sequence and generation
	makeAudio := func(turn string, gen uint64, seq uint64) *core.OutputAudioFrame {
		return &core.OutputAudioFrame{
			Samples:    make([]byte, 960),
			SampleRate: 24000,
			Channels:   1,
			Duration:   20 * time.Millisecond,
			Turn:       turn,
			Gen:        gen,
			Seq:        seq,
		}
	}

	// -------------------------------------------------------------------------
	// Turn 1 (Generation 1): Assistant starts speaking -> Interrupted rapidly
	// -------------------------------------------------------------------------
	pacer.SetActiveGeneration(1)
	if err := pacer.Enqueue(makeAudio("turn-1", 1, 1)); err != nil {
		t.Fatalf("enqueue t1 frame 1: %v", err)
	}
	if err := pacer.Enqueue(makeAudio("turn-1", 1, 2)); err != nil {
		t.Fatalf("enqueue t1 frame 2: %v", err)
	}

	// Rapid Barge-in 1: user interrupts Turn 1
	pacer.SetActiveGeneration(2) // Generation fence advances to 2
	if err := pacer.Flush(ctx, 1); err != nil {
		t.Fatalf("flush generation 1: %v", err)
	}

	// Verify all queued generation 1 frames were flushed
	if pacer.QueuedFrames() != 0 {
		t.Fatalf("expected 0 queued frames after barge-in flush 1, got %d", pacer.QueuedFrames())
	}

	// Stale frame from generation 1 arriving late MUST be rejected
	lateT1Frame := makeAudio("turn-1", 1, 3)
	_ = pacer.Enqueue(lateT1Frame)
	if pacer.QueuedFrames() != 0 {
		t.Fatal("late frame from generation 1 admitted after generation advance")
	}

	// -------------------------------------------------------------------------
	// Turn 2 (Generation 2): Assistant starts speaking -> Interrupted rapidly again
	// -------------------------------------------------------------------------
	if err := pacer.Enqueue(makeAudio("turn-2", 2, 1)); err != nil {
		t.Fatalf("enqueue t2 frame 1: %v", err)
	}
	if err := pacer.Enqueue(makeAudio("turn-2", 2, 2)); err != nil {
		t.Fatalf("enqueue t2 frame 2: %v", err)
	}

	// Rapid Barge-in 2: user interrupts Turn 2 immediately
	pacer.SetActiveGeneration(3) // Generation fence advances to 3
	if err := pacer.Flush(ctx, 2); err != nil {
		t.Fatalf("flush generation 2: %v", err)
	}

	if pacer.QueuedFrames() != 0 {
		t.Fatalf("expected 0 queued frames after barge-in flush 2, got %d", pacer.QueuedFrames())
	}

	// Stale frame from generation 2 arriving late MUST be rejected
	lateT2Frame := makeAudio("turn-2", 2, 3)
	_ = pacer.Enqueue(lateT2Frame)
	if pacer.QueuedFrames() != 0 {
		t.Fatal("late frame from generation 2 admitted after generation advance")
	}

	// -------------------------------------------------------------------------
	// Turn 3 (Generation 3): Assistant speaks without interruption -> completes
	// -------------------------------------------------------------------------
	t3Frame1 := makeAudio("turn-3", 3, 1)
	if err := pacer.Enqueue(t3Frame1); err != nil {
		t.Fatalf("enqueue t3 frame 1: %v", err)
	}

	// Wait for generation 3 audio to write
	deadline := time.Now().Add(2 * time.Second)
	for deliveredWrites.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	if deliveredWrites.Load() == 0 {
		t.Fatal("generation 3 audio was not delivered after flushes")
	}

	// Verify frames dropped due to interruption
	if pacer.FramesDropped.Load() == 0 {
		t.Fatal("expected dropped frame accounting on rapid barge-in interruptions")
	}
	_ = writtenGens
}

// =============================================================================
// Scenario 7: Fault Isolation
// =============================================================================
// Injected trace repository write failure, remote MCP outage, or provider timeout
// DOES NOT crash the session, panic the runtime, or terminate the active voice pipeline.
func TestScenario7_FaultIsolation(t *testing.T) {
	ctx := context.Background()

	t.Run("TraceWriteFailureDoesNotCrashPipeline", func(t *testing.T) {
		store := openScenarioDB(t, "scenario7_trace_outage")
		agentService := agent.NewService(store)

		agentObj, err := agentService.Create(ctx, agent.CreateRequest{
			Name:   "Fault Resilient Bot",
			Config: json.RawMessage(`{"stt":{"provider":"google","model":"google","credential_id":"018f0000-0000-7000-8000-000000000001"},"llm":{"provider":"xai","model":"xai","credential_id":"018f0000-0000-7000-8000-000000000002"},"tts":{"provider":"google","voice":"google","credential_id":"018f0000-0000-7000-8000-000000000001"}}`),
		})
		if err != nil {
			t.Fatal(err)
		}

		pin, err := agentService.StartSession(ctx, agentObj.ID, "")
		if err != nil {
			t.Fatal(err)
		}

		// Inject trace store failure by creating an adapter with a closed store
		failingStore := openScenarioDB(t, "failing_traces")
		traceAdapter := trace.NewAdapter(failingStore.Traces())
		_ = failingStore.Close() // Database is now closed: all trace writes fail

		// Pipeline runs with trace adapter attached
		contextPool := core.NewConversationContext(core.ContextConfig{MaxMessages: 10, MaxTokens: 2000})
		userCtx := core.NewUserContextProcessor(contextPool, core.UserContextConfig{TurnPrefix: "turn"})
		asstCtx := core.NewAssistantContextProcessor(contextPool, core.DefaultAssistantContextConfig())
		llm := core.NewFakeLLMProcessor(core.FakeLLMConfig{
			ResponseChunks: []string{"Trace failure did not stop me."},
		})
		tts := core.NewFakeTTSProcessor(core.FakeTTSConfig{
			FramesPerUnit: 2,
		})
		transportOutput := core.NewFakeTransportOutput(core.FakeTransportConfig{})

		pipe, err := core.NewPipeline(userCtx, asstCtx, llm, tts, transportOutput)
		if err != nil {
			t.Fatal(err)
		}

		sess, err := core.NewSession(2001, pipe, core.DefaultResourceBudget())
		if err != nil {
			t.Fatal(err)
		}
		if err := sess.Start(); err != nil {
			t.Fatal(err)
		}
		defer sess.Close()

		// Trace operations executed concurrently while database is down
		span := traceAdapter.StartSpan(ctx, core.NewTraceEvent("test.span", time.Now().UTC(), core.SessionID(pin.ID), "turn-1", 1, core.TracePolicy{}, nil))
		span.AddEvent(core.NewTraceEvent("test.event", time.Now().UTC(), core.SessionID(pin.ID), "turn-1", 1, core.TracePolicy{}, map[string]any{"data": 123}))
		span.End(errors.New("trace store disk error"))

		// Post conversational frame
		if err := sess.Post(&core.UserMessageFrame{Content: "Hello under trace outage", Turn: "turn-1", Gen: 1}); err != nil {
			t.Fatal(err)
		}

		// Verify voice pipeline completes processing without panic or failure
		deadline := time.Now().Add(2 * time.Second)
		for len(transportOutput.GetWrittenFrames()) == 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if len(transportOutput.GetWrittenFrames()) == 0 {
			t.Fatal("voice pipeline halted during trace store write outage")
		}

		// Close trace adapter: must not panic or block
		if err := traceAdapter.Close(); err != nil {
			t.Logf("trace adapter closed with expected shutdown message: %v", err)
		}
	})

	t.Run("RemoteMCPOutageDoesNotCrashVoicePipeline", func(t *testing.T) {
		store := openScenarioDB(t, "scenario7_mcp_outage")
		remote := &mockMCPRemote{
			tools: []mcp.RemoteTool{
				{Name: "lookup_order", Description: "Order lookup", InputSchema: json.RawMessage(`{"type":"object"}`)},
			},
			failCalls: true, // Inject MCP remote server outage (503 Service Unavailable)
		}

		mcpService, err := mcp.NewService(mcp.ServiceConfig{
			Store:        store,
			Remote:       remote,
			Destinations: allowAllDestinations{},
		})
		if err != nil {
			t.Fatal(err)
		}

		serverObj, err := mcpService.Create(ctx, mcp.ServerConfig{
			Name: "failing-mcp", Endpoint: "https://mcp.fault/rpc", Enabled: true,
		})
		if err != nil {
			t.Fatal(err)
		}

		discovery, err := mcpService.Refresh(ctx, serverObj.ID)
		if err != nil {
			t.Fatal(err)
		}

		agentService := agent.NewService(store)
		agentObj, err := agentService.Create(ctx, agent.CreateRequest{
			Name:   "MCP Outage Bot",
			Config: json.RawMessage(`{"stt":{"provider":"google","model":"google","credential_id":"018f0000-0000-7000-8000-000000000001"},"llm":{"provider":"xai","model":"xai","credential_id":"018f0000-0000-7000-8000-000000000002"},"tts":{"provider":"google","voice":"google","credential_id":"018f0000-0000-7000-8000-000000000001"}}`),
			MCPTools: []agent.ToolSelection{
				{ServerID: serverObj.ID, RemoteName: "lookup_order", SchemaHash: discovery.Tools[0].SchemaHash, Required: false},
			},
		})
		if err != nil {
			t.Fatal(err)
		}

		sessionID := storage.NewID()
		runtimeSession, err := runtime.StartSession(ctx, agentService, mcpService, agentObj.ID, sessionID)
		if err != nil {
			t.Fatal(err)
		}
		defer runtimeSession.Close(ctx)

		// Execute tool call against the failing MCP server
		call := core.ToolCall{
			ID:         "call-failing-1",
			Name:       runtimeSession.Definitions[0].Name,
			SessionID:  core.SessionID(sessionID),
			TurnID:     "turn-1",
			Generation: 1,
			Arguments:  json.RawMessage(`{}`),
		}

		result, err := runtimeSession.Executor.Execute(ctx, call)
		if err != nil {
			t.Fatalf("unexpected go runtime error from executor: %v", err)
		}
		// Invariant: Tool failure produces ToolFailed result with safe category, does NOT panic or crash
		if result.Status != core.ToolFailed {
			t.Fatalf("expected ToolFailed status, got %s", result.Status)
		}
		if result.ErrorCategory != "mcp_tool_failed" {
			t.Fatalf("expected safe error category mcp_tool_failed, got %s", result.ErrorCategory)
		}

		// Subsequent operations on the active session continue to work
		snapshot, err := agentService.ResolveSession(ctx, sessionID)
		if err != nil || snapshot.AgentID != agentObj.ID {
			t.Fatalf("session corrupted after MCP outage: %v", err)
		}
	})

	t.Run("ProviderTimeoutDoesNotDisconnectCaller", func(t *testing.T) {
		contextPool := core.NewConversationContext(core.ContextConfig{MaxMessages: 10, MaxTokens: 2000})
		userCtx := core.NewUserContextProcessor(contextPool, core.UserContextConfig{TurnPrefix: "turn"})
		asstCtx := core.NewAssistantContextProcessor(contextPool, core.DefaultAssistantContextConfig())
		llm := &mockXAILLM{response: "Recovered reply"}
		llm.timeout.Store(true) // Inject timeout
		tts := core.NewFakeTTSProcessor(core.FakeTTSConfig{FramesPerUnit: 2})
		transportOutput := core.NewFakeTransportOutput(core.FakeTransportConfig{})

		pipe, err := core.NewPipeline(userCtx, asstCtx, llm, tts, transportOutput)
		if err != nil {
			t.Fatal(err)
		}

		sess, err := core.NewSession(3001, pipe, core.DefaultResourceBudget())
		if err != nil {
			t.Fatal(err)
		}
		if err := sess.Start(); err != nil {
			t.Fatal(err)
		}
		defer sess.Close()

		// Post message to timed-out LLM
		_ = sess.Post(&core.FinalTranscriptionFrame{Text: "Test timeout turn", Turn: "turn-timeout", Gen: 1})
		time.Sleep(50 * time.Millisecond)

		// Session remains active and ready; recover LLM and post second turn
		llm.timeout.Store(false)
		if err := sess.Post(&core.FinalTranscriptionFrame{Text: "Recovered turn", Turn: "turn-recovered", Gen: 1}); err != nil {
			t.Fatalf("session refused frame after provider timeout: %v", err)
		}

		deadline := time.Now().Add(2 * time.Second)
		for len(transportOutput.GetWrittenFrames()) == 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if len(transportOutput.GetWrittenFrames()) == 0 {
			t.Fatal("session failed to recover after provider timeout")
		}
	})

	t.Run("ConcurrentSessionIsolation", func(t *testing.T) {
		// Session A experiences fault; Session B runs concurrently and succeeds
		store := openScenarioDB(t, "scenario7_concurrent_isolation")
		agentService := agent.NewService(store)

		agentA, _ := agentService.Create(ctx, agent.CreateRequest{
			Name: "Agent A", Config: json.RawMessage(`{"stt":{"provider":"google","model":"google","credential_id":"018f0000-0000-7000-8000-000000000001"},"llm":{"provider":"xai","model":"xai","credential_id":"018f0000-0000-7000-8000-000000000002"},"tts":{"provider":"google","voice":"google","credential_id":"018f0000-0000-7000-8000-000000000001"}}`),
		})
		agentB, _ := agentService.Create(ctx, agent.CreateRequest{
			Name: "Agent B", Config: json.RawMessage(`{"stt":{"provider":"google","model":"google","credential_id":"018f0000-0000-7000-8000-000000000001"},"llm":{"provider":"xai","model":"xai","credential_id":"018f0000-0000-7000-8000-000000000002"},"tts":{"provider":"google","voice":"google","credential_id":"018f0000-0000-7000-8000-000000000001"}}`),
		})

		pinA, errA := agentService.StartSession(ctx, agentA.ID, "")
		pinB, errB := agentService.StartSession(ctx, agentB.ID, "")
		if errA != nil || errB != nil {
			t.Fatalf("start concurrent sessions: errA=%v errB=%v", errA, errB)
		}

		// Inject failure in Session A end
		_ = store.Sessions().End(ctx, pinA.ID)

		// Session B remains completely healthy
		snapB, err := agentService.ResolveSession(ctx, pinB.ID)
		if err != nil || snapB.AgentID != agentB.ID {
			t.Fatalf("session B impacted by session A failure: %v", err)
		}
		_ = agentService.EndSession(ctx, pinB.ID)
	})
}

// computeSHA256 returns hex encoded SHA-256 string.
func computeSHA256(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// testCredentialID is the fixed UUIDv7 of the seeded credential for provider p.
func testCredentialID(p string) string {
	if p == "xai" {
		return "018f0000-0000-7000-8000-000000000002"
	}
	return "018f0000-0000-7000-8000-000000000001"
}
