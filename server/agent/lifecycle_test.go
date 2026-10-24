package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"govoice/server/storage"
)

func configWithPrompt(prompt string) json.RawMessage {
	return json.RawMessage(`{"stt":{"provider":"google","model":"google.stt"},"llm":{"provider":"google","model":"google.llm","system_prompt":"` + prompt + `"},"tts":{"provider":"google","voice":"google.tts"}}`)
}

func TestVersionsListNewestFirstAndRestorePublishesACopy(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()
	a, err := svc.Create(ctx, CreateRequest{Name: "Support", Config: configWithPrompt("one")})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"two", "three"} {
		if _, err := svc.Update(ctx, a.ID, UpdateRequest{Config: configWithPrompt(p)}); err != nil {
			t.Fatal(err)
		}
	}
	list, err := svc.Versions(ctx, a.ID)
	if err != nil || len(list) != 3 || list[0].Number != 3 || !list[0].Current || list[1].Current || list[2].Number != 1 {
		t.Fatalf("versions = %+v %v", list, err)
	}

	restored, err := svc.Restore(ctx, a.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Version.Number != 4 || restored.Version.ConfigHash != list[2].ConfigHash {
		t.Fatalf("restored = v%d %s, want v4 with v1's hash %s", restored.Version.Number, restored.Version.ConfigHash, list[2].ConfigHash)
	}
	// history stays append-only: v1..v3 are untouched and the restore is a new version on top
	after, _ := svc.Versions(ctx, a.ID)
	if len(after) != 4 || after[0].Number != 4 || after[3].ConfigHash != list[2].ConfigHash || after[1].ConfigHash != list[0].ConfigHash {
		t.Fatalf("after = %+v", after)
	}
}

func TestRestoreRefusesTheCurrentVersionAnUnknownOneAndAnArchivedAgent(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()
	a, _ := svc.Create(ctx, CreateRequest{Name: "Support", Config: configWithPrompt("one")})
	_, _ = svc.Update(ctx, a.ID, UpdateRequest{Config: configWithPrompt("two")})
	if _, err := svc.Restore(ctx, a.ID, 2); !errors.Is(err, ErrAlreadyCurrent) {
		t.Fatalf("current: %v", err)
	}
	if _, err := svc.Restore(ctx, a.ID, 9); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown: %v", err)
	}
	if _, err := svc.Versions(ctx, "01a16a7b-0000-7000-8000-000000000000"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("versions of nothing: %v", err)
	}
	if err := svc.Archive(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Restore(ctx, a.ID, 1); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("an archived agent was restored or refused for another reason: %v", err)
	}
}

func TestUnarchiveBringsAnAgentBackDisabled(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()
	a, _ := svc.Create(ctx, CreateRequest{Name: "Support", Config: configWithPrompt("one")})
	_ = svc.Archive(ctx, a.ID)
	if got, _ := svc.Get(ctx, a.ID); !got.Archived || got.Enabled {
		t.Fatalf("archived: %+v", got)
	}
	if err := svc.Unarchive(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := svc.Get(ctx, a.ID)
	if got.Archived || got.Enabled {
		t.Fatalf("after unarchive archived=%v enabled=%v, want a disabled, active agent", got.Archived, got.Enabled)
	}
	if _, err := svc.Update(ctx, a.ID, UpdateRequest{Enabled: boolPointer(true)}); err != nil {
		t.Fatalf("enabling after unarchive: %v", err)
	}
	if err := svc.Unarchive(ctx, "01a16a7b-0000-7000-8000-000000000000"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unarchive of nothing: %v", err)
	}
}

func TestRestoreBringsBackTheVersionsToolsOrRefusesWhenTheyChanged(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()
	server := storage.MCPServer{ID: storage.NewID(), Name: "crm", Endpoint: "https://mcp.example/rpc", Transport: "streamable-http", Enabled: true}
	if err := svc.store.MCP().CreateServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	d, err := svc.store.MCP().AddDiscovery(ctx, server.ID, []storage.MCPTool{{RemoteName: "lookup", SchemaJSON: []byte(`{"type":"object"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	sel := []ToolSelection{{ServerID: server.ID, RemoteName: "lookup", SchemaHash: d.Tools[0].SchemaHash, Required: true}}
	a, err := svc.Create(ctx, CreateRequest{Name: "Support", Config: configWithPrompt("one"), MCPTools: sel})
	if err != nil {
		t.Fatal(err)
	}
	none := []ToolSelection{}
	if _, err := svc.Update(ctx, a.ID, UpdateRequest{Config: configWithPrompt("two"), MCPTools: &none}); err != nil {
		t.Fatal(err)
	}
	restored, err := svc.Restore(ctx, a.ID, 1)
	if err != nil || len(restored.Version.MCPTools) != 1 || restored.Version.MCPTools[0].RemoteName != "lookup" || !restored.Version.MCPTools[0].Required {
		t.Fatalf("restored tools = %+v %v", restored.Version, err)
	}
	// the server's tool changes: the old pin no longer matches, so restoring v1 is refused rather than published stale
	if _, err := svc.store.MCP().AddDiscovery(ctx, server.ID, []storage.MCPTool{{RemoteName: "lookup", SchemaJSON: []byte(`{"type":"object","properties":{"id":{"type":"string"}}}`)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Update(ctx, a.ID, UpdateRequest{Config: configWithPrompt("three"), MCPTools: &none}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Restore(ctx, a.ID, 1); !errors.Is(err, storage.ErrInvalidToolSelection) {
		t.Fatalf("stale tool pin: %v", err)
	}
}

// A version stored before call-wide settings moved to the start step is restored without the ones the server now
// refuses on a later step, as a copy of the agent already is.
func TestRestoreDropsCallWideSettingsKeptOnALaterStep(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()
	legacy := `{"start":"a","steps":[` +
		`{"id":"a","name":"Front","config":{"stt":{"provider":"google","model":"google.stt"},"llm":{"provider":"google","model":"google.llm"},"tts":{"provider":"google","voice":"google.tts","clean_text":false},"failover":{"on":["client_error"]}}},` +
		`{"id":"b","name":"Sales","config":{"llm":{"provider":"google","model":"google.llm"},"tts":{"provider":"google","voice":"google.tts","clean_text":false},"failover":{"on":["timeout"]}}}],` +
		`"handoffs":[{"id":"e1","from":"a","to":"b","condition":"buy"}]}`
	a := storage.Agent{ID: storage.NewID(), Name: "Old", Enabled: true}
	if err := svc.store.Agents().Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.store.Agents().MutateAndPublish(ctx, a.ID, storage.AgentMutation{}, []byte(legacy)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Update(ctx, a.ID, UpdateRequest{Config: configWithPrompt("new")}); err != nil {
		t.Fatal(err)
	}
	restored, err := svc.Restore(ctx, a.ID, 1)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	var cfg struct {
		Steps []struct {
			ID     string                     `json:"id"`
			Config map[string]json.RawMessage `json:"config"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(restored.Version.Config, &cfg); err != nil || len(cfg.Steps) != 2 {
		t.Fatalf("config = %s %v", restored.Version.Config, err)
	}
	if _, ok := cfg.Steps[0].Config["failover"]; !ok {
		t.Fatal("the start step lost its call-wide failover")
	}
	if _, ok := cfg.Steps[1].Config["failover"]; ok {
		t.Fatal("a later step kept a call-wide setting the server refuses")
	}
}
