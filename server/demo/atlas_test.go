package demo

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"govoice/server/agent"
	"govoice/server/httptool"
	"govoice/server/mcp"
	"govoice/server/mcp/atlasmcp"
	"govoice/server/storage"
)

// fakeAtlasServers serves the Atlas tool discovery in memory.
type fakeAtlasServers struct {
	enables int
	list    []storage.MCPServer
	creates int
	short   bool
}

func (f *fakeAtlasServers) List(context.Context) ([]storage.MCPServer, error) { return f.list, nil }
func (f *fakeAtlasServers) Create(_ context.Context, c mcp.ServerConfig) (storage.MCPServer, error) {
	f.creates++
	m := storage.MCPServer{ID: "srv1", Name: c.Name, Endpoint: c.Endpoint, AuthEnv: c.AuthEnv, Enabled: c.Enabled}
	f.list = append(f.list, m)
	return m, nil
}
func (f *fakeAtlasServers) Enable(_ context.Context, id string) error {
	f.enables++
	for i := range f.list {
		if f.list[i].ID == id {
			f.list[i].Enabled = true
		}
	}
	return nil
}
func (f *fakeAtlasServers) Refresh(context.Context, string) (storage.MCPDiscovery, error) {
	var d storage.MCPDiscovery
	for _, n := range atlasmcp.ToolNames {
		if f.short && n == "create_callback" {
			continue
		}
		d.Tools = append(d.Tools, storage.MCPTool{RemoteName: n, SchemaHash: "h-" + n})
	}
	return d, nil
}

// fakeHTTPToolService normalises and hashes definitions exactly as the real
// service does, so the pinned hashes are the ones a real store would hold.
type fakeHTTPToolService struct {
	creates   int
	enables   int
	views     []httptool.View
	nextID    int
	nameTaken map[string]bool // the next Create for that name loses a create race once
}

func (f *fakeHTTPToolService) List(context.Context) ([]httptool.View, error) { return f.views, nil }
func (f *fakeHTTPToolService) Create(_ context.Context, d httptool.Definition) (httptool.View, error) {
	n, err := d.Normalize(context.Background(), httptool.Options{AllowLocalHTTP: true})
	if err != nil {
		return httptool.View{}, err
	}
	hash, _, err := n.Hash()
	if err != nil {
		return httptool.View{}, err
	}
	f.nextID++
	v := httptool.View{ID: fmt.Sprintf("http%d", f.nextID), Name: n.Name, Enabled: true, Hash: hash, Definition: n}
	if f.nameTaken[n.Name] {
		// Another instance won the create race: its row is now visible.
		delete(f.nameTaken, n.Name)
		f.views = append(f.views, v)
		return httptool.View{}, storage.ErrHTTPToolNameTaken
	}
	f.views = append(f.views, v)
	f.creates++
	return v, nil
}
func (f *fakeHTTPToolService) SetEnabled(_ context.Context, id string, enabled bool) (httptool.View, error) {
	f.enables++
	for i := range f.views {
		if f.views[i].ID == id {
			f.views[i].Enabled = enabled
			return f.views[i], nil
		}
	}
	return httptool.View{}, fmt.Errorf("unknown HTTP tool %s", id)
}

func (f *fakeHTTPToolService) seed(base string) {
	for _, spec := range atlasmcp.HTTPTools {
		d := httptool.Definition{Name: spec.Name, Description: spec.Description, Method: spec.Method, URL: base + spec.Path, InputSchema: spec.InputSchema}
		n, err := d.Normalize(context.Background(), httptool.Options{AllowLocalHTTP: true})
		if err != nil {
			panic(err)
		}
		hash, _, _ := n.Hash()
		f.nextID++
		f.views = append(f.views, httptool.View{ID: fmt.Sprintf("http%d", f.nextID), Name: n.Name, Enabled: true, Hash: hash, Definition: n})
	}
}

func TestInstallAtlasCreatesEverything(t *testing.T) {
	srv, tools, fl := &fakeAtlasServers{}, &fakeHTTPToolService{}, &fakeAgents{}
	res, err := InstallAtlas(context.Background(), srv, tools, fl, "http://127.0.0.1:18768")
	if err != nil {
		t.Fatal(err)
	}
	if !res.MCPCreated || !res.AgentCreated || res.Tools != len(atlasmcp.ToolNames) || res.HTTPTools != len(atlasmcp.HTTPTools) || res.HTTPToolsCreated != 2 || res.AgentID == "" {
		t.Fatalf("res = %+v", res)
	}
	if srv.list[0].AuthEnv != AtlasAuthEnv || srv.list[0].Endpoint != "http://127.0.0.1:18768/mcp" {
		t.Fatalf("MCP server = %+v", srv.list[0])
	}
	req := fl.creates[0]
	if len(req.MCPTools) != len(atlasmcp.ToolNames)+len(atlasmcp.HTTPTools) {
		t.Fatalf("tool pins = %d, want %d", len(req.MCPTools), len(atlasmcp.ToolNames)+len(atlasmcp.HTTPTools))
	}
	g, err := agent.Decode(req.Config)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Steps) != 8 || len(g.Handoffs) != 32 {
		t.Fatalf("steps=%d handoffs=%d, want 8 and 32", len(g.Steps), len(g.Handoffs))
	}
	selected := make(map[string]bool, len(req.MCPTools))
	httpIDs := make(map[string]bool, len(atlasmcp.HTTPTools))
	for _, sel := range req.MCPTools {
		selected[sel.ServerID+"/"+sel.RemoteName] = true
		if strings.HasPrefix(sel.ServerID, httptool.RefPrefix) {
			httpIDs[sel.ServerID] = true
		}
	}
	for _, n := range g.Steps {
		if !n.EndCall {
			t.Errorf("step %q must be able to end the call", n.Name)
		}
		for _, ref := range n.Tools {
			if !selected[ref] {
				t.Errorf("step %q lists unselected tool %q", n.ID, ref)
			}
		}
	}
	// The two HTTP tools must be pinned too, and used on their steps.
	if len(httpIDs) != len(atlasmcp.HTTPTools) {
		t.Fatalf("HTTP pins = %v", httpIDs)
	}
	start, _ := g.Step(g.Start)
	if len(start.Config.AsyncTools) != 1 || start.Config.AsyncTools[0] != res.MCPServerID+"/rebook_disruption" {
		t.Fatalf("async_tools = %v", start.Config.AsyncTools)
	}
	stays, _ := g.Step("stays")
	if !containsHTTPRef(stays.Tools, "weather_forecast") {
		t.Fatalf("stays tools = %v", stays.Tools)
	}
	dis, _ := g.Step("disruptions")
	if !containsHTTPRef(dis.Tools, "travel_advisory") {
		t.Fatalf("disruptions tools = %v", dis.Tools)
	}
	// Every ordered pair is unique and every step is reachable from the start.
	pairs := make(map[[2]string]bool, len(g.Handoffs))
	edges := make(map[string][]string, len(g.Steps))
	for _, e := range g.Handoffs {
		key := [2]string{e.From, e.To}
		if pairs[key] {
			t.Fatalf("duplicate handoff %v", key)
		}
		pairs[key] = true
		edges[e.From] = append(edges[e.From], e.To)
	}
	seen := map[string]bool{g.Start: true}
	queue := []string{g.Start}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, to := range edges[cur] {
			if !seen[to] {
				seen[to] = true
				queue = append(queue, to)
			}
		}
	}
	if len(seen) != len(g.Steps) {
		t.Fatalf("reachable steps = %d of %d", len(seen), len(g.Steps))
	}
	if !json.Valid(req.Config) {
		t.Fatal("config is not JSON")
	}
}

func containsHTTPRef(refs []string, name string) bool {
	for _, ref := range refs {
		if strings.HasPrefix(ref, httptool.RefPrefix) && strings.HasSuffix(ref, "/"+name) {
			return true
		}
	}
	return false
}

func TestInstallAtlasTwiceChangesNothing(t *testing.T) {
	srv, tools, fl := &fakeAtlasServers{}, &fakeHTTPToolService{}, &fakeAgents{}
	if _, err := InstallAtlas(context.Background(), srv, tools, fl, "http://127.0.0.1:18768"); err != nil {
		t.Fatal(err)
	}
	res, err := InstallAtlas(context.Background(), srv, tools, fl, "http://127.0.0.1:18768")
	if err != nil || res.MCPCreated || res.AgentCreated || res.HTTPToolsCreated != 0 || srv.creates != 1 || tools.creates != 2 || len(fl.creates) != 1 {
		t.Fatalf("second run: res=%+v err=%v creates=%d/%d/%d", res, err, srv.creates, tools.creates, len(fl.creates))
	}
}

func TestInstallAtlasFinishesAHalfDoneInstall(t *testing.T) {
	srv := &fakeAtlasServers{list: []storage.MCPServer{{ID: "srv1", Name: AtlasServerName, Endpoint: "http://127.0.0.1:18768/mcp", Enabled: true}}}
	tools := &fakeHTTPToolService{}
	tools.seed("http://127.0.0.1:18768")
	fl := &fakeAgents{}
	res, err := InstallAtlas(context.Background(), srv, tools, fl, "http://127.0.0.1:18768")
	if err != nil || res.MCPCreated || len(tools.views) != 2 || !res.AgentCreated {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestInstallAtlasRefusesAServerMissingATool(t *testing.T) {
	srv := &fakeAtlasServers{short: true}
	if _, err := InstallAtlas(context.Background(), srv, &fakeHTTPToolService{}, &fakeAgents{}, "http://127.0.0.1:18768"); err == nil || !strings.Contains(err.Error(), "create_callback") {
		t.Fatalf("err = %v", err)
	}
}

func TestInstallAtlasReenablesADisabledServer(t *testing.T) {
	srv := &fakeAtlasServers{list: []storage.MCPServer{{ID: "srv1", Name: AtlasServerName, Endpoint: "http://127.0.0.1:18768/mcp", Enabled: false}}}
	tools := &fakeHTTPToolService{}
	tools.seed("http://127.0.0.1:18768")
	tools.views[0].Enabled = false
	res, err := InstallAtlas(context.Background(), srv, tools, &fakeAgents{}, "http://127.0.0.1:18768")
	if err != nil || !res.AgentCreated || srv.enables != 1 || tools.enables != 1 || srv.creates != 0 {
		t.Fatalf("res=%+v err=%v enables=%d/%d", res, err, srv.enables, tools.enables)
	}
}

func TestInstallAtlasReplacesAnAgentPointingAtAnotherServer(t *testing.T) {
	srv := &fakeAtlasServers{list: []storage.MCPServer{{ID: "srv1", Name: AtlasServerName, Endpoint: "http://127.0.0.1:18768/mcp", Enabled: true}}}
	tools := &fakeHTTPToolService{}
	tools.seed("http://127.0.0.1:18768")
	stale := agent.Agent{ID: "old", Name: AtlasAgentName, Version: &agent.Version{MCPTools: []agent.ToolSelection{{ServerID: "gone", RemoteName: "get_traveller"}}}}
	fl := &fakeAgents{list: []agent.Agent{stale}}
	res, err := InstallAtlas(context.Background(), srv, tools, fl, "http://127.0.0.1:18768")
	if err != nil || !res.AgentCreated || res.AgentID != "agent1" || len(fl.archived) != 1 || fl.archived[0] != "old" {
		t.Fatalf("res=%+v err=%v archived=%v", res, err, fl.archived)
	}
}

func TestInstallAtlasLeavesAnAgentWithTheExactPinsAndForeignTools(t *testing.T) {
	srv, tools, fl := &fakeAtlasServers{}, &fakeHTTPToolService{}, &fakeAgents{}
	if _, err := InstallAtlas(context.Background(), srv, tools, fl, "http://127.0.0.1:18768"); err != nil {
		t.Fatal(err)
	}
	// An admin adds a tool from another server: the demo pins are still exact.
	fl.list[0].Version.MCPTools = append(fl.list[0].Version.MCPTools, agent.ToolSelection{ServerID: "other", RemoteName: "x", SchemaHash: "h"})
	res, err := InstallAtlas(context.Background(), srv, tools, fl, "http://127.0.0.1:18768")
	if err != nil || res.AgentCreated || res.AgentID != "agent1" || len(fl.archived) != 0 {
		t.Fatalf("res=%+v err=%v archived=%v", res, err, fl.archived)
	}
}

func TestInstallAtlasRepairsADriftedPin(t *testing.T) {
	srv, tools, fl := &fakeAtlasServers{}, &fakeHTTPToolService{}, &fakeAgents{}
	if _, err := InstallAtlas(context.Background(), srv, tools, fl, "http://127.0.0.1:18768"); err != nil {
		t.Fatal(err)
	}
	// The HTTP tool was edited (or the agent kept an old hash): the agent would
	// be needs-review and uncallable, so a re-install rebuilds it.
	for i := range fl.list[0].Version.MCPTools {
		if strings.HasPrefix(fl.list[0].Version.MCPTools[i].ServerID, httptool.RefPrefix) {
			fl.list[0].Version.MCPTools[i].SchemaHash = "stale"
		}
	}
	res, err := InstallAtlas(context.Background(), srv, tools, fl, "http://127.0.0.1:18768")
	if err != nil || !res.AgentCreated || res.AgentID != "agent1" || len(fl.archived) != 1 {
		t.Fatalf("res=%+v err=%v archived=%v", res, err, fl.archived)
	}
}

func TestInstallAtlasRefusesAConflictingServerEndpoint(t *testing.T) {
	srv := &fakeAtlasServers{list: []storage.MCPServer{{ID: "srv1", Name: AtlasServerName, Endpoint: "http://old.example/mcp", Enabled: true}}}
	_, err := InstallAtlas(context.Background(), srv, &fakeHTTPToolService{}, &fakeAgents{}, "http://127.0.0.1:18768")
	if err == nil || !strings.Contains(err.Error(), "http://old.example/mcp") || !strings.Contains(err.Error(), "ATLAS_DEMO_URL") {
		t.Fatalf("err = %v", err)
	}
}

// Two instances can create the same demo at once; the loser re-reads the winner's
// row instead of failing the install with a name conflict.
func TestInstallAtlasReusesAHTTPToolThatAnotherInstanceCreated(t *testing.T) {
	tools := &fakeHTTPToolService{nameTaken: map[string]bool{"weather_forecast": true}}
	res, err := InstallAtlas(context.Background(), &fakeAtlasServers{}, tools, &fakeAgents{}, "http://127.0.0.1:18768")
	if err != nil {
		t.Fatal(err)
	}
	if !res.AgentCreated || res.HTTPToolsCreated != 1 || res.HTTPTools != 2 {
		t.Fatalf("res = %+v", res)
	}
}
func TestInstallAtlasRefusesAConflictingHTTPToolURL(t *testing.T) {
	tools := &fakeHTTPToolService{}
	tools.seed("http://old.example")
	_, err := InstallAtlas(context.Background(), &fakeAtlasServers{}, tools, &fakeAgents{}, "http://127.0.0.1:18768")
	if err == nil || !strings.Contains(err.Error(), "weather_forecast") || !strings.Contains(err.Error(), "ATLAS_DEMO_URL") {
		t.Fatalf("err = %v", err)
	}
}

func TestInstallAtlasBaseURLIsNormalised(t *testing.T) {
	srv, tools, fl := &fakeAtlasServers{}, &fakeHTTPToolService{}, &fakeAgents{}
	if _, err := InstallAtlas(context.Background(), srv, tools, fl, " http://127.0.0.1:18768/ \n"); err != nil {
		t.Fatalf("surrounding space and a trailing slash must not matter: %v", err)
	}
	if srv.list[0].Endpoint != "http://127.0.0.1:18768/mcp" {
		t.Fatalf("endpoint = %q", srv.list[0].Endpoint)
	}
	if tools.views[0].Definition.URL != "http://127.0.0.1:18768/api/weather" {
		t.Fatalf("HTTP tool URL = %q", tools.views[0].Definition.URL)
	}
	for _, bad := range []string{"not a url", "ftp://example.com", "http://example.com/x?y=1", "http://user:pw@example.com", "http://127.0.0.1:18768/mcp"} {
		if _, err := InstallAtlas(context.Background(), &fakeAtlasServers{}, &fakeHTTPToolService{}, &fakeAgents{}, bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}

// The pins the installer writes must be exactly the demo's tool names, so a
// later discovery change is caught by the normal drift checks.
func TestAtlasToolNamesMatchTheFixture(t *testing.T) {
	if len(atlasmcp.ToolNames) != 18 || len(atlasmcp.HTTPTools) != 2 {
		t.Fatalf("fixture tools = %d MCP, %d HTTP", len(atlasmcp.ToolNames), len(atlasmcp.HTTPTools))
	}
	for _, spec := range atlasmcp.HTTPTools {
		if !strings.HasPrefix(spec.Path, "/api/") || spec.Method != "GET" {
			t.Fatalf("HTTP tool spec = %+v", spec)
		}
	}
}
