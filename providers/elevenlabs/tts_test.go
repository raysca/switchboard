package elevenlabs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"govoice/core"
	"govoice/core/coretest"
	"govoice/core/resilience"
)

type fake struct {
	*httptest.Server
	opens     atomic.Int32
	msgs      chan []clientMsg // BOS, text, EOS
	headers   chan http.Header
	urls      chan *url.URL
	closed    chan struct{} // one signal per finished socket handler
	status    atomic.Int32  // when set, reject the upgrade with this status...
	failFirst atomic.Int32  // ...for the first failFirst connections
}

func newFake(t *testing.T, handle func(ctx context.Context, c *websocket.Conn)) *fake {
	f := &fake{msgs: make(chan []clientMsg, 8), headers: make(chan http.Header, 8), urls: make(chan *url.URL, 8), closed: make(chan struct{}, 8)}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := f.opens.Add(1)
		f.headers <- r.Header.Clone()
		f.urls <- r.URL
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
		var got []clientMsg
		for i := 0; i < 3; i++ {
			_, data, err := c.Read(r.Context())
			if err != nil {
				return
			}
			var m clientMsg
			_ = json.Unmarshal(data, &m)
			got = append(got, m)
		}
		f.msgs <- got
		handle(r.Context(), c)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fake) cfg() TTSConfig {
	cfg := DefaultTTSConfig()
	cfg.Endpoint = "ws" + strings.TrimPrefix(f.URL, "http")
	cfg.APIKey = "xi-test-key-0123456789"
	cfg.Timeout = 2 * time.Second
	cfg.RetryPolicy = resilience.Policy{MaxAttempts: 3, InitialBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond}
	return cfg
}

func send(ctx context.Context, c *websocket.Conn, v any) {
	b, _ := json.Marshal(v)
	_ = c.Write(ctx, websocket.MessageText, b)
}

func pcm(n int) string { return base64.StdEncoding.EncodeToString(make([]byte, n)) }

// audioMsg is one server message: audio plus alignment of text (1 ms per char
// slot starting at startMs, 2 ms long each), chunk-relative like the real API.
func audioMsg(n int, text string, startMs float64) map[string]any {
	m := map[string]any{"audio": pcm(n)}
	if text != "" {
		var chars []string
		var st, du []float64
		for i, r := range []rune(text) {
			chars = append(chars, string(r))
			st = append(st, startMs+float64(i)*2)
			du = append(du, 2)
		}
		m["alignment"] = map[string]any{"chars": chars, "charStartTimesMs": st, "charDurationsMs": du}
		// Normalized text differs from what the LLM wrote; it must not be used.
		upper := make([]string, len(chars))
		for i, c := range chars {
			upper[i] = strings.ToUpper(c)
		}
		m["normalizedAlignment"] = map[string]any{"chars": upper, "charStartTimesMs": st, "charDurationsMs": du}
	}
	return m
}

var final = map[string]any{"isFinal": true}

type harness struct {
	proc *TTSProcessor
	sess *core.Session
	sink *coretest.PassthroughProcessor
}

func start(t *testing.T, cfg TTSConfig, tracer ...core.Tracer) *harness {
	t.Helper()
	proc, err := NewTTSProcessor(cfg)
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

// Odd-sized chunks come out as 20 ms frames; the last is final; characters are
// grouped into words, offset by earlier audio (a word may span messages);
// protocol messages, URL and auth header are right.
func TestTTS_StreamsFramesWithWordTimings(t *testing.T) {
	f := newFake(t, func(ctx context.Context, c *websocket.Conn) {
		send(ctx, c, audioMsg(1500, "Hello th", 0))
		send(ctx, c, audioMsg(1500, "ere ", 0))
		send(ctx, c, final)
	})
	cfg := f.cfg()
	st := 0.4
	cfg.Stability = &st
	h := start(t, cfg)
	h.say(t, "Hello there", true)
	waitFor(t, 3*time.Second, "stop", func() bool { return h.stopped() == 1 })

	msgs, hdr, u := <-f.msgs, <-f.headers, <-f.urls
	if msgs[0].Text != " " || msgs[0].VoiceSettings["stability"] != 0.4 || msgs[1].Text != "Hello there " || !msgs[1].Flush || msgs[0].ContextID == "" || msgs[0].ContextID != msgs[1].ContextID {
		t.Fatalf("messages: %+v", msgs)
	}
	q := u.Query()
	if !strings.HasSuffix(u.Path, "/v1/text-to-speech/"+defaultVoice+"/multi-stream-input") || q.Get("model_id") != "eleven_flash_v2_5" || q.Get("output_format") != "pcm_24000" || q.Get("sync_alignment") != "true" {
		t.Fatalf("url: %v", u)
	}
	if hdr.Get("xi-api-key") != cfg.APIKey || strings.Contains(u.String(), cfg.APIKey) {
		t.Fatalf("auth header %v, url %v", hdr, u)
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
	// Second message starts after 1500 bytes = 31.25 ms of audio.
	off := 31250 * time.Microsecond
	wt := h.timings()
	if len(wt) != 2 || wt[0].Word != "Hello" || wt[0].Start != 0 || wt[0].End != 10*time.Millisecond ||
		wt[1].Word != "there" || wt[1].Start != 12*time.Millisecond || wt[1].End != off+6*time.Millisecond {
		t.Fatalf("timings: %+v", wt)
	}
	if len(frames[*core.TTSStartedFrame](h)) != 1 {
		t.Fatal("want one TTSStarted")
	}
	u2 := frames[*core.UsageMetricsFrame](h)
	if len(u2) != 1 || u2[0].Provider != "elevenlabs" || *u2[0].Characters != 11 || *u2[0].AudioDurationMs != 62 {
		t.Fatalf("usage: %+v", u2)
	}
	if len(h.errors()) != 0 {
		t.Fatalf("errors: %v", h.errors())
	}
}

// Unsupported sample rates are a config error at construction.
func TestNewTTSProcessor_RejectsBadSampleRate(t *testing.T) {
	cfg := DefaultTTSConfig()
	cfg.APIKey, cfg.SampleRate = "k", 12345
	if _, err := NewTTSProcessor(cfg); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("err=%v", err)
	}
	cfg.SampleRate = 16000
	if _, err := NewTTSProcessor(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestTTS_RetriesBeforeAudio(t *testing.T) {
	f := newFake(t, func(ctx context.Context, c *websocket.Conn) {
		send(ctx, c, audioMsg(400, "", 0))
		send(ctx, c, final)
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

func TestTTS_NoRetryAfterAudio(t *testing.T) {
	f := newFake(t, func(ctx context.Context, c *websocket.Conn) {
		send(ctx, c, audioMsg(3000, "", 0))
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

// In-band input errors and 1008 closes are the caller's fault: no retry, breaker stays closed.
func TestTTS_ClientErrorNotRetriedNorCounted(t *testing.T) {
	cases := map[string]func(ctx context.Context, c *websocket.Conn){
		"inband": func(ctx context.Context, c *websocket.Conn) {
			send(ctx, c, map[string]any{"message": "invalid voice, xi-api-key=abcdefgh12345678", "error": "input_error", "code": 1008})
		},
		"close1008": func(ctx context.Context, c *websocket.Conn) { _ = c.Close(websocket.StatusPolicyViolation, "bad") },
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFake(t, h)
			cfg := f.cfg()
			cfg.CircuitBreaker = resilience.NewBreaker(resilience.Config{FailureThreshold: 1, Cooldown: time.Hour})
			hn := start(t, cfg)
			hn.say(t, "hello", true)
			waitFor(t, 3*time.Second, "error", func() bool { return len(hn.errors()) == 1 })
			var pe *ProviderError
			if !errors.As(hn.errors()[0], &pe) || !pe.ClientError || pe.Retryable || strings.Contains(pe.Message, "abcdefgh12345678") {
				t.Fatalf("error: %v", hn.errors()[0])
			}
			if f.opens.Load() != 1 || !cfg.CircuitBreaker.Allow() {
				t.Fatalf("opens=%d, breaker must stay closed", f.opens.Load())
			}
		})
	}
}

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

func TestTTS_InterruptionCancelsStream(t *testing.T) {
	f := newFake(t, func(ctx context.Context, c *websocket.Conn) {
		send(ctx, c, audioMsg(3000, "", 0))
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

// Two units in flight finish in arrival order.
func TestTTS_UnitsPlayInOrder(t *testing.T) {
	var n atomic.Int32
	f := newFake(t, func(ctx context.Context, c *websocket.Conn) {
		if n.Add(1) == 1 {
			time.Sleep(60 * time.Millisecond) // first unit is slower
		}
		send(ctx, c, audioMsg(400, "", 0))
		send(ctx, c, final)
	})
	h := start(t, f.cfg())
	for i := uint64(1); i <= 2; i++ {
		if err := h.sess.Post(&core.SynthesisUnitFrame{Turn: "t1", Gen: 1, UnitID: i, Text: "x", IsFinal: i == 2}); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, 3*time.Second, "stops", func() bool { return h.stopped() == 2 })
	a := h.audio()
	if len(a) != 2 || a[0].UnitID != 1 || a[1].UnitID != 2 {
		t.Fatalf("order: %+v", a)
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
	f := newFake(t, func(ctx context.Context, c *websocket.Conn) {
		send(ctx, c, audioMsg(400, "", 0))
		send(ctx, c, final)
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

// Load errors retry even when sent as 1008; bad voice/key/quota do not.
func TestMessageErrorClassification(t *testing.T) {
	for _, c := range []struct {
		kind, msg string
		code      int
		retry     bool
	}{
		{"too_many_concurrent_requests", "Too many concurrent requests", 1008, true},
		{"", "voice service is busy, try again", 0, true},
		{"", "voice generation failed", 0, true},
		{"voice_not_found", "A voice with that id was not found", 1008, false},
		{"quota_exceeded", "insufficient quota", 0, false},
		{"invalid_api_key", "bad key", 0, false},
		{"input_error", "something", 1008, false},
	} {
		if pe := messageError(c.kind, c.msg, c.code); pe.Retryable != c.retry || pe.ClientError == c.retry {
			t.Errorf("%s/%s: %+v", c.kind, c.msg, pe)
		}
	}
}
