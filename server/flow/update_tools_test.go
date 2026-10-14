package flow

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"govoice/server/storage"
)

// toolsRig is a published flow plus an MCP server offering "echo" and "lookup".
type toolsRig struct {
	svc    *Service
	flow   Flow
	server string
	hashes map[string]string
}

func newToolsRig(t *testing.T) *toolsRig {
	t.Helper()
	svc := testService(t)
	ctx := context.Background()
	m := storage.MCPServer{ID: storage.NewID(), Name: "crm", Endpoint: "https://mcp.example/rpc", Transport: "streamable-http", Enabled: true, ValidationState: "healthy"}
	if err := svc.store.MCP().CreateServer(ctx, m); err != nil {
		t.Fatal(err)
	}
	d, err := svc.store.MCP().AddDiscovery(ctx, m.ID, []storage.MCPTool{
		{RemoteName: "echo", SchemaJSON: []byte(`{"type":"object"}`)},
		{RemoteName: "lookup", SchemaJSON: []byte(`{"type":"object","properties":{"id":{"type":"string"}}}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	hashes := map[string]string{}
	for _, tl := range d.Tools {
		hashes[tl.RemoteName] = tl.SchemaHash
	}
	return &toolsRig{svc: svc, flow: publishedFlow(t, svc, "P"), server: m.ID, hashes: hashes}
}

func (r *toolsRig) sel(name string) ToolSelection {
	return ToolSelection{ServerID: r.server, RemoteName: name, SchemaHash: r.hashes[name]}
}

// stepsConfig is a two-step config whose first step lists the given tool keys.
func stepsConfig(tools ...string) json.RawMessage {
	quoted := make([]string, len(tools))
	for i, k := range tools {
		quoted[i] = `"` + k + `"`
	}
	return json.RawMessage(`{"start":"a","steps":[{"id":"a","name":"A","tools":[` + strings.Join(quoted, ",") + `],"config":` + string(draftConfig("P")) + `}],"routes":[]}`)
}

func TestUpdateCarriesTheToolSelectionAndPublishesOneVersion(t *testing.T) {
	r := newToolsRig(t)
	ctx := context.Background()
	tools := []ToolSelection{r.sel("echo")}
	got, err := r.svc.Update(ctx, r.flow.ID, UpdateRequest{Config: stepsConfig(r.server + "/echo"), MCPTools: &tools})
	if err != nil {
		t.Fatal(err)
	}
	if got.Version.Number != 2 || len(got.Version.MCPTools) != 1 || got.Version.MCPTools[0].RemoteName != "echo" || !strings.Contains(string(got.Version.Config), r.server+"/echo") {
		t.Fatalf("version %+v", got.Version)
	}
}

func TestUpdateWithoutASelectionKeepsTheCurrentOneAndItsRule(t *testing.T) {
	r := newToolsRig(t)
	ctx := context.Background()
	if _, err := r.svc.Update(ctx, r.flow.ID, UpdateRequest{Config: stepsConfig(r.server + "/echo"), MCPTools: &[]ToolSelection{r.sel("echo")}}); err != nil {
		t.Fatal(err)
	}
	got, err := r.svc.Update(ctx, r.flow.ID, UpdateRequest{Config: stepsConfig(r.server + "/echo")})
	if err != nil || len(got.Version.MCPTools) != 1 {
		t.Fatalf("keep: %+v err %v", got.Version, err)
	}
	// A step may not list a tool the flow has not selected.
	if _, err := r.svc.Update(ctx, r.flow.ID, UpdateRequest{Config: stepsConfig(r.server + "/lookup")}); err == nil || !strings.Contains(err.Error(), "has not selected") {
		t.Fatalf("an unselected step tool: %v", err)
	}
}

func TestUpdateRefusesASelectionThatDropsAToolAStillUses(t *testing.T) {
	r := newToolsRig(t)
	ctx := context.Background()
	if _, err := r.svc.Update(ctx, r.flow.ID, UpdateRequest{Config: stepsConfig(r.server + "/echo"), MCPTools: &[]ToolSelection{r.sel("echo")}}); err != nil {
		t.Fatal(err)
	}
	none := []ToolSelection{}
	if _, err := r.svc.Update(ctx, r.flow.ID, UpdateRequest{Config: stepsConfig(r.server + "/echo"), MCPTools: &none}); err == nil || !strings.Contains(err.Error(), "has not selected") {
		t.Fatalf("got %v", err)
	}
	cur, _ := r.svc.Get(ctx, r.flow.ID)
	if cur.Version.Number != 2 {
		t.Fatalf("a rejected update published version %d", cur.Version.Number)
	}
	// Dropping the tool from the steps too clears the selection.
	got, err := r.svc.Update(ctx, r.flow.ID, UpdateRequest{Config: stepsConfig(), MCPTools: &none})
	if err != nil || len(got.Version.MCPTools) != 0 {
		t.Fatalf("clear: %+v err %v", got.Version, err)
	}
}

func TestUpdateRefusesAStaleHash(t *testing.T) {
	r := newToolsRig(t)
	stale := r.sel("echo")
	stale.SchemaHash = "0000"
	_, err := r.svc.Update(context.Background(), r.flow.ID, UpdateRequest{Config: stepsConfig(), MCPTools: &[]ToolSelection{stale}})
	if !errors.Is(err, storage.ErrInvalidToolSelection) {
		t.Fatalf("got %v", err)
	}
}

func TestASelectionWithoutAConfigIsRefusedNotSilentlyDropped(t *testing.T) {
	r := newToolsRig(t)
	_, err := r.svc.Update(context.Background(), r.flow.ID, UpdateRequest{MCPTools: &[]ToolSelection{r.sel("lookup")}})
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("got %v", err)
	}
	cur, _ := r.svc.Get(context.Background(), r.flow.ID)
	if cur.Version.Number != 1 {
		t.Fatalf("a refused update published version %d", cur.Version.Number)
	}
}

func TestMalformedSelectionsAreTheCallersMistake(t *testing.T) {
	r := newToolsRig(t)
	ctx := context.Background()
	dup := []ToolSelection{r.sel("echo"), r.sel("echo")}
	empty := []ToolSelection{{ServerID: r.server, RemoteName: "", SchemaHash: "h"}}
	notAnID := []ToolSelection{{ServerID: "not-an-id", RemoteName: "echo", SchemaHash: "h"}}
	for name, sels := range map[string][]ToolSelection{"duplicate": dup, "empty name": empty, "bad server id": notAnID} {
		sels := sels
		if _, err := r.svc.Update(ctx, r.flow.ID, UpdateRequest{Config: stepsConfig(), MCPTools: &sels}); !errors.Is(err, storage.ErrInvalidToolSelection) {
			t.Errorf("update %s: %v", name, err)
		}
		if _, _, err := r.svc.ResolveDraft(ctx, r.flow.ID, stepsConfig(), &sels); !errors.Is(err, storage.ErrInvalidToolSelection) {
			t.Errorf("draft %s: %v", name, err)
		}
	}
}
