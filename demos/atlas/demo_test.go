package atlas_test

import (
	"slices"
	"strings"
	"testing"

	"govoice/demos/atlas"
	"govoice/demos/atlas/mcp/atlasmcp"
	"govoice/demos/spec"
	"govoice/server/agent"
)

const (
	testServerID    = "srv-inventory"
	weatherPinID    = "http:1"
	advisoryPinID   = "http:2"
	weatherTool     = "weather_forecast"
	advisoryTool    = "travel_advisory"
	asyncTool       = "rebook_disruption"
	wantStepCount   = 8
	wantHandoffs    = 32
	wantMCPTools    = 18
	wantHTTPTools   = 2
	wantAllToolRefs = wantMCPTools + wantHTTPTools
)

// resolution is the fake seed result every test builds the agent from.
func resolution() spec.Resolution {
	return spec.Resolution{
		ServerIDs: map[string]string{"inventory": testServerID},
		HTTPPins: map[string]spec.ToolPin{
			weatherTool:  {ServerID: weatherPinID, RemoteName: weatherTool, SchemaHash: "h-weather", Required: true},
			advisoryTool: {ServerID: advisoryPinID, RemoteName: advisoryTool, SchemaHash: "h-advisory", Required: true},
		},
	}
}

func build(t *testing.T, r spec.Resolution) (spec.AgentSpec, agent.Definition) {
	t.Helper()
	a, err := (atlas.Demo{}).Agent(r)
	if err != nil {
		t.Fatalf("Agent: %v", err)
	}
	g, err := agent.Decode(a.Config)
	if err != nil {
		t.Fatalf("the demo config must decode as an agent: %v", err)
	}
	return a, g
}

func TestDemoMetadata(t *testing.T) {
	d := atlas.Demo{}
	if got := d.Name(); got != atlas.Name {
		t.Fatalf("Name() = %q, want %q", got, atlas.Name)
	}
	servers := d.MCPServers()
	if len(servers) != 1 {
		t.Fatalf("MCPServers() returned %d servers, want 1", len(servers))
	}
	s := servers[0]
	if s.Logical != "inventory" || s.Name != atlas.ServerName {
		t.Errorf("server = logical %q name %q", s.Logical, s.Name)
	}
	if s.EndpointPath != "/mcp" || s.TokenEnv != "ATLAS_MCP_TOKEN" || s.SecretName != "demo-atlas-mcp" {
		t.Errorf("server endpoint/token/secret = %q/%q/%q", s.EndpointPath, s.TokenEnv, s.SecretName)
	}
	if s.Description == "" {
		t.Error("the MCP server needs a description")
	}
	if !slices.Equal(s.Tools, atlasmcp.ToolNames) || len(s.Tools) != wantMCPTools {
		t.Errorf("server tools = %v, want the %d fixture tools", s.Tools, wantMCPTools)
	}

	httpTools := d.HTTPTools()
	if len(httpTools) != wantHTTPTools {
		t.Fatalf("HTTPTools() returned %d tools, want %d", len(httpTools), wantHTTPTools)
	}
	wantPaths := map[string]string{weatherTool: "/api/weather", advisoryTool: "/api/advisory"}
	for _, tool := range httpTools {
		if wantPaths[tool.Name] != tool.Path {
			t.Errorf("HTTP tool %q path = %q, want %q", tool.Name, tool.Path, wantPaths[tool.Name])
		}
		if tool.Method != "GET" || tool.Description == "" || len(tool.InputSchema) == 0 {
			t.Errorf("HTTP tool %q is incomplete: %+v", tool.Name, tool)
		}
	}
}

func TestAgentIsAValidEightStepDefinition(t *testing.T) {
	a, g := build(t, resolution())
	if a.Name != atlas.AgentName {
		t.Fatalf("agent name = %q, want %q", a.Name, atlas.AgentName)
	}
	if a.Description == "" {
		t.Fatal("the agent needs a description")
	}
	if len(g.Steps) != wantStepCount || len(g.Handoffs) != wantHandoffs {
		t.Fatalf("config has %d steps and %d handoffs, want %d and %d", len(g.Steps), len(g.Handoffs), wantStepCount, wantHandoffs)
	}
	if g.Start != "concierge" {
		t.Errorf("start = %q, want concierge", g.Start)
	}
	for _, s := range g.Steps {
		if !s.EndCall {
			t.Errorf("step %q must be able to end the call", s.Name)
		}
	}
	if got := g.StartStep().Config.AsyncTools; !slices.Equal(got, spec.Refs(testServerID, asyncTool)) {
		t.Errorf("start step async_tools = %v, want %v", got, spec.Refs(testServerID, asyncTool))
	}
}

func TestStepToolsAreTheMCPAndHTTPPins(t *testing.T) {
	_, g := build(t, resolution())
	expected := make(map[string]bool, wantAllToolRefs)
	for _, ref := range spec.Refs(testServerID, atlasmcp.ToolNames...) {
		expected[ref] = true
	}
	weatherRef := spec.HTTPRef(resolution().HTTPPins[weatherTool])
	advisoryRef := spec.HTTPRef(resolution().HTTPPins[advisoryTool])
	expected[weatherRef], expected[advisoryRef] = true, true

	seen := make(map[string]bool, len(expected))
	for _, s := range g.Steps {
		for _, ref := range s.Tools {
			if !expected[ref] {
				t.Errorf("step %q exposes %q, which is not an MCP or HTTP pin", s.ID, ref)
			}
			seen[ref] = true
		}
	}
	if len(seen) != len(expected) {
		t.Errorf("the steps expose %d of the %d pins: %v", len(seen), len(expected), expected)
	}

	// The split is part of the demo the prompts are tuned to.
	mcp := func(names ...string) []string { return spec.Refs(testServerID, names...) }
	want := map[string][]string{
		"concierge":   mcp("get_traveller", "list_bookings"),
		"flights":     mcp("search_flights", "book_flight", "change_flight", "cancel_booking"),
		"stays":       append(mcp("search_stays", "book_stay", "cancel_booking"), weatherRef),
		"ground":      mcp("search_transfers", "book_transfer"),
		"payments":    mcp("list_payment_methods", "charge_booking", "issue_refund"),
		"loyalty":     mcp("get_loyalty", "apply_points"),
		"disruptions": append(mcp("list_bookings", "report_disruption", "rebook_disruption"), advisoryRef),
		"human":       mcp("create_callback"),
	}
	for _, s := range g.Steps {
		if !slices.Equal(s.Tools, want[s.ID]) {
			t.Errorf("step %q tools = %v, want %v", s.ID, s.Tools, want[s.ID])
		}
	}
}

func TestAgentRefusesMissingResolution(t *testing.T) {
	if _, err := (atlas.Demo{}).Agent(spec.Resolution{}); err == nil || !strings.Contains(err.Error(), "inventory") {
		t.Fatalf("Agent without an inventory server id = %v, want an error naming the server", err)
	}
	noPins := resolution()
	noPins.HTTPPins = nil
	if _, err := (atlas.Demo{}).Agent(noPins); err == nil || !strings.Contains(err.Error(), weatherTool) {
		t.Fatalf("Agent without HTTP pins = %v, want an error naming the HTTP tool", err)
	}
}
