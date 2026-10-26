package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"govoice/server/agent"
	"govoice/server/mcp/atlasmcp"
)

// atlasRig is a managed development server whose environment points at a live
// atlasmcp, so an install exercises the real MCP and HTTP tool services.
func atlasRig(t *testing.T, mods ...func(*Config)) *Server {
	t.Helper()
	if !xaiSpeechEnabled {
		t.Skip("the Atlas agent uses xAI TTS: build with -tags xaispeech")
	}
	t.Setenv("GOVOICE_MANAGEMENT_API_KEY", testMgmtKey)
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Token.Secret = "voice-test-secret-32-bytes-long-and-separate"
	cfg.MCP.AllowLocalHTTP = true
	for _, m := range mods {
		m(cfg)
	}
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	d, err := atlasmcp.New(atlasmcp.Options{Token: "atlas-test-token"})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(d.Handler())
	t.Cleanup(ts.Close)
	vars := testProviderVars(t)
	vars["ATLAS_DEMO_URL"] = ts.URL
	vars["MCP_TOKEN_ATLAS"] = "atlas-test-token"
	vars["ELEVENLABS_API_KEY"] = "test-elevenlabs-key"
	s.env = mapEnv(vars)
	enableTestManagement(t, s)
	return s
}

func TestDemoAPIReportsAtlasUnavailableWithoutTheEndpoint(t *testing.T) {
	s := apiTestServer(t)
	enableTestManagement(t, s)
	w := apiRequest(s, http.MethodGet, "/api/v1/demo", testMgmtKey, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"atlas":{"available":false`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if w := apiRequest(s, http.MethodPost, "/api/v1/demo/atlas", testMgmtKey, ""); w.Code != http.StatusConflict {
		t.Fatalf("install without the endpoint: %d", w.Code)
	}
}

func TestDemoAPIInstallsTheAtlasDemoOnce(t *testing.T) {
	s := atlasRig(t)
	type installResp struct {
		Data struct {
			AgentCreated bool   `json:"agent_created"`
			AgentID      string `json:"agent_id"`
			Tools        int    `json:"tools"`
			HTTPTools    int    `json:"http_tools"`
			HTTPCreated  int    `json:"http_tools_created"`
		} `json:"data"`
	}
	install := func() installResp {
		w := apiRequest(s, http.MethodPost, "/api/v1/demo/atlas", testMgmtKey, "")
		if w.Code != 200 {
			t.Fatalf("install: %d %s", w.Code, w.Body.String())
		}
		var out installResp
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	first := install()
	if !first.Data.AgentCreated || first.Data.AgentID == "" || first.Data.Tools != 18 || first.Data.HTTPTools != 2 || first.Data.HTTPCreated != 2 {
		t.Fatalf("first install = %+v", first.Data)
	}
	second := install()
	if second.Data.AgentCreated || second.Data.AgentID != first.Data.AgentID || second.Data.HTTPCreated != 0 {
		t.Fatalf("second install = %+v", second.Data)
	}
	w := apiRequest(s, http.MethodGet, "/api/v1/demo", testMgmtKey, "")
	if !strings.Contains(w.Body.String(), `"atlas":{"available":true,"installed":true`) || !strings.Contains(w.Body.String(), first.Data.AgentID) {
		t.Fatalf("status: %s", w.Body.String())
	}
	got, err := s.agents.Get(context.Background(), first.Data.AgentID)
	if err != nil || got.Version == nil {
		t.Fatalf("agent: %+v %v", got, err)
	}
	def, err := agent.Decode(got.Version.Config)
	if err != nil || len(def.Steps) != 8 || len(def.Handoffs) != 32 {
		t.Fatalf("config: steps=%d handoffs=%d err=%v", len(def.Steps), len(def.Handoffs), err)
	}
	if len(got.Version.MCPTools) != 20 {
		t.Fatalf("pins = %d, want 20", len(got.Version.MCPTools))
	}
}

func TestDemoAPIReportsAtlasUnavailableWithoutTheToken(t *testing.T) {
	s := atlasRig(t)
	base := s.env
	s.env = func(k string) string {
		if k == "MCP_TOKEN_ATLAS" {
			return ""
		}
		return base(k)
	}
	w := apiRequest(s, http.MethodGet, "/api/v1/demo", testMgmtKey, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"atlas":{"available":false`) {
		t.Fatalf("status: %d %s", w.Code, w.Body.String())
	}
	w = apiRequest(s, http.MethodPost, "/api/v1/demo/atlas", testMgmtKey, "")
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "MCP_TOKEN_ATLAS") {
		t.Fatalf("install: %d %s", w.Code, w.Body.String())
	}
}

func TestDemoAutoInstallAtlasAtStart(t *testing.T) {
	s := atlasRig(t, func(c *Config) {
		c.Demo.AutoInstall = "atlas"
		c.PublicListenAddr, c.AdminListenAddr = "127.0.0.1:0", "127.0.0.1:0"
	})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	})
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		w := apiRequest(s, http.MethodGet, "/api/v1/demo", testMgmtKey, "")
		if strings.Contains(w.Body.String(), `"atlas":{"available":true,"installed":true`) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the Atlas demo was not auto-installed")
}

func TestDemoConfigAcceptsAtlasAndRefusesUnknown(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Token.Secret = "voice-test-secret-32-bytes-long-and-separate"
	cfg.Demo.AutoInstall = "atlas"
	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("atlas must be accepted: %v", err)
	}
	cfg.Demo.AutoInstall = "travel"
	if err := ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), "demo.auto_install") {
		t.Fatalf("unknown demo = %v", err)
	}
}
