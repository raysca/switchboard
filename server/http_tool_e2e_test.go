package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"govoice/server/agent"
)

// httpToolFixture is the service an HTTP tool calls: it records what it was sent.
type httpToolFixture struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen []seenRequest
}

type seenRequest struct{ Method, Path, Query, Auth, Body string }

func newHTTPToolFixture(t *testing.T) *httpToolFixture {
	f := &httpToolFixture{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.seen = append(f.seen, seenRequest{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, r.Header.Get("Authorization"), string(body)})
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"A-1","status":"shipped"}`)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *httpToolFixture) last() seenRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.seen) == 0 {
		return seenRequest{}
	}
	return f.seen[len(f.seen)-1]
}

func (r *mcpCallRig) apiJSON(method, path, body string, want int) json.RawMessage {
	r.t.Helper()
	w := apiRequest(r.s, method, path, e2eMgmtKey, body)
	if w.Code != want {
		r.t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body)
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	return env.Data
}

const orderToolBody = `{"name":"get_order","description":"Look up an order by id.","method":"GET","url":"%s/orders/{order_id}","auth":{"secret":"%s"},"input_schema":{"type":"object","properties":{"order_id":{"type":"string"}},"required":["order_id"]}}`

func (r *mcpCallRig) createHTTPTool(base, secret string) (id, hash string) {
	r.t.Helper()
	r.apiJSON(http.MethodPost, "/api/v1/http-tools", `{}`, http.StatusBadRequest) // an empty definition is refused
	body := strings.Replace(strings.Replace(orderToolBody, "%s", base, 1), "%s", secret, 1)
	var tool struct{ ID, Hash string }
	if err := json.Unmarshal(r.apiJSON(http.MethodPost, "/api/v1/http-tools", body, http.StatusCreated), &tool); err != nil || tool.ID == "" {
		r.t.Fatalf("create tool: %v", err)
	}
	return tool.ID, tool.Hash
}

func (r *mcpCallRig) agentWithHTTPTool(toolID, hash string, required bool) agent.Agent {
	r.t.Helper()
	cfg := `{"stt":{"provider":"echo","model":"s"},"llm":{"provider":"echo","model":"m","system_prompt":"P"},"tts":{"provider":"echo","voice":"v"}}`
	a, err := r.s.agents.Create(r.t.Context(), agent.CreateRequest{Name: "HTTP " + toolID[:8], Config: json.RawMessage(cfg), MCPTools: []agent.ToolSelection{{ServerID: "http:" + toolID, RemoteName: "get_order", SchemaHash: hash, Required: required}}})
	if err != nil {
		r.t.Fatalf("agent: %v", err)
	}
	return a
}

// An agent calls an HTTP tool in a real call: the service sees the filled-in request with the secret, the model
// reads the result, nothing about the request or the secret reaches a management response, and editing the tool
// sends the agent back to review.
func TestHTTPToolCallEndToEnd(t *testing.T) {
	r := newMCPCallRig(t)
	f := newHTTPToolFixture(t)
	r.apiJSON(http.MethodPut, "/api/v1/secrets/crm-api", `{"value":"tok-http"}`, 200)

	id, hash := r.createHTTPTool(f.srv.URL, "crm-api")
	a := r.agentWithHTTPTool(id, hash, true)
	if a.Readiness.State != agent.ReadinessReady {
		t.Fatalf("readiness %+v", a.Readiness)
	}

	c := r.dial(a.ID)
	c.say(`call tool get_order {"order_id":"A-1"}`)
	c.waitReplies("shipped", 1)
	if got := f.last(); got.Method != "GET" || got.Path != "/orders/A-1" || got.Auth != "Bearer tok-http" {
		t.Fatalf("service saw %+v", got)
	}
	eventually(t, "http_tool.execute span", func() bool { return r.spanNames(c.sessionID)["http_tool.execute"] })

	// the tool view says whether its secret resolves and never shows a value; the tool list does not leak it either
	for _, path := range []string{"/api/v1/http-tools", "/api/v1/http-tools/" + id, "/api/v1/secrets"} {
		w := apiRequest(r.s, http.MethodGet, path, e2eMgmtKey, "")
		if w.Code != 200 || strings.Contains(w.Body.String(), "tok-http") {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
	}
	if view := r.apiJSON(http.MethodGet, "/api/v1/http-tools/"+id, "", 200); !strings.Contains(string(view), `"auth_secret_set":true`) {
		t.Fatalf("view: %s", view)
	}

	// a pinned tool cannot be deleted, and its secret cannot be deleted either
	r.apiJSON(http.MethodDelete, "/api/v1/http-tools/"+id, "", http.StatusConflict)
	r.apiJSON(http.MethodDelete, "/api/v1/secrets/crm-api", "", http.StatusConflict)

	// test calls use the same path and report what a model would see
	res := r.apiJSON(http.MethodPost, "/api/v1/http-tools/"+id+"/test", `{"arguments":{"order_id":"T-9"}}`, 200)
	if !strings.Contains(string(res), `"status":"completed"`) || f.last().Path != "/orders/T-9" {
		t.Fatalf("test call: %s %+v", res, f.last())
	}
	bad := r.apiJSON(http.MethodPost, "/api/v1/http-tools/"+id+"/test", `{"arguments":{}}`, 200)
	if !strings.Contains(string(bad), `"error_category":"invalid_arguments"`) {
		t.Fatalf("invalid arguments: %s", bad)
	}

	// editing the tool leaves the agent pinned to the reviewed definition, so it needs a review
	edited := strings.Replace(strings.Replace(orderToolBody, "%s", f.srv.URL, 1), "%s", "crm-api", 1)
	edited = strings.Replace(edited, "Look up an order by id.", "Look up an order by its id (changed).", 1)
	r.apiJSON(http.MethodPut, "/api/v1/http-tools/"+id, edited, 200)
	got, err := r.s.agents.Get(t.Context(), a.ID)
	if err != nil || got.Readiness.State != agent.ReadinessNeedsToolReview {
		t.Fatalf("after an edit: %+v %v", got.Readiness, err)
	}
	if len(got.Readiness.ToolDependencies) != 1 || got.Readiness.ToolDependencies[0].State != "tool_edited" {
		t.Fatalf("dependencies: %+v", got.Readiness.ToolDependencies)
	}
}

func TestHTTPToolWithoutItsSecretMakesTheAgentUnavailable(t *testing.T) {
	r := newMCPCallRig(t)
	f := newHTTPToolFixture(t)
	id, hash := r.createHTTPTool(f.srv.URL, "no-such-secret")
	a := r.agentWithHTTPTool(id, hash, true)
	if a.Readiness.State != agent.ReadinessToolUnavailable || len(a.Readiness.ToolDependencies) != 1 || a.Readiness.ToolDependencies[0].State != "secret_missing" {
		t.Fatalf("readiness %+v", a.Readiness)
	}
	// an optional tool only warns
	optional := r.agentWithHTTPTool(id, hash, false)
	if optional.Readiness.State != agent.ReadinessWithToolWarning {
		t.Fatalf("optional readiness %+v", optional.Readiness)
	}
}

func TestHTTPToolDefinitionIsValidatedAtTheEdge(t *testing.T) {
	r := newMCPCallRig(t)
	for name, body := range map[string]string{
		"http to a public host":  `{"name":"x","description":"d","method":"GET","url":"http://api.example.com/x","input_schema":{"type":"object"}}`,
		"private host":           `{"name":"x","description":"d","method":"GET","url":"https://10.0.0.5/x","input_schema":{"type":"object"}}`,
		"query in url":           `{"name":"x","description":"d","method":"GET","url":"https://api.example.com/x?a=1","input_schema":{"type":"object"}}`,
		"credential header":      `{"name":"x","description":"d","method":"GET","url":"https://api.example.com/x","headers":{"Authorization":"Bearer x"},"input_schema":{"type":"object"}}`,
		"provider key as secret": `{"name":"x","description":"d","method":"GET","url":"https://api.example.com/x","auth":{"secret":"XAI_API_KEY"},"input_schema":{"type":"object"}}`,
	} {
		w := apiRequest(r.s, http.MethodPost, "/api/v1/http-tools", e2eMgmtKey, body)
		if w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_http_tool") {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	// names are unique
	f := newHTTPToolFixture(t)
	r.createHTTPTool(f.srv.URL, "crm-api")
	body := strings.Replace(strings.Replace(orderToolBody, "%s", f.srv.URL, 1), "%s", "crm-api", 1)
	if w := apiRequest(r.s, http.MethodPost, "/api/v1/http-tools", e2eMgmtKey, body); w.Code != 409 {
		t.Errorf("duplicate name: %d %s", w.Code, w.Body)
	}
}
