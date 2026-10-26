package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"govoice/server/agent"
	"govoice/server/demo"
	"govoice/server/mcp"
	"govoice/server/mcp/dentalmcp"
	"govoice/server/storage"
)

// dentalRig is a managed development server whose environment points at a live dentalmcp.
func dentalRig(t *testing.T, mods ...func(*Config)) *Server {
	t.Helper()
	if !xaiSpeechEnabled {
		t.Skip("the dental agent uses xAI TTS: build with -tags xaispeech")
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
	install := func() (created bool, agentID string) {
		w := apiRequest(s, http.MethodPost, "/api/v1/demo/dental", testMgmtKey, "")
		if w.Code != 200 {
			t.Fatalf("install: %d %s", w.Code, w.Body.String())
		}
		var out struct {
			Data struct {
				AgentCreated bool   `json:"agent_created"`
				AgentID      string `json:"agent_id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Data.AgentCreated, out.Data.AgentID
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
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	})
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

// An install requested after Shutdown began must not start (Shutdown may already have waited).
func TestAutoInstallDemoSkipsOnceShuttingDown(t *testing.T) {
	s := dentalRig(t)
	s.shuttingDown.Store(true)
	s.autoInstallDemo()
	if s.stopDemo != nil || !s.waitDemo(context.Background()) {
		t.Fatal("an install was started while shutting down")
	}
	if f, err := demo.FindAgent(context.Background(), s.agents); err != nil || f.ID != "" {
		t.Fatalf("agent installed during shutdown: %+v %v", f, err)
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

type racyAgents struct {
	list    []agent.Agent
	creates int
	err     error
}

func (f *racyAgents) List(context.Context, int, int) ([]agent.Agent, error) {
	time.Sleep(20 * time.Millisecond)
	return append([]agent.Agent(nil), f.list...), f.err
}
func (f *racyAgents) Create(_ context.Context, r agent.CreateRequest) (agent.Agent, error) {
	f.creates++
	fl := agent.Agent{ID: "agent1", Name: r.Name}
	f.list = append(f.list, fl)
	return fl, nil
}

// The startup auto-install and a "Load demo" click must not both create the demo.
func TestInstallDentalIsSerialised(t *testing.T) {
	s, srv, fl := &Server{}, &racyServers{}, &racyAgents{}
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
		t.Fatalf("server creates %d, agent creates %d, want 1 and 1", srv.creates, fl.creates)
	}
}

// A failed agent lookup is an error, never "not installed" (which would offer a duplicate).
func TestDemoStatusAgentLookupError(t *testing.T) {
	if _, err := demoStatus(context.Background(), &racyAgents{err: errors.New("db down")}, "https://d/mcp", "", ""); err == nil {
		t.Fatal("lookup error must surface")
	}
	st, err := demoStatus(context.Background(), &racyAgents{list: []agent.Agent{{ID: "f1", Name: demo.AgentName}}}, "https://d/mcp", "", "")
	if err != nil || !st.Dental.Installed || st.Dental.AgentID != "f1" || !st.Dental.Available {
		t.Fatalf("%+v %v", st, err)
	}
}

func (f *racyAgents) Archive(context.Context, string) error { return nil }

// gatedServers hands the context an install runs on to the test, then waits for release. With
// ignoreCancel it keeps waiting after that context is cancelled (an install that has not reached a
// cancellation point yet).
type gatedServers struct {
	*racyServers
	got          chan context.Context
	release      chan struct{}
	ignoreCancel bool
}

func newGatedServers(ignoreCancel bool) *gatedServers {
	return &gatedServers{racyServers: &racyServers{}, got: make(chan context.Context, 1), release: make(chan struct{}), ignoreCancel: ignoreCancel}
}

func (g *gatedServers) List(ctx context.Context) ([]storage.MCPServer, error) {
	select {
	case g.got <- ctx:
	default:
	}
	if g.ignoreCancel {
		<-g.release
	} else {
		select {
		case <-g.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return g.racyServers.List(ctx)
}

// The install is shared by every caller (singleflight): the caller that started it hanging up must not
// cancel it, or every joiner fails with context.Canceled too.
func TestDentalInstallOutlivesTheCallerThatStartedIt(t *testing.T) {
	s, fl, srv := &Server{}, &racyAgents{}, newGatedServers(false)
	caller, hangUp := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, err := s.installDental(caller, srv, fl, "https://d/mcp"); first <- err }()
	flight := <-srv.got
	hangUp()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("the caller that hung up got %v, want context.Canceled", err)
	}
	if err := flight.Err(); err != nil {
		t.Fatalf("the shared install was cancelled with its first caller (%v): every joiner would fail", err)
	}
	close(srv.release)
	if _, err := s.installDental(context.Background(), srv, fl, "https://d/mcp"); err != nil {
		t.Fatal(err)
	}
	if srv.creates != 1 || fl.creates != 1 {
		t.Fatalf("server creates %d, agent creates %d, want one install", srv.creates, fl.creates)
	}
}

// Shutdown cancels a running install and waits for it (waitDemo) before it closes the store; once shutting
// down, no install starts.
func TestShutdownCancelsTheDemoInstallAndWaitsForIt(t *testing.T) {
	s, fl, srv := &Server{}, &racyAgents{}, newGatedServers(true)
	done := make(chan error, 1)
	go func() { _, err := s.installDental(context.Background(), srv, fl, "https://d/mcp"); done <- err }()
	flight := <-srv.got
	s.mu.Lock() // what Shutdown does first
	s.shuttingDown.Store(true)
	s.stopDemoLocked()
	s.mu.Unlock()
	if !errors.Is(flight.Err(), context.Canceled) {
		t.Fatalf("Shutdown did not cancel the install: %v", flight.Err())
	}
	short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if s.waitDemo(short) {
		t.Fatal("waitDemo returned while the install still ran: the store would close under it")
	}
	close(srv.release)
	if !s.waitDemo(context.Background()) {
		t.Fatal("waitDemo did not return after the install ended")
	}
	<-done
	if _, err := s.installDental(context.Background(), srv, fl, "https://d/mcp"); !errors.Is(err, errDemoShuttingDown) {
		t.Fatalf("an install after Shutdown began got %v, want errDemoShuttingDown", err)
	}
}

// End to end on a real management server: an API install is mid-discovery against an MCP endpoint that
// never answers when Shutdown begins. Shutdown must cancel that install (the endpoint sees the request
// abort), wait for it, and only then close the store, long before the install's own 30 s bound.
func TestShutdownCancelsARealDemoInstallBeforeClosingTheStore(t *testing.T) {
	s := dentalRig(t)
	reached, aborted := make(chan struct{}), make(chan struct{})
	var once, abortedOnce sync.Once
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(reached) })
		_, _ = io.Copy(io.Discard, r.Body) // net/http notices a hang-up only after the body is read
		<-r.Context().Done()
		abortedOnce.Do(func() { close(aborted) })
	}))
	t.Cleanup(hang.Close)
	base := s.env
	s.env = func(k string) string {
		if k == "DENTAL_MCP_URL" {
			return hang.URL + "/mcp"
		}
		return base(k)
	}
	posted := make(chan int, 1)
	go func() { posted <- apiRequest(s, http.MethodPost, "/api/v1/demo/dental", testMgmtKey, "").Code }()
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the install never reached the MCP endpoint")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	_ = s.Shutdown(ctx)
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("Shutdown took %v: it waited out the install instead of cancelling it", d)
	}
	select {
	case <-aborted:
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown returned but the install's request was never cancelled")
	}
	if code := <-posted; code != http.StatusBadGateway {
		t.Fatalf("the in-flight install request got %d, want 502", code)
	}
	if !s.waitDemo(context.Background()) {
		t.Fatal("the install is still registered after Shutdown")
	}
}

// A panic inside the shared install runs on singleflight's own goroutine, where no request or
// auto-install recover reaches it: it must come back as an error, not kill the daemon.
type panicServers struct{ *racyServers }

func (panicServers) List(context.Context) ([]storage.MCPServer, error) { panic("bad discovery") }

func TestDentalInstallPanicIsAnErrorNotACrash(t *testing.T) {
	s := &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	_, err := s.installDental(context.Background(), panicServers{&racyServers{}}, &racyAgents{}, "https://d/mcp")
	if err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("a panicking install returned %v, want a 'panicked' error", err)
	}
	if !s.waitDemo(context.Background()) {
		t.Fatal("the panicked install is still registered on the wait group")
	}
}

// Shutdown must wait for a running install (demoWG) before it closes the store.
func TestShutdownWaitsForTheDemoWaitGroup(t *testing.T) {
	s := dentalRig(t)
	s.demoWG.Add(1)
	done := make(chan struct{})
	go func() { _ = s.Shutdown(context.Background()); close(done) }()
	select {
	case <-done:
		t.Fatal("Shutdown returned (and closed the store) while a demo install was running")
	case <-time.After(300 * time.Millisecond):
	}
	s.demoWG.Done()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return after the install ended")
	}
}
