package elevenlabs

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"govoice/core"
	"govoice/core/coretest"
	"govoice/core/resilience"
	"govoice/core/sttunit"
)

const sttKey = "xi-test-key-0123456789"

type sttChunk struct {
	MessageType string `json:"message_type"`
	Audio       string `json:"audio_base_64"`
	Commit      bool   `json:"commit"`
	SampleRate  int    `json:"sample_rate"`
}

// sttFake is a local ElevenLabs realtime STT: handle runs once per accepted socket.
type sttFake struct {
	*httptest.Server
	opens   atomic.Int32
	urls    chan *url.URL
	headers chan http.Header
	dropped chan struct{}
}

func newSTTFake(t *testing.T, handle func(ctx context.Context, c *websocket.Conn)) *sttFake {
	f := &sttFake{urls: make(chan *url.URL, 8), headers: make(chan http.Header, 8), dropped: make(chan struct{}, 8)}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.opens.Add(1)
		f.urls <- r.URL
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

func (f *sttFake) cfg() STTConfig {
	cfg := DefaultSTTConfig()
	cfg.Endpoint = "ws" + strings.TrimPrefix(f.URL, "http")
	cfg.APIKey = sttKey
	cfg.DrainTimeout = 300 * time.Millisecond
	cfg.RetryPolicy = resilience.Policy{MaxAttempts: 2, InitialBackoff: time.Millisecond, AttemptTimeout: 2 * time.Second}
	return cfg
}

func sttMsg(ctx context.Context, c *websocket.Conn, typ, text string) {
	b, _ := json.Marshal(map[string]string{"message_type": typ, "text": text})
	_ = c.Write(ctx, websocket.MessageText, b)
}
func sttErr(ctx context.Context, c *websocket.Conn, typ, text string) {
	b, _ := json.Marshal(map[string]string{"message_type": typ, "error": text})
	_ = c.Write(ctx, websocket.MessageText, b)
}

// readChunk reads one client message; ok is false when the socket ended.
func readChunk(ctx context.Context, c *websocket.Conn) (m sttChunk, ok bool) {
	_, data, err := c.Read(ctx)
	if err != nil {
		return m, false
	}
	_ = json.Unmarshal(data, &m)
	return m, true
}

func holdSTT(ctx context.Context, c *websocket.Conn) {
	for {
		if _, ok := readChunk(ctx, c); !ok {
			return
		}
	}
}

type sttHarness struct {
	proc *STTProcessor
	sess *core.Session
	sink *coretest.PassthroughProcessor
}

func startSTT(t *testing.T, cfg STTConfig, tracer core.Tracer) *sttHarness {
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
	return &sttHarness{proc, sess, sink}
}

func (h *sttHarness) audio(gen uint64) {
	_ = h.sess.Post(&core.InputAudioFrame{Samples: make([]byte, 640), SampleRate: 16000, Channels: 1, Gen: gen})
}
func (h *sttHarness) speak() {
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	h.audio(1)
}
func (h *sttHarness) candidates() (out []*core.TranscriptCandidateFrame) {
	for _, f := range h.sink.GetFrames() {
		if c, ok := f.(*core.TranscriptCandidateFrame); ok {
			out = append(out, c)
		}
	}
	return
}
func (h *sttHarness) errors() (out []error) {
	for _, f := range h.sink.GetFrames() {
		if e, ok := f.(*core.ErrorFrame); ok {
			out = append(out, e.Err)
		}
	}
	return
}

func sttWait(t *testing.T, what string, cond func() bool) {
	t.Helper()
	until := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(until) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
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

func sttBreaker() *resilience.Breaker {
	return resilience.NewBreaker(resilience.Config{FailureThreshold: 1, Cooldown: time.Hour})
}

// Partial and committed text accumulate per turn; the commit is manual (only on
// UserStoppedSpeaking) and the committed text answering it ends the utterance
// and closes the socket.
func TestSTT_CumulativeTextAndManualCommit(t *testing.T) {
	var commits atomic.Int32
	var chunks atomic.Int32
	f := newSTTFake(t, func(ctx context.Context, c *websocket.Conn) {
		sttMsg(ctx, c, "session_started", "")
		for {
			m, ok := readChunk(ctx, c)
			if !ok {
				return
			}
			if m.Commit {
				commits.Add(1)
				sttMsg(ctx, c, "committed_transcript", "how are you")
				holdSTT(ctx, c) // returns when the client closes
				return
			}
			if chunks.Add(1) == 1 {
				sttMsg(ctx, c, "partial_transcript", "hello")
				sttMsg(ctx, c, "committed_transcript", "hello there")
				sttMsg(ctx, c, "partial_transcript", "how are")
			}
		}
	})
	cfg := f.cfg()
	cfg.Language = "en-US"
	h := startSTT(t, cfg, nil)
	h.speak()
	sttWait(t, "interim+final", func() bool { return len(h.candidates()) >= 3 })
	if commits.Load() != 0 {
		t.Fatal("committed before the turn ended")
	}
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{})
	sttWait(t, "utterance final", func() bool { c := h.candidates(); return len(c) >= 4 })
	want := []struct {
		text       string
		final, eou bool
	}{{"hello", false, false}, {"hello there", true, false}, {"hello there how are", false, false}, {"hello there how are you", true, true}}
	c := h.candidates()
	for i, w := range want {
		if c[i].Text != w.text || c[i].Final != w.final || c[i].EndOfUtterance != w.eou || c[i].Provider != "elevenlabs" || c[i].Turn != "turn-1-1" {
			t.Errorf("candidate %d = %+v, want %+v", i, c[i], w)
		}
	}
	u := <-f.urls
	q := u.Query()
	for k, v := range map[string]string{"model_id": "scribe_v2_realtime", "audio_format": "pcm_16000", "commit_strategy": "manual", "language_code": "en"} {
		if q.Get(k) != v {
			t.Errorf("query %s=%q, want %q (%s)", k, q.Get(k), v, u.RawQuery)
		}
	}
	if strings.Contains(u.String(), sttKey) {
		t.Error("API key in URL")
	}
	if got := (<-f.headers).Get("xi-api-key"); got != sttKey {
		t.Errorf("xi-api-key %q", got)
	}
	select {
	case <-f.dropped:
	case <-time.After(2 * time.Second):
		t.Fatal("socket not closed after the committed answer")
	}
	sttWait(t, "release", func() bool { return h.proc.activeCount() == 0 })
	// A settled normal turn must not look like a lost one.
	if n := len(h.errors()); n != 0 {
		t.Fatalf("normal turn produced %d error frames: %v", n, h.errors())
	}
}

// The wire message carries base64 audio and the configured rate.
func TestSTT_ChunkWireFormat(t *testing.T) {
	got := make(chan sttChunk, 4)
	f := newSTTFake(t, func(ctx context.Context, c *websocket.Conn) {
		for {
			m, ok := readChunk(ctx, c)
			if !ok {
				return
			}
			got <- m
		}
	})
	h := startSTT(t, f.cfg(), nil)
	h.speak()
	m := <-got
	if m.MessageType != "input_audio_chunk" || m.SampleRate != 16000 || m.Commit || len(m.Audio) != 856 { // base64 of 640 bytes
		t.Fatalf("%+v", m)
	}
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{})
	if m := <-got; !m.Commit || m.MessageType != "input_audio_chunk" {
		t.Fatalf("commit chunk %+v", m)
	}
}

// With timestamps on, one commit may arrive in both forms: counted once.
func TestSTT_CommittedFormsDeduplicated(t *testing.T) {
	f := newSTTFake(t, func(ctx context.Context, c *websocket.Conn) {
		for {
			m, ok := readChunk(ctx, c)
			if !ok {
				return
			}
			if m.Commit {
				sttMsg(ctx, c, "committed_transcript", "good day")
				sttMsg(ctx, c, "committed_transcript_with_timestamps", "good day")
			}
		}
	})
	h := startSTT(t, f.cfg(), nil)
	h.speak()
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{})
	sttWait(t, "final", func() bool { return len(h.candidates()) >= 1 })
	time.Sleep(30 * time.Millisecond)
	if c := h.candidates(); len(c) != 1 || c[0].Text != "good day" {
		t.Fatalf("%+v", c)
	}
	// Only the with-timestamps form (no plain one) also works.
	f2 := newSTTFake(t, func(ctx context.Context, c *websocket.Conn) {
		for {
			if m, ok := readChunk(ctx, c); !ok {
				return
			} else if m.Commit {
				sttMsg(ctx, c, "committed_transcript_with_timestamps", "only ts")
			}
		}
	})
	cfg := f2.cfg()
	cfg.IncludeTimestamps = true
	h2 := startSTT(t, cfg, nil)
	h2.speak()
	_ = h2.sess.Post(&core.UserStoppedSpeakingFrame{})
	sttWait(t, "final", func() bool { return len(h2.candidates()) == 1 })
	if !strings.Contains((<-f2.urls).RawQuery, "include_timestamps=true") {
		t.Error("include_timestamps missing")
	}
}

func TestSTT_ConfigValidation(t *testing.T) {
	if _, err := NewSTTProcessor(STTConfig{}); !errors.Is(err, ErrMissingAPIKey) {
		t.Fatalf("err=%v", err)
	}
	if _, err := NewSTTProcessor(STTConfig{APIKey: "k", SampleRate: 12345}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("err=%v", err)
	}
}

func TestSTTMessageErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		kind      string
		client    bool
		retryable bool
	}{
		{"auth_error", true, false}, {"quota_exceeded", true, false}, {"input_error", true, false},
		{"invalid_request", true, false}, {"chunk_size_exceeded", true, false}, {"unaccepted_terms", true, false},
		{"rate_limited", false, true}, {"commit_throttled", false, true}, {"queue_overflow", false, true},
		{"resource_exhausted", false, true}, {"transcriber_error", false, true}, {"error", false, true},
	} {
		pe := sttMessageError(tc.kind, "boom")
		v := classify(pe)
		if pe.ClientError != tc.client || v.ClientError != tc.client || v.Retryable != tc.retryable || pe.Capability != "stt" {
			t.Errorf("%s: %+v %+v", tc.kind, pe, v)
		}
	}
}

// A client-fault error message is surfaced but never counts for the breaker;
// an outage-class one opens it. The API key is scrubbed from server text.
func TestSTT_ErrorMessagesBreakerAndRedaction(t *testing.T) {
	for _, tc := range []struct {
		kind   string
		client bool
	}{{"auth_error", true}, {"quota_exceeded", true}, {"input_error", true}, {"rate_limited", false}, {"transcriber_error", false}} {
		f := newSTTFake(t, func(ctx context.Context, c *websocket.Conn) {
			_, _ = readChunk(ctx, c)
			sttErr(ctx, c, tc.kind, "failed for key "+sttKey)
		})
		cfg := f.cfg()
		cfg.CircuitBreaker = sttBreaker()
		h := startSTT(t, cfg, nil)
		h.speak()
		sttWait(t, tc.kind+" error frame", func() bool { return len(h.errors()) > 0 })
		var pe *ProviderError
		if !errors.As(h.errors()[0], &pe) || pe.ClientError != tc.client {
			t.Fatalf("%s: %v", tc.kind, h.errors()[0])
		}
		if strings.Contains(pe.Error(), sttKey) || !strings.Contains(pe.Message, "[REDACTED]") {
			t.Errorf("%s: key leaked: %v", tc.kind, pe)
		}
		if cfg.CircuitBreaker.Allow() != tc.client {
			t.Errorf("%s: breaker allow=%v", tc.kind, !tc.client)
		}
		sttWait(t, "release", func() bool { return h.proc.activeCount() == 0 })
		if n := func() uint64 { _, _, _, e := h.proc.STTCounts(); return e }(); n != 1 {
			t.Errorf("%s: error count %d", tc.kind, n)
		}
	}
}

// Bad key on the upgrade: not retried, not an outage.
func TestSTT_BadKeyNotRetriedAndExemptFromBreaker(t *testing.T) {
	var opens atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		opens.Add(1)
		http.Error(w, "bad credentials", http.StatusUnauthorized)
	}))
	defer srv.Close()
	cfg := DefaultSTTConfig()
	cfg.Endpoint, cfg.APIKey = "ws"+strings.TrimPrefix(srv.URL, "http"), sttKey
	cfg.CircuitBreaker = sttBreaker()
	h := startSTT(t, cfg, nil)
	err := h.proc.ensureStream(context.Background())
	var pe *ProviderError
	if !errors.As(err, &pe) || pe.Code != "UNAUTHORIZED" || pe.Retryable || !pe.ClientError {
		t.Fatalf("err=%v", err)
	}
	if opens.Load() != 1 || !cfg.CircuitBreaker.Allow() {
		t.Fatalf("opens=%d, breaker must stay closed", opens.Load())
	}
}

func TestSTT_OpenRetriesTransientFailure(t *testing.T) {
	var opens atomic.Int32
	inner := newSTTFake(t, holdSTT)
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if opens.Add(1) == 1 {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		inner.Config.Handler.ServeHTTP(w, r)
	}))
	defer flaky.Close()
	cfg := inner.cfg()
	cfg.Endpoint = "ws" + strings.TrimPrefix(flaky.URL, "http")
	h := startSTT(t, cfg, nil)
	if err := h.proc.ensureStream(context.Background()); err != nil {
		t.Fatal(err)
	}
	if opens.Load() != 2 {
		t.Fatalf("opens=%d", opens.Load())
	}
}

// A handshake that hangs is cut at the per-attempt deadline, retried, then
// fails as a timeout the breaker counts.
func TestSTT_HungOpenIsBoundedTimeout(t *testing.T) {
	var opens atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		opens.Add(1)
		<-r.Context().Done()
	}))
	defer srv.Close()
	cfg := DefaultSTTConfig()
	cfg.Endpoint, cfg.APIKey = "ws"+strings.TrimPrefix(srv.URL, "http"), sttKey
	cfg.RetryPolicy = resilience.Policy{MaxAttempts: 2, InitialBackoff: time.Millisecond, AttemptTimeout: 80 * time.Millisecond}
	cfg.CircuitBreaker = resilience.NewBreaker(resilience.Config{FailureThreshold: 2, Cooldown: time.Hour})
	h := startSTT(t, cfg, nil)
	err := h.proc.ensureStream(context.Background())
	if !errors.Is(err, resilience.ErrTimeout) || opens.Load() != 2 {
		t.Fatalf("err=%v opens=%d", err, opens.Load())
	}
	if cfg.CircuitBreaker.Allow() {
		t.Fatal("timeouts must open the breaker")
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
	h := startSTT(t, cfg, nil)
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

// 429 on the upgrade is an outage-class, retryable error.
func TestSTT_RateLimitUpgradeIsOutage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "slow down", 429) }))
	defer srv.Close()
	cfg := DefaultSTTConfig()
	cfg.Endpoint, cfg.APIKey = "ws"+strings.TrimPrefix(srv.URL, "http"), sttKey
	cfg.RetryPolicy = resilience.Policy{MaxAttempts: 1}
	cfg.CircuitBreaker = sttBreaker()
	h := startSTT(t, cfg, nil)
	err := h.proc.ensureStream(context.Background())
	if v := classify(err); !v.Retryable || v.ClientError {
		t.Fatalf("%v %+v", err, v)
	}
	if cfg.CircuitBreaker.Allow() {
		t.Fatal("429 must count")
	}
}

// A mid-stream abnormal close is an outage: error frame and breaker failure.
func TestSTT_MidStreamDropReportsError(t *testing.T) {
	f := newSTTFake(t, func(ctx context.Context, c *websocket.Conn) {
		_, _ = readChunk(ctx, c)
		_ = c.Close(websocket.StatusInternalError, "boom")
	})
	cfg := f.cfg()
	cfg.CircuitBreaker = sttBreaker()
	h := startSTT(t, cfg, nil)
	h.speak()
	sttWait(t, "error frame", func() bool { return len(h.errors()) > 0 })
	var pe *ProviderError
	if !errors.As(h.errors()[0], &pe) || !pe.Retryable || pe.ClientError {
		t.Fatalf("err=%v", h.errors()[0])
	}
	if cfg.CircuitBreaker.Allow() {
		t.Fatal("outage-class drop must open the breaker")
	}
}

// Idle closes (clean close, or an idle-worded reason) are our silence, not an outage.
func TestSTT_IdleCloseIsNotAnOutage(t *testing.T) {
	for _, tc := range []struct {
		code   websocket.StatusCode
		reason string
	}{{websocket.StatusNormalClosure, ""}, {websocket.StatusGoingAway, "bye"}, {websocket.StatusInternalError, "closed after idle timeout"}} {
		f := newSTTFake(t, func(ctx context.Context, c *websocket.Conn) {
			_, _ = readChunk(ctx, c)
			_ = c.Close(tc.code, tc.reason)
		})
		cfg := f.cfg()
		cfg.CircuitBreaker = sttBreaker()
		h := startSTT(t, cfg, nil)
		h.audio(1)
		sttWait(t, "release", func() bool { return f.opens.Load() == 1 && h.proc.activeCount() == 0 })
		if len(h.errors()) != 0 || !cfg.CircuitBreaker.Allow() {
			t.Fatalf("%v: idle close reported as outage: %v", tc.code, h.errors())
		}
	}
}

// Close tears the socket down; the trace span ends exactly once; nothing leaks.
func TestSTT_CloseTearsDownEndsTraceOnceNoLeak(t *testing.T) {
	base := runtime.NumGoroutine()
	f := newSTTFake(t, holdSTT)
	rec := &spanRecorder{}
	h := startSTT(t, f.cfg(), rec)
	h.speak()
	sttWait(t, "stream", func() bool { return f.opens.Load() == 1 && h.proc.activeCount() == 1 })
	_ = h.proc.Close()
	_ = h.proc.Close()
	select {
	case <-f.dropped:
	case <-time.After(2 * time.Second):
		t.Fatal("server never saw the socket close")
	}
	sttWait(t, "span end", func() bool { rec.mu.Lock(); defer rec.mu.Unlock(); return rec.ends["stt.stream"] == 1 })
	time.Sleep(20 * time.Millisecond)
	rec.mu.Lock()
	if rec.ends["stt.stream"] != 1 {
		t.Fatalf("span ended %d times", rec.ends["stt.stream"])
	}
	rec.mu.Unlock()
	h.audio(1)
	time.Sleep(50 * time.Millisecond)
	if f.opens.Load() != 1 {
		t.Fatalf("opened after close: %d", f.opens.Load())
	}
	h.sess.Close()
	f.Close()
	for i := 0; i < 100 && runtime.NumGoroutine() > base+3; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > base+3 {
		t.Fatalf("goroutines %d -> %d", base, n)
	}
}

// A server that never answers the commit is cut off at DrainTimeout.
func TestSTT_DrainTimeoutForcesClose(t *testing.T) {
	f := newSTTFake(t, holdSTT)
	h := startSTT(t, f.cfg(), nil)
	h.speak()
	sttWait(t, "stream", func() bool { return h.proc.activeCount() == 1 })
	start := time.Now()
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{})
	select {
	case <-f.dropped:
	case <-time.After(2 * time.Second):
		t.Fatal("drain timeout did not close the socket")
	}
	if time.Since(start) < 250*time.Millisecond {
		t.Fatalf("closed before the drain timeout: %v", time.Since(start))
	}
	sttWait(t, "release", func() bool { return h.proc.activeCount() == 0 })
}

// A stream that never got audio has nothing to commit: closed without a commit.
func TestSTT_NoAudioNoCommit(t *testing.T) {
	var commits atomic.Int32
	f := newSTTFake(t, func(ctx context.Context, c *websocket.Conn) {
		for {
			m, ok := readChunk(ctx, c)
			if !ok {
				return
			}
			if m.Commit {
				commits.Add(1)
			}
		}
	})
	h := startSTT(t, f.cfg(), nil)
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	sttWait(t, "stream", func() bool { return h.proc.activeCount() == 1 })
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{})
	select {
	case <-f.dropped:
	case <-time.After(2 * time.Second):
		t.Fatal("not closed")
	}
	if commits.Load() != 0 {
		t.Fatal("commit without audio")
	}
}

// Audio between turns opens an unbound stream; the next turn adopts it and
// keeps its text. After the turn, later audio opens a fresh unbound stream.
func TestSTT_UnboundStreamAdoptedByNextTurn(t *testing.T) {
	var n atomic.Int32
	f := newSTTFake(t, func(ctx context.Context, c *websocket.Conn) {
		for {
			m, ok := readChunk(ctx, c)
			if !ok {
				return
			}
			if m.Commit {
				continue
			}
			if n.Add(1) == 1 {
				sttMsg(ctx, c, "committed_transcript", "hi")
			} else {
				sttMsg(ctx, c, "partial_transcript", "there")
			}
		}
	})
	h := startSTT(t, f.cfg(), nil)
	last := func() *core.TranscriptCandidateFrame {
		c := h.candidates()
		if len(c) == 0 {
			return nil
		}
		return c[len(c)-1]
	}
	h.audio(1)
	sttWait(t, "unbound stream", func() bool { return f.opens.Load() == 1 })
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	h.audio(1)
	sttWait(t, "adopted candidate", func() bool { c := last(); return c != nil && c.Text == "hi there" })
	if c := last(); c.Turn != "turn-1-1" || f.opens.Load() != 1 {
		t.Fatalf("candidate %+v opens=%d", c, f.opens.Load())
	}
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{})
	h.audio(1)
	sttWait(t, "fresh stream", func() bool { return f.opens.Load() == 2 })
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 2})
	h.audio(1)
	sttWait(t, "second turn adopts it", func() bool { c := last(); return c != nil && c.Turn == "turn-1-2" })
	if n := f.opens.Load(); n != 2 {
		t.Fatalf("the second turn opened its own stream: opens=%d", n)
	}
}

// A same-turn TurnEventStarted at a newer generation rebinds the open stream
// (no reopen, text kept) so the session fence does not drop it.
func TestSTT_SameTurnNewerGenerationRebindsStream(t *testing.T) {
	var n atomic.Int32
	f := newSTTFake(t, func(ctx context.Context, c *websocket.Conn) {
		for {
			m, ok := readChunk(ctx, c)
			if !ok {
				return
			}
			if m.Commit {
				continue
			}
			text := "stop"
			if n.Add(1) > 1 {
				text = "now"
			}
			sttMsg(ctx, c, "committed_transcript", text)
		}
	})
	h := startSTT(t, f.cfg(), nil)
	h.speak()
	sttWait(t, "first candidate", func() bool { return len(h.candidates()) == 1 })
	h.sess.IncrementGeneration()
	_ = h.sess.Post(&core.TurnEventFrame{Kind: core.TurnEventStarted, Turn: "turn-1-1", Gen: 2})
	h.audio(2)
	sttWait(t, "rebound candidate", func() bool { return len(h.candidates()) == 2 })
	if c := h.candidates()[1]; c.Text != "stop now" || c.Turn != "turn-1-1" || c.Gen != 2 || f.opens.Load() != 1 {
		t.Fatalf("candidate %+v opens=%d", c, f.opens.Load())
	}
}

// Legacy barge-in cuts the stream; the reopened one carries the new generation.
func TestSTT_InterruptionReopensAtNewGeneration(t *testing.T) {
	f := newSTTFake(t, func(ctx context.Context, c *websocket.Conn) {
		for {
			m, ok := readChunk(ctx, c)
			if !ok {
				return
			}
			if !m.Commit {
				sttMsg(ctx, c, "committed_transcript", "go on")
			}
		}
	})
	h := startSTT(t, f.cfg(), nil)
	h.speak()
	sttWait(t, "first candidate", func() bool { return len(h.candidates()) == 1 })
	h.sess.IncrementGeneration()
	_ = h.sess.Post(&core.InterruptionFrame{Gen: 2})
	h.audio(2)
	sttWait(t, "candidate after barge-in", func() bool { return len(h.candidates()) == 2 })
	if c := h.candidates()[1]; c.Gen != 2 || c.Turn != "turn-1-1" || f.opens.Load() != 2 {
		t.Fatalf("candidate %+v opens=%d", c, f.opens.Load())
	}
}

// TurnCompleted for the owning turn commits, like UserStoppedSpeaking.
func TestSTT_TurnCompletedCommits(t *testing.T) {
	f := newSTTFake(t, func(ctx context.Context, c *websocket.Conn) {
		for {
			m, ok := readChunk(ctx, c)
			if !ok {
				return
			}
			if m.Commit {
				sttMsg(ctx, c, "committed_transcript", "done")
			}
		}
	})
	h := startSTT(t, f.cfg(), nil)
	h.speak()
	sttWait(t, "stream", func() bool { return h.proc.activeCount() == 1 })
	_ = h.sess.Post(&core.TurnEventFrame{Kind: core.TurnEventCompleted, Turn: "turn-1-1", Gen: 1})
	sttWait(t, "final", func() bool { c := h.candidates(); return len(c) == 1 && c[0].EndOfUtterance })
}

// A client error (bad key) between outage failures must not reset the
// breaker's failure count: the provider is still failing for everyone else.
func TestSTT_ClientErrorDoesNotResetBreaker(t *testing.T) {
	var opens atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if opens.Add(1) == 2 {
			http.Error(w, "bad key", http.StatusUnauthorized)
			return
		}
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer srv.Close()
	cfg := DefaultSTTConfig()
	cfg.Endpoint, cfg.APIKey = "ws"+strings.TrimPrefix(srv.URL, "http"), sttKey
	cfg.RetryPolicy = resilience.Policy{MaxAttempts: 1}
	cfg.CircuitBreaker = resilience.NewBreaker(resilience.Config{FailureThreshold: 2, Cooldown: time.Hour})
	h := startSTT(t, cfg, nil)
	for i := 0; i < 3; i++ {
		_ = h.proc.ensureStream(context.Background())
	}
	if cfg.CircuitBreaker.Allow() {
		t.Fatal("502, 401, 502: the two outage failures must open the breaker")
	}
}

// insufficient_audio_activity is our silence: the following policy close is
// neither an error frame nor a breaker failure.
func TestSTT_InsufficientAudioActivityIsQuiet(t *testing.T) {
	f := newSTTFake(t, func(ctx context.Context, c *websocket.Conn) {
		_, _ = readChunk(ctx, c)
		sttErr(ctx, c, "insufficient_audio_activity", "not enough audio")
		_ = c.Close(websocket.StatusPolicyViolation, "insufficient audio activity")
	})
	cfg := f.cfg()
	cfg.CircuitBreaker = sttBreaker()
	h := startSTT(t, cfg, nil)
	h.audio(1)
	sttWait(t, "release", func() bool { return f.opens.Load() == 1 && h.proc.activeCount() == 0 })
	time.Sleep(20 * time.Millisecond)
	if len(h.errors()) != 0 || !cfg.CircuitBreaker.Allow() {
		t.Fatalf("reported as failure: %v", h.errors())
	}
}

// An open cancelled while it holds the half-open probe releases the probe:
// the breaker is not wedged, and the cancel is not counted as a failure.
func TestSTT_CancelledOpenDoesNotWedgeBreaker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer srv.Close()
	cfg := DefaultSTTConfig()
	cfg.Endpoint, cfg.APIKey = "ws"+strings.TrimPrefix(srv.URL, "http"), sttKey
	cfg.CircuitBreaker = resilience.NewBreaker(resilience.Config{FailureThreshold: 1, Cooldown: 10 * time.Millisecond})
	cfg.CircuitBreaker.RecordFailure() // open
	time.Sleep(20 * time.Millisecond)  // cooled down: the next open is the half-open probe
	h := startSTT(t, cfg, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	ctx, cancelNow := context.WithCancel(ctx)
	time.AfterFunc(30*time.Millisecond, cancelNow)
	if err := h.proc.ensureStream(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	if !cfg.CircuitBreaker.Allow() {
		t.Fatal("cancelled probe wedged the breaker")
	}
}

// A8: a clean close while the turn still owes its transcript invalidates it.
func TestSTT_CleanCloseMidUtteranceInvalidatesTurn(t *testing.T) {
	f := newSTTFake(t, func(ctx context.Context, c *websocket.Conn) {
		_, _ = readChunk(ctx, c)
		_ = c.Close(websocket.StatusNormalClosure, "")
	})
	cfg := f.cfg()
	cfg.CircuitBreaker = sttBreaker()
	h := startSTT(t, cfg, nil)
	h.speak()
	sttWait(t, "invalidating error", func() bool {
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

// The same provider idle-close that is quiet for an unbound stream (see
// TestSTT_InsufficientAudioActivityIsQuiet) loses a bound, unfinished turn.
func TestSTT_IdleCloseOfBoundUnfinishedTurnInvalidates(t *testing.T) {
	f := newSTTFake(t, func(ctx context.Context, c *websocket.Conn) {
		_, _ = readChunk(ctx, c)
		sttErr(ctx, c, "insufficient_audio_activity", "not enough audio")
		_ = c.Close(websocket.StatusPolicyViolation, "insufficient audio activity")
	})
	cfg := f.cfg()
	cfg.CircuitBreaker = sttBreaker()
	h := startSTT(t, cfg, nil)
	h.speak()
	sttWait(t, "invalidating error", func() bool {
		for _, err := range h.errors() {
			if errors.Is(err, core.ErrTranscriptInvalidated) {
				if !strings.Contains(err.Error(), "insufficient_audio_activity") {
					t.Errorf("the provider's reason is missing from %v", err)
				}
				return true
			}
		}
		return false
	})
	if !cfg.CircuitBreaker.Allow() {
		t.Fatal("an idle close is not an outage")
	}
}

// Deterministic pin of the unbound-resurface contract: text committed while no
// turn was bound (the runner drops those results) is in the next result's
// cumulative Text; a duplicate commit form is swallowed; an empty commit after
// our commit settles the turn with the text so far as the ending Final.
func TestSTTConn_ResurfacesUnboundTextAndDedupsCommitForms(t *testing.T) {
	c := &sttConn{}
	_, _ = c.transcript(&sttMessage{Type: "committed_transcript", Text: "hi"}) // dropped by the runner
	got, done := c.transcript(&sttMessage{Type: "partial_transcript", Text: "there"})
	if want := (sttunit.Result{Text: "hi there"}); got != want || done {
		t.Fatalf("partial: %+v done=%v, want %+v", got, done, want)
	}
	got, done = c.transcript(&sttMessage{Type: "committed_transcript", Text: "there"})
	if want := (sttunit.Result{Text: "hi there", Final: true}); got != want || done {
		t.Fatalf("commit: %+v done=%v, want %+v", got, done, want)
	}
	if got, _ := c.transcript(&sttMessage{Type: "committed_transcript_with_timestamps", Text: "there"}); got != (sttunit.Result{}) {
		t.Fatalf("duplicate form not swallowed: %+v", got)
	}
	c.commitSent.Store(true)
	got, done = c.transcript(&sttMessage{Type: "committed_transcript", Text: ""})
	if want := (sttunit.Result{Text: "hi there", Final: true, EndOfUtterance: true}); got != want || !done {
		t.Fatalf("end: %+v done=%v, want %+v", got, done, want)
	}
}

// Nothing heard at all: the commit we asked for is answered with an empty
// Final that ends the utterance (the runner settles the turn on it).
func TestSTTConn_EmptyFinalAfterOurCommitEndsTheUtterance(t *testing.T) {
	c := &sttConn{}
	c.commitSent.Store(true)
	got, done := c.transcript(&sttMessage{Type: "committed_transcript", Text: ""})
	if want := (sttunit.Result{Final: true, EndOfUtterance: true}); got != want || !done {
		t.Fatalf("empty final: %+v done=%v, want %+v", got, done, want)
	}
}
