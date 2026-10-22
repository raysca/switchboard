package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"govoice/server/storage"
)

func testService(t *testing.T) *Service { return testServiceWith(t, ready()) }

func testServiceWith(t *testing.T, av Availability) *Service {
	t.Helper()
	s, err := storage.Open(context.Background(), storage.Config{Dialect: storage.SQLite, DSN: t.TempDir() + "/agents.db"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return NewService(s, av)
}

func validConfig() json.RawMessage {
	return json.RawMessage(`{"stt":{"provider":"google","model":"google.stt"},"llm":{"provider":"google","model":"google.llm"},"tts":{"provider":"google","voice":"google.tts"}}`)
}

func TestCreateListGetAndUpdatePublishImmutableVersions(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()
	a, err := svc.Create(ctx, CreateRequest{Name: "Support", Config: validConfig()})
	if err != nil {
		t.Fatal(err)
	}
	if a.Version == nil || a.Version.Number != 1 || a.Readiness.State != ReadinessReady {
		t.Fatalf("created flow=%+v", a)
	}
	changed := json.RawMessage(`{"stt":{"provider":"google","model":"google.stt"},"llm":{"provider":"google","model":"google.llm"},"tts":{"provider":"google","voice":"google.tts"}}`)
	updated, err := svc.Update(ctx, a.ID, UpdateRequest{Name: stringPointer("Support v2"), Config: changed})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Version.Number != 2 || updated.Version.ConfigHash != a.Version.ConfigHash {
		t.Fatalf("updated version=%+v, initial=%+v", updated.Version, a.Version)
	}
	old, err := svc.store.Agents().Version(ctx, a.Version.ID)
	if err != nil || string(old.ConfigJSON) != string(a.Version.Config) {
		t.Fatalf("old version changed: %+v err=%v", old, err)
	}
	items, err := svc.List(ctx, 10, 0)
	if err != nil || len(items) != 1 || items[0].Name != "Support v2" {
		t.Fatalf("list=%+v err=%v", items, err)
	}
}

// Flows are attached to providers at creation; an incomplete config is
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
	svc = testServiceWith(t, fakeAvail{"google/stt": {Reason: "missing_credential"}, "google/llm": {Reason: "unknown_provider"}})
	_, err = svc.Create(context.Background(), CreateRequest{Name: "Unavailable", Config: validConfig()})
	for _, want := range []string{"missing_credential_stt", "unknown_provider_llm"} {
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
	_, err = svc.Update(ctx, a.ID, UpdateRequest{Config: json.RawMessage(`{"stt":{"provider":"google","model":"x"},"llm":{"provider":"google","model":"y"},"tts":{"provider":"google","voice":"z"}}`)})
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
		t.Fatalf("archived flow should remain readable: %v", err)
	}
	if _, err := svc.StartSession(ctx, a.ID, "new"); err == nil {
		t.Fatal("archived flow accepted a new session")
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
	session, err := svc.StartCapturedSession(ctx, snapshot, "", string(runtimeJSON), "", []ToolSelection{selection})
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
	historicalRow, err := svc.store.Agents().Version(ctx, v1.ID)
	historical := toVersion(historicalRow)
	if err != nil || len(historical.MCPTools) != 1 || historical.MCPTools[0] != selection {
		t.Fatalf("historical selection=%+v err=%v", historical.MCPTools, err)
	}
	if _, err := svc.Update(ctx, a.ID, UpdateRequest{Config: json.RawMessage(`{"stt":{"provider":"google","model":"s2"},"llm":{"provider":"google","model":"l2"},"tts":{"provider":"google","voice":"t2"}}`)}); err != nil {
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
		t.Fatalf("failed create left flow row count=%d err=%v", count, err)
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
			config := json.RawMessage(fmt.Sprintf(`{"stt":{"provider":"google"},"llm":{"provider":"google","model":"m%d"},"tts":{"provider":"google"}}`, i))
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

func TestRuntimeResolutionFollowsProviderAvailability(t *testing.T) {
	av := fakeAvail{}
	svc := testServiceWith(t, av)
	ctx := context.Background()
	a, err := svc.Create(ctx, CreateRequest{Name: "Env disappears", Config: validConfig()})
	if err != nil {
		t.Fatal(err)
	}
	av["google/stt"] = Unavailable{Reason: "missing_credential", MissingEnv: []string{"GOOGLE_APPLICATION_CREDENTIALS"}}
	if _, err := svc.ResolveCurrent(ctx, a.ID); err == nil {
		t.Fatal("runtime resolved an flow whose provider lost its credentials")
	}
	got, _ := svc.Get(ctx, a.ID)
	if got.Readiness.State != ReadinessNotReady || !strings.Contains(strings.Join(got.Readiness.Reasons, ","), "missing_credential_stt") ||
		len(got.Readiness.MissingEnv) != 1 || got.Readiness.MissingEnv[0] != "GOOGLE_APPLICATION_CREDENTIALS" {
		t.Fatalf("readiness = %+v", got.Readiness)
	}
	delete(av, "google/stt")
	if _, err := svc.ResolveCurrent(ctx, a.ID); err != nil {
		t.Fatalf("flow did not recover: %v", err)
	}
}

func stringPointer(value string) *string { return &value }

// An earlier publish can be read back by number, so a client can show it or publish it again.
func TestVersionByNumberReturnsAnEarlierPublish(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()
	a, err := svc.Create(ctx, CreateRequest{Name: "Support", Config: validConfig()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Update(ctx, a.ID, UpdateRequest{Config: validConfig()}); err != nil {
		t.Fatal(err)
	}
	first, err := svc.VersionByNumber(ctx, a.ID, 1)
	if err != nil || first.Number != 1 || first.ConfigHash != a.Version.ConfigHash {
		t.Fatalf("version 1 = %+v err=%v, created = %+v", first, err, a.Version)
	}
	if second, err := svc.VersionByNumber(ctx, a.ID, 2); err != nil || second.Number != 2 {
		t.Fatalf("version 2 = %+v err=%v", second, err)
	}
	if _, err := svc.VersionByNumber(ctx, a.ID, 3); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("a version that was never published should be sql.ErrNoRows, got %v", err)
	}
}

func TestPublishRefusesCallWideSettingsOnANonStartStep(t *testing.T) {
	svc := testServiceWith(t, ready())
	front := `{"stt":{"provider":"google","model":"google.stt"},"llm":{"provider":"google","model":"google.llm"},"tts":{"provider":"google","voice":"google.tts"}}`
	mk := func(frontExtra, salesTTS, salesExtra string) json.RawMessage {
		front := front[:len(front)-1] + frontExtra + "}"
		return json.RawMessage(`{"start":"front","steps":[{"id":"front","name":"Front","config":` + front + `},` +
			`{"id":"sales","name":"Sales","config":{"llm":{"provider":"google","model":"google.llm"},"tts":{"provider":"google","voice":"google.tts"` + salesTTS + `}` + salesExtra + `}}],` +
			`"routes":[{"id":"e1","from":"front","to":"sales","condition":"wants to buy"}]}`)
	}
	for name, tc := range map[string]struct {
		cfg  json.RawMessage
		want string
	}{
		"clean_text": {mk("", `,"clean_text":false`, ""), `step "sales": tts.clean_text applies to the whole call; set it on the start step`},
		"failover":   {mk("", "", `,"failover":{"on":["timeout"]}`), `step "sales": failover applies to the whole call; set it on the start step`},
	} {
		_, err := svc.Create(context.Background(), CreateRequest{Name: name, Config: tc.cfg})
		if !errors.Is(err, ErrInvalidConfig) || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err=%v, want %q", name, err, tc.want)
		}
	}
	// the start step may carry both
	if _, err := svc.Create(context.Background(), CreateRequest{Name: "ok", Config: mk(`,"failover":{"on":["timeout"]}`, "", "")}); err != nil {
		t.Fatalf("start-step failover: %v", err)
	}
	start := front[:len(front)-1] + `,"failover":{"on":["timeout"]}}`
	start = strings.Replace(start, `"voice":"google.tts"}`, `"voice":"google.tts","clean_text":false}`, 1)
	cfg := json.RawMessage(`{"start":"front","steps":[{"id":"front","name":"Front","config":` + start + `},{"id":"sales","name":"Sales","config":{"llm":{"provider":"google","model":"google.llm"},"tts":{"provider":"google","voice":"google.tts"}}}],"routes":[{"id":"e1","from":"front","to":"sales","condition":"wants to buy"}]}`)
	if _, err := svc.Create(context.Background(), CreateRequest{Name: "ok2", Config: cfg}); err != nil {
		t.Fatalf("start-step both: %v", err)
	}
}

// A stored version may carry settings publish now refuses (a call-wide setting on a later step; a route sound the
// server dropped). Duplicate copies it anyway: the dead step settings are left out (they were ignored), the sound
// name is kept (the call just runs without it).
func TestDuplicateCopiesALegacyStoredVersion(t *testing.T) {
	svc := testServiceWith(t, &fakePolicy{cues: []string{"chime"}})
	ctx := context.Background()
	routed := json.RawMessage(strings.Replace(twoStepCfg(""), `"routes":`, `"route_cue":{"name":"chime"},"routes":`, 1))
	src, err := svc.Create(ctx, CreateRequest{Name: "src", Config: routed})
	if err != nil {
		t.Fatal(err)
	}
	i := strings.LastIndex(string(routed), `"tts":{"provider":"cartesia","voice":"v"}}}`) // the sales step, the last one
	legacy := string(routed)[:i] + `"tts":{"provider":"cartesia","voice":"v","clean_text":false},"failover":{"on":["timeout"]}}}` + string(routed)[i+len(`"tts":{"provider":"cartesia","voice":"v"}}}`):]
	legacy = strings.Replace(legacy, `"chime"`, `"gone"`, 1)
	if _, err := svc.store.DB().Exec(`DROP TRIGGER agent_versions_immutable_update`); err != nil { // the test's own database: stands in for a version stored by an older binary
		t.Fatal(err)
	}
	if _, err := svc.store.DB().Exec(`UPDATE agent_versions SET config_json=? WHERE agent_id=?`, legacy, src.ID); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(legacy, `"failover"`) || !strings.Contains(legacy, `"gone"`) {
		t.Fatalf("legacy fixture lost its settings: %s", legacy)
	}
	cp, err := svc.Duplicate(ctx, src.ID, "copy")
	if err != nil {
		t.Fatalf("duplicate: %v", err)
	}
	g, err := Decode(cp.Version.Config)
	if err != nil {
		t.Fatal(err)
	}
	sales, _ := g.Step("sales")
	if sales.Config.TTS.CleanText != nil || len(sales.Config.Failover.On) != 0 {
		t.Fatalf("dead step settings were copied: %+v", sales.Config)
	}
	if g.RouteCue == nil || g.RouteCue.Name != "gone" {
		t.Fatalf("route cue = %+v, want the stored name", g.RouteCue)
	}
	// new edits are still checked
	if _, err := svc.Create(ctx, CreateRequest{Name: "new", Config: json.RawMessage(legacy)}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("a new publish of the legacy config must be refused: %v", err)
	}
}

func TestReadinessCountsAMissingMCPTokenVariable(t *testing.T) {
	for _, tc := range []struct {
		name     string
		required bool
		set      bool
		want     string
		state    string
		env      []string
	}{
		{"required missing", true, false, ReadinessToolUnavailable, "auth_env_missing", []string{"MCP_TOKEN_X"}},
		{"optional missing", false, false, ReadinessWithToolWarning, "auth_env_missing", []string{"MCP_TOKEN_X"}},
		{"set", true, true, ReadinessReady, "ready", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := testService(t)
			ctx := context.Background()
			svc.SetTokenEnv(func(string) bool { return tc.set })
			m := storage.MCPServer{ID: storage.NewID(), Name: "tok", Endpoint: "https://mcp.example/rpc", Transport: "streamable-http", AuthEnv: "MCP_TOKEN_X", Enabled: true, ValidationState: "healthy"}
			if err := svc.store.MCP().CreateServer(ctx, m); err != nil {
				t.Fatal(err)
			}
			d, err := svc.store.MCP().AddDiscovery(ctx, m.ID, []storage.MCPTool{
				{RemoteName: "a", SchemaJSON: []byte(`{"type":"object"}`)},
				{RemoteName: "b", SchemaJSON: []byte(`{"type":"object"}`)}})
			if err != nil {
				t.Fatal(err)
			}
			a, err := svc.Create(ctx, CreateRequest{Name: "tok", Config: validConfig()})
			if err != nil {
				t.Fatal(err)
			}
			sel := []ToolSelection{
				{ServerID: m.ID, RemoteName: "a", SchemaHash: d.Tools[0].SchemaHash, Required: tc.required},
				{ServerID: m.ID, RemoteName: "b", SchemaHash: d.Tools[1].SchemaHash, Required: tc.required}}
			if _, err := svc.SetMCPTools(ctx, a.ID, sel); err != nil {
				t.Fatal(err)
			}
			got, err := svc.Get(ctx, a.ID)
			if err != nil {
				t.Fatal(err)
			}
			r := got.Readiness
			if r.State != tc.want || r.ToolDependencies[0].State != tc.state || fmt.Sprint(r.MissingEnv) != fmt.Sprint(tc.env) {
				t.Fatalf("readiness=%+v", r)
			}
		})
	}
}
