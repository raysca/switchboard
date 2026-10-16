package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/resilience"
	coretools "govoice/core/tools"
)

type fakeSession struct {
	gen atomic.Uint64
	mu  sync.Mutex
	got []core.Frame
	wg  sync.WaitGroup
}

func newSession() *fakeSession { s := &fakeSession{}; s.gen.Store(1); return s }

func (s *fakeSession) ID() uint64                  { return 1 }
func (s *fakeSession) Context() context.Context    { return context.Background() }
func (s *fakeSession) CurrentGeneration() uint64   { return s.gen.Load() }
func (s *fakeSession) IncrementGeneration() uint64 { return s.gen.Add(1) }
func (s *fakeSession) Go(fn func(context.Context)) error {
	s.wg.Add(1)
	go func() { defer s.wg.Done(); fn(context.Background()) }()
	return nil
}
func (s *fakeSession) Reenter(f core.Frame, _ core.Direction, _ int, gen uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if gen == s.gen.Load() {
		s.got = append(s.got, f)
	}
	return nil
}

type sink struct{}

func (sink) Push(core.Frame) error                          { return nil }
func (sink) PushDirection(core.Frame, core.Direction) error { return nil }

// run drives one user turn against a local test server (hence AllowPrivate).
func run(t *testing.T, cfg Config) *fakeSession {
	t.Helper()
	cfg.AllowPrivate = true
	p, err := NewProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := newSession()
	_ = p.Attach(s, 0)
	if err := p.ProcessFrame(context.Background(), &core.UserMessageFrame{Turn: "t1", Gen: 1, Content: "hi"}, core.Downstream, sink{}); err != nil {
		t.Fatal(err)
	}
	s.wg.Wait()
	return s
}

func sse(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, e := range events {
		fmt.Fprintf(w, "data: %s\n\n", e)
	}
}

func TestStreamingTextUsageAndRequestShape(t *testing.T) {
	var path, auth string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		sse(w, `{"choices":[{"delta":{"role":"assistant","content":"Hello "}}]}`,
			`{"choices":[{"delta":{"content":"world"}}]}`,
			`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`, "[DONE]")
	}))
	defer srv.Close()
	s := run(t, Config{BaseURL: srv.URL + "/v1/", Model: "llama-3.3-70b", APIKey: "k-123", Provider: "groq", SystemPrompt: "sys"})

	if path != "/v1/chat/completions" || auth != "Bearer k-123" {
		t.Fatalf("path=%q auth=%q", path, auth)
	}
	if body["model"] != "llama-3.3-70b" || body["stream"] != true || body["stream_options"].(map[string]any)["include_usage"] != true {
		t.Fatalf("body %v", body)
	}
	for _, leak := range []string{"store", "input", "reasoning", "search_parameters", "tools"} {
		if _, ok := body[leak]; ok {
			t.Fatalf("vendor-specific %q leaked: %v", leak, body)
		}
	}
	msgs := body["messages"].([]any)
	if len(msgs) != 2 || msgs[0].(map[string]any)["role"] != "system" {
		t.Fatalf("messages %v", msgs)
	}
	var deltas []string
	var final *core.LLMTextFrame
	var u *core.UsageMetricsFrame
	for _, f := range s.got {
		switch f := f.(type) {
		case *core.LLMTextDeltaFrame:
			deltas = append(deltas, f.Delta)
		case *core.LLMTextFrame:
			final = f
		case *core.UsageMetricsFrame:
			u = f
		}
	}
	if strings.Join(deltas, "") != "Hello world" || final == nil || final.Text != "Hello world" {
		t.Fatalf("deltas=%v final=%+v", deltas, final)
	}
	if u == nil || u.Provider != "groq" || *u.PromptTokens != 10 || *u.CompletionTokens != 2 || *u.TotalTokens != 12 {
		t.Fatalf("usage %+v", u)
	}
}

func TestParallelToolCallsRoundTrip(t *testing.T) {
	var reqs []map[string]any
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		mu.Lock()
		reqs = append(reqs, m)
		n := len(reqs)
		mu.Unlock()
		if n == 1 {
			// two calls interleaved by index, arguments split across chunks
			sse(w, `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"weather","arguments":"{\"city\":"}}]}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_b","function":{"name":"weather","arguments":"{\"city\":\"Paris\"}"}}]}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"London\"}"}}]}}]}`,
				`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`, "[DONE]")
			return
		}
		sse(w, `{"choices":[{"delta":{"content":"done"}}]}`,
			`{"choices":[],"usage":{"prompt_tokens":20,"completion_tokens":1,"total_tokens":21}}`, "[DONE]")
	}))
	defer srv.Close()

	defs := []core.ToolDefinition{{Name: "weather", Description: "d", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	var executed atomic.Int32
	loop, err := coretools.NewToolLoop(defs, core.ToolExecutorFunc(func(_ context.Context, c core.ToolCall) (core.ToolResult, error) {
		executed.Add(1)
		return core.ToolResult{CallID: c.ID, Status: core.ToolSucceeded, Result: json.RawMessage(fmt.Sprintf(`{"echo":%s}`, c.Arguments))}, nil
	}), core.NewConversationContext(core.ContextConfig{}), coretools.DefaultToolLoopConfig())
	if err != nil {
		t.Fatal(err)
	}
	s := run(t, Config{BaseURL: srv.URL, ToolDefinitions: defs, ToolLoop: loop, HTTPClient: srv.Client()})

	if executed.Load() != 2 || len(reqs) != 2 {
		t.Fatalf("executed=%d requests=%d", executed.Load(), len(reqs))
	}
	if tools := reqs[0]["tools"].([]any); tools[0].(map[string]any)["type"] != "function" || tools[0].(map[string]any)["function"].(map[string]any)["name"] != "weather" {
		t.Fatalf("tools %v", tools)
	}
	msgs := reqs[1]["messages"].([]any)
	asst := msgs[len(msgs)-3].(map[string]any)
	calls := asst["tool_calls"].([]any)
	if asst["role"] != "assistant" || len(calls) != 2 {
		t.Fatalf("assistant message %v", asst)
	}
	if a := calls[0].(map[string]any)["function"].(map[string]any)["arguments"]; a != `{"city":"London"}` {
		t.Fatalf("args %v", a)
	}
	r1, r2 := msgs[len(msgs)-2].(map[string]any), msgs[len(msgs)-1].(map[string]any)
	if r1["role"] != "tool" || r1["tool_call_id"] != "call_a" || r2["tool_call_id"] != "call_b" || !strings.Contains(r1["content"].(string), "London") {
		t.Fatalf("results %v %v", r1, r2)
	}
	var u *core.UsageMetricsFrame
	for _, f := range s.got {
		if f, ok := f.(*core.UsageMetricsFrame); ok {
			u = f
		}
	}
	if u == nil || *u.PromptTokens != 25 || *u.TotalTokens != 29 {
		t.Fatalf("usage not summed over rounds: %+v", u)
	}
}

func TestHistoryFoldsParallelCalls(t *testing.T) {
	got := historyMessages([]core.Message{
		{Role: core.RoleUser, Content: "q"},
		{Role: core.RoleAssistant, Kind: core.MessageToolCall, ToolCallID: "a", ToolName: "f", Content: `{}`},
		{Role: core.RoleAssistant, Kind: core.MessageToolCall, ToolCallID: "b", ToolName: "f", Content: `{}`},
		{Role: core.RoleTool, Kind: core.MessageToolResult, ToolCallID: "a", Content: `1`},
		{Role: core.RoleTool, Kind: core.MessageToolResult, ToolCallID: "b", Content: `2`},
	})
	if len(got) != 4 || len(got[1].ToolCalls) != 2 || got[2].Role != "tool" || got[3].ToolCallID != "b" {
		t.Fatalf("%+v", got)
	}
}

func TestClassificationAndRetry(t *testing.T) {
	var hits atomic.Int32
	status := http.StatusServiceUnavailable
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 && status >= 500 {
			w.WriteHeader(status)
			return
		}
		if status < 500 {
			w.WriteHeader(status)
			fmt.Fprint(w, `{"error":"bad key sk-abcdefghijklmnopqrstuv"}`)
			return
		}
		sse(w, `{"choices":[{"delta":{"content":"ok"}}]}`, "[DONE]")
	}))
	defer srv.Close()
	fast := resilience.DefaultPolicy()
	fast.MaxAttempts, fast.InitialBackoff = 3, time.Millisecond
	// 5xx is retried and succeeds on the second attempt.
	s := run(t, Config{BaseURL: srv.URL, RetryPolicy: fast})
	if hits.Load() != 2 || len(s.got) == 0 {
		t.Fatalf("hits=%d frames=%d", hits.Load(), len(s.got))
	}
	// 401 is not retried, is sanitized, and does not open the breaker.
	hits.Store(0)
	status = http.StatusUnauthorized
	br := resilience.NewBreaker(resilience.Config{FailureThreshold: 1, Cooldown: time.Hour})
	s = run(t, Config{BaseURL: srv.URL, RetryPolicy: fast, CircuitBreaker: br})
	if hits.Load() != 1 || !br.Allow() {
		t.Fatalf("hits=%d: 401 must not retry or open the breaker", hits.Load())
	}
	ef, ok := s.got[0].(*core.ErrorFrame)
	if !ok || strings.Contains(ef.Err.Error(), "sk-abcdef") {
		t.Fatalf("error frame %+v", s.got)
	}
}

func TestMissingDoneAndHangTimeout(t *testing.T) {
	// A stream that ends without [DONE] is an error (no truncated reply).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
	}))
	defer srv.Close()
	s := run(t, Config{BaseURL: srv.URL})
	if _, ok := s.got[len(s.got)-1].(*core.ErrorFrame); !ok {
		t.Fatalf("expected ErrorFrame, got %+v", s.got)
	}

	// A server that never answers is cut at the attempt timeout and trips the breaker.
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.Copy(io.Discard, r.Body); <-r.Context().Done() }))
	defer hang.Close()
	br := resilience.NewBreaker(resilience.Config{FailureThreshold: 1, Cooldown: time.Hour})
	pol := resilience.DefaultPolicy()
	pol.MaxAttempts = 1
	start := time.Now()
	s = run(t, Config{BaseURL: hang.URL, Timeout: 50 * time.Millisecond, RetryPolicy: pol, CircuitBreaker: br})
	if time.Since(start) > 3*time.Second || br.Allow() {
		t.Fatalf("elapsed=%v: hang must time out and open the breaker", time.Since(start))
	}
	if _, ok := s.got[len(s.got)-1].(*core.ErrorFrame); !ok {
		t.Fatalf("timed-out request ended silently: %+v", s.got)
	}
}

func TestBadConfig(t *testing.T) {
	for _, u := range []string{"ftp://x", "http://api.example.com/v1", "https://"} {
		if _, err := NewProcessor(Config{BaseURL: u}); err == nil {
			t.Fatalf("%q accepted without allow_private", u)
		}
	}
	defs := []core.ToolDefinition{{Name: "t", InputSchema: json.RawMessage(`{}`)}}
	if _, err := NewProcessor(Config{ToolDefinitions: defs}); err == nil {
		t.Fatal("tools without ToolLoop accepted")
	}
}

func TestAllowedIP(t *testing.T) {
	for _, c := range []struct {
		ip           string
		def, private bool
	}{
		{"8.8.8.8", true, true},
		{"2606:4700::1111", true, true},
		{"169.254.169.254", false, false},
		{"::ffff:169.254.169.254", false, false},
		{"fd00:ec2::254", false, false},
		{"fe80::1", false, false},
		{"0.0.0.0", false, false},
		{"127.0.0.1", false, true},
		{"::1", false, true},
		{"10.1.2.3", false, true},
		{"192.168.1.1", false, true},
		{"100.64.0.1", false, true},
		{"fd12::1", false, true},
	} {
		ip := netip.MustParseAddr(c.ip)
		if allowedIP(ip, false) != c.def || allowedIP(ip, true) != c.private {
			t.Errorf("%s: want default=%v private=%v", c.ip, c.def, c.private)
		}
	}
}

// The real (guarded) client refuses loopback at dial time without
// allow_private: no request reaches the server, no retry, breaker stays closed.
func TestDialGuardBlocksPrivateByDefault(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer srv.Close()
	br := resilience.NewBreaker(resilience.Config{FailureThreshold: 1, Cooldown: time.Hour})
	p, err := NewProcessor(Config{BaseURL: srv.URL, CircuitBreaker: br}) // https://127.0.0.1:port
	if err != nil {
		t.Fatal(err)
	}
	s := newSession()
	_ = p.Attach(s, 0)
	_ = p.ProcessFrame(context.Background(), &core.UserMessageFrame{Turn: "t1", Content: "hi"}, core.Downstream, sink{})
	s.wg.Wait()
	ef, ok := s.got[len(s.got)-1].(*core.ErrorFrame)
	if hits.Load() != 0 || !ok || !strings.Contains(ef.Err.Error(), "blocked") || !br.Allow() {
		t.Fatalf("hits=%d frames=%+v breakerOpen=%v", hits.Load(), s.got, !br.Allow())
	}
}

// Redirects are never followed: a 3xx to anywhere (here another server) is an error.
func TestRedirectNotFollowed(t *testing.T) {
	var followed atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed.Add(1) }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/latest/meta-data", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	s := run(t, Config{BaseURL: srv.URL})
	if _, ok := s.got[len(s.got)-1].(*core.ErrorFrame); !ok || followed.Load() != 0 {
		t.Fatalf("followed=%d frames=%+v", followed.Load(), s.got)
	}
}

// A server that rejects stream_options gets one retry without it.
func TestStreamOptionsFallback(t *testing.T) {
	var withOpts, without atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), "stream_options") {
			withOpts.Add(1)
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"message":"property 'stream_options' is unsupported"}}`)
			return
		}
		without.Add(1)
		sse(w, `{"choices":[{"delta":{"content":"ok"}}]}`, "[DONE]")
	}))
	defer srv.Close()
	s := run(t, Config{BaseURL: srv.URL})
	if f, ok := s.got[len(s.got)-1].(*core.LLMTextFrame); !ok || f.Text != "ok" || withOpts.Load() != 1 || without.Load() != 1 {
		t.Fatalf("with=%d without=%d frames=%+v", withOpts.Load(), without.Load(), s.got)
	}
}

func TestMidStreamErrorEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sse(w, `{"choices":[{"delta":{"content":"par"}}]}`, `{"error":{"message":"overloaded"}}`, "[DONE]")
	}))
	defer srv.Close()
	s := run(t, Config{BaseURL: srv.URL})
	if ef, ok := s.got[len(s.got)-1].(*core.ErrorFrame); !ok || !strings.Contains(ef.Err.Error(), "overloaded") {
		t.Fatalf("frames=%+v", s.got)
	}
}

// Barge-in cancels the in-flight request: the HTTP body is closed and nothing
// (final text, error) is emitted for the interrupted turn.
func TestInterruptionClosesStream(t *testing.T) {
	gone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sse(w, `{"choices":[{"delta":{"content":"Hel"}}]}`)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(gone)
	}))
	defer srv.Close()
	p, err := NewProcessor(Config{BaseURL: srv.URL, AllowPrivate: true})
	if err != nil {
		t.Fatal(err)
	}
	s := newSession()
	_ = p.Attach(s, 0)
	_ = p.ProcessFrame(context.Background(), &core.UserMessageFrame{Turn: "t1", Content: "hi"}, core.Downstream, sink{})
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(time.Millisecond) {
		s.mu.Lock()
		n := len(s.got)
		s.mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no delta")
		}
	}
	_ = p.ProcessFrame(context.Background(), &core.InterruptionFrame{}, core.Downstream, sink{})
	select {
	case <-gone:
	case <-time.After(2 * time.Second):
		t.Fatal("request not cancelled")
	}
	s.wg.Wait()
	if len(s.got) != 1 {
		t.Fatalf("frames after interruption: %+v", s.got)
	}
}

// A retired model (NIM answers 404/410): one request, a client error that
// names the model, and no key echoed back.
func TestRetiredModelIsClientError(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusGone} {
		var hits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			w.WriteHeader(code)
			fmt.Fprintf(w, `{"detail":"model gone for key %s"}`, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		}))
		br := resilience.NewBreaker(resilience.Config{FailureThreshold: 1, Cooldown: time.Minute})
		s := run(t, Config{BaseURL: srv.URL, Model: "old/model", APIKey: "nvapi-0123456789abcdefghij", CircuitBreaker: br})
		srv.Close()
		ef, ok := s.got[len(s.got)-1].(*core.ErrorFrame)
		if !ok {
			t.Fatalf("%d: frames=%+v", code, s.got)
		}
		msg := ef.Err.Error()
		if !strings.Contains(msg, `model "old/model" not found or deprecated`) || strings.Contains(msg, "0123456789") {
			t.Fatalf("%d: %s", code, msg)
		}
		if hits.Load() != 1 || br.WouldRefuse() || !classify(ef.Err).ClientError {
			t.Fatalf("%d: hits=%d breaker open=%v", code, hits.Load(), br.WouldRefuse())
		}
	}
}

func TestToolLoopFramesReachTheSession(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		if n.Add(1) == 1 {
			sse(w, `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"weather","arguments":"{}"}}]}}]}`, "[DONE]")
			return
		}
		sse(w, `{"choices":[{"delta":{"content":"done"}}]}`, "[DONE]")
	}))
	defer srv.Close()
	defs := []core.ToolDefinition{{Name: "weather", Description: "d", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	loop, err := coretools.NewToolLoop(defs, core.ToolExecutorFunc(func(_ context.Context, c core.ToolCall) (core.ToolResult, error) {
		return core.ToolResult{CallID: c.ID, Status: core.ToolSucceeded, Result: json.RawMessage(`{}`)}, nil
	}), core.NewConversationContext(core.ContextConfig{}), coretools.DefaultToolLoopConfig())
	if err != nil {
		t.Fatal(err)
	}
	s := run(t, Config{BaseURL: srv.URL, ToolDefinitions: defs, ToolLoop: loop, HTTPClient: srv.Client()})
	var got []string
	for _, f := range s.got {
		switch f := f.(type) {
		case *core.ToolCallFrame:
			got = append(got, "call:"+f.Call.ID)
		case *core.ToolResultFrame:
			got = append(got, "result:"+f.Result.CallID+":"+string(f.Result.Status))
		case *core.LLMTextFrame:
			got = append(got, "text:"+f.Text)
		}
	}
	if strings.Join(got, ",") != "call:call_a,result:call_a:succeeded,text:done" {
		t.Fatalf("frames %v", got)
	}
}
