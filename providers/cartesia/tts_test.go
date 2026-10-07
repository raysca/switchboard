package cartesia

import (
	"context"
	"encoding/base64"
	"encoding/json"
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

type fake struct {
	*httptest.Server
	opens     atomic.Int32
	reqs      chan request
	headers   chan http.Header
	closed    chan struct{} // one signal per finished socket handler
	status    atomic.Int32  // when set, reject the upgrade with this status...
	failFirst atomic.Int32  // ...for the first failFirst connections
}

func newFake(t *testing.T, handle func(ctx context.Context, c *websocket.Conn, req request)) *fake {
	f := &fake{reqs: make(chan request, 8), headers: make(chan http.Header, 8), closed: make(chan struct{}, 8)}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := f.opens.Add(1)
		f.headers <- r.Header.Clone()
		if s := int(f.status.Load()); s != 0 && n <= f.failFirst.Load() {
			w.WriteHeader(s)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		defer func() { f.closed <- struct{}{} }()
		_, data, err := c.Read(r.Context())
		if err != nil {
			return
		}
		var req request
		_ = json.Unmarshal(data, &req)
		f.reqs <- req
		handle(r.Context(), c, req)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fake) cfg() TTSConfig {
	cfg := DefaultTTSConfig()
	cfg.Endpoint = "ws" + strings.TrimPrefix(f.URL, "http")
	cfg.APIKey = "cartesia-test-key-0123456789"
	cfg.Timeout = 2 * time.Second
	cfg.RetryPolicy = resilience.Policy{MaxAttempts: 3, InitialBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond}
	return cfg
}

func send(ctx context.Context, c *websocket.Conn, v any) {
	b, _ := json.Marshal(v)
	_ = c.Write(ctx, websocket.MessageText, b)
}

func pcm(n int) string { return base64.StdEncoding.EncodeToString(make([]byte, n)) }

func chunk(n int) map[string]any { return map[string]any{"type": "chunk", "data": pcm(n)} }

func timestamps(words []string, start, end []float64) map[string]any {
	return map[string]any{"type": "timestamps", "word_timestamps": map[string]any{"words": words, "start": start, "end": end}}
}

type harness struct {
	proc *TTSProcessor
	sess *core.Session
	sink *core.PassthroughProcessor
}

func start(t *testing.T, cfg TTSConfig, tracer ...core.Tracer) *harness {
	t.Helper()
	proc, err := NewTTSProcessor(cfg)
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
	if len(tracer) > 0 {
		if err := sess.SetTracer(tracer[0], core.TracePolicy{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := sess.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	return &harness{proc, sess, sink}
}

func (h *harness) say(t *testing.T, text string, final bool) {
	t.Helper()
	if err := h.sess.Post(&core.SynthesisUnitFrame{Turn: "t1", Gen: 1, UnitID: 1, Text: text, IsFinal: final}); err != nil {
		t.Fatal(err)
	}
}

func frames[T core.Frame](h *harness) (out []T) {
	for _, f := range h.sink.GetFrames() {
		if v, ok := f.(T); ok {
			out = append(out, v)
		}
	}
	return
}

func (h *harness) audio() []*core.OutputAudioFrame { return frames[*core.OutputAudioFrame](h) }
func (h *harness) stopped() int                    { return len(frames[*core.TTSStoppedFrame](h)) }
func (h *harness) timings() (out []core.WordTiming) {
	for _, a := range h.audio() {
		out = append(out, a.WordTimings...)
	}
	return
}
func (h *harness) errors() (out []error) {
	for _, e := range frames[*core.ErrorFrame](h) {
		out = append(out, e.Err)
	}
	return
}

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	until := time.Now().Add(d)
	for !cond() {
		if time.Now().After(until) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

const chunk20ms = 960 // 24 kHz mono s16le

// Streamed chunks of odd sizes come out as 20 ms frames; the last is final;
// word timings land on frames in unit-relative time; request and headers are right.
func TestTTS_StreamsFramesWithWordTimings(t *testing.T) {
	f := newFake(t, func(ctx context.Context, c *websocket.Conn, _ request) {
		send(ctx, c, timestamps([]string{"Hello"}, []float64{0}, []float64{0.04}))
		send(ctx, c, chunk(1500))
		send(ctx, c, timestamps([]string{"there"}, []float64{0.05}, []float64{0.1}))
		send(ctx, c, chunk(1500))
		send(ctx, c, map[string]any{"type": "done", "done": true})
	})
	h := start(t, f.cfg())
	h.say(t, "Hello there", true)
	waitFor(t, 3*time.Second, "stop", func() bool { return h.stopped() == 1 })

	req, hdr := <-f.reqs, <-f.headers
	if req.Output.Encoding != "pcm_s16le" || req.Output.Container != "raw" || req.Output.SampleRate != 24000 || !req.Timestamps || req.Transcript != "Hello there" || req.Voice["id"] == "" {
		t.Fatalf("bad request: %+v", req)
	}
	if hdr.Get("X-API-Key") == "" || hdr.Get("Cartesia-Version") == "" {
		t.Fatalf("headers: %v", hdr)
	}

	a := h.audio()
	total := 0
	for i, fr := range a {
		total += len(fr.Samples)
		last := i == len(a)-1
		if fr.IsFinal != last || fr.TurnFinal != last || fr.SampleRate != 24000 || fr.Channels != 1 || fr.Seq != uint64(i+1) {
			t.Fatalf("frame %d: %+v", i, fr)
		}
		if !last && len(fr.Samples) != chunk20ms {
			t.Fatalf("frame %d is %d bytes", i, len(fr.Samples))
		}
	}
	if total != 3000 {
		t.Fatalf("audio bytes %d", total)
	}
	if len(a[0].WordTimings) != 1 || a[0].WordTimings[0].Word != "Hello" { // reported before the first chunk
		t.Fatalf("first frame timings: %+v", a[0].WordTimings)
	}
	wt := h.timings()
	if len(wt) != 2 || wt[0].Word != "Hello" || wt[1].Word != "there" || wt[1].Start != 50*time.Millisecond || wt[1].End != 100*time.Millisecond {
		t.Fatalf("timings: %+v", wt)
	}
	if len(frames[*core.TTSStartedFrame](h)) != 1 {
		t.Fatal("want one TTSStarted")
	}
	u := frames[*core.UsageMetricsFrame](h)
	if len(u) != 1 || u[0].Provider != "cartesia" || *u[0].Characters != 11 || *u[0].AudioDurationMs != 62 {
		t.Fatalf("usage: %+v", u)
	}
	if len(h.errors()) != 0 {
		t.Fatalf("errors: %v", h.errors())
	}
}

// A refused upgrade before any audio is retried and the unit still plays.
func TestTTS_RetriesBeforeAudio(t *testing.T) {
	f := newFake(t, func(ctx context.Context, c *websocket.Conn, _ request) {
		send(ctx, c, chunk(400))
		send(ctx, c, map[string]any{"type": "done"})
	})
	f.status.Store(503)
	f.failFirst.Store(1)
	h := start(t, f.cfg())
	h.say(t, "hi", false)
	waitFor(t, 3*time.Second, "stop", func() bool { return h.stopped() == 1 })
	if f.opens.Load() < 2 || len(h.errors()) != 0 || h.proc.RetriesTotal.Load() == 0 {
		t.Fatalf("opens=%d errs=%v", f.opens.Load(), h.errors())
	}
}

// Once audio has played, a dropped socket is not retried (no replay): the unit
// is closed out with a final frame and the error is reported.
func TestTTS_NoRetryAfterAudio(t *testing.T) {
	f := newFake(t, func(ctx context.Context, c *websocket.Conn, _ request) {
		send(ctx, c, chunk(3000))
		_ = c.Close(websocket.StatusInternalError, "boom")
	})
	h := start(t, f.cfg())
	h.say(t, "hello", true)
	waitFor(t, 3*time.Second, "error", func() bool { return len(h.errors()) == 1 && h.stopped() == 1 })
	if f.opens.Load() != 1 {
		t.Fatalf("opens=%d", f.opens.Load())
	}
	a := h.audio()
	if len(a) == 0 || !a[len(a)-1].IsFinal {
		t.Fatalf("unit not closed out: %d frames", len(a))
	}
}

// An in-band 4xx error is the caller's fault: no retry, breaker stays closed.
// Legacy versions put the text in "error", 2026-03-01+ in "message".
func TestTTS_ClientErrorNotRetriedNorCounted(t *testing.T) {
	for _, field := range []string{"error", "message"} {
		t.Run(field, func(t *testing.T) { testClientError(t, field) })
	}
}

func testClientError(t *testing.T, field string) {
	f := newFake(t, func(ctx context.Context, c *websocket.Conn, _ request) {
		send(ctx, c, map[string]any{"type": "error", field: "voice not found, api_key=abcdefgh12345678", "status_code": 400})
	})
	cfg := f.cfg()
	cfg.CircuitBreaker = resilience.NewBreaker(resilience.Config{FailureThreshold: 1, Cooldown: time.Hour})
	h := start(t, cfg)
	h.say(t, "hello", true)
	waitFor(t, 3*time.Second, "error", func() bool { return len(h.errors()) == 1 })
	var pe *ProviderError
	if !errors.As(h.errors()[0], &pe) || !pe.ClientError || pe.Retryable || !strings.Contains(pe.Message, "voice not found") || strings.Contains(pe.Message, "abcdefgh12345678") {
		t.Fatalf("error: %v", h.errors()[0])
	}
	if f.opens.Load() != 1 || !cfg.CircuitBreaker.Allow() {
		t.Fatalf("opens=%d, breaker must stay closed", f.opens.Load())
	}
}

// Server outages open the shared breaker; later units are refused fast.
func TestTTS_OutageOpensBreaker(t *testing.T) {
	f := newFake(t, nil)
	f.status.Store(500)
	f.failFirst.Store(100)
	cfg := f.cfg()
	cfg.RetryPolicy.MaxAttempts = 1
	cfg.CircuitBreaker = resilience.NewBreaker(resilience.Config{FailureThreshold: 1, Cooldown: time.Hour})
	h := start(t, cfg)
	h.say(t, "hello", true)
	waitFor(t, 3*time.Second, "error", func() bool { return len(h.errors()) == 1 })
	if cfg.CircuitBreaker.Allow() {
		t.Fatal("breaker should be open")
	}
}

// Barge-in closes the socket and the rest of the unit is never emitted.
func TestTTS_InterruptionCancelsStream(t *testing.T) {
	f := newFake(t, func(ctx context.Context, c *websocket.Conn, _ request) {
		send(ctx, c, chunk(3000))
		_, _, _ = c.Read(ctx) // returns when the client hangs up
	})
	h := start(t, f.cfg())
	h.say(t, "a long sentence", true)
	waitFor(t, 3*time.Second, "audio", func() bool { return len(h.audio()) > 0 })
	_ = h.sess.Post(&core.InterruptionFrame{})
	select {
	case <-f.closed:
	case <-time.After(3 * time.Second):
		t.Fatal("socket not closed on interruption")
	}
	time.Sleep(30 * time.Millisecond)
	if h.stopped() != 0 || len(h.errors()) != 0 {
		t.Fatal("interrupted unit must not complete or error")
	}
}

func TestTTS_EmptyTextKeepsDrainBoundary(t *testing.T) {
	f := newFake(t, nil)
	h := start(t, f.cfg())
	h.say(t, "  ", true)
	waitFor(t, 3*time.Second, "stop", func() bool { return h.stopped() == 1 })
	a := h.audio()
	if len(a) != 1 || !a[0].IsFinal || !a[0].TurnFinal || f.opens.Load() != 0 {
		t.Fatalf("frames=%+v opens=%d", a, f.opens.Load())
	}
}

func TestNewTTSProcessor_RequiresKey(t *testing.T) {
	if _, err := NewTTSProcessor(DefaultTTSConfig()); !errors.Is(err, ErrMissingAPIKey) {
		t.Fatalf("err=%v", err)
	}
}

type spanRecorder struct {
	mu   sync.Mutex
	ends map[string]int
}
type recSpan struct {
	r    *spanRecorder
	name string
}

func (r *spanRecorder) StartSpan(_ context.Context, ev core.TraceEvent) core.Span {
	return recSpan{r, ev.Name}
}
func (recSpan) SetAttribute(string, any) {}
func (recSpan) AddEvent(core.TraceEvent) {}
func (s recSpan) End(error) {
	s.r.mu.Lock()
	defer s.r.mu.Unlock()
	if s.r.ends == nil {
		s.r.ends = map[string]int{}
	}
	s.r.ends[s.name]++
}
func (r *spanRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ends["tts.request"]
}

func TestTTS_TraceSpanEndsOnce(t *testing.T) {
	f := newFake(t, func(ctx context.Context, c *websocket.Conn, _ request) {
		send(ctx, c, chunk(400))
		send(ctx, c, map[string]any{"type": "done"})
	})
	rec := &spanRecorder{}
	h := start(t, f.cfg(), rec)
	h.say(t, "hi", true)
	waitFor(t, 3*time.Second, "span", func() bool { return rec.count() == 1 })
	time.Sleep(20 * time.Millisecond)
	if rec.count() != 1 {
		t.Fatalf("span ended %d times", rec.count())
	}
}
