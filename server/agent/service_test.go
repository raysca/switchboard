package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"govoice/server/storage"
)

func testService(t *testing.T) *Service {
	t.Helper()
	s, err := storage.Open(context.Background(), storage.Config{Dialect: storage.SQLite, DSN: t.TempDir() + "/agents.db"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	seedCredentials(t, s, "google", "xai")
	return NewService(s)
}

// seedCredentials stores one credential per provider so complete agents are
// ready; readiness requires a resolvable credential for every stage.
func seedCredentials(t *testing.T, s *storage.Store, providers ...string) {
	t.Helper()
	for _, p := range providers {
		if err := s.Credentials().Put(context.Background(), storage.Credential{ID: testCredentialID(p), Provider: p, MaskedSuffix: "abcd",
			ValidationState: "unvalidated", CapabilitiesJSON: "[]", Ciphertext: []byte("x"), KeyID: "k"}); err != nil {
			t.Fatal(err)
		}
	}
}

func validConfig() json.RawMessage {
	return json.RawMessage(`{"stt":{"provider":"google","model":"google.stt","credential_id":"018f0000-0000-7000-8000-000000000001"},"llm":{"provider":"google","model":"google.llm","credential_id":"018f0000-0000-7000-8000-000000000001"},"tts":{"provider":"google","voice":"google.tts","credential_id":"018f0000-0000-7000-8000-000000000001"}}`)
}

func TestCreateListGetAndUpdatePublishImmutableVersions(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()
	a, err := svc.Create(ctx, CreateRequest{Name: "Support", Config: validConfig()})
	if err != nil {
		t.Fatal(err)
	}
	if a.Version == nil || a.Version.Number != 1 || a.Readiness.State != ReadinessReady {
		t.Fatalf("created agent=%+v", a)
	}
	changed := json.RawMessage(`{"stt":{"provider":"google","model":"google.stt","credential_id":"018f0000-0000-7000-8000-000000000001"},"llm":{"provider":"google","model":"google.llm","credential_id":"018f0000-0000-7000-8000-000000000001"},"tts":{"provider":"google","voice":"google.tts","credential_id":"018f0000-0000-7000-8000-000000000001"}}`)
	updated, err := svc.Update(ctx, a.ID, UpdateRequest{Name: stringPointer("Support v2"), Config: changed})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Version.Number != 2 || updated.Version.ConfigHash != a.Version.ConfigHash {
		t.Fatalf("updated version=%+v, initial=%+v", updated.Version, a.Version)
	}
	old, err := svc.GetVersion(ctx, a.ID, a.Version.ID)
	if err != nil || string(old.Config) != string(a.Version.Config) {
		t.Fatalf("old version changed: %+v err=%v", old, err)
	}
	items, err := svc.List(ctx, 10, 0)
	if err != nil || len(items) != 1 || items[0].Name != "Support v2" {
		t.Fatalf("list=%+v err=%v", items, err)
	}
}

// Agents are attached to providers at creation; an incomplete config is
// rejected with bounded, content-free reasons.
func TestCreateRequiresAttachedProvidersWithSafeReasons(t *testing.T) {
	svc := testService(t)
	_, err := svc.Create(context.Background(), CreateRequest{Name: "Incomplete", Config: json.RawMessage(`{"llm":{"system_prompt":"private user text"}}`)})
	if !errors.Is(err, ErrNotAttached) || strings.Contains(err.Error(), "private user text") {
		t.Fatalf("err=%v", err)
	}
	for _, want := range []string{"missing_stt", "missing_llm", "missing_tts"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err %q lacks %s", err, want)
		}
	}
	bad := `{"stt":{"provider":"google","credential_id":"018f0000-0000-7000-8000-000000000002"},"llm":{"provider":"google","model":"m","credential_id":"018f0000-0000-7000-8000-0000000000ff"},"tts":{"provider":"google"}}`
	_, err = svc.Create(context.Background(), CreateRequest{Name: "Wrong creds", Config: json.RawMessage(bad)})
	for _, want := range []string{"invalid_credential_stt", "missing_credential_llm", "missing_credential_tts"} {
		if !errors.Is(err, ErrNotAttached) || !strings.Contains(err.Error(), want) {
			t.Fatalf("err %v lacks %s", err, want)
		}
	}
}

func TestDuplicateArchiveAndVersionScopedSnapshots(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()
	a, err := svc.Create(ctx, CreateRequest{Name: "Original", Config: validConfig()})
	if err != nil {
		t.Fatal(err)
	}
	session, err := svc.StartSession(ctx, a.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Update(ctx, a.ID, UpdateRequest{Config: json.RawMessage(`{"stt":{"provider":"google","model":"x","credential_id":"018f0000-0000-7000-8000-000000000001"},"llm":{"provider":"google","model":"y","credential_id":"018f0000-0000-7000-8000-000000000001"},"tts":{"provider":"google","voice":"z","credential_id":"018f0000-0000-7000-8000-000000000001"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := svc.ResolveSession(ctx, session.ID)
	if err != nil || pinned.Version.ID != session.AgentVersionID || pinned.Version.ConfigHash != session.ConfigHash || string(pinned.Version.Config) != string(a.Version.Config) {
		t.Fatalf("pinned snapshot=%+v session=%+v err=%v", pinned, session, err)
	}
	copy, err := svc.Duplicate(ctx, a.ID, "Copy")
	current, getErr := svc.Get(ctx, a.ID)
	if err != nil || getErr != nil || copy.ID == a.ID || copy.Version.ConfigHash != current.Version.ConfigHash {
		t.Fatalf("duplicate=%+v err=%v", copy, err)
	}
	if err := svc.Archive(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get(ctx, a.ID); err != nil {
		t.Fatalf("archived agent should remain readable: %v", err)
	}
	if _, err := svc.StartSession(ctx, a.ID, "new"); err == nil {
		t.Fatal("archived agent accepted a new session")
	}
}

func TestInvalidUpdateDoesNotChangeMutableAgentFields(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()
	created, err := svc.Create(ctx, CreateRequest{Name: "Before", Config: validConfig()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Update(ctx, created.ID, UpdateRequest{Name: stringPointer("Must not persist"), Config: json.RawMessage(`[]`)}); err == nil {
		t.Fatal("array config accepted")
	}
	after, err := svc.Get(ctx, created.ID)
	if err != nil || after.Name != "Before" || after.Version.Number != 1 {
		t.Fatalf("invalid update partially persisted: %+v err=%v", after, err)
	}
}

func TestMetadataUpdatePreservesDisabledState(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()
	disabled := false
	created, err := svc.Create(ctx, CreateRequest{Name: "Before", Config: validConfig(), Enabled: &disabled})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := svc.Update(ctx, created.ID, UpdateRequest{Name: stringPointer("After")})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Enabled || updated.Readiness.State != ReadinessNotReady {
		t.Fatalf("metadata update changed enabled/readiness: %+v", updated)
	}
}

func TestMCPSelectionsAreVersionScopedAndSchemaDriftRequiresExplicitReview(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()
	store := svc.store
	m := storage.MCPServer{ID: storage.NewID(), Name: "orders", Endpoint: "https://mcp.example/rpc", Transport: "streamable-http", Enabled: true, ValidationState: "healthy"}
	if err := store.MCP().CreateServer(ctx, m); err != nil {
		t.Fatal(err)
	}
	first, err := store.MCP().AddDiscovery(ctx, m.ID, []storage.MCPTool{{RemoteName: "lookup", SchemaJSON: []byte(`{"type":"object","properties":{"id":{"type":"string"}}}`)}})
	if err != nil {
		t.Fatal(err)
	}
	a, err := svc.Create(ctx, CreateRequest{Name: "with mcp", Config: validConfig()})
	if err != nil {
		t.Fatal(err)
	}
	selection := ToolSelection{ServerID: m.ID, RemoteName: "lookup", SchemaHash: first.Tools[0].SchemaHash, Required: true}
	version1, err := svc.SetMCPTools(ctx, a.ID, []ToolSelection{selection})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := svc.ResolveCurrent(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	runtimeJSON, _ := json.Marshal(snapshot.MCPTools)
	session, err := svc.StartCapturedSession(ctx, snapshot, "", string(runtimeJSON), []ToolSelection{selection})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.MCP().AddDiscovery(ctx, m.ID, []storage.MCPTool{{RemoteName: "lookup", SchemaJSON: []byte(`{"type":"object","properties":{"id":{"type":"integer"}}}`)}})
	if err != nil {
		t.Fatal(err)
	}
	current, err := svc.Get(ctx, a.ID)
	if err != nil || current.Readiness.State != ReadinessNeedsToolReview {
		t.Fatalf("schema drift readiness=%+v err=%v", current.Readiness, err)
	}
	if _, err := svc.ResolveCurrent(ctx, a.ID); err == nil {
		t.Fatal("required schema drift accepted for new session")
	}
	if _, err := svc.SetMCPTools(ctx, a.ID, []ToolSelection{{ServerID: m.ID, RemoteName: "lookup", SchemaHash: second.Tools[0].SchemaHash, Required: true}}); err != nil {
		t.Fatal(err)
	}
	pinned, err := svc.ResolveSession(ctx, session.ID)
	if err != nil || pinned.Version.ID != version1.ID || len(pinned.MCPTools) != 1 || pinned.MCPTools[0].SchemaHash != first.Tools[0].SchemaHash {
		t.Fatalf("historical snapshot changed: %+v err=%v", pinned, err)
	}
}

func TestOptionalUnavailableMCPToolWarnsWithoutBlockingVoiceReadiness(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()
	store := svc.store
	m := storage.MCPServer{ID: storage.NewID(), Name: "optional", Endpoint: "https://mcp.example/rpc", Transport: "streamable-http", Enabled: true, ValidationState: "healthy"}
	if err := store.MCP().CreateServer(ctx, m); err != nil {
		t.Fatal(err)
	}
	d, err := store.MCP().AddDiscovery(ctx, m.ID, []storage.MCPTool{{RemoteName: "optional_tool", SchemaJSON: []byte(`{"type":"object"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	a, err := svc.Create(ctx, CreateRequest{Name: "optional", Config: validConfig()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetMCPTools(ctx, a.ID, []ToolSelection{{ServerID: m.ID, RemoteName: "optional_tool", SchemaHash: d.Tools[0].SchemaHash, Required: false}}); err != nil {
		t.Fatal(err)
	}
	if err := store.MCP().DisableServer(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	current, err := svc.Get(ctx, a.ID)
	if err != nil || current.Readiness.State != ReadinessWithToolWarning {
		t.Fatalf("optional disabled readiness=%+v err=%v", current.Readiness, err)
	}
	if _, err := svc.ResolveCurrent(ctx, a.ID); err != nil {
		t.Fatalf("optional tool blocked voice readiness: %v", err)
	}
}

func TestDetachRequiresExplicitMCPReviewAndHistoricalSelectionsRemainReadable(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()
	m := storage.MCPServer{ID: storage.NewID(), Name: "review", Endpoint: "https://mcp.example/rpc", Transport: "streamable-http", Enabled: true, ValidationState: "healthy"}
	if err := svc.store.MCP().CreateServer(ctx, m); err != nil {
		t.Fatal(err)
	}
	d, err := svc.store.MCP().AddDiscovery(ctx, m.ID, []storage.MCPTool{{RemoteName: "lookup", SchemaJSON: []byte(`{"type":"object"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	a, err := svc.Create(ctx, CreateRequest{Name: "review", Config: validConfig()})
	if err != nil {
		t.Fatal(err)
	}
	selection := ToolSelection{ServerID: m.ID, RemoteName: "lookup", SchemaHash: d.Tools[0].SchemaHash, Required: true}
	v1, err := svc.SetMCPTools(ctx, a.ID, []ToolSelection{selection})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.store.MCP().DetachAndDelete(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	current, err := svc.Get(ctx, a.ID)
	if err != nil || current.Readiness.State != ReadinessNeedsToolReview || len(current.Readiness.Reasons) == 0 {
		t.Fatalf("detached required tool lost review requirement: readiness=%+v err=%v", current.Readiness, err)
	}
	if _, err := svc.ResolveCurrent(ctx, a.ID); err == nil {
		t.Fatal("new session admitted before explicit review")
	}
	historical, err := svc.GetVersion(ctx, a.ID, v1.ID)
	if err != nil || len(historical.MCPTools) != 1 || historical.MCPTools[0] != selection {
		t.Fatalf("historical selection=%+v err=%v", historical.MCPTools, err)
	}
	if _, err := svc.Update(ctx, a.ID, UpdateRequest{Config: json.RawMessage(`{"stt":{"provider":"google","model":"s2","credential_id":"018f0000-0000-7000-8000-000000000001"},"llm":{"provider":"google","model":"l2","credential_id":"018f0000-0000-7000-8000-000000000001"},"tts":{"provider":"google","voice":"t2","credential_id":"018f0000-0000-7000-8000-000000000001"}}`)}); err != nil {
		t.Fatal(err)
	}
	if changed, err := svc.Get(ctx, a.ID); err != nil || changed.Readiness.State != ReadinessNeedsToolReview {
		t.Fatalf("configuration publish silently cleared MCP review: %+v err=%v", changed.Readiness, err)
	}
	updated, err := svc.SetMCPTools(ctx, a.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := svc.Get(ctx, a.ID); err != nil || got.Readiness.State != ReadinessReady || updated.MCPReviewRequired {
		t.Fatalf("explicit review did not clear marker: readiness=%+v version=%+v err=%v", got.Readiness, updated, err)
	}
}

func TestDuplicatePreservesMCPReviewRequirementUntilExplicitReview(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()
	m := storage.MCPServer{ID: storage.NewID(), Name: "duplicate-review", Endpoint: "https://mcp.example/rpc", Transport: "streamable-http", Enabled: true, ValidationState: "healthy"}
	if err := svc.store.MCP().CreateServer(ctx, m); err != nil {
		t.Fatal(err)
	}
	d, err := svc.store.MCP().AddDiscovery(ctx, m.ID, []storage.MCPTool{{RemoteName: "lookup", SchemaJSON: []byte(`{"type":"object"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	a, err := svc.Create(ctx, CreateRequest{Name: "review source", Config: validConfig()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetMCPTools(ctx, a.ID, []ToolSelection{{ServerID: m.ID, RemoteName: "lookup", SchemaHash: d.Tools[0].SchemaHash, Required: true}}); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.MCP().DetachAndDelete(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	duplicate, err := svc.Duplicate(ctx, a.ID, "review copy")
	if err != nil {
		t.Fatal(err)
	}
	if duplicate.Version == nil || !duplicate.Version.MCPReviewRequired || duplicate.Readiness.State != ReadinessNeedsToolReview {
		t.Fatalf("duplicate lost explicit-review requirement: version=%+v readiness=%+v", duplicate.Version, duplicate.Readiness)
	}
	if _, err := svc.ResolveCurrent(ctx, duplicate.ID); err == nil {
		t.Fatal("duplicate resolved before explicit MCP review")
	}
	if _, err := svc.StartSession(ctx, duplicate.ID, ""); err == nil {
		t.Fatal("duplicate admitted a session before explicit MCP review")
	}
	if _, err := svc.SetMCPTools(ctx, duplicate.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.StartSession(ctx, duplicate.ID, ""); err != nil {
		t.Fatalf("explicitly reviewed duplicate did not admit a session: %v", err)
	}
}

func TestCreateAndUpdatePublishAtomicallyOnDatabaseFailure(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()
	if _, err := svc.store.DB().Exec(`CREATE TRIGGER reject_agent_version BEFORE INSERT ON agent_versions WHEN NEW.config_json='{"fail":true}' BEGIN SELECT RAISE(ABORT, 'injected publish failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, CreateRequest{Name: "Rejected", Config: json.RawMessage(`{"fail":true}`)}); err == nil {
		t.Fatal("create with rejected initial version succeeded")
	}
	var count int
	if err := svc.store.DB().QueryRow("SELECT count(*) FROM agents WHERE name='Rejected'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed create left agent row count=%d err=%v", count, err)
	}
	created, err := svc.Create(ctx, CreateRequest{Name: "Before", Config: validConfig()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Update(ctx, created.ID, UpdateRequest{Name: stringPointer("After"), Config: json.RawMessage(`{"fail":true}`)}); err == nil {
		t.Fatal("update with rejected version succeeded")
	}
	after, err := svc.Get(ctx, created.ID)
	if err != nil || after.Name != "Before" || after.Version.Number != 1 || after.Version.ConfigHash != created.Version.ConfigHash {
		t.Fatalf("failed update partially committed: %+v err=%v", after, err)
	}
}

func TestConcurrentConfigUpdatesAllocateContiguousVersions(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()
	created, err := svc.Create(ctx, CreateRequest{Name: "Concurrent", Config: validConfig()})
	if err != nil {
		t.Fatal(err)
	}
	const updates = 8
	errCh := make(chan error, updates)
	for i := 0; i < updates; i++ {
		go func(i int) {
			config := json.RawMessage(fmt.Sprintf(`{"stt":{"provider":"google","credential_id":"018f0000-0000-7000-8000-000000000001"},"llm":{"provider":"google","model":"m%d","credential_id":"018f0000-0000-7000-8000-000000000001"},"tts":{"provider":"google","credential_id":"018f0000-0000-7000-8000-000000000001"}}`, i))
			_, err := svc.Update(ctx, created.ID, UpdateRequest{Config: config})
			errCh <- err
		}(i)
	}
	for i := 0; i < updates; i++ {
		if err := <-errCh; err != nil {
			t.Errorf("concurrent update failed: %v", err)
		}
	}
	current, err := svc.Get(ctx, created.ID)
	if err != nil || current.Version.Number != updates+1 {
		t.Fatalf("current version=%+v err=%v", current.Version, err)
	}
	var versions int
	if err := svc.store.DB().QueryRow("SELECT count(*) FROM agent_versions WHERE agent_id=?", created.ID).Scan(&versions); err != nil || versions != updates+1 {
		t.Fatalf("version rows=%d err=%v", versions, err)
	}
}

func TestConcurrentPartialUpdatesPreserveOmittedFields(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()
	created, err := svc.Create(ctx, CreateRequest{Name: "Original", Description: "initial", Config: validConfig()})
	if err != nil {
		t.Fatal(err)
	}
	disabled := false
	updates := []UpdateRequest{
		{Name: stringPointer("Renamed")},
		{Description: stringPointer("updated description")},
		{Enabled: &disabled},
	}
	errCh := make(chan error, len(updates))
	for _, update := range updates {
		update := update
		go func() {
			_, err := svc.Update(ctx, created.ID, update)
			errCh <- err
		}()
	}
	for range updates {
		if err := <-errCh; err != nil {
			t.Errorf("partial update failed: %v", err)
		}
	}
	got, err := svc.Get(ctx, created.ID)
	if err != nil || got.Name != "Renamed" || got.Description != "updated description" || got.Enabled {
		t.Fatalf("concurrent patches lost fields: %+v err=%v", got, err)
	}
}

func TestRuntimeResolutionRejectsIncompleteConfigAndCanUseReadinessEvaluator(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()
	// An attached credential that disappears later makes the agent uncallable.
	attached, err := svc.Create(ctx, CreateRequest{Name: "Loses credential", Config: validConfig()})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.store.Credentials().Delete(ctx, testCredentialID("google")); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ResolveCurrent(ctx, attached.ID); err == nil {
		t.Fatal("runtime resolved an agent whose credential was deleted")
	}
	if got, _ := svc.Get(ctx, attached.ID); got.Readiness.State != ReadinessNotReady || !strings.Contains(strings.Join(got.Readiness.Reasons, ","), "missing_credential_") {
		t.Fatalf("readiness after credential deletion = %+v", got.Readiness)
	}
	seedCredentials(t, svc.store, "google")
	readySvc := NewService(svc.store, ReadinessEvaluator(func(context.Context, string, json.RawMessage) (bool, error) {
		return false, nil
	}))
	complete, err := readySvc.Create(ctx, CreateRequest{Name: "External unavailable", Config: validConfig()})
	if err != nil {
		t.Fatal(err)
	}
	if complete.Readiness.State != ReadinessNotReady {
		t.Fatalf("external readiness was not reflected: %+v", complete.Readiness)
	}
	if _, err := readySvc.ResolveCurrent(ctx, complete.ID); err == nil {
		t.Fatal("runtime resolved agent rejected by readiness evaluator")
	}
	failedSvc := NewService(svc.store, ReadinessEvaluator(func(context.Context, string, json.RawMessage) (bool, error) {
		return false, fmt.Errorf("provider error contains secret sentinel")
	}))
	failed, err := failedSvc.Create(ctx, CreateRequest{Name: "Evaluator error", Config: validConfig()})
	if err != nil || failed.Readiness.State != ReadinessNotReady || len(failed.Readiness.Reasons) != 1 || failed.Readiness.Reasons[0] != "readiness_unavailable" {
		t.Fatalf("unsafe evaluator failure readiness=%+v err=%v", failed.Readiness, err)
	}
	if strings.Contains(strings.Join(failed.Readiness.Reasons, " "), "secret sentinel") {
		t.Fatal("readiness exposed external error detail")
	}
	successSvc := NewService(svc.store, ReadinessEvaluator(func(context.Context, string, json.RawMessage) (bool, error) {
		return true, nil
	}))
	fullyReady, err := successSvc.Create(ctx, CreateRequest{Name: "Validated", Config: validConfig()})
	if err != nil || fullyReady.Readiness.State != ReadinessReady {
		t.Fatalf("validated readiness=%+v err=%v", fullyReady.Readiness, err)
	}
	if _, err := successSvc.ResolveCurrent(ctx, fullyReady.ID); err != nil {
		t.Fatalf("validated agent could not resolve: %v", err)
	}
}

func stringPointer(value string) *string { return &value }

// testCredentialID is the fixed UUIDv7 of the seeded credential for provider p.
func testCredentialID(p string) string {
	if p == "xai" {
		return "018f0000-0000-7000-8000-000000000002"
	}
	return "018f0000-0000-7000-8000-000000000001"
}
