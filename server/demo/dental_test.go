package demo

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"govoice/server/agent"
	"govoice/server/mcp"
	"govoice/server/mcp/dentalmcp"
	"govoice/server/storage"
)

type fakeServers struct {
	enables int
	list    []storage.MCPServer
	creates int
}

func (f *fakeServers) List(context.Context) ([]storage.MCPServer, error) { return f.list, nil }
func (f *fakeServers) Create(_ context.Context, c mcp.ServerConfig) (storage.MCPServer, error) {
	f.creates++
	m := storage.MCPServer{ID: "srv1", Name: c.Name, Endpoint: c.Endpoint, AuthEnv: c.AuthEnv, Enabled: c.Enabled}
	f.list = append(f.list, m)
	return m, nil
}
func (f *fakeServers) Enable(_ context.Context, id string) error {
	f.enables++
	for i := range f.list {
		if f.list[i].ID == id {
			f.list[i].Enabled = true
		}
	}
	return nil
}
func (f *fakeServers) Refresh(context.Context, string) (storage.MCPDiscovery, error) {
	var d storage.MCPDiscovery
	for _, n := range dentalmcp.ToolNames {
		d.Tools = append(d.Tools, storage.MCPTool{RemoteName: n, SchemaHash: "h-" + n})
	}
	return d, nil
}

type fakeAgents struct {
	list     []agent.Agent
	creates  []agent.CreateRequest
	archived []string
}

func (f *fakeAgents) Archive(_ context.Context, id string) error {
	f.archived = append(f.archived, id)
	for i := range f.list {
		if f.list[i].ID == id {
			f.list[i].Archived = true
		}
	}
	return nil
}

func (f *fakeAgents) List(context.Context, int, int) ([]agent.Agent, error) { return f.list, nil }
func (f *fakeAgents) Create(_ context.Context, r agent.CreateRequest) (agent.Agent, error) {
	f.creates = append(f.creates, r)
	fl := agent.Agent{ID: "flow1", Name: r.Name}
	f.list = append(f.list, fl)
	return fl, nil
}

func TestInstallDentalCreatesTheServerAndTheAgent(t *testing.T) {
	srv, fl := &fakeServers{}, &fakeAgents{}
	res, err := InstallDental(context.Background(), srv, fl, "https://dental.example.com/mcp")
	if err != nil || !res.MCPCreated || !res.AgentCreated || res.Tools != len(dentalmcp.ToolNames) || res.AgentID != "flow1" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if srv.list[0].AuthEnv != AuthEnv || srv.list[0].Endpoint != "https://dental.example.com/mcp" {
		t.Fatalf("server = %+v", srv.list[0])
	}
	req := fl.creates[0]
	if len(req.MCPTools) != len(dentalmcp.ToolNames) {
		t.Fatalf("flow tool selections = %d", len(req.MCPTools))
	}
	// The stored config is a valid three-step flow the server accepts.
	g, err := agent.Decode(req.Config)
	if err != nil || len(g.Steps) != 3 || len(g.Handoffs) != 2 {
		t.Fatalf("config: steps=%d routes=%d err=%v", len(g.Steps), len(g.Handoffs), err)
	}
	for _, s := range g.Steps {
		if !s.EndCall {
			t.Errorf("step %q must be able to end the call", s.Name)
		}
		for _, ref := range s.Tools {
			if !strings.HasPrefix(ref, "srv1/") {
				t.Errorf("step %q tool %q must point at the installed server", s.Name, ref)
			}
		}
	}
	// Providers: three different stacks, one STT on the start step only.
	front, _ := g.Step("front")
	if front.Config.STT.Provider != "google" || front.Config.LLM.Provider != "google" || front.Config.TTS.Provider != "elevenlabs" {
		t.Fatalf("front stack = %+v", front.Config)
	}
	newp, _ := g.Step("new")
	ret, _ := g.Step("returning")
	if newp.Config.LLM.Provider != "xai" || newp.Config.TTS.Provider != "xai" || ret.Config.TTS.Provider != "google" {
		t.Fatalf("step stacks: new=%+v returning=%+v", newp.Config, ret.Config)
	}
	if !json.Valid(req.Config) {
		t.Fatal("config is not JSON")
	}
}

func TestInstallDentalTwiceChangesNothing(t *testing.T) {
	srv, fl := &fakeServers{}, &fakeAgents{}
	if _, err := InstallDental(context.Background(), srv, fl, "https://d/mcp"); err != nil {
		t.Fatal(err)
	}
	res, err := InstallDental(context.Background(), srv, fl, "https://d/mcp")
	if err != nil || res.MCPCreated || res.AgentCreated || srv.creates != 1 || len(fl.creates) != 1 || res.AgentID != "flow1" {
		t.Fatalf("second run: res=%+v err=%v creates=%d/%d", res, err, srv.creates, len(fl.creates))
	}
}

func TestInstallDentalFinishesAHalfDoneInstall(t *testing.T) {
	srv := &fakeServers{list: []storage.MCPServer{{ID: "srv1", Name: ServerName, Endpoint: "https://d/mcp"}}}
	fl := &fakeAgents{}
	res, err := InstallDental(context.Background(), srv, fl, "https://d/mcp")
	if err != nil || res.MCPCreated || !res.AgentCreated || srv.creates != 0 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestInstallDentalRefusesAServerMissingATool(t *testing.T) {
	srv := &fakeServers{}
	srv.list = []storage.MCPServer{{ID: "srv1", Name: ServerName, Endpoint: "https://d/mcp"}}
	short := &shortServers{fakeServers: srv}
	if _, err := InstallDental(context.Background(), short, &fakeAgents{}, "https://d/mcp"); err == nil {
		t.Fatal("a server that does not offer every dental tool must not get a flow")
	}
}

type shortServers struct{ *fakeServers }

func (s *shortServers) Refresh(context.Context, string) (storage.MCPDiscovery, error) {
	return storage.MCPDiscovery{Tools: []storage.MCPTool{{RemoteName: "find_patient", SchemaHash: "h"}}}, nil
}

func TestInstallDentalReenablesADisabledServer(t *testing.T) {
	srv := &fakeServers{list: []storage.MCPServer{{ID: "srv1", Name: ServerName, Endpoint: "https://d/mcp", Enabled: false}}}
	res, err := InstallDental(context.Background(), srv, &fakeAgents{}, "https://d/mcp")
	if err != nil || res.MCPCreated || !res.AgentCreated || srv.creates != 0 || srv.enables != 1 {
		t.Fatalf("res=%+v err=%v creates=%d enables=%d", res, err, srv.creates, srv.enables)
	}
}

func TestInstallDentalReplacesAnAgentPointingAtAnotherServer(t *testing.T) {
	srv := &fakeServers{list: []storage.MCPServer{{ID: "srv1", Name: ServerName, Endpoint: "https://d/mcp", Enabled: true}}}
	stale := agent.Agent{ID: "old", Name: AgentName, Version: &agent.Version{MCPTools: []agent.ToolSelection{{ServerID: "gone", RemoteName: "find_patient"}}}}
	fl := &fakeAgents{list: []agent.Agent{stale}}
	res, err := InstallDental(context.Background(), srv, fl, "https://d/mcp")
	if err != nil || !res.AgentCreated || res.AgentID != "flow1" || len(fl.archived) != 1 || fl.archived[0] != "old" {
		t.Fatalf("res=%+v err=%v archived=%v", res, err, fl.archived)
	}
	// A flow that points at the current server is left alone.
	fresh := &fakeAgents{list: []agent.Agent{{ID: "ok", Name: AgentName, Version: &agent.Version{MCPTools: []agent.ToolSelection{{ServerID: "srv1"}}}}}}
	res, err = InstallDental(context.Background(), srv, fresh, "https://d/mcp")
	if err != nil || res.AgentCreated || res.AgentID != "ok" || len(fresh.archived) != 0 {
		t.Fatalf("res=%+v err=%v archived=%v", res, err, fresh.archived)
	}
}

func TestInstallDentalRefusesAServerWithAnotherEndpoint(t *testing.T) {
	srv := &fakeServers{list: []storage.MCPServer{{ID: "srv1", Name: ServerName, Endpoint: "https://old/mcp", Enabled: true}}}
	_, err := InstallDental(context.Background(), srv, &fakeAgents{}, "https://new/mcp")
	if err == nil || !strings.Contains(err.Error(), "https://old/mcp") || !strings.Contains(err.Error(), "DENTAL_MCP_URL") {
		t.Fatalf("err = %v", err)
	}
}

func TestInstallDentalKeepsAnEditedAgentWithAForeignTool(t *testing.T) {
	srv := &fakeServers{list: []storage.MCPServer{{ID: "srv1", Name: ServerName, Endpoint: "https://d/mcp", Enabled: true}}}
	edited := agent.Agent{ID: "mine", Name: AgentName, Version: &agent.Version{MCPTools: []agent.ToolSelection{{ServerID: "srv1", RemoteName: "find_patient"}, {ServerID: "other", RemoteName: "x"}}}}
	fl := &fakeAgents{list: []agent.Agent{edited}}
	res, err := InstallDental(context.Background(), srv, fl, "https://d/mcp")
	if err != nil || res.AgentCreated || res.AgentID != "mine" || len(fl.archived) != 0 {
		t.Fatalf("res=%+v err=%v archived=%v", res, err, fl.archived)
	}
}

func TestInstallDentalEndpointComparisonIsNormalised(t *testing.T) {
	srv := &fakeServers{list: []storage.MCPServer{{ID: "srv1", Name: ServerName, Endpoint: "https://d.example.com/mcp", Enabled: true}}}
	if _, err := InstallDental(context.Background(), srv, &fakeAgents{}, " https://d.example.com/mcp \n"); err != nil {
		t.Fatalf("surrounding space must not matter: %v", err)
	}
	if _, err := InstallDental(context.Background(), srv, &fakeAgents{}, "not a url"); err == nil {
		t.Fatal("an unparseable endpoint must be an error")
	}
}
