package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"govoice/server/mcp/dentalmcp"
)

// dentalRig is a managed development server whose environment points at a live dentalmcp.
func dentalRig(t *testing.T, mods ...func(*Config)) *Server {
	t.Helper()
	if !xaiSpeechEnabled {
		t.Skip("the dental flow uses xAI TTS: build with -tags xaispeech")
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
	d, err := dentalmcp.New(dentalmcp.Options{Token: "dental-test-token"})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(d.Handler())
	t.Cleanup(ts.Close)
	vars := testProviderVars(t)
	vars["DENTAL_MCP_URL"] = ts.URL + "/mcp"
	vars["MCP_TOKEN_DENTAL"] = "dental-test-token"
	vars["ELEVENLABS_API_KEY"] = "test-elevenlabs-key"
	s.env = mapEnv(vars)
	enableTestManagement(t, s)
	return s
}

func TestDemoAPIReportsAvailabilityAndInstalls(t *testing.T) {
	s := apiTestServer(t)
	enableTestManagement(t, s)
	w := apiRequest(s, http.MethodGet, "/api/v1/demo", testMgmtKey, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"available":false`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if w := apiRequest(s, http.MethodPost, "/api/v1/demo/dental", testMgmtKey, ""); w.Code != http.StatusConflict {
		t.Fatalf("install without the endpoint: %d", w.Code)
	}
	if w := apiRequest(s, http.MethodGet, "/api/v1/demo", "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("no key: %d", w.Code)
	}
}

func TestDemoAPIInstallsTheDentalDemoOnce(t *testing.T) {
	s := dentalRig(t)
	install := func() (created bool, flowID string) {
		w := apiRequest(s, http.MethodPost, "/api/v1/demo/dental", testMgmtKey, "")
		if w.Code != 200 {
			t.Fatalf("install: %d %s", w.Code, w.Body.String())
		}
		var out struct {
			Data struct {
				FlowCreated bool   `json:"flow_created"`
				FlowID      string `json:"flow_id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Data.FlowCreated, out.Data.FlowID
	}
	created, id := install()
	if !created || id == "" {
		t.Fatalf("first install created=%v id=%q", created, id)
	}
	if again, id2 := install(); again || id2 != id {
		t.Fatalf("second install created=%v id=%q want %q", again, id2, id)
	}
	w := apiRequest(s, http.MethodGet, "/api/v1/demo", testMgmtKey, "")
	if !strings.Contains(w.Body.String(), `"installed":true`) || !strings.Contains(w.Body.String(), id) {
		t.Fatalf("status: %s", w.Body.String())
	}
}

func TestDemoAutoInstallAtStart(t *testing.T) {
	s := dentalRig(t, func(c *Config) {
		c.Demo.AutoInstall = "dental"
		c.PublicListenAddr, c.AdminListenAddr = "127.0.0.1:0", "127.0.0.1:0"
	})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.adminServer.Close(); s.publicServer.Close() })
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		w := apiRequest(s, http.MethodGet, "/api/v1/demo", testMgmtKey, "")
		if strings.Contains(w.Body.String(), `"installed":true`) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("dental demo was not auto-installed")
}

func TestDemoAutoInstallWithoutManagementDoesNotPanic(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Token.Secret = "voice-test-secret-32-bytes-long-and-separate"
	cfg.Demo.AutoInstall = "dental"
	cfg.PublicListenAddr, cfg.AdminListenAddr = "127.0.0.1:0", "127.0.0.1:0"
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.env = mapEnv(map[string]string{"DENTAL_MCP_URL": "http://127.0.0.1:1/mcp"})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.adminServer.Close(); s.publicServer.Close() })
	time.Sleep(200 * time.Millisecond) // a panic in a background goroutine would kill the test binary
}
