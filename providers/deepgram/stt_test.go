package deepgram

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"govoice/core"
	"govoice/core/resilience"
)

// fakeDeepgram is a local Deepgram: handler runs once per accepted socket.
type fakeDeepgram struct {
	*httptest.Server
	opens   atomic.Int32
	query   chan string
	auth    chan string
	dropped chan struct{} // closed-by-client signal for the last socket
}

func newFake(t *testing.T, handle func(ctx context.Context, c *websocket.Conn)) *fakeDeepgram {
	f := &fakeDeepgram{query: make(chan string, 8), auth: make(chan string, 8), dropped: make(chan struct{}, 8)}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.opens.Add(1)
		f.query <- r.URL.RawQuery
		f.auth <- r.Header.Get("Authorization")
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		handle(r.Context(), c)
		f.dropped <- struct{}{}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeDeepgram) cfg() STTConfig {
	cfg := DefaultSTTConfig()
	cfg.Endpoint = "ws" + strings.TrimPrefix(f.URL, "http")
	cfg.APIKey = "dg-test-key-0123456789"
	cfg.DrainTimeout = 300 * time.Millisecond
	cfg.RetryPolicy = resilience.Policy{MaxAttempts: 3, InitialBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond, AttemptTimeout: 2 * time.Second}
	return cfg
}

func results(text string, final, speechFinal, fromFinalize bool) []byte {
	b := `{"type":"Results","is_final":` + boolStr(final) + `,"speech_final":` + boolStr(speechFinal) +
		`,"from_finalize":` + boolStr(fromFinalize) + `,"channel":{"alternatives":[{"transcript":"` + text + `"}]}}`
	return []byte(b)
}
func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

type harness struct {
	proc *STTProcessor
	sess *core.Session
	sink *core.PassthroughProcessor
}

func start(t *testing.T, cfg STTConfig, tracer core.Tracer) *harness {
	t.Helper()
	proc, err := NewSTTProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sink := &core.PassthroughProcessor{}
	pipe, err := core.NewPipeline(proc, sink)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := core.NewSession(1, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	if tracer != nil {
		if err := sess.SetTracer(tracer, core.TracePolicy{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := sess.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	return &harness{proc, sess, sink}
}

func (h *harness) speak(t *testing.T) {
	t.Helper()
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	_ = h.sess.Post(&core.InputAudioFrame{Samples: make([]byte, 640), SampleRate: 16000, Channels: 1, Gen: 1})
}

func (h *harness) candidates() (out []*core.TranscriptCandidateFrame) {
	for _, f := range h.sink.GetFrames() {
		if c, ok := f.(*core.TranscriptCandidateFrame); ok {
			out = append(out, c)
		}
	}
	return
}

func (h *harness) errors() (out []error) {
	for _, f := range h.sink.GetFrames() {
		if e, ok := f.(*core.ErrorFrame); ok {
			out = append(out, e.Err)
		}
	}
	return
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	until := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(until) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// Interim then final; endpointing is off, Finalize on stop yields the
// utterance-final candidate with cumulative text.
func TestSTT_InterimFinalAndFinalizeOnStop(t *testing.T) {
	var gotCtl []string
	var mu sync.Mutex
	f := newFake(t, func(ctx context.Context, c *websocket.Conn) {
		for {
			typ, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			if typ == websocket.MessageBinary {
				_ = c.Write(ctx, websocket.MessageText, results("hello", false, false, false))
				_ = c.Write(ctx, websocket.MessageText, results("hello there", true, false, false))
				_ = c.Write(ctx, websocket.MessageText, results("how are you", false, false, false))
				continue
			}
			mu.Lock()
			gotCtl = append(gotCtl, string(data))
			mu.Unlock()
			if strings.Contains(string(data), "Finalize") {
				_ = c.Write(ctx, websocket.MessageText, results("how are you", true, false, true))
			}
			if strings.Contains(string(data), "CloseStream") {
				_ = c.Close(websocket.StatusNormalClosure, "")
				return
			}
		}
	})
	h := start(t, f.cfg(), nil)
	h.speak(t)
	waitFor(t, "interim+final", func() bool { return len(h.candidates()) >= 3 })
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{})
	waitFor(t, "utterance final", func() bool {
		c := h.candidates()
		return len(c) >= 4 && c[len(c)-1].EndOfUtterance
	})

	c := h.candidates()
	want := []struct {
		text  string
		final bool
	}{{"hello", false}, {"hello there", true}, {"hello there how are you", false}, {"hello there how are you", true}}
	for i, w := range want {
		if c[i].Text != w.text || c[i].Final != w.final || c[i].Provider != "deepgram" || c[i].Turn != "turn-1-1" {
			t.Errorf("candidate %d = %+v, want %+v", i, c[i], w)
		}
	}
	if c[1].EndOfUtterance || !c[3].EndOfUtterance {
		t.Errorf("only the finalize result ends the utterance: %+v %+v", c[1], c[3])
	}
	q := <-f.query
	for _, p := range []string{"endpointing=false", "interim_results=true", "model=nova-3", "encoding=linear16", "sample_rate=16000"} {
		if !strings.Contains(q, p) {
			t.Errorf("query %q missing %s", q, p)
		}
	}
	if a := <-f.auth; a != "Token dg-test-key-0123456789" {
		t.Errorf("auth header %q", a)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(gotCtl) != 2 || !strings.Contains(gotCtl[0], "Finalize") || !strings.Contains(gotCtl[1], "CloseStream") {
		t.Errorf("control messages %v", gotCtl)
	}
}

func TestSTT_MissingKey(t *testing.T) {
	if _, err := NewSTTProcessor(STTConfig{}); !errors.Is(err, ErrMissingAPIKey) {
		t.Fatalf("err=%v", err)
	}
}

// Client error on the upgrade (bad key): not retried, not an outage.
func TestSTT_BadKeyNotRetriedAndExemptFromBreaker(t *testing.T) {
	var opens atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		opens.Add(1)
		http.Error(w, "bad credentials", http.StatusUnauthorized)
	}))
	defer srv.Close()
	cfg := DefaultSTTConfig()
	cfg.Endpoint, cfg.APIKey = "ws"+strings.TrimPrefix(srv.URL, "http"), "dg-test-key-0123456789"
	cfg.CircuitBreaker = resilience.NewBreaker(resilience.Config{FailureThreshold: 1, Cooldown: time.Hour})
	h := start(t, cfg, nil)
	err := h.proc.ensureStream(context.Background())
	var pe *ProviderError
	if !errors.As(err, &pe) || pe.Code != "UNAUTHORIZED" || pe.Retryable || !pe.ClientError {
		t.Fatalf("err=%v", err)
	}
	if opens.Load() != 1 || !cfg.CircuitBreaker.Allow() {
		t.Fatalf("opens=%d, breaker must stay closed", opens.Load())
	}
}

// 5xx on open is retried; success on the second attempt carries on.
func TestSTT_OpenRetriesTransientFailure(t *testing.T) {
	var opens atomic.Int32
	inner := newFake(t, func(ctx context.Context, c *websocket.Conn) { _, _, _ = c.Read(ctx) })
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if opens.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		inner.Config.Handler.ServeHTTP(w, r)
	}))
	defer flaky.Close()
	cfg := inner.cfg()
	cfg.Endpoint = "ws" + strings.TrimPrefix(flaky.URL, "http")
	h := start(t, cfg, nil)
	if err := h.proc.ensureStream(context.Background()); err != nil {
		t.Fatal(err)
	}
	if opens.Load() != 2 {
		t.Fatalf("opens=%d", opens.Load())
	}
}

// Repeated outages open the shared breaker; further opens are refused fast.
func TestSTT_BreakerOpensOnOutage(t *testing.T) {
	var opens atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		opens.Add(1)
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer srv.Close()
	cfg := DefaultSTTConfig()
	cfg.Endpoint, cfg.APIKey = "ws"+strings.TrimPrefix(srv.URL, "http"), "dg-test-key-0123456789"
	cfg.RetryPolicy = resilience.Policy{MaxAttempts: 1}
	cfg.CircuitBreaker = resilience.NewBreaker(resilience.Config{FailureThreshold: 2, Cooldown: time.Hour})
	h := start(t, cfg, nil)
	for i := 0; i < 2; i++ {
		if err := h.proc.ensureStream(context.Background()); err == nil {
			t.Fatal("expected failure")
		}
	}
	if err := h.proc.ensureStream(context.Background()); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("err=%v, want circuit open", err)
	}
	if opens.Load() != 2 {
		t.Fatalf("opens=%d; open breaker must not dial", opens.Load())
	}
}

func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		status           int
		retryable, ready bool
	}{{401, false, true}, {400, false, true}, {429, true, false}, {503, true, false}} {
		pe := MapError("stt", errors.New("x"), &http.Response{StatusCode: tc.status, Header: http.Header{"Retry-After": {"3"}}})
		v := classify(pe)
		if v.Retryable != tc.retryable || v.ClientError != tc.ready {
			t.Errorf("%d: %+v", tc.status, v)
		}
		if tc.status == 429 && v.RetryAfter != 3*time.Second {
			t.Errorf("retry-after %v", v.RetryAfter)
		}
	}
	if v := classify(errors.New("connection reset")); !v.Retryable || v.ClientError {
		t.Errorf("raw network error: %+v", v)
	}
	if v := classify(context.Canceled); !v.ClientError {
		t.Errorf("cancel counted as outage")
	}
	if got := SanitizeString("Authorization: Token abcdefgh12345678"); strings.Contains(got, "abcdefgh") {
		t.Errorf("leaked: %s", got)
	}
}

// A mid-stream abnormal close surfaces an ErrorFrame and counts for the breaker.
func TestSTT_MidStreamDropReportsError(t *testing.T) {
	f := newFake(t, func(ctx context.Context, c *websocket.Conn) {
		_, _, _ = c.Read(ctx)
		_ = c.Close(websocket.StatusInternalError, "NET-0000")
	})
	cfg := f.cfg()
	cfg.CircuitBreaker = resilience.NewBreaker(resilience.Config{FailureThreshold: 1, Cooldown: time.Hour})
	h := start(t, cfg, nil)
	h.speak(t)
	waitFor(t, "error frame", func() bool { return len(h.errors()) > 0 })
	var pe *ProviderError
	if !errors.As(h.errors()[0], &pe) || !pe.Retryable {
		t.Fatalf("err=%v", h.errors()[0])
	}
	if cfg.CircuitBreaker.Allow() {
		t.Fatal("outage-class drop must open the breaker")
	}
}

// Close tears the socket down and the trace span ends exactly once.
func TestSTT_CloseTearsDownAndEndsTraceOnce(t *testing.T) {
	f := newFake(t, func(ctx context.Context, c *websocket.Conn) {
		for {
			if _, _, err := c.Read(ctx); err != nil {
				return
			}
		}
	})
	rec := &lifecycleRecorder{}
	h := start(t, f.cfg(), rec)
	h.speak(t)
	waitFor(t, "stream", func() bool { return f.opens.Load() == 1 && h.proc.activeCount() == 1 })
	_ = h.proc.Close()
	_ = h.proc.Close() // idempotent
	select {
	case <-f.dropped:
	case <-time.After(2 * time.Second):
		t.Fatal("server never saw the socket close")
	}
	waitFor(t, "span end", func() bool { return rec.endCount() == 1 })
	time.Sleep(20 * time.Millisecond)
	if rec.endCount() != 1 {
		t.Fatalf("span ended %d times", rec.endCount())
	}
	if h.proc.activeCount() != 0 {
		t.Fatal("stream not released")
	}
	// Audio after Close opens nothing.
	_ = h.sess.Post(&core.InputAudioFrame{Samples: make([]byte, 640), SampleRate: 16000, Channels: 1, Gen: 1})
	time.Sleep(50 * time.Millisecond)
	if f.opens.Load() != 1 {
		t.Fatalf("opened after close: %d", f.opens.Load())
	}
}

// A server that never closes after CloseStream is cut off at DrainTimeout.
func TestSTT_DrainTimeoutForcesClose(t *testing.T) {
	f := newFake(t, func(ctx context.Context, c *websocket.Conn) {
		for {
			if _, _, err := c.Read(ctx); err != nil {
				return
			}
		}
	})
	h := start(t, f.cfg(), nil)
	h.speak(t)
	waitFor(t, "stream", func() bool { return h.proc.activeCount() == 1 })
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{})
	select {
	case <-f.dropped:
	case <-time.After(2 * time.Second):
		t.Fatal("drain timeout did not close the socket")
	}
	waitFor(t, "release", func() bool { return h.proc.activeCount() == 0 })
}

func (p *STTProcessor) activeCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.all)
}

type lifecycleRecorder struct {
	mu   sync.Mutex
	ends map[string]int
}
type lifecycleSpan struct {
	r    *lifecycleRecorder
	name string
}

func (r *lifecycleRecorder) StartSpan(_ context.Context, ev core.TraceEvent) core.Span {
	return lifecycleSpan{r, ev.Name}
}
func (lifecycleSpan) SetAttribute(string, any) {}
func (lifecycleSpan) AddEvent(core.TraceEvent) {}
func (s lifecycleSpan) End(error) {
	s.r.mu.Lock()
	defer s.r.mu.Unlock()
	if s.r.ends == nil {
		s.r.ends = map[string]int{}
	}
	s.r.ends[s.name]++
}
func (r *lifecycleRecorder) endCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ends["stt.stream"]
}

// Deepgram's idle timeout (NET-0001, our silence) ends the stream quietly: no
// error frame, no breaker failure.
func TestSTT_IdleTimeoutCloseIsNotAnOutage(t *testing.T) {
	f := newFake(t, func(ctx context.Context, c *websocket.Conn) {
		_, _, _ = c.Read(ctx)
		_ = c.Close(websocket.StatusInternalError, "Deepgram did not receive audio data or a text message within the timeout window. See https://dpgr.am/net0001")
	})
	cfg := f.cfg()
	cfg.CircuitBreaker = resilience.NewBreaker(resilience.Config{FailureThreshold: 1, Cooldown: time.Hour})
	h := start(t, cfg, nil)
	h.speak(t)
	waitFor(t, "release", func() bool { return f.opens.Load() == 1 && h.proc.activeCount() == 0 })
	if len(h.errors()) != 0 || !cfg.CircuitBreaker.Allow() {
		t.Fatalf("idle close reported as outage: %v", h.errors())
	}
}

// Audio between turns opens an unbound stream; the next turn adopts it (VAD
// reports speech start after the onset audio was sent) and keeps its text.
// UserStopped finalizes it and later audio opens a fresh unbound stream.
func TestSTT_UnboundStreamAdoptedByNextTurn(t *testing.T) {
	var n atomic.Int32
	ctl := make(chan string, 8)
	f := newFake(t, func(ctx context.Context, c *websocket.Conn) {
		for {
			typ, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			if typ != websocket.MessageBinary {
				ctl <- string(data)
				continue
			}
			if n.Add(1) == 1 {
				_ = c.Write(ctx, websocket.MessageText, results("hi", true, false, false))
			} else {
				_ = c.Write(ctx, websocket.MessageText, results("there", false, false, false))
			}
		}
	})
	h := start(t, f.cfg(), nil)
	audio := func() {
		_ = h.sess.Post(&core.InputAudioFrame{Samples: make([]byte, 640), SampleRate: 16000, Channels: 1, Gen: 1})
	}
	committed := func() string {
		h.proc.mu.Lock()
		defer h.proc.mu.Unlock()
		if h.proc.active == nil {
			return ""
		}
		h.proc.active.mu.Lock()
		defer h.proc.active.mu.Unlock()
		return h.proc.active.committed
	}
	audio()
	waitFor(t, "onset final", func() bool { return committed() == "hi" })
	if len(h.candidates()) != 0 {
		t.Fatal("unbound stream emitted a candidate")
	}
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	audio()
	waitFor(t, "adopted candidate", func() bool { return len(h.candidates()) == 1 })
	if c := h.candidates()[0]; c.Text != "hi there" || c.Turn != "turn-1-1" || f.opens.Load() != 1 {
		t.Fatalf("candidate %+v opens=%d", c, f.opens.Load())
	}
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{})
	for _, want := range []string{"Finalize", "CloseStream"} {
		select {
		case got := <-ctl:
			if !strings.Contains(got, want) {
				t.Fatalf("control %q, want %s", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("no %s", want)
		}
	}
	audio()
	waitFor(t, "fresh stream", func() bool {
		h.proc.mu.Lock()
		defer h.proc.mu.Unlock()
		return f.opens.Load() == 2 && h.proc.active != nil
	})
	h.proc.mu.Lock()
	turn, _ := h.proc.active.owner()
	h.proc.mu.Unlock()
	if turn != "" {
		t.Fatalf("stream after the turn is bound to %q", turn)
	}
}

// A turn manager barges in mid-utterance: the open stream moves to the new
// generation (no reopen, cumulative text kept) so the fence does not drop it.
func TestSTT_SameTurnNewerGenerationAdoptsStream(t *testing.T) {
	var n atomic.Int32
	f := newFake(t, func(ctx context.Context, c *websocket.Conn) {
		for {
			typ, _, err := c.Read(ctx)
			if err != nil {
				return
			}
			if typ != websocket.MessageBinary {
				continue
			}
			text := "stop"
			if n.Add(1) > 1 {
				text = "now"
			}
			_ = c.Write(ctx, websocket.MessageText, results(text, true, false, false))
		}
	})
	h := start(t, f.cfg(), nil)
	h.speak(t)
	waitFor(t, "first candidate", func() bool { return len(h.candidates()) == 1 })
	h.sess.IncrementGeneration()
	_ = h.sess.Post(&core.TurnEventFrame{Kind: core.TurnEventStarted, Turn: "turn-1-1", Gen: 2})
	_ = h.sess.Post(&core.InputAudioFrame{Samples: make([]byte, 640), SampleRate: 16000, Channels: 1, Gen: 2})
	waitFor(t, "adopted candidate", func() bool { return len(h.candidates()) == 2 })
	if c := h.candidates()[1]; c.Text != "stop now" || c.Turn != "turn-1-1" || c.Gen != 2 || f.opens.Load() != 1 {
		t.Fatalf("candidate %+v opens=%d", c, f.opens.Load())
	}
}

// Legacy barge-in (no turn manager) cuts the stream; the reopened stream for
// the rest of the utterance carries the new generation so it is not fenced.
func TestSTT_InterruptionReopensAtNewGeneration(t *testing.T) {
	f := newFake(t, func(ctx context.Context, c *websocket.Conn) {
		for {
			typ, _, err := c.Read(ctx)
			if err != nil {
				return
			}
			if typ == websocket.MessageBinary {
				_ = c.Write(ctx, websocket.MessageText, results("go on", true, false, false))
			}
		}
	})
	h := start(t, f.cfg(), nil)
	h.speak(t)
	waitFor(t, "first candidate", func() bool { return len(h.candidates()) == 1 })
	h.sess.IncrementGeneration()
	_ = h.sess.Post(&core.InterruptionFrame{Gen: 2})
	_ = h.sess.Post(&core.InputAudioFrame{Samples: make([]byte, 640), SampleRate: 16000, Channels: 1, Gen: 2})
	waitFor(t, "candidate after barge-in", func() bool { return len(h.candidates()) == 2 })
	if c := h.candidates()[1]; c.Gen != 2 || c.Turn != "turn-1-1" {
		t.Fatalf("candidate %+v", c)
	}
}
