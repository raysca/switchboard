package cartesia

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"govoice/core"
	"govoice/core/coretest"
	"govoice/core/resilience"
	"govoice/core/sttunit"
)

const sttKey = "cartesia-test-key-0123456789"

type sfake struct {
	*httptest.Server
	opens   atomic.Int32
	queries chan string
	headers chan http.Header
	dropped chan struct{}
}

func newSFake(t *testing.T, handle func(ctx context.Context, c *websocket.Conn)) *sfake {
	f := &sfake{queries: make(chan string, 16), headers: make(chan http.Header, 16), dropped: make(chan struct{}, 16)}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.opens.Add(1)
		f.queries <- r.URL.RawQuery
		f.headers <- r.Header.Clone()
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

func (f *sfake) cfg() STTConfig {
	cfg := DefaultSTTConfig()
	cfg.Endpoint = "ws" + strings.TrimPrefix(f.URL, "http")
	cfg.APIKey = sttKey
	cfg.DrainTimeout = 300 * time.Millisecond
	cfg.RetryPolicy = resilience.Policy{MaxAttempts: 2, InitialBackoff: time.Millisecond, AttemptTimeout: 2 * time.Second}
	return cfg
}

func transcript(text string, final bool) []byte {
	return []byte(`{"type":"transcript","is_final":` + map[bool]string{true: "true", false: "false"}[final] + `,"text":"` + text + `","language":"en"}`)
}

type sharness struct {
	proc *STTProcessor
	sess *core.Session
	sink *coretest.PassthroughProcessor
}

func sstart(t *testing.T, cfg STTConfig, tracer core.Tracer) *sharness {
	t.Helper()
	proc, err := NewSTTProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sink := &coretest.PassthroughProcessor{}
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
	return &sharness{proc, sess, sink}
}

func (h *sharness) audio(gen uint64) {
	_ = h.sess.Post(&core.InputAudioFrame{Samples: make([]byte, 640), SampleRate: 16000, Channels: 1, Gen: gen})
}
func (h *sharness) speak() {
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	h.audio(1)
}
func (h *sharness) candidates() (out []*core.TranscriptCandidateFrame) {
	for _, f := range h.sink.GetFrames() {
		if c, ok := f.(*core.TranscriptCandidateFrame); ok {
			out = append(out, c)
		}
	}
	return
}
func (h *sharness) errors() (out []error) {
	for _, f := range h.sink.GetFrames() {
		if e, ok := f.(*core.ErrorFrame); ok {
			out = append(out, e.Err)
		}
	}
	return
}

// ensureStream dials one stream the way the runner's stream task does, then
// closes it (dial, retry and breaker tests).
func (p *STTProcessor) ensureStream(ctx context.Context) error {
	c, err := p.dial(ctx)
	if err == nil {
		c.Close()
	}
	return err
}

func (p *STTProcessor) activeCount() int { return p.Streams() }

func swait(t *testing.T, what string, cond func() bool) {
	t.Helper()
	until := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(until) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// echo answers each audio frame via script and handles finalize/close.
func echo(script func(n int) [][]byte, ctl chan<- string) func(context.Context, *websocket.Conn) {
	return func(ctx context.Context, c *websocket.Conn) {
		n := 0
		for {
			typ, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			if typ == websocket.MessageBinary {
				n++
				for _, m := range script(n) {
					_ = c.Write(ctx, websocket.MessageText, m)
				}
				continue
			}
			if ctl != nil {
				ctl <- string(data)
			}
			switch string(data) {
			case "finalize":
				_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"flush_done"}`))
			case "close":
				_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"done"}`))
				_ = c.Close(websocket.StatusNormalClosure, "")
				return
			}
		}
	}
}

func TestSTT_InterimFinalCumulativeAndFinalizeOnStop(t *testing.T) {
	ctl := make(chan string, 4)
	f := newSFake(t, echo(func(int) [][]byte {
		return [][]byte{transcript("hello", false), transcript("hello there", true), transcript(" how are", false)}
	}, ctl))
	h := sstart(t, f.cfg(), nil)
	h.speak()
	swait(t, "candidates", func() bool { return len(h.candidates()) >= 3 })
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{})
	swait(t, "utterance final", func() bool {
		c := h.candidates()
		return c[len(c)-1].EndOfUtterance
	})
	c := h.candidates()
	want := []struct {
		text  string
		final bool
	}{{"hello", false}, {"hello there", true}, {"hello there how are", false}, {"hello there how are", true}}
	for i, w := range want {
		if c[i].Text != w.text || c[i].Final != w.final || c[i].Provider != "cartesia" || c[i].Turn != "turn-1-1" {
			t.Errorf("candidate %d = %+v, want %+v", i, c[i], w)
		}
	}
	if c[1].EndOfUtterance || !c[3].EndOfUtterance {
		t.Errorf("only flush_done ends the utterance")
	}
	for _, w := range []string{"finalize", "close"} {
		if got := <-ctl; got != w {
			t.Errorf("control %q, want %q", got, w)
		}
	}
	q := <-f.queries
	for _, p := range []string{"model=ink-whisper", "encoding=pcm_s16le", "sample_rate=16000", "language=en", "cartesia_version="} {
		if !strings.Contains(q, p) {
			t.Errorf("query %q missing %s", q, p)
		}
	}
	if strings.Contains(q, sttKey) {
		t.Error("key in URL")
	}
	hd := <-f.headers
	if hd.Get("X-API-Key") != sttKey || hd.Get("Cartesia-Version") == "" {
		t.Errorf("headers %v", hd)
	}
	swait(t, "release", func() bool { return h.proc.activeCount() == 0 })
}

func TestSTT_OptionalQueryParams(t *testing.T) {
	cfg := DefaultSTTConfig()
	cfg.APIKey, cfg.MinVolume, cfg.MaxSilenceSecs = sttKey, 0.2, 1.5
	p, _ := NewSTTProcessor(cfg)
	u, _ := p.wsURL()
	if !strings.Contains(u, "min_volume=0.2") || !strings.Contains(u, "max_silence_duration_secs=1.5") || strings.Contains(u, sttKey) {
		t.Fatal(u)
	}
}

func TestSTT_MissingKey(t *testing.T) {
	if _, err := NewSTTProcessor(STTConfig{}); !errors.Is(err, ErrMissingAPIKey) {
		t.Fatal(err)
	}
}

func TestSTT_BadKeyNotRetriedAndExemptFromBreaker(t *testing.T) {
	var opens atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		opens.Add(1)
		http.Error(w, "bad X-API-Key: "+sttKey, http.StatusUnauthorized)
	}))
	defer srv.Close()
	cfg := DefaultSTTConfig()
	cfg.Endpoint, cfg.APIKey = "ws"+strings.TrimPrefix(srv.URL, "http"), sttKey
	cfg.CircuitBreaker = resilience.NewBreaker(resilience.Config{FailureThreshold: 1, Cooldown: time.Hour})
	h := sstart(t, cfg, nil)
	err := h.proc.ensureStream(context.Background())
	var pe *ProviderError
	if !errors.As(err, &pe) || pe.Code != "UNAUTHORIZED" || pe.Retryable || !pe.ClientError {
		t.Fatalf("err=%v", err)
	}
	if strings.Contains(err.Error(), sttKey) {
		t.Fatal("secret in error")
	}
	if opens.Load() != 1 || !cfg.CircuitBreaker.Allow() {
		t.Fatalf("opens=%d, breaker must stay closed", opens.Load())
	}
}

func TestSTT_OpenRetriesTransient5xxAnd429(t *testing.T) {
	for _, status := range []int{503, 429} {
		var opens atomic.Int32
		inner := newSFake(t, func(ctx context.Context, c *websocket.Conn) { _, _, _ = c.Read(ctx) })
		flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if opens.Add(1) == 1 {
				w.Header().Set("Retry-After", "0")
				http.Error(w, "down", status)
				return
			}
			inner.Config.Handler.ServeHTTP(w, r)
		}))
		cfg := inner.cfg()
		cfg.Endpoint = "ws" + strings.TrimPrefix(flaky.URL, "http")
		h := sstart(t, cfg, nil)
		if err := h.proc.ensureStream(context.Background()); err != nil || opens.Load() != 2 {
			t.Fatalf("status %d: err=%v opens=%d", status, err, opens.Load())
		}
		flaky.Close()
	}
}

func TestSTT_BreakerOpensOnOutage(t *testing.T) {
	var opens atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		opens.Add(1)
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer srv.Close()
	cfg := DefaultSTTConfig()
	cfg.Endpoint, cfg.APIKey = "ws"+strings.TrimPrefix(srv.URL, "http"), sttKey
	cfg.RetryPolicy = resilience.Policy{MaxAttempts: 1}
	cfg.CircuitBreaker = resilience.NewBreaker(resilience.Config{FailureThreshold: 2, Cooldown: time.Hour})
	h := sstart(t, cfg, nil)
	for i := 0; i < 2; i++ {
		if err := h.proc.ensureStream(context.Background()); err == nil {
			t.Fatal("expected failure")
		}
	}
	if err := h.proc.ensureStream(context.Background()); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("err=%v", err)
	}
	if opens.Load() != 2 {
		t.Fatalf("opens=%d", opens.Load())
	}
}

// A hung upgrade is bounded by the attempt timeout and counted as an outage.
func TestSTT_HungOpenTimesOutAndTripsBreaker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer srv.Close()
	cfg := DefaultSTTConfig()
	cfg.Endpoint, cfg.APIKey = "ws"+strings.TrimPrefix(srv.URL, "http"), sttKey
	cfg.RetryPolicy = resilience.Policy{MaxAttempts: 2, AttemptTimeout: 50 * time.Millisecond, InitialBackoff: time.Millisecond}
	cfg.CircuitBreaker = resilience.NewBreaker(resilience.Config{FailureThreshold: 1, Cooldown: time.Hour})
	h := sstart(t, cfg, nil)
	err := h.proc.ensureStream(context.Background())
	if !errors.Is(err, resilience.ErrTimeout) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
	if cfg.CircuitBreaker.Allow() {
		t.Fatal("hang must open the breaker")
	}
}

func TestClassifySTT(t *testing.T) {
	for _, tc := range []struct {
		status           int
		retryable, ready bool
	}{{401, false, true}, {403, false, true}, {400, false, true}, {429, true, false}, {503, true, false}} {
		pe := MapError("stt", errors.New("x"), &http.Response{StatusCode: tc.status, Header: http.Header{"Retry-After": {"3"}}})
		if v := classify(pe); v.Retryable != tc.retryable || v.ClientError != tc.ready {
			t.Errorf("%d: %+v", tc.status, v)
		}
	}
	policy := MapError("stt", websocket.CloseError{Code: websocket.StatusPolicyViolation}, nil)
	if !policy.ClientError || policy.Retryable {
		t.Errorf("1008: %+v", policy)
	}
	if m := messageError("stt", 401, "bad"); !m.ClientError || m.Capability != "stt" {
		t.Errorf("in-band 401: %+v", m)
	}
	if m := messageError("stt", 500, "boom"); m.ClientError || !m.Retryable {
		t.Errorf("in-band 500: %+v", m)
	}
	if v := classify(context.Canceled); !v.ClientError {
		t.Error("cancel counted as outage")
	}
}

func TestSTT_MidStreamDropReportsError(t *testing.T) {
	f := newSFake(t, func(ctx context.Context, c *websocket.Conn) {
		_, _, _ = c.Read(ctx)
		_ = c.Close(websocket.StatusInternalError, "boom")
	})
	cfg := f.cfg()
	cfg.CircuitBreaker = resilience.NewBreaker(resilience.Config{FailureThreshold: 1, Cooldown: time.Hour})
	h := sstart(t, cfg, nil)
	h.speak()
	swait(t, "error frame", func() bool { return len(h.errors()) > 0 })
	var pe *ProviderError
	if !errors.As(h.errors()[0], &pe) || !pe.Retryable {
		t.Fatalf("err=%v", h.errors()[0])
	}
	if cfg.CircuitBreaker.Allow() {
		t.Fatal("drop must open the breaker")
	}
}

func TestSTT_InBandErrors(t *testing.T) {
	for _, tc := range []struct {
		status     int
		wantClient bool
	}{{401, true}, {429, false}, {503, false}} {
		f := newSFake(t, func(ctx context.Context, c *websocket.Conn) {
			_, _, _ = c.Read(ctx)
			_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"error","status_code":`+map[int]string{401: "401", 429: "429", 503: "503"}[tc.status]+`,"message":"oops api_key=`+sttKey+`"}`))
			<-ctx.Done()
		})
		cfg := f.cfg()
		cfg.CircuitBreaker = resilience.NewBreaker(resilience.Config{FailureThreshold: 1, Cooldown: time.Hour})
		h := sstart(t, cfg, nil)
		h.speak()
		swait(t, "error frame", func() bool { return len(h.errors()) > 0 })
		var pe *ProviderError
		if !errors.As(h.errors()[0], &pe) || pe.ClientError != tc.wantClient || pe.StatusCode != tc.status {
			t.Fatalf("%d: %v", tc.status, h.errors()[0])
		}
		if strings.Contains(pe.Error(), sttKey) {
			t.Fatal("secret in error")
		}
		if cfg.CircuitBreaker.Allow() != tc.wantClient {
			t.Fatalf("%d: breaker allow=%v", tc.status, !tc.wantClient)
		}
	}
}

func TestSTT_IdleCloseIsNotAnOutage(t *testing.T) {
	f := newSFake(t, func(ctx context.Context, c *websocket.Conn) {
		_, _, _ = c.Read(ctx)
		_ = c.Close(websocket.StatusInternalError, "connection idle timeout")
	})
	cfg := f.cfg()
	cfg.CircuitBreaker = resilience.NewBreaker(resilience.Config{FailureThreshold: 1, Cooldown: time.Hour})
	h := sstart(t, cfg, nil)
	h.audio(1)
	swait(t, "release", func() bool { return f.opens.Load() == 1 && h.proc.activeCount() == 0 })
	if len(h.errors()) != 0 || !cfg.CircuitBreaker.Allow() {
		t.Fatalf("idle close reported as outage: %v", h.errors())
	}
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

func TestSTT_CloseTearsDownEndsTraceOnceNoLeak(t *testing.T) {
	before := runtime.NumGoroutine()
	f := newSFake(t, func(ctx context.Context, c *websocket.Conn) {
		for {
			if _, _, err := c.Read(ctx); err != nil {
				return
			}
		}
	})
	rec := &lifecycleRecorder{}
	h := sstart(t, f.cfg(), rec)
	h.speak()
	swait(t, "stream", func() bool { return f.opens.Load() == 1 && h.proc.activeCount() == 1 })
	_ = h.proc.Close()
	_ = h.proc.Close()
	select {
	case <-f.dropped:
	case <-time.After(2 * time.Second):
		t.Fatal("socket not closed")
	}
	swait(t, "span end", func() bool { return rec.endCount() == 1 })
	time.Sleep(20 * time.Millisecond)
	if rec.endCount() != 1 || h.proc.activeCount() != 0 {
		t.Fatalf("ends=%d active=%d", rec.endCount(), h.proc.activeCount())
	}
	h.audio(1)
	time.Sleep(50 * time.Millisecond)
	if f.opens.Load() != 1 {
		t.Fatal("opened after close")
	}
	h.sess.Close()
	http.DefaultClient.CloseIdleConnections()
	swait(t, "goroutines settle", func() bool { return runtime.NumGoroutine() <= before+4 })
}

func TestSTT_DrainTimeoutForcesClose(t *testing.T) {
	f := newSFake(t, func(ctx context.Context, c *websocket.Conn) {
		for {
			if _, _, err := c.Read(ctx); err != nil {
				return
			}
		}
	})
	h := sstart(t, f.cfg(), nil)
	h.speak()
	swait(t, "stream", func() bool { return h.proc.activeCount() == 1 })
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{})
	select {
	case <-f.dropped:
	case <-time.After(2 * time.Second):
		t.Fatal("drain timeout did not close the socket")
	}
	swait(t, "release", func() bool { return h.proc.activeCount() == 0 })
}

func TestSTT_UnboundStreamAdoptedByNextTurn(t *testing.T) {
	f := newSFake(t, echo(func(n int) [][]byte {
		if n == 1 {
			return [][]byte{transcript("hi", true)}
		}
		return [][]byte{transcript(" there", false)}
	}, nil))
	h := sstart(t, f.cfg(), nil)
	last := func() *core.TranscriptCandidateFrame {
		c := h.candidates()
		if len(c) == 0 {
			return nil
		}
		return c[len(c)-1]
	}
	h.audio(1)
	swait(t, "unbound stream", func() bool { return f.opens.Load() == 1 })
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	h.audio(1)
	swait(t, "adopted candidate", func() bool { c := last(); return c != nil && c.Text == "hi there" })
	if c := last(); c.Turn != "turn-1-1" || f.opens.Load() != 1 {
		t.Fatalf("candidate %+v opens=%d", c, f.opens.Load())
	}
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{})
	swait(t, "closed", func() bool { return h.proc.activeCount() == 0 })
	h.audio(1)
	swait(t, "fresh stream", func() bool { return f.opens.Load() == 2 })
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 2})
	h.audio(1)
	swait(t, "second turn adopts it", func() bool { c := last(); return c != nil && c.Turn == "turn-1-2" })
	if n := f.opens.Load(); n != 2 {
		t.Fatalf("the second turn opened its own stream: opens=%d", n)
	}
}

func TestSTT_SameTurnNewerGenerationAdoptsStream(t *testing.T) {
	f := newSFake(t, echo(func(n int) [][]byte {
		if n == 1 {
			return [][]byte{transcript("stop", true)}
		}
		return [][]byte{transcript(" now", true)}
	}, nil))
	h := sstart(t, f.cfg(), nil)
	h.speak()
	swait(t, "first", func() bool { return len(h.candidates()) == 1 })
	h.sess.IncrementGeneration()
	_ = h.sess.Post(&core.TurnEventFrame{Kind: core.TurnEventStarted, Turn: "turn-1-1", Gen: 2})
	h.audio(2)
	swait(t, "second", func() bool { return len(h.candidates()) == 2 })
	if c := h.candidates()[1]; c.Text != "stop now" || c.Gen != 2 || f.opens.Load() != 1 {
		t.Fatalf("candidate %+v opens=%d", c, f.opens.Load())
	}
}

func TestSTT_InterruptionReopensAtNewGeneration(t *testing.T) {
	f := newSFake(t, echo(func(int) [][]byte { return [][]byte{transcript("go on", true)} }, nil))
	h := sstart(t, f.cfg(), nil)
	h.speak()
	swait(t, "first", func() bool { return len(h.candidates()) == 1 })
	h.sess.IncrementGeneration()
	_ = h.sess.Post(&core.InterruptionFrame{Gen: 2})
	h.audio(2)
	swait(t, "after barge-in", func() bool { return len(h.candidates()) == 2 })
	if c := h.candidates()[1]; c.Gen != 2 || c.Turn != "turn-1-1" || c.Text != "go on" {
		t.Fatalf("candidate %+v", c)
	}
}

func TestSTT_WarmSkippedWhileBreakerOpen(t *testing.T) {
	br := resilience.NewBreaker(resilience.Config{FailureThreshold: 1, Cooldown: time.Hour})
	br.RecordFailure()
	cfg := DefaultSTTConfig()
	cfg.APIKey, cfg.CircuitBreaker = sttKey, br
	p, _ := NewSTTProcessor(cfg)
	if err := p.Warm(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// Cartesia auto-finalizes on its own silence detection mid-turn: each final
// delta is appended verbatim (deltas carry their own leading space), and only
// flush_done after our finalize ends the utterance.
func TestSTT_ServerAutoFinalsComposeWithinTurn(t *testing.T) {
	f := newSFake(t, echo(func(n int) [][]byte {
		switch n {
		case 1:
			return [][]byte{transcript("Hi", false), transcript("Hi.", true)}
		case 2:
			return [][]byte{transcript(" I need", false), transcript(" I need help.", true)}
		}
		return nil
	}, nil))
	h := sstart(t, f.cfg(), nil)
	h.speak()
	swait(t, "first final", func() bool { return len(h.candidates()) >= 2 })
	h.audio(1)
	swait(t, "second final", func() bool { return len(h.candidates()) >= 4 })
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{})
	swait(t, "eou", func() bool { c := h.candidates(); return c[len(c)-1].EndOfUtterance })
	c := h.candidates()
	got := []string{}
	for _, x := range c {
		got = append(got, x.Text)
	}
	want := []string{"Hi", "Hi.", "Hi. I need", "Hi. I need help.", "Hi. I need help."}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("texts %q, want %q", got, want)
	}
	for _, x := range c[:len(c)-1] {
		if x.EndOfUtterance {
			t.Fatalf("server auto-final must not end the utterance: %+v", x)
		}
	}
}

// A caller hang-up during the open is not an outage.
func TestSTT_CancelledOpenDoesNotTripBreaker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer srv.Close()
	cfg := DefaultSTTConfig()
	cfg.Endpoint, cfg.APIKey = "ws"+strings.TrimPrefix(srv.URL, "http"), sttKey
	cfg.CircuitBreaker = resilience.NewBreaker(resilience.Config{FailureThreshold: 1, Cooldown: time.Hour})
	h := sstart(t, cfg, nil)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(30*time.Millisecond, cancel)
	if err := h.proc.ensureStream(ctx); err == nil {
		t.Fatal("expected error")
	}
	if !cfg.CircuitBreaker.Allow() {
		t.Fatal("cancel must not open the breaker")
	}
}

func TestSTT_WhisperOnlyParamsOmittedForOtherModels(t *testing.T) {
	cfg := DefaultSTTConfig()
	cfg.APIKey, cfg.Model, cfg.MinVolume, cfg.MaxSilenceSecs = sttKey, "ink-2", 0.2, 1.5
	p, _ := NewSTTProcessor(cfg)
	u, _ := p.wsURL()
	if strings.Contains(u, "language=") || strings.Contains(u, "min_volume") || strings.Contains(u, "max_silence") || !strings.Contains(u, "model=ink-2") {
		t.Fatal(u)
	}
}

// A8: a clean close while the turn still owes its transcript invalidates it.
func TestSTT_CleanCloseMidUtteranceInvalidatesTurn(t *testing.T) {
	f := newSFake(t, func(ctx context.Context, c *websocket.Conn) {
		_, _, _ = c.Read(ctx)
		_ = c.Close(websocket.StatusNormalClosure, "")
	})
	cfg := f.cfg()
	cfg.CircuitBreaker = resilience.NewBreaker(resilience.Config{FailureThreshold: 1, Cooldown: time.Hour})
	h := sstart(t, cfg, nil)
	h.speak()
	swait(t, "invalidating error", func() bool {
		for _, err := range h.errors() {
			if errors.Is(err, core.ErrTranscriptInvalidated) {
				return true
			}
		}
		return false
	})
	if !cfg.CircuitBreaker.Allow() {
		t.Fatal("a clean close is not an outage")
	}
}

// Text heard while the stream was unbound (the runner drops those results) is
// re-surfaced by the next result: the Conn's text is cumulative.
func TestSTTConn_ResurfacesUnboundTextAndFlushPromotesInterim(t *testing.T) {
	c := &sttConn{}
	_ = c.transcript(&sttMessage{Text: "hi", IsFinal: true}) // dropped by the runner
	_ = c.transcript(&sttMessage{Text: " the", IsFinal: false})
	got := c.transcript(&sttMessage{Text: " there", IsFinal: false})
	if want := (sttunit.Result{Text: "hi there"}); got != want {
		t.Fatalf("interim: %+v, want %+v", got, want)
	}
	if got, want := c.flushed(), (sttunit.Result{Text: "hi there", Final: true, EndOfUtterance: true}); got != want {
		t.Fatalf("flush: %+v, want %+v", got, want)
	}
	got = c.transcript(&sttMessage{Text: " now", IsFinal: true})
	if want := (sttunit.Result{Text: "hi there now", Final: true}); got != want {
		t.Fatalf("final after flush: %+v, want %+v", got, want)
	}
	if got := (&sttConn{}).transcript(&sttMessage{IsFinal: true}); got != (sttunit.Result{Final: true}) {
		t.Fatalf("empty final: %+v", got)
	}
}
