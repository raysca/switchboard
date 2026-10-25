package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"govoice/server/agent"
	"govoice/server/api"
	"govoice/server/eval"
	"govoice/server/storage"
	"govoice/transport/wsclient"
)

func evalToken(t *testing.T, s *Server, principal string) string {
	t.Helper()
	a, err := s.agents.Create(t.Context(), agent.CreateRequest{Name: principal, Config: agentConfigJSON("SALES", "v")})
	if err != nil {
		t.Fatal(err)
	}
	resp, _, err := s.issueVoiceToken(t.Context(), api.VoiceTokenRequest{Principal: principal, AgentID: a.ID}, false)
	if err != nil {
		t.Fatal(err)
	}
	return resp.Token
}

func TestVoiceTokenEndpointReservesEvalPrincipal(t *testing.T) {
	s := evalTestServer(t)
	a, err := s.agents.Create(t.Context(), agent.CreateRequest{Name: "A", Config: agentConfigJSON("SALES", "v")})
	if err != nil {
		t.Fatal(err)
	}
	w := apiRequest(s, http.MethodPost, "/api/v1/voice-tokens", "management-test-key-32-bytes-long-and-separate", `{"principal":"`+eval.Principal+`","agent_id":"`+a.ID+`"}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid_request") {
		t.Fatalf("agent-eval principal: %d %s, want 400 invalid_request", w.Code, w.Body)
	}
	if _, _, err := s.issueVoiceToken(t.Context(), api.VoiceTokenRequest{Principal: eval.Principal, AgentID: a.ID}, false); err != nil {
		t.Fatalf("the internal issue path must stay open: %v", err)
	}
}

func TestInternalListenerRefusesOriginAndKeepsToken(t *testing.T) {
	s := evalTestServer(t)
	addr := localTarget{s}.PublicAddr()
	tok := evalToken(t, s, eval.Principal)
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/ws?token="+tok, nil)
	req.Header.Set("Origin", "http://evil.example")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	resp, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("Origin request answered %d, want 403", resp.StatusCode)
	}
	cl, err := wsclient.Dial(t.Context(), addr, tok, false)
	if err != nil {
		t.Fatalf("the token was consumed by the refused request: %v", err)
	}
	cl.Close()
}

func TestInternalListenerDoesNotConsumeOtherTokens(t *testing.T) {
	s := evalTestServer(t)
	tok := evalToken(t, s, "web-widget")
	if cl, err := wsclient.Dial(t.Context(), localTarget{s}.PublicAddr(), tok, false); err == nil {
		cl.Close()
		t.Fatal("the internal listener accepted a web-widget token")
	}
	cl, err := wsclient.Dial(t.Context(), s.publicAddr, tok, false)
	if err != nil {
		t.Fatalf("the refused token was consumed: %v", err)
	}
	cl.Close()
}

func TestJudgeGateFreeModes(t *testing.T) {
	url, _, _ := fakeJudge(t, 200, judgeVerdict)
	s := judgeServer(t, url, false)
	for _, p := range []string{"echo"} { // offline would dial its endpoint; its gate is the predicate below
		judges, closeJudges := s.judgeFor(t.Context(), storage.EvalSuite{JudgeProvider: p}, nil)
		r := scoreOf(t, eval.RunScorers(t.Context(), judgedInput(), judges, nil), "correctness")
		closeJudges()
		if strings.Contains(r.Reason, errJudgeNeedsLive.Error()) {
			t.Errorf("%s judge without live: %q, want no live gate", p, r.Reason)
		}
	}
	if !s.judgeLiveRequired("openai") || s.judgeLiveRequired("echo") || s.judgeLiveRequired("offline") {
		t.Fatal("judgeLiveRequired: paid must need live, echo and a loopback offline must not")
	}
	// A remote offline endpoint is somebody's paid model.
	s.cfg.Providers.Offline.LLMEndpoint = "https://llm.example.com/v1/responses"
	if !s.judgeLiveRequired("offline") || !s.evalLiveRequired(agentConfigJSONWith(t, "offline")) {
		t.Fatal("a non-loopback offline endpoint must count as paid")
	}
	s.cfg.Providers.Offline.LLMEndpoint = "http://[::1]:11434/v1/responses"
	if s.judgeLiveRequired("offline") || s.evalLiveRequired(agentConfigJSONWith(t, "offline")) {
		t.Fatal("a loopback offline endpoint must be free")
	}
}

func TestLocalTargetDraftTokenSkipsVersionReadiness(t *testing.T) {
	s := evalTestServer(t)
	a := paidAgent(t, s)
	s.env = mapEnv(nil)
	tok, err := localTarget{s}.DraftToken(t.Context(), eval.Agent{ID: a.ID, Draft: agentConfigJSON("SALES", "v")})
	if err != nil || tok == "" {
		t.Fatalf("DraftToken on an agent without keys: %v", err)
	}
}

// slowCloseListener delays the serve loop's exit after Close, so a Shutdown that does not join it returns first.
type slowCloseListener struct{ net.Listener }

func (l slowCloseListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		time.Sleep(300 * time.Millisecond)
	}
	return c, err
}

func TestShutdownJoinsEvalListener(t *testing.T) {
	s := evalTestServerPre(t, func(s *Server) {
		s.evalListen = func() (net.Listener, error) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			return slowCloseListener{ln}, err
		}
	})
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.evalDone:
	default:
		t.Fatal("Shutdown returned before the eval listener's serve loop ended")
	}
}

func TestStartEvalWorkerBindFailureReturnsAndResets(t *testing.T) {
	s := evalTestServer(t)
	m := s.mgmt.Load()
	s.stopEvalWorker(t.Context())
	m.eval.Store(nil)
	s.evalListen = func() (net.Listener, error) { return nil, errors.New("no port") }
	if err := s.startEvalWorker(); err == nil || !strings.Contains(err.Error(), "no port") {
		t.Fatalf("startEvalWorker = %v, want the bind error", err)
	}
	if m.eval.Load() != nil {
		t.Fatal("a failed bind left the worker registered")
	}
}
