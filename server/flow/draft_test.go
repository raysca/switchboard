package flow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"govoice/server/storage"
)

func draftConfig(prompt string) json.RawMessage {
	return json.RawMessage(`{"stt":{"provider":"google","model":"google.stt"},"llm":{"provider":"google","model":"google.llm","system_prompt":"` + prompt + `"},"tts":{"provider":"google","voice":"google.tts"}}`)
}

func publishedFlow(t *testing.T, svc *Service, prompt string) Flow {
	t.Helper()
	f, err := svc.Create(context.Background(), CreateRequest{Name: "Support", Config: draftConfig(prompt)})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestResolveDraftReturnsTheDraftOnTheCurrentVersion(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()
	f := publishedFlow(t, svc, "PUBLISHED")
	snap, readiness, err := svc.ResolveDraft(ctx, f.ID, draftConfig("DRAFT"), nil)
	if err != nil || readiness.State != ReadinessReady {
		t.Fatalf("readiness %+v err %v", readiness, err)
	}
	wantHash, _, _ := storage.ConfigHash(draftConfig("DRAFT"))
	if !snap.Draft || snap.FlowID != f.ID || snap.Version.ID != f.Version.ID || snap.Version.Number != f.Version.Number {
		t.Fatalf("snapshot %+v: a draft must sit on the current version %+v", snap, f.Version)
	}
	if snap.ConfigHash != wantHash || snap.ConfigHash == f.Version.ConfigHash || snap.Version.ConfigHash != wantHash {
		t.Fatalf("hash %q, want the draft's %q (published %q)", snap.ConfigHash, wantHash, f.Version.ConfigHash)
	}
	if !strings.Contains(string(snap.Version.Config), "DRAFT") || strings.Contains(string(snap.Version.Config), "PUBLISHED") {
		t.Fatalf("config %s", snap.Version.Config)
	}
	again, err := svc.Get(ctx, f.ID)
	if err != nil || again.Version.Number != 1 || strings.Contains(string(again.Version.Config), "DRAFT") {
		t.Fatalf("the stored flow changed: %+v err %v", again.Version, err)
	}
}

func TestResolveDraftRejectsWhatAPublishWouldReject(t *testing.T) {
	ctx := context.Background()
	svc := testService(t)
	f := publishedFlow(t, svc, "P")
	for name, cfg := range map[string]json.RawMessage{
		"not json":            json.RawMessage(`{`),
		"not an object":       json.RawMessage(`[1]`),
		"empty":               nil,
		"a tool not attached": json.RawMessage(`{"start":"a","steps":[{"id":"a","name":"A","tools":["srv/lookup"],"config":` + string(draftConfig("D")) + `}],"routes":[]}`),
	} {
		if _, _, err := svc.ResolveDraft(ctx, f.ID, cfg, nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A provider the environment lacks is rejected like a publish rejects it.
	strict := NewService(svc.store, fakeAvail{"google/llm": Unavailable{Reason: "missing_env"}}) // same store, an environment without the llm key
	if _, _, err := strict.ResolveDraft(ctx, f.ID, draftConfig("D"), nil); !errors.Is(err, ErrNotAttached) {
		t.Errorf("unattached provider: %v, want ErrNotAttached", err)
	}
}

// An incomplete draft is refused as a publish refuses it (the flow must be attached to providers),
// so what reaches the readiness check is a draft a publish would accept; readiness then reflects the
// flow's attached MCP tools.
func TestResolveDraftRefusesAnIncompleteDraftLikeAPublish(t *testing.T) {
	svc := testService(t)
	f := publishedFlow(t, svc, "P")
	incomplete := json.RawMessage(`{"stt":{"provider":"google","model":"google.stt"},"llm":{"provider":"google"},"tts":{"provider":"google","voice":"google.tts"}}`)
	if _, _, err := svc.ResolveDraft(context.Background(), f.ID, incomplete, nil); !errors.Is(err, ErrNotAttached) {
		t.Fatalf("incomplete draft: %v, want ErrNotAttached", err)
	}
}

func TestResolveDraftNeedsAUsableFlow(t *testing.T) {
	ctx := context.Background()
	svc := testService(t)
	if _, _, err := svc.ResolveDraft(ctx, storage.NewID(), draftConfig("D"), nil); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown flow: %v", err)
	}
	f := publishedFlow(t, svc, "P")
	if err := svc.Archive(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.ResolveDraft(ctx, f.ID, draftConfig("D"), nil); err == nil {
		t.Fatal("an archived flow can take no draft call")
	}
}

func TestResolveDraftWritesNothing(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()
	f := publishedFlow(t, svc, "P")
	var before, after int
	count := func(n *int) {
		if err := svc.store.DB().QueryRowContext(ctx, "SELECT count(*) FROM agent_versions").Scan(n); err != nil {
			t.Fatal(err)
		}
	}
	count(&before)
	if _, _, err := svc.ResolveDraft(ctx, f.ID, draftConfig("D"), nil); err != nil {
		t.Fatal(err)
	}
	count(&after)
	if before != after {
		t.Fatalf("versions %d -> %d", before, after)
	}
}

func TestResolveDraftCanSelectToolsTheFlowHasNotPublished(t *testing.T) {
	r := newToolsRig(t)
	ctx := context.Background()
	cfg := stepsConfig(r.server + "/echo")
	// Without a selection the S1 rule stands: a step may only list tools the flow has.
	if _, _, err := r.svc.ResolveDraft(ctx, r.flow.ID, cfg, nil); err == nil || !strings.Contains(err.Error(), "has not selected") {
		t.Fatalf("an unselected step tool: %v", err)
	}
	tools := []ToolSelection{r.sel("echo")}
	snap, readiness, err := r.svc.ResolveDraft(ctx, r.flow.ID, cfg, &tools)
	if err != nil || readiness.State != ReadinessReady {
		t.Fatalf("readiness %+v err %v", readiness, err)
	}
	if len(snap.MCPTools) != 1 || snap.MCPTools[0].RemoteName != "echo" || !snap.Draft {
		t.Fatalf("snapshot tools %+v", snap.MCPTools)
	}
	again, err := r.svc.Get(ctx, r.flow.ID)
	if err != nil || again.Version.Number != 1 || len(again.Version.MCPTools) != 0 {
		t.Fatalf("a draft must not change the stored flow: %+v err %v", again.Version, err)
	}
}

func TestResolveDraftRefusesAToolSelectionAPublishWouldRefuse(t *testing.T) {
	r := newToolsRig(t)
	ctx := context.Background()
	stale := r.sel("echo")
	stale.SchemaHash = "0000"
	if _, _, err := r.svc.ResolveDraft(ctx, r.flow.ID, stepsConfig(), &[]ToolSelection{stale}); !errors.Is(err, storage.ErrInvalidToolSelection) {
		t.Fatalf("stale hash: %v", err)
	}
	if err := r.svc.store.MCP().DisableServer(ctx, r.server); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.svc.ResolveDraft(ctx, r.flow.ID, stepsConfig(), &[]ToolSelection{r.sel("echo")}); !errors.Is(err, storage.ErrInvalidToolSelection) {
		t.Fatalf("disabled server: %v", err)
	}
}
