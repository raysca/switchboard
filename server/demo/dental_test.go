package demo

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"govoice/server/flow"
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

type fakeFlows struct {
	list     []flow.Flow
	creates  []flow.CreateRequest
	archived []string
}

func (f *fakeFlows) Archive(_ context.Context, id string) error {
	f.archived = append(f.archived, id)
	for i := range f.list {
		if f.list[i].ID == id {
			f.list[i].Archived = true
		}
	}
	return nil
}

func (f *fakeFlows) List(context.Context, int, int) ([]flow.Flow, error) { return f.list, nil }
func (f *fakeFlows) Create(_ context.Context, r flow.CreateRequest) (flow.Flow, error) {
	f.creates = append(f.creates, r)
	fl := flow.Flow{ID: "flow1", Name: r.Name}
	f.list = append(f.list, fl)
	return fl, nil
}

func TestInstallDentalCreatesTheServerAndTheFlow(t *testing.T) {
	srv, fl := &fakeServers{}, &fakeFlows{}
	res, err := InstallDental(context.Background(), srv, fl, "https://dental.example.com/mcp")
	if err != nil || !res.MCPCreated || !res.FlowCreated || res.Tools != len(dentalmcp.ToolNames) || res.FlowID != "flow1" {
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
	g, err := flow.Decode(req.Config)
	if err != nil || len(g.Steps) != 3 || len(g.Routes) != 2 {
		t.Fatalf("config: steps=%d routes=%d err=%v", len(g.Steps), len(g.Routes), err)
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
	srv, fl := &fakeServers{}, &fakeFlows{}
	if _, err := InstallDental(context.Background(), srv, fl, "https://d/mcp"); err != nil {
		t.Fatal(err)
	}
	res, err := InstallDental(context.Background(), srv, fl, "https://d/mcp")
	if err != nil || res.MCPCreated || res.FlowCreated || srv.creates != 1 || len(fl.creates) != 1 || res.FlowID != "flow1" {
		t.Fatalf("second run: res=%+v err=%v creates=%d/%d", res, err, srv.creates, len(fl.creates))
	}
}

func TestInstallDentalFinishesAHalfDoneInstall(t *testing.T) {
	srv := &fakeServers{list: []storage.MCPServer{{ID: "srv1", Name: ServerName, Endpoint: "https://d/mcp"}}}
	fl := &fakeFlows{}
	res, err := InstallDental(context.Background(), srv, fl, "https://d/mcp")
	if err != nil || res.MCPCreated || !res.FlowCreated || srv.creates != 0 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestInstallDentalRefusesAServerMissingATool(t *testing.T) {
	srv := &fakeServers{}
	srv.list = []storage.MCPServer{{ID: "srv1", Name: ServerName, Endpoint: "https://d/mcp"}}
	short := &shortServers{fakeServers: srv}
	if _, err := InstallDental(context.Background(), short, &fakeFlows{}, "https://d/mcp"); err == nil {
		t.Fatal("a server that does not offer every dental tool must not get a flow")
	}
}

type shortServers struct{ *fakeServers }

func (s *shortServers) Refresh(context.Context, string) (storage.MCPDiscovery, error) {
	return storage.MCPDiscovery{Tools: []storage.MCPTool{{RemoteName: "find_patient", SchemaHash: "h"}}}, nil
}

func TestInstallDentalReenablesADisabledServer(t *testing.T) {
	srv := &fakeServers{list: []storage.MCPServer{{ID: "srv1", Name: ServerName, Endpoint: "https://d/mcp", Enabled: false}}}
	res, err := InstallDental(context.Background(), srv, &fakeFlows{}, "https://d/mcp")
	if err != nil || res.MCPCreated || !res.FlowCreated || srv.creates != 0 || srv.enables != 1 {
		t.Fatalf("res=%+v err=%v creates=%d enables=%d", res, err, srv.creates, srv.enables)
	}
}

func TestInstallDentalReplacesAFlowPointingAtAnotherServer(t *testing.T) {
	srv := &fakeServers{list: []storage.MCPServer{{ID: "srv1", Name: ServerName, Endpoint: "https://d/mcp", Enabled: true}}}
	stale := flow.Flow{ID: "old", Name: FlowName, Version: &flow.Version{MCPTools: []flow.ToolSelection{{ServerID: "gone", RemoteName: "find_patient"}}}}
	fl := &fakeFlows{list: []flow.Flow{stale}}
	res, err := InstallDental(context.Background(), srv, fl, "https://d/mcp")
	if err != nil || !res.FlowCreated || res.FlowID != "flow1" || len(fl.archived) != 1 || fl.archived[0] != "old" {
		t.Fatalf("res=%+v err=%v archived=%v", res, err, fl.archived)
	}
	// A flow that points at the current server is left alone.
	fresh := &fakeFlows{list: []flow.Flow{{ID: "ok", Name: FlowName, Version: &flow.Version{MCPTools: []flow.ToolSelection{{ServerID: "srv1"}}}}}}
	res, err = InstallDental(context.Background(), srv, fresh, "https://d/mcp")
	if err != nil || res.FlowCreated || res.FlowID != "ok" || len(fresh.archived) != 0 {
		t.Fatalf("res=%+v err=%v archived=%v", res, err, fresh.archived)
	}
}

func TestInstallDentalRefusesAServerWithAnotherEndpoint(t *testing.T) {
	srv := &fakeServers{list: []storage.MCPServer{{ID: "srv1", Name: ServerName, Endpoint: "https://old/mcp", Enabled: true}}}
	_, err := InstallDental(context.Background(), srv, &fakeFlows{}, "https://new/mcp")
	if err == nil || !strings.Contains(err.Error(), "https://old/mcp") || !strings.Contains(err.Error(), "DENTAL_MCP_URL") {
		t.Fatalf("err = %v", err)
	}
}
