package server

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"govoice/server/mcp/mcptest"
)

const mcpRigKey = "management-test-key-32-bytes-long-and-separate"

// mcpRig is a server with the management plane, local test MCP servers on loopback
// (mcp.allow_local_http) and an environment the test can change while it runs.
type mcpRig struct {
	t      *testing.T
	s      *Server
	mcp    *mcptest.Server
	ts     *httptest.Server
	hole   string // black-hole address
	refuse string // a loopback port nothing listens on
	mu     sync.Mutex
	vars   map[string]string
}

func newMCPRig(t *testing.T, token string) *mcpRig {
	t.Helper()
	t.Setenv("GOVOICE_MANAGEMENT_API_KEY", mcpRigKey)
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Token.Secret = "voice-test-secret-32-bytes-long-and-separate"
	cfg.MCP.AllowLocalHTTP = true
	cfg.MCP.Timeout = 800 * time.Millisecond
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := &mcpRig{t: t, s: s, vars: testProviderVars(t)}
	s.env = func(k string) string { r.mu.Lock(); defer r.mu.Unlock(); return r.vars[k] }
	enableTestManagement(t, s)

	r.mcp = mcptest.New(mcptest.Options{Token: token})
	r.ts = httptest.NewServer(r.mcp.Handler())
	t.Cleanup(r.ts.Close)

	hl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, stop := mcptest.ServeBlackHole(hl)
	t.Cleanup(stop)
	r.hole = hl.Addr().String()

	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r.refuse = free.Addr().String()
	_ = free.Close()
	return r
}

func (r *mcpRig) setEnv(k, v string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if v == "" {
		delete(r.vars, k)
	} else {
		r.vars[k] = v
	}
}

func (r *mcpRig) api(method, path, body string) (int, map[string]any) {
	r.t.Helper()
	w := apiRequest(r.s, method, path, mcpRigKey, body)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// create registers an MCP server and returns its id ("" if refused).
func (r *mcpRig) create(name, endpoint, authEnv string) (string, int) {
	r.t.Helper()
	body, _ := json.Marshal(map[string]any{"name": name, "endpoint": endpoint, "auth_env": authEnv, "enabled": true})
	code, out := r.api(http.MethodPost, "/api/v1/mcp-servers", string(body))
	if code != http.StatusCreated {
		return "", code
	}
	return out["data"].(map[string]any)["id"].(string), code
}

// validate runs discovery and returns the server's validation state.
func (r *mcpRig) validate(id string) string {
	r.t.Helper()
	code, out := r.api(http.MethodPost, "/api/v1/mcp-servers/"+id+"/validate", "")
	if code != http.StatusOK {
		r.t.Fatalf("validate %s: %d %v", id, code, out)
	}
	return out["data"].(map[string]any)["validation_state"].(string)
}

// tool runs a tool test and returns the status code and the error message, if any.
func (r *mcpRig) tool(id, name, args string) (int, string) {
	r.t.Helper()
	code, out := r.api(http.MethodPost, "/api/v1/mcp-servers/"+id+"/tools/"+name+"/test", `{"arguments":`+args+`}`)
	if e, ok := out["error"].(map[string]any); ok {
		return code, e["message"].(string)
	}
	return code, ""
}

func (r *mcpRig) lastAuth(path string) string {
	recs := r.mcp.Records()
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].Path == path {
			return recs[i].Auth
		}
	}
	return ""
}

// settles waits for the goroutine count to come back near its baseline.
func settles(t *testing.T, before int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before+3 {
		if time.Now().After(deadline) {
			t.Fatalf("goroutines %d, baseline %d", runtime.NumGoroutine(), before)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestMCPAdminHealthyServers(t *testing.T) {
	r := newMCPRig(t, "tok-1")
	r.setEnv("MCP_TOKEN_TEST", "tok-1")
	ok, _ := r.create("ok", r.ts.URL+"/ok", "MCP_TOKEN_TEST")
	open, _ := r.create("open", r.ts.URL+"/open", "")
	if s := r.validate(ok); s != "healthy" {
		t.Fatalf("/ok state %s", s)
	}
	if s := r.validate(open); s != "healthy" {
		t.Fatalf("/open state %s", s)
	}
	if code, msg := r.tool(ok, "lookup_order", `{"order_id":"42"}`); code != http.StatusOK {
		t.Fatalf("tool test: %d %s", code, msg)
	}
	if a := r.lastAuth("/ok"); a != "match" {
		t.Fatalf("/ok auth %q", a)
	}
	if code, msg := r.tool(open, "echo", `{"text":"hi"}`); code != http.StatusOK {
		t.Fatalf("open tool: %d %s", code, msg)
	}
	if a := r.lastAuth("/open"); a != "none" {
		t.Fatalf("/open received a token it was not configured with: %q", a)
	}
}

func TestMCPAdminFailures(t *testing.T) {
	r := newMCPRig(t, "tok-1")
	before := runtime.NumGoroutine()
	cases := []struct{ name, endpoint, want string }{
		{"auth", r.ts.URL + "/fail/auth", "authentication_failed"},
		{"list", r.ts.URL + "/fail/list", "discovery_failed"},
		{"refused", "http://" + r.refuse + "/mcp", "connection_failed"},
		{"black hole", "http://" + r.hole + "/mcp", "connection_failed"},
	}
	for _, c := range cases {
		id, code := r.create(c.name, c.endpoint, "")
		if id == "" {
			t.Fatalf("%s: create refused (%d)", c.name, code)
		}
		began := time.Now()
		if s := r.validate(id); s != c.want {
			t.Errorf("%s: state %s, want %s", c.name, s, c.want)
		}
		if took := time.Since(began); took > r.s.cfg.MCP.Timeout+500*time.Millisecond {
			t.Errorf("%s: validate took %v, budget %v", c.name, took, r.s.cfg.MCP.Timeout)
		}
	}
	// A name that never resolves is refused when it is registered.
	if id, code := r.create("unresolvable", "http://mcp.example.invalid/mcp", ""); id != "" || code != http.StatusBadRequest {
		t.Fatalf("unresolvable host accepted: id=%q code=%d", id, code)
	}
	// Failing tools: each call fails, none brings the server down.
	ft, _ := r.create("fail tools", r.ts.URL+"/fail/tools", "")
	if s := r.validate(ft); s != "healthy" {
		t.Fatalf("/fail/tools state %s", s)
	}
	for _, tool := range []string{"tool_error", "protocol_error", "drop", "too_big"} {
		if code, _ := r.tool(ft, tool, `{}`); code == http.StatusOK {
			t.Errorf("%s: want a failure, got 200", tool)
		}
	}
	settles(t, before)
}

func TestMCPAdminWentAwayAndCameBack(t *testing.T) {
	r := newMCPRig(t, "tok-1")
	addr := r.ts.Listener.Addr().String()
	id, _ := r.create("open", r.ts.URL+"/open", "")
	if s := r.validate(id); s != "healthy" {
		t.Fatalf("state %s", s)
	}
	r.ts.Close()
	if s := r.validate(id); s != "connection_failed" {
		t.Fatalf("after shutdown: %s", s)
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Skipf("address %s not reusable: %v", addr, err)
	}
	back := httptest.NewUnstartedServer(mcptest.New(mcptest.Options{}).Handler())
	back.Listener = l
	back.Start()
	t.Cleanup(back.Close)
	if s := r.validate(id); s != "healthy" {
		t.Fatalf("after restart: %s", s)
	}
}

func TestMCPCredentialMatrix(t *testing.T) {
	r := newMCPRig(t, "tok-1")
	withVar, _ := r.create("ok with var", r.ts.URL+"/ok", "MCP_TOKEN_TEST")
	noEnv, _ := r.create("ok without auth_env", r.ts.URL+"/ok", "")

	// auth_env set, variable missing: refused before any request reaches the server.
	r.mcp.Reset()
	if s := r.validate(withVar); s == "healthy" {
		t.Fatalf("missing variable: state %s", s)
	}
	if n := len(r.mcp.Records()); n != 0 {
		t.Fatalf("a request was sent without the variable: %+v", r.mcp.Records())
	}
	_, out := r.api(http.MethodGet, "/api/v1/mcp-servers/"+withVar, "")
	if set, _ := out["data"].(map[string]any)["auth_env_set"].(bool); set {
		t.Fatal("auth_env_set reported true for a missing variable")
	}

	// No auth_env: typed authentication failure, the server saw no token.
	if s := r.validate(noEnv); s != "authentication_failed" {
		t.Fatalf("no auth_env: %s", s)
	}
	if a := r.lastAuth("/ok"); a != "none" {
		t.Fatalf("no auth_env sent a token: %q", a)
	}

	// Wrong value: typed authentication failure.
	r.setEnv("MCP_TOKEN_TEST", "wrong")
	if s := r.validate(withVar); s != "authentication_failed" {
		t.Fatalf("wrong token: %s", s)
	}
	if a := r.lastAuth("/ok"); a != "mismatch" {
		t.Fatalf("wrong token recorded as %q", a)
	}

	// Right value, then rotation: the next connection uses the new value, no restart.
	r.setEnv("MCP_TOKEN_TEST", "tok-1")
	if s := r.validate(withVar); s != "healthy" {
		t.Fatalf("right token: %s", s)
	}
	r.setEnv("MCP_TOKEN_TEST", "tok-2")
	if s := r.validate(withVar); s != "authentication_failed" {
		t.Fatalf("rotated variable not used: %s", s)
	}

	// A variable without the MCP_TOKEN_ prefix is refused at create.
	if id, code := r.create("bad env", r.ts.URL+"/ok", "GOVOICE_MANAGEMENT_API_KEY"); id != "" || code != http.StatusBadRequest {
		t.Fatalf("unprefixed auth_env accepted: id=%q code=%d", id, code)
	}
}

func TestMCPAllowLocalHTTPOffRefusesLoopback(t *testing.T) {
	t.Setenv("GOVOICE_MANAGEMENT_API_KEY", mcpRigKey)
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Token.Secret = "voice-test-secret-32-bytes-long-and-separate"
	cfg.MCP.AllowLocalHTTP = false
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.env = testProviderEnv(t)
	enableTestManagement(t, s)
	srv := httptest.NewServer(mcptest.New(mcptest.Options{}).Handler())
	defer srv.Close()
	w := apiRequest(s, http.MethodPost, "/api/v1/mcp-servers", mcpRigKey, `{"name":"x","endpoint":"`+srv.URL+`/open","enabled":true}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid_mcp_server") {
		t.Fatalf("loopback HTTP accepted with the flag off: %d %s", w.Code, w.Body.String())
	}
}
