package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"govoice/server/demo"
	"govoice/server/flow"
	"govoice/server/mcp"
	"govoice/server/mcp/dentalmcp"
	"govoice/server/storage"
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

func TestDemoAutoInstallWithoutManagementLogsAndReturns(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Token.Secret = "voice-test-secret-32-bytes-long-and-separate"
	cfg.Demo.AutoInstall = "dental"
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	s.logger = slog.New(slog.NewTextHandler(&logs, nil))
	s.env = mapEnv(map[string]string{"DENTAL_MCP_URL": "http://127.0.0.1:1/mcp"})
	s.autoInstallDemo() // returns before spawning anything: there is nothing to install into
	if !strings.Contains(logs.String(), "management storage is disabled") || s.stopDemo != nil {
		t.Fatalf("logs = %q, stopDemo set = %v", logs.String(), s.stopDemo != nil)
	}
}

// Shutdown must wait for the startup install before the store closes under it, but only as
// long as its own context allows.
func TestWaitDemoBlocksUntilTheInstallEndsOrTheContextExpires(t *testing.T) {
	s := &Server{}
	release := make(chan struct{})
	s.demoWG.Add(1)
	go func() { defer s.demoWG.Done(); <-release }()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if s.waitDemo(ctx) {
		t.Fatal("returned before the install finished")
	}
	close(release)
	if !s.waitDemo(context.Background()) {
		t.Fatal("did not return after the install finished")
	}
}

type racyServers struct {
	list    []storage.MCPServer
	creates int
}

func (f *racyServers) List(context.Context) ([]storage.MCPServer, error) {
	time.Sleep(20 * time.Millisecond) // widen the list-then-create window
	return append([]storage.MCPServer(nil), f.list...), nil
}
func (f *racyServers) Create(_ context.Context, c mcp.ServerConfig) (storage.MCPServer, error) {
	f.creates++
	m := storage.MCPServer{ID: "srv1", Name: c.Name, Endpoint: c.Endpoint, AuthEnv: c.AuthEnv, Enabled: c.Enabled}
	f.list = append(f.list, m)
	return m, nil
}
func (f *racyServers) Enable(context.Context, string) error { return nil }
func (f *racyServers) Refresh(context.Context, string) (storage.MCPDiscovery, error) {
	var d storage.MCPDiscovery
	for _, n := range dentalmcp.ToolNames {
		d.Tools = append(d.Tools, storage.MCPTool{RemoteName: n, SchemaHash: "h-" + n})
	}
	return d, nil
}

type racyFlows struct {
	list    []flow.Flow
	creates int
	err     error
}

func (f *racyFlows) List(context.Context, int, int) ([]flow.Flow, error) {
	time.Sleep(20 * time.Millisecond)
	return append([]flow.Flow(nil), f.list...), f.err
}
func (f *racyFlows) Create(_ context.Context, r flow.CreateRequest) (flow.Flow, error) {
	f.creates++
	fl := flow.Flow{ID: "flow1", Name: r.Name}
	f.list = append(f.list, fl)
	return fl, nil
}

// The startup auto-install and a "Load demo" click must not both create the demo.
func TestInstallDentalIsSerialised(t *testing.T) {
	s, srv, fl := &Server{}, &racyServers{}, &racyFlows{}
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.installDental(context.Background(), srv, fl, "https://d/mcp"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if srv.creates != 1 || fl.creates != 1 {
		t.Fatalf("server creates %d, flow creates %d, want 1 and 1", srv.creates, fl.creates)
	}
}

// A failed flow lookup is an error, never "not installed" (which would offer a duplicate).
func TestDemoStatusFlowLookupError(t *testing.T) {
	if _, err := demoStatus(context.Background(), &racyFlows{err: errors.New("db down")}, "https://d/mcp"); err == nil {
		t.Fatal("lookup error must surface")
	}
	st, err := demoStatus(context.Background(), &racyFlows{list: []flow.Flow{{ID: "f1", Name: demo.FlowName}}}, "https://d/mcp")
	if err != nil || !st.Dental.Installed || st.Dental.FlowID != "f1" || !st.Dental.Available {
		t.Fatalf("%+v %v", st, err)
	}
}

func (f *racyFlows) Archive(context.Context, string) error { return nil }
