//go:build fakeproviders

package server

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"

	"govoice/server/agent"
)

// lockedBuffer is a log sink safe for the server's concurrent writers.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// Spec section 8: a provider secret lives only in the environment. Nothing the
// server returns, exports or logs may carry one, across a real call.
func TestNoSecretLeavesTheServer(t *testing.T) {
	sentinels := []string{"sk-SENTINEL-xai", "SENTINEL-google", "SENTINEL-private-key", "SENTINEL-mcp"}
	sa := filepath.Join(t.TempDir(), "sa.json")
	if err := os.WriteFile(sa, []byte(`{"type":"service_account","client_email":"x@example.iam","private_key":"SENTINEL-private-key"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"XAI_API_KEY": "sk-SENTINEL-xai", "GOOGLE_API_KEY": "SENTINEL-google", "GOOGLE_APPLICATION_CREDENTIALS": sa, "MCP_TOKEN_CRM": "SENTINEL-mcp"}

	logs := &lockedBuffer{}
	captured := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	prev := slog.Default()
	slog.SetDefault(captured)
	t.Cleanup(func() { slog.SetDefault(prev) })

	t.Setenv("GOVOICE_FAKE_LLM_LABEL", "1")
	t.Setenv("GOVOICE_MANAGEMENT_API_KEY", e2eMgmtKey)
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Token.Secret = "voice-test-secret-32-bytes-long-and-separate"
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.logger = captured
	s.env = mapEnv(env)
	s.SetVADExecutor(&mockVADExecutor{ready: true, available: 4, total: 4})
	enableTestManagement(t, s)
	r := &graphRigE2E{t: t, s: s, probe: newProbe()}
	r.ts = httptest.NewServer(s.wsServer)
	t.Cleanup(r.ts.Close)

	plain, err := s.agents.Create(t.Context(), agent.CreateRequest{Name: "Plain", Config: json.RawMessage(
		`{"stt":{"provider":"google","model":"chirp_2"},"llm":{"provider":"xai","model":"grok-2-1212"},"tts":{"provider":"google","voice":"en-US-Chirp3-HD-Aoede"}}`)})
	if err != nil || plain.Readiness.State != agent.ReadinessReady {
		t.Fatalf("plain agent: %+v err=%v", plain.Readiness, err)
	}
	graphID := r.createGraphAgent("salesvoice")
	w := apiRequest(s, http.MethodPost, "/api/v1/mcp-servers", e2eMgmtKey, `{"name":"crm","endpoint":"https://1.1.1.1/rpc","transport":"streamable-http","auth_env":"MCP_TOKEN_CRM","enabled":true}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("mcp create: %d %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), `"auth_env_set":true`) {
		t.Fatalf("mcp create view: %s", w.Body)
	}

	// A real call through the fake stack: resolve, build, speak, hand off, hang up.
	c := r.dial(graphID)
	eventually(t, "session", func() bool { return r.activeSessions() == 1 })
	c.say("hello")
	c.waitReplies("[FRONT", 1)
	c.say("please transfer to sales")
	c.waitReplies("[SALES", 1)
	_ = c.conn.Close(websocket.StatusNormalClosure, "bye")
	eventually(t, "session ended", func() bool { return r.activeSessions() == 0 })

	var out strings.Builder
	get := func(path string) {
		w := apiRequest(s, http.MethodGet, path, e2eMgmtKey, "")
		if w.Code >= 500 && path != "/ready" {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
		out.WriteString(path + "\n" + w.Body.String() + "\n")
	}
	for _, p := range []string{"/api/v1/providers", "/api/v1/agents", "/api/v1/agents/" + plain.ID, "/api/v1/agents/" + graphID,
		"/api/v1/mcp-servers", "/api/v1/sessions", "/api/v1/sessions/" + c.sessionID, "/api/v1/sessions/" + c.sessionID + "/traces",
		"/api/v1/server-config", "/api/v1/operations/status", "/debug/timeline?session=1", "/metrics", "/ready"} {
		get(p)
	}
	if !strings.Contains(out.String(), "handoff") {
		t.Fatal("the captured surface is missing the call (timeline/traces empty?)")
	}
	if logs.String() == "" {
		t.Fatal("no server log captured")
	}
	out.WriteString(logs.String())
	for _, sentinel := range sentinels {
		if i := strings.Index(out.String(), sentinel); i >= 0 {
			lo := max(0, i-200)
			t.Errorf("secret %q left the server: ...%s...", sentinel, out.String()[lo:min(len(out.String()), i+80)])
		}
	}
}
