package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"govoice/demos"
	"govoice/demos/spec"
)

const (
	testKey   = "test-management-key-32-bytes"
	testToken = "test-mcp-token-value"
)

// testDemo is a small real Demo used by the fake-API tests. Its Agent needs the
// resolved ids exactly like the shipped demos, so a run exercises the same
// placeholder-then-real resolution path.
type testDemo struct {
	name      string
	servers   []spec.MCPServerSpec
	httpTools []spec.HTTPToolSpec
	agentName string
}

func (d testDemo) Name() string { return d.name }

func (d testDemo) MCPServers() []spec.MCPServerSpec { return d.servers }

func (d testDemo) HTTPTools() []spec.HTTPToolSpec { return d.httpTools }

func (d testDemo) Agent(r spec.Resolution) (spec.AgentSpec, error) {
	serverID := r.ServerIDs["scheduling"]
	if serverID == "" {
		return spec.AgentSpec{}, fmt.Errorf("%s: no server id resolved", d.name)
	}
	refs := spec.Refs(serverID, d.servers[0].Tools...)
	for _, tool := range d.httpTools {
		pin, ok := r.HTTPPins[tool.Name]
		if !ok || pin.ServerID == "" {
			return spec.AgentSpec{}, fmt.Errorf("%s: no pin resolved for HTTP tool %q", d.name, tool.Name)
		}
		refs = append(refs, spec.HTTPRef(pin))
	}
	config := spec.Stage{
		"start": "front",
		"steps": []spec.Stage{
			spec.Step("front", "Front desk", true, refs, spec.Stage{"llm": spec.Stage{"provider": "echo", "model": "test"}}, 0, 0),
		},
	}
	raw, err := json.Marshal(config)
	if err != nil {
		return spec.AgentSpec{}, err
	}
	return spec.AgentSpec{Name: d.agentName, Description: "Test demo agent.", Config: raw}, nil
}

func defaultTestDemo() testDemo {
	return testDemo{
		name: "dental",
		servers: []spec.MCPServerSpec{{
			Logical:      "scheduling",
			Name:         "Test scheduling",
			Description:  "Fake scheduling for the seed tests.",
			EndpointPath: "/mcp",
			TokenEnv:     "TEST_MCP_TOKEN",
			SecretName:   "demo-test-mcp",
			Tools:        []string{"find_patient", "book_appointment"},
		}},
		agentName: "Test front desk",
	}
}

func httpTestDemo() testDemo {
	demo := defaultTestDemo()
	demo.httpTools = []spec.HTTPToolSpec{{
		Name:        "weather_forecast",
		Description: "The forecast for a city.",
		Method:      "GET",
		Path:        "/api/weather",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
	}}
	return demo
}

func testGetenv(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func seedOptions(serverURL string, demo spec.Demo) options {
	return options{
		serverURL:      serverURL,
		apiKey:         testKey,
		keyEnv:         "TEST_MANAGEMENT_KEY",
		demo:           demo,
		mcpBases:       map[string]string{"scheduling": "http://127.0.0.1:19001"},
		httpBase:       "http://127.0.0.1:19002",
		wait:           time.Second,
		pollInterval:   time.Millisecond,
		requestTimeout: 5 * time.Second,
		getenv:         testGetenv(map[string]string{"TEST_MCP_TOKEN": testToken}),
		log:            io.Discard,
	}
}

func runSeed(t *testing.T, serverURL string, demo spec.Demo, mutate ...func(*options)) (report, error) {
	t.Helper()
	options := seedOptions(serverURL, demo)
	for _, change := range mutate {
		change(&options)
	}
	return run(t.Context(), options)
}

// newSeededFake starts a fake API with the demo's tools discoverable.
func newSeededFake(t *testing.T, demo testDemo) (*fakeAPI, string) {
	t.Helper()
	fake := newFakeAPI(t, testKey)
	fake.discovery = demo.servers[0].Tools
	server := fake.serve()
	return fake, server.URL
}

func TestFirstSeedCreatesOneReadyDemo(t *testing.T) {
	demo := defaultTestDemo()
	fake, serverURL := newSeededFake(t, demo)

	got, err := runSeed(t, serverURL, demo)
	if err != nil {
		t.Fatal(err)
	}
	if stored, ok := fake.secrets["demo-test-mcp"]; !ok || stored != testToken {
		t.Fatalf("token secret = %q, %v; want the environment token", stored, ok)
	}
	if len(fake.servers) != 1 {
		t.Fatalf("MCP servers = %d, want 1", len(fake.servers))
	}
	server := fake.servers[0]
	if server.row.Name != "Test scheduling" || server.row.Endpoint != "http://127.0.0.1:19001/mcp" {
		t.Fatalf("server row = %+v", server.row)
	}
	if server.row.Transport != transportStreamableHTTP || server.row.AuthEnv != "demo-test-mcp" || !server.row.Enabled {
		t.Fatalf("server shape = %+v", server.row)
	}
	live := fake.liveAgents()
	if len(live) != 1 {
		t.Fatalf("live agents = %d, want 1", len(live))
	}
	agent := live[0]
	if agent.Name != "Test front desk" {
		t.Fatalf("agent name = %q", agent.Name)
	}
	if len(agent.Pins) != 2 {
		t.Fatalf("pins = %d, want 2: %+v", len(agent.Pins), agent.Pins)
	}
	for _, pin := range agent.Pins {
		if pin.ServerID != server.row.ID || !pin.Required || pin.SchemaHash != "schema-"+pin.RemoteName {
			t.Fatalf("pin = %+v", pin)
		}
	}
	if got.Created.AgentID != agent.ID || got.Created.Readiness != "ready" {
		t.Fatalf("report = %+v", got.Created)
	}
	// The order is the contract: token secret, server, validate, agent.
	secretWrite := fake.indexOf("PUT /api/v1/secrets/demo-test-mcp")
	serverCreate := fake.indexOf("POST /api/v1/mcp-servers")
	validate := fake.indexOf("POST /api/v1/mcp-servers/" + server.row.ID + "/validate")
	agentCreate := fake.indexOf("POST /api/v1/agents")
	if secretWrite < 0 || serverCreate < 0 || validate < 0 || agentCreate < 0 ||
		!(secretWrite < serverCreate && serverCreate < validate && validate < agentCreate) {
		t.Fatalf("create order: secret=%d server=%d validate=%d agent=%d in %v", secretWrite, serverCreate, validate, agentCreate, fake.recorded())
	}
}

func TestSecondSeedLeavesOneLiveAgentAndReusesTheHTTPTool(t *testing.T) {
	demo := httpTestDemo()
	fake, serverURL := newSeededFake(t, demo)

	if _, err := runSeed(t, serverURL, demo); err != nil {
		t.Fatal(err)
	}
	if len(fake.tools) != 1 {
		t.Fatalf("HTTP tools = %d, want 1", len(fake.tools))
	}
	tool := fake.tools[0]
	if tool.versions != 1 {
		t.Fatalf("versions after the first run = %d, want 1", tool.versions)
	}

	got, err := runSeed(t, serverURL, demo, func(o *options) { o.yes = true })
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.agents) != 2 {
		t.Fatalf("agents = %d, want the archived first run plus the new one", len(fake.agents))
	}
	live := fake.liveAgents()
	if len(live) != 1 {
		t.Fatalf("live agents = %d, want 1", len(live))
	}
	if got.Created.AgentID != live[0].ID {
		t.Fatalf("report agent %s != live agent %s", got.Created.AgentID, live[0].ID)
	}
	if got.Wiped.AgentsArchived != 1 || got.Wiped.MCPServersDeleted != 1 || got.Wiped.SecretsDeleted != 1 {
		t.Fatalf("wipe report = %+v", got.Wiped)
	}
	if len(fake.liveServers()) != 1 || len(fake.tools) != 1 {
		t.Fatalf("servers=%d tools=%d, want one of each", len(fake.liveServers()), len(fake.tools))
	}
	if tool.versions != 1 {
		t.Fatalf("HTTP tool update count = %d, want the second run to keep the definition", tool.versions)
	}
	var httpPin *toolSelection
	for i := range live[0].Pins {
		if strings.HasPrefix(live[0].Pins[i].ServerID, httpToolRefPrefix) {
			httpPin = &live[0].Pins[i]
		}
	}
	if httpPin == nil || httpPin.SchemaHash != tool.view.Hash {
		t.Fatalf("HTTP pin = %+v, tool hash %q", httpPin, tool.view.Hash)
	}
}

func TestWipeOrderAndHTTPToolIsNeverDeleted(t *testing.T) {
	demo := httpTestDemo()
	fake, serverURL := newSeededFake(t, demo)
	agent := fake.addAgent("Test front desk", false)
	server := fake.addServer("Test scheduling", "http://127.0.0.1:19001/mcp", "demo-test-mcp", demo.servers[0].Tools)
	fake.setSecret("demo-test-mcp", "stale-token")
	fake.addHTTPTool(httpDefinitionRequest{
		Name: "weather_forecast", Description: "The forecast for a city.", Method: "GET",
		URL: "http://127.0.0.1:19002/api/weather", InputSchema: demo.httpTools[0].InputSchema,
	})

	if _, err := runSeed(t, serverURL, demo, func(o *options) { o.yes = true }); err != nil {
		t.Fatal(err)
	}
	archive := fake.indexOf("POST /api/v1/agents/" + agent.ID + "/archive")
	detach := fake.indexOf("POST /api/v1/mcp-servers/" + server.row.ID + "/detach-delete")
	secret := fake.indexOf("DELETE /api/v1/secrets/demo-test-mcp")
	if archive < 0 || detach < 0 || secret < 0 {
		t.Fatalf("wipe requests missing: archive=%d detach=%d secret=%d in %v", archive, detach, secret, fake.recorded())
	}
	if !(archive < detach && detach < secret) {
		t.Fatalf("wipe order: archive=%d detach=%d secret=%d, want archive < detach < secret", archive, detach, secret)
	}
	if fake.indexOf("DELETE /api/v1/http-tools") >= 0 {
		t.Fatalf("seed deleted an HTTP tool: %v", fake.recorded())
	}
	if fake.findHTTPTool("weather_forecast") == nil {
		t.Fatal("the HTTP tool disappeared")
	}
}

func TestHTTPToolReuseViaPUT(t *testing.T) {
	demo := httpTestDemo()
	fake, serverURL := newSeededFake(t, demo)
	stale := fake.addHTTPTool(httpDefinitionRequest{
		Name: "weather_forecast", Description: "An old description.", Method: "POST",
		URL: "http://example.invalid/old", InputSchema: json.RawMessage(`{"type":"object"}`),
	})

	got, err := runSeed(t, serverURL, demo, func(o *options) { o.yes = true })
	if err != nil {
		t.Fatal(err)
	}
	if fake.indexOf("POST /api/v1/http-tools") >= 0 {
		t.Fatalf("seed created a duplicate HTTP tool: %v", fake.recorded())
	}
	if fake.indexOf("PUT /api/v1/http-tools/"+stale.view.ID) < 0 {
		t.Fatalf("seed did not update the existing HTTP tool: %v", fake.recorded())
	}
	if len(fake.tools) != 1 {
		t.Fatalf("HTTP tools = %d, want 1", len(fake.tools))
	}
	if stale.versions != 2 {
		t.Fatalf("definition versions = %d, want 2", stale.versions)
	}
	want := "http://127.0.0.1:19002/api/weather"
	if stale.view.Definition.URL != want || stale.view.Definition.Method != "GET" || stale.view.Definition.Description != "The forecast for a city." {
		t.Fatalf("updated definition = %+v", stale.view.Definition)
	}
	live := fake.liveAgents()
	if len(live) != 1 {
		t.Fatalf("live agents = %d, want 1", len(live))
	}
	if got.Created.HTTPTools["weather_forecast"] != stale.view.Hash {
		t.Fatalf("report hash %q != current hash %q", got.Created.HTTPTools["weather_forecast"], stale.view.Hash)
	}
	for _, pin := range live[0].Pins {
		if strings.HasPrefix(pin.ServerID, httpToolRefPrefix) && pin.SchemaHash != stale.view.Hash {
			t.Fatalf("agent pinned %q, current hash is %q", pin.SchemaHash, stale.view.Hash)
		}
	}
}

func TestHTTPToolConflictIsRefusedWithoutYes(t *testing.T) {
	demo := httpTestDemo()
	fake, serverURL := newSeededFake(t, demo)
	stale := fake.addHTTPTool(httpDefinitionRequest{
		Name: "weather_forecast", Description: "Another team's tool.", Method: "GET",
		URL: "http://example.invalid/other", InputSchema: json.RawMessage(`{"type":"object"}`),
	})
	_, err := runSeed(t, serverURL, demo)
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("seed rewrote a same-named HTTP tool without --yes: %v", err)
	}
	if fake.indexOf("PUT /api/v1/http-tools/"+stale.view.ID) >= 0 {
		t.Fatalf("the existing HTTP tool was updated: %v", fake.recorded())
	}
}

func TestDisabledHTTPToolIsEnabledAndPinned(t *testing.T) {
	demo := httpTestDemo()
	fake, serverURL := newSeededFake(t, demo)
	tool := fake.addHTTPTool(httpDefinitionRequest{
		Name: "weather_forecast", Description: "The forecast for a city.", Method: "GET",
		URL: "http://127.0.0.1:19002/api/weather", InputSchema: demo.httpTools[0].InputSchema,
	})
	tool.view.Enabled = false
	if _, err := runSeed(t, serverURL, demo); err != nil {
		t.Fatal(err)
	}
	if !tool.view.Enabled {
		t.Fatal("the disabled HTTP tool was not enabled")
	}
	live := fake.liveAgents()
	if len(live) != 1 {
		t.Fatalf("live agents = %d", len(live))
	}
	for _, pin := range live[0].Pins {
		if strings.HasPrefix(pin.ServerID, httpToolRefPrefix) && pin.SchemaHash != tool.view.Hash {
			t.Fatalf("agent pinned %q, current hash is %q", pin.SchemaHash, tool.view.Hash)
		}
	}
}

func TestWipeOnlyRefusesASecretUsedByAnotherHTTPTool(t *testing.T) {
	demo := defaultTestDemo()
	fake, serverURL := newSeededFake(t, demo)
	agent := fake.addAgent("Test front desk", false)
	tool := fake.addHTTPTool(httpDefinitionRequest{
		Name: "someone_elses_tool", Description: "Uses the demo secret.", Method: "GET",
		URL: "http://example.invalid/x", InputSchema: json.RawMessage(`{"type":"object"}`),
	})
	tool.view.Definition.Auth = json.RawMessage(`{"secret":"demo-test-mcp"}`)
	fake.setSecret("demo-test-mcp", "someone-elses-token")
	if _, err := runSeed(t, serverURL, demo, func(o *options) {
		o.yes = true
		o.wipeOnly = true
		o.mcpBases = nil
	}); err == nil || !strings.Contains(err.Error(), "HTTP tool") {
		t.Fatalf("wipe-only did not refuse an HTTP tool that uses the secret: %v", err)
	}
	if agent.Archived {
		t.Fatal("the demo agent was archived before the conflict was refused")
	}
}

func TestWipeOnlyRefusesASecretUsedByAnotherServer(t *testing.T) {
	demo := defaultTestDemo()
	fake, serverURL := newSeededFake(t, demo)
	agent := fake.addAgent("Test front desk", false)
	fake.addServer("Somebody else", "http://127.0.0.1:19001/mcp", "demo-test-mcp", demo.servers[0].Tools)
	fake.setSecret("demo-test-mcp", "someone-elses-token")
	if _, err := runSeed(t, serverURL, demo, func(o *options) {
		o.yes = true
		o.wipeOnly = true
		o.mcpBases = nil
	}); err == nil || !strings.Contains(err.Error(), "not part of demo") {
		t.Fatalf("wipe-only did not refuse a shared secret: %v", err)
	}
	if agent.Archived {
		t.Fatal("the demo agent was archived before the conflict was refused")
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	demo := httpTestDemo()
	fake, serverURL := newSeededFake(t, demo)
	fake.addAgent("Test front desk", false)
	fake.addServer("Test scheduling", "http://127.0.0.1:19001/mcp", "demo-test-mcp", demo.servers[0].Tools)
	fake.setSecret("demo-test-mcp", "stale-token")
	fake.addHTTPTool(httpDefinitionRequest{
		Name: "weather_forecast", Description: "The forecast for a city.", Method: "GET",
		URL: "http://127.0.0.1:19002/api/weather", InputSchema: demo.httpTools[0].InputSchema,
	})
	fake.resetOps()

	got, err := runSeed(t, serverURL, demo, func(o *options) { o.dryRun = true })
	if err != nil {
		t.Fatal(err)
	}
	if writes := fake.writes(); len(writes) != 0 {
		t.Fatalf("dry run wrote: %v", writes)
	}
	if !got.DryRun {
		t.Fatal("report does not mark the dry run")
	}
	joined := strings.Join(got.Actions, "\n")
	for _, want := range []string{"would archive demo agent", "would detach-delete MCP server", "would delete token secret", "would create agent"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("dry-run actions missing %q:\n%s", want, joined)
		}
	}
}

func TestMissingTokenFailsBeforeWrites(t *testing.T) {
	demo := defaultTestDemo()
	fake, serverURL := newSeededFake(t, demo)

	_, err := runSeed(t, serverURL, demo, func(o *options) {
		o.getenv = testGetenv(map[string]string{})
	})
	if err == nil {
		t.Fatal("seed succeeded without the MCP token")
	}
	if !strings.Contains(err.Error(), "TEST_MCP_TOKEN") {
		t.Fatalf("error does not name the environment variable: %v", err)
	}
	if writes := fake.writes(); len(writes) != 0 {
		t.Fatalf("seed wrote before refusing: %v", writes)
	}
}

func TestDiscoveryMissingToolFails(t *testing.T) {
	demo := defaultTestDemo()
	fake, serverURL := newSeededFake(t, demo)
	fake.discovery = []string{"find_patient"} // book_appointment is absent

	_, err := runSeed(t, serverURL, demo)
	if err == nil {
		t.Fatal("seed succeeded with a missing discovered tool")
	}
	if !strings.Contains(err.Error(), "book_appointment") {
		t.Fatalf("error does not name the missing tool: %v", err)
	}
	if live := fake.liveAgents(); len(live) != 0 {
		t.Fatalf("an agent was created despite the missing tool: %v", live)
	}
}

func TestNonReadyAgentFails(t *testing.T) {
	demo := defaultTestDemo()
	fake, serverURL := newSeededFake(t, demo)
	fake.agentState = "not_ready"
	fake.agentReasons = []string{"required_tool_unavailable"}

	got, err := runSeed(t, serverURL, demo)
	if err == nil {
		t.Fatal("seed accepted a not_ready agent")
	}
	if !strings.Contains(err.Error(), "not_ready") {
		t.Fatalf("error does not report the readiness state: %v", err)
	}
	if got.Created.AgentID == "" {
		t.Fatal("report lost the created agent id")
	}
	if live := fake.liveAgents(); len(live) != 1 {
		t.Fatalf("live agents = %d, want the created agent to remain for inspection", len(live))
	}
}

func TestWipeOnlyNeedsNoMCPURLs(t *testing.T) {
	demo := defaultTestDemo()
	fake, serverURL := newSeededFake(t, demo)
	agent := fake.addAgent("Test front desk", false)
	fake.addServer("Test scheduling", "http://127.0.0.1:19001/mcp", "demo-test-mcp", demo.servers[0].Tools)
	fake.resetOps()
	if _, err := runSeed(t, serverURL, demo, func(o *options) {
		o.yes = true
		o.wipeOnly = true
		o.mcpBases = nil
		o.httpBase = ""
	}); err != nil {
		t.Fatalf("wipe-only without --mcp: %v", err)
	}
	if !agent.Archived {
		t.Fatal("the live agent was not archived")
	}
}

func TestWipeOnlyLeavesNothingAndCreatesNothing(t *testing.T) {
	demo := defaultTestDemo()
	fake, serverURL := newSeededFake(t, demo)
	agent := fake.addAgent("Test front desk", false)
	server := fake.addServer("Test scheduling", "http://127.0.0.1:19001/mcp", "demo-test-mcp", demo.servers[0].Tools)
	fake.setSecret("demo-test-mcp", "stale-token")
	fake.resetOps()

	got, err := runSeed(t, serverURL, demo, func(o *options) {
		o.yes = true
		o.wipeOnly = true
	})
	if err != nil {
		t.Fatal(err)
	}
	if !agent.Archived {
		t.Fatal("the live agent was not archived")
	}
	if !server.gone {
		t.Fatal("the MCP server was not deleted")
	}
	if _, ok := fake.secrets["demo-test-mcp"]; ok {
		t.Fatal("the token secret was not deleted")
	}
	if got.Created.AgentID != "" || len(got.Created.MCPServers) != 0 {
		t.Fatalf("wipe-only created resources: %+v", got.Created)
	}
	if got.Wiped.AgentsArchived != 1 || got.Wiped.MCPServersDeleted != 1 || got.Wiped.SecretsDeleted != 1 {
		t.Fatalf("wipe report = %+v", got.Wiped)
	}
	for _, op := range fake.writes() {
		if strings.HasPrefix(op, "POST /api/v1/mcp-servers ") || strings.HasPrefix(op, "POST /api/v1/agents ") ||
			strings.HasPrefix(op, "PUT /api/v1/secrets/") || strings.HasPrefix(op, "POST /api/v1/http-tools") {
			t.Fatalf("wipe-only created something: %v", fake.writes())
		}
	}
}

func TestExistingDemoRequiresYes(t *testing.T) {
	demo := defaultTestDemo()
	fake, serverURL := newSeededFake(t, demo)
	fake.addAgent("Test front desk", false)
	fake.resetOps()

	_, err := runSeed(t, serverURL, demo)
	if err == nil {
		t.Fatal("seed wiped an existing demo without --yes")
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("error does not ask for --yes: %v", err)
	}
	if writes := fake.writes(); len(writes) != 0 {
		t.Fatalf("seed wrote before refusing without --yes: %v", writes)
	}

	if _, err := runSeed(t, serverURL, demo, func(o *options) { o.yes = true }); err != nil {
		t.Fatalf("--yes did not allow the wipe: %v", err)
	}
}

func TestDetachDeletePollsUntilTheRowIsGone(t *testing.T) {
	demo := defaultTestDemo()
	fake, serverURL := newSeededFake(t, demo)
	server := fake.addServer("Test scheduling", "http://127.0.0.1:19001/mcp", "demo-test-mcp", demo.servers[0].Tools)
	fake.pendingPolls = 2
	fake.resetOps()

	got, err := runSeed(t, serverURL, demo, func(o *options) {
		o.yes = true
		o.wipeOnly = true
	})
	if err != nil {
		t.Fatal(err)
	}
	if !server.gone {
		t.Fatal("the pending MCP server was not removed")
	}
	if got.Wiped.MCPServersDeleted != 1 {
		t.Fatalf("wipe report = %+v", got.Wiped)
	}
	if polls := fake.indexOf("GET /api/v1/mcp-servers/" + server.row.ID); polls < 0 {
		t.Fatalf("seed did not poll the pending row: %v", fake.recorded())
	}
}

func TestExtraMCPBaseIsIgnoredAndMissingBaseIsRefused(t *testing.T) {
	demo := defaultTestDemo()
	fake, serverURL := newSeededFake(t, demo)

	// The shared make target passes every demo's base URL; an unused one is ignored.
	if _, err := runSeed(t, serverURL, demo, func(o *options) {
		o.mcpBases = map[string]string{"scheduling": "http://127.0.0.1:19001", "inventory": "http://127.0.0.1:19002"}
	}); err != nil {
		t.Fatalf("extra base URL refused: %v", err)
	}
	fake.resetOps()
	_, err := runSeed(t, serverURL, demo, func(o *options) { o.mcpBases = map[string]string{} })
	if err == nil || !strings.Contains(err.Error(), "--mcp scheduling") {
		t.Fatalf("missing base: %v", err)
	}
	if writes := fake.writes(); len(writes) != 0 {
		t.Fatalf("a refused run wrote: %v", writes)
	}
}

func TestCLIReportsJSONAndNeverPrintsTheToken(t *testing.T) {
	demo, err := demos.Get("dental")
	if err != nil {
		t.Fatal(err)
	}
	fake := newFakeAPI(t, testKey)
	fake.discovery = demo.MCPServers()[0].Tools
	server := fake.serve()
	var stdout, stderr bytes.Buffer
	argv := []string{
		"--server", server.URL,
		"--demo", "dental",
		"--mcp", "scheduling=http://127.0.0.1:19001",
		"--json",
	}
	code := runCLI(context.Background(), argv, testGetenv(map[string]string{
		"GOVOICE_MANAGEMENT_API_KEY": testKey,
		"DENTAL_MCP_TOKEN":           testToken,
	}), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d; stderr: %s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), testToken) || strings.Contains(stderr.String(), testToken) {
		t.Fatal("the token value reached the output")
	}
	var got report
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, stdout.String())
	}
	if got.Demo != "dental" || got.Created.AgentID == "" || got.Created.Readiness != "ready" {
		t.Fatalf("report = %+v", got)
	}
	if fake.secrets["demo-dental-mcp"] != testToken {
		t.Fatal("the token was not stored under the demo secret name")
	}
}

func TestTokenAndManagementKeyAreTrimmed(t *testing.T) {
	demo := defaultTestDemo()
	fake, serverURL := newSeededFake(t, demo)
	got, err := runSeed(t, serverURL, demo, func(o *options) {
		o.apiKey = " " + testKey + "\n"
		o.getenv = testGetenv(map[string]string{"TEST_MCP_TOKEN": " " + testToken + "\n"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if fake.secrets["demo-test-mcp"] != testToken {
		t.Fatalf("stored token = %q, want %q", fake.secrets["demo-test-mcp"], testToken)
	}
	if got.Created.AgentID == "" {
		t.Fatal("no agent was created")
	}
}

func TestDryRunRunsPreflight(t *testing.T) {
	demo := defaultTestDemo()
	_, serverURL := newSeededFake(t, demo)
	if _, err := runSeed(t, serverURL, demo, func(o *options) {
		o.dryRun = true
		o.getenv = testGetenv(nil)
	}); err == nil || !strings.Contains(err.Error(), "TEST_MCP_TOKEN") {
		t.Fatalf("dry run did not run the token preflight: %v", err)
	}
}

func TestParseBaseURLRejectsUnsafeValuesWithoutEchoingThem(t *testing.T) {
	for _, bad := range []string{
		"https://user:super-secret@example.invalid",
		"https://example.invalid/path?token=super-secret",
		"https://example.invalid/path#frag",
		"https://example.invalid/mcp",
		"not a url",
	} {
		_, err := parseBaseURL(bad)
		if err == nil {
			t.Errorf("%q was accepted", bad)
			continue
		}
		msg := err.Error()
		for _, secret := range []string{"super-secret", "user:", "token="} {
			if strings.Contains(msg, secret) {
				t.Errorf("error for %q echoes %q: %s", bad, secret, msg)
			}
		}
	}
	for in, want := range map[string]string{
		"https://example.invalid/":       "https://example.invalid",
		" https://example.invalid/base ": "https://example.invalid/base",
	} {
		got, err := parseBaseURL(in)
		if err != nil || got != want {
			t.Errorf("parseBaseURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestCLIUsageErrors(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runCLI(context.Background(), []string{"--server", "http://127.0.0.1:1"}, testGetenv(nil), &stdout, &stderr); code != 2 {
		t.Fatalf("missing --demo exit code = %d", code)
	}
	if code := runCLI(context.Background(), []string{"--demo", "dental"}, testGetenv(nil), &stdout, &stderr); code != 2 {
		t.Fatalf("missing --server exit code = %d", code)
	}
	if code := runCLI(context.Background(), []string{"--server", "http://127.0.0.1:1", "--demo", "nope"}, testGetenv(nil), &stdout, &stderr); code != 2 {
		t.Fatalf("unknown demo exit code = %d", code)
	}
	if code := runCLI(context.Background(), []string{"--server", "http://127.0.0.1:1", "--demo", "dental"}, testGetenv(nil), &stdout, &stderr); code != 2 {
		t.Fatalf("missing management key exit code = %d", code)
	}
	if !strings.Contains(stderr.String(), "GOVOICE_MANAGEMENT_API_KEY") {
		t.Fatalf("stderr does not name the key variable: %s", stderr.String())
	}
}

func TestPendingDeletionTimesOutWithTheConfiguredWait(t *testing.T) {
	demo := defaultTestDemo()
	fake, serverURL := newSeededFake(t, demo)
	fake.addServer("Test scheduling", "http://127.0.0.1:19001/mcp", "demo-test-mcp", demo.servers[0].Tools)
	fake.pendingPolls = 1000
	fake.resetOps()

	got, err := runSeed(t, serverURL, demo, func(o *options) {
		o.yes = true
		o.wipeOnly = true
		o.wait = 20 * time.Millisecond
	})
	if err == nil {
		t.Fatal("seed did not time out on a deletion that never finishes")
	}
	if !strings.Contains(err.Error(), "still pending deletion") {
		t.Fatalf("error does not report the pending deletion: %v", err)
	}
	if got.Wiped.MCPServersDeleted != 0 {
		t.Fatalf("a timed-out deletion was counted as deleted: %+v", got.Wiped)
	}
}
