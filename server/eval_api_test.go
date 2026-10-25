package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"govoice/server/agent"
	"govoice/server/api"
)

// evalAPIServer is a server with the management plane (no listeners, no worker) and one ready paid (google) agent.
// env adds variables to the server's own environment map, never the process's.
func evalAPIServer(t *testing.T, env map[string]string) (*Server, string) {
	t.Helper()
	s := apiTestServer(t)
	vars := testProviderVars(t)
	for k, v := range env {
		vars[k] = v
	}
	s.env = mapEnv(vars)
	return s, readyTestAgent(t, s)
}

func evalErrCode(t *testing.T, body []byte) string {
	t.Helper()
	var e api.ErrorResponse
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	return e.Error.Code
}

func TestManagementEvalPathWinsOverAgents(t *testing.T) {
	s := apiTestServer(t)
	s.setAgentManagementAPI(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }))
	// No eval handler yet: 503 with its own code, and the agents handler is not asked.
	w := apiRequest(s, http.MethodGet, "/api/v1/agents/x/eval/cases", testMgmtKey, "")
	if w.Code != http.StatusServiceUnavailable || evalErrCode(t, w.Body.Bytes()) != "eval_management_unavailable" || w.Header().Get("X-Request-ID") == "" {
		t.Fatalf("nil eval handler: %d %s", w.Code, w.Body)
	}
	var gotID string
	s.setEvalManagementAPI(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotID = api.RequestIDFromContext(r.Context())
		w.WriteHeader(http.StatusAccepted)
	}))
	for _, p := range []string{"/api/v1/agents/x/eval", "/api/v1/agents/x/eval/cases", "/api/v1/agents/x/eval/runs/y"} {
		gotID = ""
		w := apiRequest(s, http.MethodGet, p, testMgmtKey, "")
		if w.Code != http.StatusAccepted || gotID == "" || gotID != w.Header().Get("X-Request-ID") {
			t.Fatalf("%s: %d id %q header %q", p, w.Code, gotID, w.Header().Get("X-Request-ID"))
		}
	}
	// Look-alikes and the other agent paths still reach the agents handler.
	for _, p := range []string{"/api/v1/agents/x/evaluation", "/api/v1/agents/x", "/api/v1/agents/x/versions", "/api/v1/agents/eval", "/api/v1/agents//eval"} {
		if w := apiRequest(s, http.MethodGet, p, testMgmtKey, ""); w.Code != http.StatusTeapot {
			t.Fatalf("%s: %d, want the agents handler", p, w.Code)
		}
	}
}

func TestEvalAPIRequiresManagementKey(t *testing.T) {
	s, id := evalAPIServer(t, nil)
	base := "/api/v1/agents/" + id + "/eval"
	// The failed-attempt limiter allows a few 401s, so three probes: no key, a wrong key, and a write.
	for _, tc := range []struct{ method, path, key, body string }{
		{http.MethodGet, "/cases", "", ""},
		{http.MethodGet, "/runs", "wrong-key-wrong-key-wrong-key-wrong", ""},
		{http.MethodPost, "/runs", "", `{}`},
	} {
		w := apiRequest(s, tc.method, base+tc.path, tc.key, tc.body)
		if w.Code != http.StatusUnauthorized || evalErrCode(t, w.Body.Bytes()) != "unauthorized" {
			t.Fatalf("%s %s key %q: %d %s", tc.method, tc.path, tc.key, w.Code, w.Body)
		}
	}
	for _, p := range []string{"/cases", "/cases/export", "/suite", "/scorers", "/runs"} {
		if w := apiRequest(s, http.MethodGet, base+p, testMgmtKey, ""); w.Code != http.StatusOK {
			t.Fatalf("%s with the key: %d %s", p, w.Code, w.Body)
		}
	}
}

const evalCaseBody = `{"yaml":"name: hello\ncaller: {script: [\"hi\"]}\n"}`

// The paid gate follows the server's own environment map: a paid agent without GOVOICE_LIVE is refused, with it
// accepted; a free mode and a keyless agent never need it; a paid judge is refused without it.
func TestEvalAPILiveGateFollowsServerEnv(t *testing.T) {
	for _, live := range []bool{false, true} {
		env := map[string]string{}
		if live {
			env["GOVOICE_LIVE"] = "1"
		}
		s, id := evalAPIServer(t, env)
		base := "/api/v1/agents/" + id + "/eval"
		if w := apiRequest(s, http.MethodPost, base+"/cases", testMgmtKey, evalCaseBody); w.Code != http.StatusCreated {
			t.Fatalf("add case: %d %s", w.Code, w.Body)
		}
		keep := apiRequest(s, http.MethodPost, base+"/runs", testMgmtKey, `{"providers":"keep"}`)
		judge := apiRequest(s, http.MethodPut, base+"/suite", testMgmtKey, `{"runs":1,"min_pass":1,"pass_threshold":1,"judge":{"provider":"google"}}`)
		if live {
			if keep.Code != http.StatusAccepted || judge.Code != http.StatusOK {
				t.Fatalf("with GOVOICE_LIVE=1: keep %d %s, judge %d %s", keep.Code, keep.Body, judge.Code, judge.Body)
			}
		} else {
			if keep.Code != http.StatusForbidden || evalErrCode(t, keep.Body.Bytes()) != "eval_live_required" || !strings.Contains(keep.Body.String(), "GOVOICE_LIVE=1") {
				t.Fatalf("keep without GOVOICE_LIVE: %d %s", keep.Code, keep.Body)
			}
			if judge.Code != http.StatusForbidden || evalErrCode(t, judge.Body.Bytes()) != "eval_live_required" {
				t.Fatalf("paid judge without GOVOICE_LIVE: %d %s", judge.Code, judge.Body)
			}
		}
		echo, err := s.agents.Create(t.Context(), agent.CreateRequest{Name: "Echo", Config: agentConfigJSONWith(t, "echo")})
		if err != nil {
			t.Fatal(err)
		}
		echoBase := "/api/v1/agents/" + echo.ID + "/eval"
		if w := apiRequest(s, http.MethodPost, echoBase+"/cases", testMgmtKey, evalCaseBody); w.Code != http.StatusCreated {
			t.Fatalf("add case: %d %s", w.Code, w.Body)
		}
		if w := apiRequest(s, http.MethodPost, echoBase+"/runs", testMgmtKey, `{"providers":"keep"}`); w.Code != http.StatusAccepted {
			t.Fatalf("live=%v keyless agent in keep mode: %d %s", live, w.Code, w.Body)
		}
		for _, mode := range []string{`{}`, `{"providers":"offline"}`, `{"providers":"echo"}`} {
			if w := apiRequest(s, http.MethodPost, base+"/runs", testMgmtKey, mode); w.Code != http.StatusAccepted {
				t.Fatalf("live=%v %s: %d %s", live, mode, w.Code, w.Body)
			}
		}
		if w := apiRequest(s, http.MethodPut, base+"/suite", testMgmtKey, `{"runs":1,"min_pass":1,"pass_threshold":1,"judge":{"provider":"offline"}}`); w.Code != http.StatusOK {
			t.Fatalf("free judge: %d %s", w.Code, w.Body)
		}
		if w := apiRequest(s, http.MethodPut, base+"/suite", testMgmtKey, `{"runs":1,"min_pass":1,"pass_threshold":1,"judge":{"provider":"nowhere"}}`); w.Code != http.StatusBadRequest {
			t.Fatalf("unknown judge provider: %d %s", w.Code, w.Body)
		}
	}
}
