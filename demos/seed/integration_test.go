package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"govoice/demos"
	"govoice/demos/dental/mcp/dentalmcp"
	"govoice/demos/spec"
	"govoice/server"
)

// TestIntegrationSeedsTheRealDentalDemoTwice starts the real server on SQLite
// and the real dental MCP fixture, then runs seed twice through the management
// API. The second run must leave exactly one live, callable agent while the
// first one is archived, and it must reuse the same tools.
func TestIntegrationSeedsTheRealDentalDemoTwice(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// The dental demo uses these provider stacks; presence of their credentials
	// makes readiness green without any provider call being made.
	serviceAccount := filepath.Join(t.TempDir(), "service-account.json")
	if err := os.WriteFile(serviceAccount, []byte(`{"type":"service_account"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	const managementKey = "integration-management-key-32-bytes"
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", serviceAccount)
	t.Setenv("GOOGLE_API_KEY", "integration-google-api-key")
	t.Setenv("XAI_API_KEY", "integration-xai-api-key")
	t.Setenv("ELEVENLABS_API_KEY", "integration-elevenlabs-api-key")
	t.Setenv("GOVOICE_MANAGEMENT_API_KEY", managementKey)
	t.Setenv("SWITCHBOARD_SECRETS_KEY", "k1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("GOVOICE_DATABASE_DSN", filepath.Join(t.TempDir(), "agents.db"))

	adminAddr := freeLoopbackAddr(t)
	config := server.DefaultConfig(server.ProfileDevelopment)
	config.PublicListenAddr = "127.0.0.1:0"
	config.AdminListenAddr = adminAddr
	config.MCP.AllowLocalHTTP = true
	config.Token.Secret = server.DevInsecureTokenSecret
	srv, err := server.NewServer(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.EnableManagement(ctx); err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		shutdownCtx, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		_ = srv.Shutdown(shutdownCtx)
	})

	const mcpToken = "integration-dental-token"
	fixture, err := dentalmcp.New(dentalmcp.Options{Token: mcpToken})
	if err != nil {
		t.Fatal(err)
	}
	fixtureServer := httptest.NewServer(fixture.Handler())
	t.Cleanup(fixtureServer.Close)

	baseDemo, err := demos.Get("dental")
	if err != nil {
		t.Fatal(err)
	}
	demo := echoDemo{base: baseDemo}
	serverURL := "http://" + adminAddr
	options := options{
		serverURL:      serverURL,
		apiKey:         managementKey,
		keyEnv:         "GOVOICE_MANAGEMENT_API_KEY",
		demo:           demo,
		mcpBases:       map[string]string{"scheduling": fixtureServer.URL},
		httpBase:       fixtureServer.URL,
		wait:           20 * time.Second,
		pollInterval:   50 * time.Millisecond,
		requestTimeout: 30 * time.Second,
		getenv:         testGetenv(map[string]string{"DENTAL_MCP_TOKEN": mcpToken}),
		log:            os.Stderr,
	}

	first, err := run(ctx, options)
	if err != nil {
		t.Fatalf("first seed: %v", err)
	}
	if first.Created.AgentID == "" || first.Created.Readiness != "ready" {
		t.Fatalf("first seed report = %+v", first.Created)
	}
	assertOneLiveDentalAgent(t, ctx, serverURL, managementKey, demo.MCPServers()[0].Name, demo.MCPServers()[0].SecretName, first.Created.AgentID)

	options.yes = true
	second, err := run(ctx, options)
	if err != nil {
		t.Fatalf("second seed: %v", err)
	}
	if second.Created.AgentID == "" || second.Created.AgentID == first.Created.AgentID {
		t.Fatalf("second seed agent = %q, first = %q", second.Created.AgentID, first.Created.AgentID)
	}
	if second.Created.Readiness != "ready" {
		t.Fatalf("second seed readiness = %q", second.Created.Readiness)
	}
	if second.Wiped.AgentsArchived != 1 || second.Wiped.MCPServersDeleted != 1 || second.Wiped.SecretsDeleted != 1 {
		t.Fatalf("second seed wiped = %+v", second.Wiped)
	}
	assertOneLiveDentalAgent(t, ctx, serverURL, managementKey, demo.MCPServers()[0].Name, demo.MCPServers()[0].SecretName, second.Created.AgentID)

	// The first run's agent remains stored and archived; only one is live.
	agents := listAgents(t, ctx, serverURL, managementKey)
	if len(agents) != 2 {
		t.Fatalf("stored agents = %d, want 2 (one archived, one live): %+v", len(agents), agents)
	}
	live := 0
	for _, agent := range agents {
		if !agent.Archived {
			live++
		}
		if agent.Name != "Smile Dental front desk" {
			t.Fatalf("unexpected agent name %q", agent.Name)
		}
	}
	if live != 1 {
		t.Fatalf("live agents = %d, want 1", live)
	}
}

// echoDemo keeps the real demo's MCP servers, tools and agent shape but swaps
// every provider stack for the keyless echo provider. The default build has no
// xAI speech adapters, so the shipped dental stages (google, elevenlabs, xai)
// could never be ready here; the management-API behaviour under test is the
// wipe/create/reset path, not the provider wiring (the browser end-to-end suite
// covers that call path).
type echoDemo struct{ base spec.Demo }

func (d echoDemo) Name() string { return d.base.Name() }

func (d echoDemo) MCPServers() []spec.MCPServerSpec { return d.base.MCPServers() }

func (d echoDemo) HTTPTools() []spec.HTTPToolSpec { return d.base.HTTPTools() }

func (d echoDemo) Agent(r spec.Resolution) (spec.AgentSpec, error) {
	agent, err := d.base.Agent(r)
	if err != nil {
		return spec.AgentSpec{}, err
	}
	config, err := echoConfig(agent.Config)
	if err != nil {
		return spec.AgentSpec{}, err
	}
	return spec.AgentSpec{Name: agent.Name, Description: agent.Description, Config: config}, nil
}

// echoConfig rewrites every stt/llm/tts stage to the echo provider while
// keeping steps, tool references, handoffs and the stages' prompt fields.
func echoConfig(raw json.RawMessage) (json.RawMessage, error) {
	var config map[string]any
	if err := json.Unmarshal(raw, &config); err != nil {
		return nil, err
	}
	steps, _ := config["steps"].([]any)
	for _, stepValue := range steps {
		step, ok := stepValue.(map[string]any)
		if !ok {
			continue
		}
		stage, ok := step["config"].(map[string]any)
		if !ok {
			continue
		}
		for _, name := range []string{"stt", "llm", "tts"} {
			original, ok := stage[name].(map[string]any)
			if !ok {
				continue
			}
			stage[name] = echoStage(name, original)
		}
	}
	return json.Marshal(config)
}

func echoStage(name string, original map[string]any) map[string]any {
	out := map[string]any{"provider": "echo"}
	switch name {
	case "stt":
		out["model"] = "text"
	case "llm":
		out["model"] = "m"
	case "tts":
		out["voice"] = "v"
	}
	for field, value := range original {
		switch field {
		case "provider", "model", "voice":
			continue
		}
		out[field] = value
	}
	return out
}

// assertOneLiveDentalAgent checks the management API's own view: one live agent
// with the created id and ready readiness, one enabled healthy MCP server with
// the demo secret, and that secret in the encrypted store.
func assertOneLiveDentalAgent(t *testing.T, ctx context.Context, serverURL, key, serverName, secretName, wantAgentID string) {
	t.Helper()
	agents := listAgents(t, ctx, serverURL, key)
	live := make([]agentRow, 0, 1)
	for _, agent := range agents {
		if !agent.Archived {
			live = append(live, agent)
		}
	}
	if len(live) != 1 || live[0].ID != wantAgentID {
		t.Fatalf("live agents = %+v, want exactly %s", live, wantAgentID)
	}
	var detail envelope[agentDetail]
	getJSON(t, ctx, serverURL+"/api/v1/agents/"+wantAgentID, key, &detail)
	if detail.Data.Readiness.State != "ready" {
		t.Fatalf("agent readiness = %+v", detail.Data.Readiness)
	}
	var servers envelope[[]mcpServerRow]
	getJSON(t, ctx, serverURL+"/api/v1/mcp-servers", key, &servers)
	found := false
	for _, row := range servers.Data {
		if row.Name != serverName {
			continue
		}
		found = true
		if !row.Enabled || row.ValidationState != "healthy" || row.AuthEnv != secretName {
			t.Fatalf("MCP server = %+v", row)
		}
	}
	if !found {
		t.Fatalf("no %q MCP server in %+v", serverName, servers.Data)
	}
	var secrets envelope[secretListView]
	getJSON(t, ctx, serverURL+"/api/v1/secrets", key, &secrets)
	if !secrets.Data.StoreEnabled {
		t.Fatal("the secret store is off")
	}
	stored := false
	for _, secret := range secrets.Data.Secrets {
		if secret.Name == secretName {
			stored = true
		}
	}
	if !stored {
		t.Fatalf("secret %q is not in %+v", secretName, secrets.Data.Secrets)
	}
}

func listAgents(t *testing.T, ctx context.Context, serverURL, key string) []agentRow {
	t.Helper()
	var response envelope[[]agentRow]
	getJSON(t, ctx, serverURL+"/api/v1/agents?limit=100&offset=0", key, &response)
	return response.Data
}

func getJSON(t *testing.T, ctx context.Context, url, key string, out any) {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+key)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", url, response.StatusCode)
	}
	if err := json.NewDecoder(response.Body).Decode(out); err != nil {
		t.Fatalf("GET %s: decode: %v", url, err)
	}
}

// freeLoopbackAddr reserves a loopback port and returns its address. The server
// does not expose the bound address of a :0 listener, so a test picks one.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}
