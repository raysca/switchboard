package agent

import (
	"context"
	"encoding/json"
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
	return NewService(s)
}

func validConfig() json.RawMessage {
	return json.RawMessage(`{"stt":"google.stt","llm":"google.llm","tts":"google.tts"}`)
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
	changed := json.RawMessage(`{"tts":"google.tts","llm":"google.llm","stt":"google.stt"}`)
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

func TestReadinessHasBoundedSafeReasonsForInvalidConfig(t *testing.T) {
	svc := testService(t)
	a, err := svc.Create(context.Background(), CreateRequest{Name: "Incomplete", Config: json.RawMessage(`{"system_prompt":"private user text"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if a.Readiness.State != ReadinessNotReady || len(a.Readiness.Reasons) != 3 {
		t.Fatalf("readiness=%+v", a.Readiness)
	}
	for _, reason := range a.Readiness.Reasons {
		if len(reason) > 64 || reason == "private user text" {
			t.Fatalf("unsafe readiness reason %q", reason)
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
	_, err = svc.Update(ctx, a.ID, UpdateRequest{Config: json.RawMessage(`{"stt":"x","llm":"y","tts":"z"}`)})
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

func TestImportV1Pipeline(t *testing.T) {
	svc := testService(t)
	a, err := svc.ImportV1(context.Background(), "legacy", "Imported", "Existing V1 pipeline", V1Pipeline{STT: "google.stt", LLM: "xai.llm", TTS: "google.tts", MaxMessages: 20, MaxTokens: 200})
	if err != nil {
		t.Fatal(err)
	}
	var imported map[string]any
	if err := json.Unmarshal(a.Version.Config, &imported); err != nil {
		t.Fatal(err)
	}
	if imported["llm"] != "xai.llm" || imported["max_messages"] != float64(20) || imported["max_tokens"] != float64(200) {
		t.Fatalf("imported V1 config=%s", a.Version.Config)
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

func stringPointer(value string) *string { return &value }
