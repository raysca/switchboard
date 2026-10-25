package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"govoice/server/agent"
	"govoice/server/api"
	"govoice/server/eval"
	"govoice/transport/wsclient"
)

// evalTestServer is a started, managed development server on ephemeral ports with the echo provider.
func evalTestServer(t *testing.T, mods ...func(*Config)) *Server {
	t.Helper()
	t.Setenv("GOVOICE_MANAGEMENT_API_KEY", e2eMgmtKey)
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Providers.Echo.Mimic, cfg.Providers.Echo.LabelReplies = "instant", true
	cfg.Token.Secret = "voice-test-secret-32-bytes-long-and-separate"
	cfg.PublicListenAddr, cfg.AdminListenAddr = "127.0.0.1:0", "127.0.0.1:0"
	for _, m := range mods {
		m(cfg)
	}
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.env = testProviderEnv(t)
	s.SetVADExecutor(&mockVADExecutor{ready: true, available: 4, total: 4})
	enableTestManagement(t, s)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	})
	return s
}

func TestLocalTargetDialsOwnListener(t *testing.T) {
	s := evalTestServer(t)
	a, err := s.agents.Create(t.Context(), agent.CreateRequest{Name: "Dealership", Config: agentConfigJSON("SALES", "salesvoice")})
	if err != nil || a.Readiness.State != agent.ReadinessReady {
		t.Fatalf("agent: %+v err=%v", a.Readiness, err)
	}
	ea := eval.Agent{ID: a.ID, Name: a.Name, Start: "front", Draft: agentConfigJSON("SALES", "salesvoice"),
		StepNames: map[string]string{"front": "Front desk", "sales": "Sales"}}
	obs, err := eval.RunCall(t.Context(), localTarget{s}, ea, eval.ScriptCaller([]string{"hello"}), eval.Opts{TurnTimeout: 10 * time.Second, Quiet: 300 * time.Millisecond})
	if err != nil {
		t.Fatalf("RunCall: %v", err)
	}
	if got := obs.Stored.Session; !got.Draft || got.Caller != eval.Principal || got.ID != obs.SessionID {
		t.Fatalf("stored session = %+v, want a draft call from %s", got, eval.Principal)
	}
	if len(obs.Stored.Turns) < 1 {
		t.Fatalf("stored turns = %d, want at least 1", len(obs.Stored.Turns))
	}
}

// prodEvalMods is the shipped production shape: a trusted proxy in front, a non-wildcard origin allowlist.
func prodEvalMods(c *Config) {
	c.Profile, c.VAD.Mode = ProfileProduction, "process" // the VAD executor is the test's mock
	c.TLS.TrustedProxyMode, c.TLS.TrustedProxies = true, []string{"10.0.0.0/8"}
	c.OriginAllowlist = []string{"https://app.example.com"}
}

func dialAs(t *testing.T, s *Server, addr, principal string) error {
	t.Helper()
	a, err := s.agents.Create(t.Context(), agent.CreateRequest{Name: principal, Config: agentConfigJSON("SALES", "v")})
	if err != nil {
		t.Fatal(err)
	}
	resp, _, err := s.issueVoiceToken(t.Context(), api.VoiceTokenRequest{Principal: principal, AgentID: a.ID}, false)
	if err != nil {
		t.Fatal(err)
	}
	cl, err := wsclient.Dial(t.Context(), addr, resp.Token, false)
	if err == nil {
		cl.Close()
	}
	return err
}

// The public listener's browser rules (Origin, TLS) must not refuse the server's own eval calls: they dial the
// internal loopback listener instead.
func TestEvalCallsWorkOnProductionProfile(t *testing.T) {
	for name, mod := range map[string]func(*Config){
		"trusted proxy": prodEvalMods,
		"allowlist only": func(c *Config) {
			prodEvalMods(c)
			c.TLS.TrustedProxyMode, c.TLS.TrustedProxies = false, nil
			c.InsecureOverrides.AllowPlainWSInProduction = true // a missing Origin is then the only refusal (403)
		},
		"direct tls": func(c *Config) {
			prodEvalMods(c)
			c.TLS.TrustedProxyMode, c.TLS.TrustedProxies = false, nil
			c.TLS.CertFile, c.TLS.KeyFile = writeSelfSigned(t, t.TempDir())
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := evalTestServer(t, mod)
			a, err := s.agents.Create(t.Context(), agent.CreateRequest{Name: "Dealership", Config: agentConfigJSON("SALES", "salesvoice")})
			if err != nil || a.Readiness.State != agent.ReadinessReady {
				t.Fatalf("agent: %+v err=%v", a.Readiness, err)
			}
			ea := eval.Agent{ID: a.ID, Name: a.Name, Start: "front", Draft: agentConfigJSON("SALES", "salesvoice"),
				StepNames: map[string]string{"front": "Front desk", "sales": "Sales"}}
			obs, err := eval.RunCall(t.Context(), localTarget{s}, ea, eval.ScriptCaller([]string{"hello"}), eval.Opts{TurnTimeout: 10 * time.Second, Quiet: 300 * time.Millisecond})
			if err != nil {
				t.Fatalf("RunCall: %v", err)
			}
			if len(obs.Stored.Turns) < 1 || obs.Stored.Session.Caller != eval.Principal {
				t.Fatalf("stored = %+v", obs.Stored)
			}
			// The public gate is unchanged: no Origin, no TLS evidence, no entry.
			if err := dialAs(t, s, s.publicAddr, "someone"); err == nil {
				t.Fatal("the public listener accepted a call with no Origin on the production profile")
			}
		})
	}
}

func TestInternalListenerOnlyServesAgentEval(t *testing.T) {
	s := evalTestServer(t, prodEvalMods)
	if err := dialAs(t, s, localTarget{s}.PublicAddr(), "someone-else"); err == nil {
		t.Fatal("the internal listener accepted a token for another principal")
	}
	if err := dialAs(t, s, localTarget{s}.PublicAddr(), eval.Principal); err != nil {
		t.Fatalf("agent-eval token refused: %v", err)
	}
}

// The internal listener is loopback-only, exists only with a worker, and goes away with Shutdown.
func TestInternalListenerLifecycle(t *testing.T) {
	off := evalTestServer(t, func(c *Config) { c.Evaluation.MaxConcurrentCalls = 0 })
	if got := (localTarget{off}).PublicAddr(); got != "" {
		t.Fatalf("worker off, but an internal listener is bound at %q", got)
	}
	s := evalTestServer(t)
	addr := localTarget{s}.PublicAddr()
	if host, _, err := net.SplitHostPort(addr); err != nil || host != "127.0.0.1" {
		t.Fatalf("internal addr %q, want 127.0.0.1:port", addr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// A draining server refuses before it looks at the token.
	s.shuttingDown.Store(true)
	resp, err := http.Get("http://" + addr + "/ws?token=x")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("draining internal listener answered %d, want 503", resp.StatusCode)
	}
	if err := s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.evalDone: // joined: the serve loop is gone when Shutdown returns
	default:
		t.Fatal("the internal listener's serve loop outlived Shutdown")
	}
	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		c.Close()
		t.Fatal("the internal listener still accepts after Shutdown")
	}
}

func TestIssueVoiceToken(t *testing.T) {
	s := evalTestServer(t)
	if _, _, err := s.issueVoiceToken(t.Context(), api.VoiceTokenRequest{Principal: "p", AgentID: "missing"}, false); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown agent: %v, want sql.ErrNoRows", err)
	}
	a, err := s.agents.Create(t.Context(), agent.CreateRequest{Name: "A", Config: agentConfigJSON("SALES", "v")})
	if err != nil {
		t.Fatal(err)
	}
	resp, id, err := s.issueVoiceToken(t.Context(), api.VoiceTokenRequest{Principal: "p", AgentID: a.ID}, false)
	if err != nil || resp.Token == "" || id == "" {
		t.Fatalf("issue: %+v %q %v", resp, id, err)
	}
	if got, err := evalTokenID(resp.Token); err != nil || got != id {
		t.Fatalf("token id %q err %v, want %q", got, err, id)
	}
	// A draft that names an unknown config surfaces the draft error the handler maps, not a token.
	_, _, err = s.issueVoiceToken(t.Context(), api.VoiceTokenRequest{Principal: "p", AgentID: a.ID, Draft: &api.DraftRequest{Config: json.RawMessage(`{"start":"x"}`)}}, false)
	if !errors.Is(err, agent.ErrInvalidConfig) {
		t.Fatalf("invalid draft: %v, want agent.ErrInvalidConfig", err)
	}
}

// An eval draft is rewritten (echo/offline), so the readiness of the agent's real providers must not block it.
func TestIssueVoiceTokenDraftSkipsVersionReadiness(t *testing.T) {
	s := evalTestServer(t)
	a := paidAgent(t, s)
	s.env = mapEnv(nil) // the paid providers lose their keys: the current version is not ready
	if got, err := s.agents.Get(t.Context(), a.ID); err != nil || agent.Callable(got.Readiness.State) {
		t.Fatalf("setup: readiness %q err %v, want not callable", got.Readiness.State, err)
	}
	in := api.VoiceTokenRequest{Principal: eval.Principal, AgentID: a.ID, Draft: &api.DraftRequest{Config: agentConfigJSON("SALES", "v")}}
	var nr *notReadyError
	if _, _, err := s.issueVoiceToken(t.Context(), in, false); !errors.As(err, &nr) || nr.Draft {
		t.Fatalf("API path: %v, want the agent not-ready refusal", err)
	}
	if resp, _, err := s.issueVoiceToken(t.Context(), in, true); err != nil || resp.Token == "" {
		t.Fatalf("in-process draft token: %v", err)
	}
	// Without a draft the version still has to be ready.
	in.Draft = nil
	if _, _, err := s.issueVoiceToken(t.Context(), in, true); !errors.As(err, &nr) {
		t.Fatalf("plain token: %v, want not-ready", err)
	}
}

// evalTokenID reads the jti of a token the server just issued (the signature is not checked).
func evalTokenID(tok string) (string, error) {
	var c jwt.RegisteredClaims
	if _, _, err := jwt.NewParser().ParseUnverified(tok, &c); err != nil {
		return "", err
	}
	return c.ID, nil
}

// fakeJudge answers every chat completion with body (status code) and counts the requests and the keys it saw.
func fakeJudge(t *testing.T, status int, body string) (url string, keys *atomic.Value, calls *atomic.Int32) {
	t.Helper()
	keys, calls = new(atomic.Value), new(atomic.Int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		keys.Store(r.Header.Get("Authorization"))
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/v1", keys, calls
}

func completerServer(t *testing.T, baseURL, key string) *Server {
	t.Helper()
	s := apiTestServer(t)
	s.cfg.Providers.OpenAILLM.AllowPrivateBaseURL = true
	vars := testProviderVars(t)
	vars["OPENAI_API_KEY"], vars["OPENAI_BASE_URL"] = key, baseURL
	s.env = mapEnv(vars)
	return s
}

func TestCompleterFakeJudge(t *testing.T) {
	url, keys, calls := fakeJudge(t, 200, `{"choices":[{"message":{"role":"assistant","content":"{\"score\":1,\"reason\":\"ok\"}"}}]}`)
	s := completerServer(t, url, "judge-key-1")
	ask, _, closeFn, err := s.completer(t.Context(), "openai", "")
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	out, err := ask(t.Context(), "rubric", "evidence")
	if err != nil || out != `{"score":1,"reason":"ok"}` {
		t.Fatalf("ask = %q, %v", out, err)
	}
	if calls.Load() != 1 || keys.Load() != "Bearer judge-key-1" {
		t.Fatalf("calls=%d auth=%v", calls.Load(), keys.Load())
	}
}

func TestCompleterRedactsTheKeyAndNamesNoValue(t *testing.T) {
	url, _, _ := fakeJudge(t, 500, "upstream rejected key judge-key-2-secret")
	s := completerServer(t, url, "judge-key-2-secret")
	ask, _, closeFn, err := s.completer(t.Context(), "openai", "gpt-4o-mini")
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	_, err = ask(t.Context(), "r", "e")
	if err == nil || strings.Contains(err.Error(), "judge-key-2-secret") {
		t.Fatalf("error must exist and not carry the key: %v", err)
	}

	// A missing key names the variable, never a value; an unknown provider and a provider without an llm stage are refused.
	s2 := completerServer(t, url, "")
	if _, _, _, err := s2.completer(t.Context(), "openai", ""); err == nil || !strings.Contains(err.Error(), "OPENAI_API_KEY") {
		t.Fatalf("missing key: %v, want it to name OPENAI_API_KEY", err)
	}
	for _, p := range []string{"nope", "deepgram"} {
		if _, _, _, err := s.completer(t.Context(), p, ""); err == nil {
			t.Fatalf("provider %q was accepted as a judge", p)
		}
	}
}

func TestCompleterOfflineNeedsItsEndpoint(t *testing.T) {
	s := apiTestServer(t)
	s.cfg.Providers.Offline.LLMEndpoint = ""
	if _, _, _, err := s.completer(t.Context(), "offline", ""); err == nil {
		t.Fatal("offline with no endpoint must be refused, not fall back to a cloud provider")
	}
}
