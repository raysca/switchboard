package nvidia

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"govoice/core"
	"govoice/core/coretest"
	"govoice/core/resilience"
	"govoice/providers/nvidia/rivapb"
)

// ttsFake is an in-process Riva TTS server on a bufconn listener.
type ttsFake struct {
	rivapb.UnimplementedRivaSpeechSynthesisServer
	handle func(n int32, ctx context.Context, s rivapb.RivaSpeechSynthesis_SynthesizeOnlineServer) error
	opens  atomic.Int32
	reqs   chan *rivapb.SynthesizeSpeechRequest
	md     chan metadata.MD
	done   chan struct{} // one send per finished handler
}

func (f *ttsFake) SynthesizeOnline(s rivapb.RivaSpeechSynthesis_SynthesizeOnlineServer) error {
	n := f.opens.Add(1)
	defer func() { f.done <- struct{}{} }()
	md, _ := metadata.FromIncomingContext(s.Context())
	f.md <- md
	req, err := s.Recv()
	if err != nil {
		return err
	}
	f.reqs <- req
	return f.handle(n, s.Context(), s)
}

func newTTSFake(t *testing.T, handle func(n int32, ctx context.Context, s rivapb.RivaSpeechSynthesis_SynthesizeOnlineServer) error) (*ttsFake, *grpc.ClientConn) {
	t.Helper()
	f := &ttsFake{handle: handle, reqs: make(chan *rivapb.SynthesizeSpeechRequest, 16), md: make(chan metadata.MD, 16), done: make(chan struct{}, 16)}
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	rivapb.RegisterRivaSpeechSynthesisServer(srv, f)
	go srv.Serve(lis)
	cc, err := grpc.NewClient("passthrough:///bufnet", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cc.Close(); srv.Stop() })
	return f, cc
}

func ttsCfg(cc *grpc.ClientConn) TTSConfig {
	c := DefaultTTSConfig()
	c.Conn, c.APIKey, c.FunctionID = cc, "nvapi-test-key-0123456789", "fn-123"
	c.Timeout = 2 * time.Second
	c.RetryPolicy = resilience.Policy{MaxAttempts: 3, InitialBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond}
	return c
}

func audio(b []byte) *rivapb.SynthesizeSpeechResponse {
	return &rivapb.SynthesizeSpeechResponse{Audio: b}
}

// tone is n samples of a sine at freq Hz sampled at rate, as s16le.
func tone(freq float64, rate, n int) []byte {
	b := make([]byte, 2*n)
	for i := 0; i < n; i++ {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(int16(12000*math.Sin(2*math.Pi*freq*float64(i)/float64(rate)))))
	}
	return b
}

type ttsHarness struct {
	proc *TTSProcessor
	sess *core.Session
	sink *coretest.PassthroughProcessor
}

func startTTS(t *testing.T, cfg TTSConfig) *ttsHarness {
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
	if err := sess.Start(); err != nil {
		t.Fatal(err)
	}
	h := &ttsHarness{proc, sess, sink}
	t.Cleanup(func() {
		sess.Close()
		if n := sess.LeakedTasks(); n != 0 {
			t.Errorf("leaked %d tasks", n)
		}
	})
	return h
}

func (h *ttsHarness) say(t *testing.T, text string, final bool) {
	t.Helper()
	if err := h.sess.Post(&core.SynthesisUnitFrame{Turn: "t1", Gen: 1, UnitID: 7, Text: text, IsFinal: final}); err != nil {
		t.Fatal(err)
	}
}

func framesOf[T core.Frame](h *ttsHarness) (out []T) {
	for _, f := range h.sink.GetFrames() {
		if v, ok := f.(T); ok {
			out = append(out, v)
		}
	}
	return
}

func (h *ttsHarness) audio() []*core.OutputAudioFrame { return framesOf[*core.OutputAudioFrame](h) }
func (h *ttsHarness) stopped() int                    { return len(framesOf[*core.TTSStoppedFrame](h)) }
func (h *ttsHarness) errs() (out []*core.ErrorFrame)  { return framesOf[*core.ErrorFrame](h) }

func waitTTS(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for until := time.Now().Add(3 * time.Second); !cond(); time.Sleep(2 * time.Millisecond) {
		if time.Now().After(until) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

const frame24k = 960 // 20 ms mono s16le at 24 kHz

// With RivaRate overridden to 22050, odd-sized Riva messages (one splitting a
// sample) are resampled 22.05 -> 24 kHz and come out as whole 20 ms frames; the
// last is final; request and auth are right.
func TestTTS_StreamsFramesAfterResample(t *testing.T) {
	src := tone(440, 22050, 22050/2) // 0.5 s
	f, cc := newTTSFake(t, func(_ int32, _ context.Context, s rivapb.RivaSpeechSynthesis_SynthesizeOnlineServer) error {
		for _, c := range [][]byte{src[:1001], src[1001:7003], src[7003:]} { // 1001 and 7003 split a sample
			if err := s.Send(audio(c)); err != nil {
				return err
			}
		}
		return nil
	})
	cfg := ttsCfg(cc)
	cfg.RivaRate = 22050
	h := startTTS(t, cfg)
	h.say(t, "Hello there", true)
	waitTTS(t, "stop", func() bool { return h.stopped() == 1 })

	req, md := <-f.reqs, <-f.md
	if req.GetText() != "Hello there" || req.GetEncoding() != rivapb.AudioEncoding_LINEAR_PCM || req.GetSampleRateHz() != 22050 ||
		req.GetLanguageCode() != "en-US" || req.GetVoiceName() == "" {
		t.Fatalf("bad request: %v", req)
	}
	if md.Get("function-id")[0] != "fn-123" || md.Get("authorization")[0] != "Bearer nvapi-test-key-0123456789" {
		t.Fatalf("metadata: %v", md)
	}
	a := h.audio()
	if len(a) < 20 {
		t.Fatalf("frames=%d", len(a))
	}
	total := 0
	for i, fr := range a {
		total += len(fr.Samples)
		last := i == len(a)-1
		if fr.SampleRate != 24000 || fr.Channels != 1 || fr.UnitID != 7 || fr.IsFinal != last || fr.TurnFinal != last || len(fr.WordTimings) != 0 {
			t.Fatalf("frame %d: %+v", i, fr)
		}
		if !last && len(fr.Samples) != frame24k {
			t.Fatalf("frame %d is %d bytes, want %d", i, len(fr.Samples), frame24k)
		}
	}
	if want := 22050 / 2 * 24000 / 22050 * 2; total < want-8 || total > want+8 { // resampler holds back at most a sample
		t.Fatalf("total=%d want ~%d", total, want)
	}
	if len(framesOf[*core.TTSStartedFrame](h)) != 1 || len(framesOf[*core.UsageMetricsFrame](h)) != 1 {
		t.Fatal("want one TTSStarted and one usage frame")
	}
	if req, errs, retries, _ := h.proc.TTSCounts(); req != 1 || errs != 0 || retries != 0 {
		t.Fatalf("counts %d %d %d", req, errs, retries)
	}
}

// A 1 kHz sine keeps its frequency whichever native rate Riva is asked for.
func TestTTS_ResampleKeepsFrequency(t *testing.T) {
	for _, rivaRate := range []int{22050, 44100} {
		src := tone(1000, rivaRate, rivaRate) // 1 s
		_, cc := newTTSFake(t, func(_ int32, _ context.Context, s rivapb.RivaSpeechSynthesis_SynthesizeOnlineServer) error {
			for off := 0; off < len(src); off += 4096 {
				if err := s.Send(audio(src[off:min(off+4096, len(src))])); err != nil {
					return err
				}
			}
			return nil
		})
		cfg := ttsCfg(cc)
		cfg.RivaRate = rivaRate
		h := startTTS(t, cfg)
		h.say(t, "tone", true)
		waitTTS(t, "stop", func() bool { return h.stopped() == 1 })
		var pcm []byte
		for _, fr := range h.audio() {
			pcm = append(pcm, fr.Samples...)
		}
		n := len(pcm) / 2
		cross := 0
		prev := int16(binary.LittleEndian.Uint16(pcm))
		for i := 1; i < n; i++ {
			s := int16(binary.LittleEndian.Uint16(pcm[2*i:]))
			if prev < 0 && s >= 0 {
				cross++
			}
			prev = s
		}
		if hz := float64(cross) * 24000 / float64(n); math.Abs(hz-1000) > 15 {
			t.Fatalf("rate %d: output tone %.1f Hz, want 1000", rivaRate, hz)
		}
		if secs := float64(n) / 24000; math.Abs(secs-1) > 0.01 {
			t.Fatalf("rate %d: duration %.3f s", rivaRate, secs)
		}
	}
}

// By default the pipeline rate is requested from Riva (it resamples server
// side): odd-split bytes pass through unchanged.
func TestTTS_PipelineRateRequested(t *testing.T) {
	src := tone(500, 24000, 4800) // 0.2 s
	f, cc := newTTSFake(t, func(_ int32, _ context.Context, s rivapb.RivaSpeechSynthesis_SynthesizeOnlineServer) error {
		if err := s.Send(audio(src[:333])); err != nil {
			return err
		}
		return s.Send(audio(src[333:]))
	})
	h := startTTS(t, ttsCfg(cc))
	h.say(t, "native", true)
	waitTTS(t, "stop", func() bool { return h.stopped() == 1 })
	var got []byte
	for _, fr := range h.audio() {
		got = append(got, fr.Samples...)
	}
	if string(got) != string(src) || (<-f.reqs).GetSampleRateHz() != 24000 {
		t.Fatalf("got %d bytes, want %d identical", len(got), len(src))
	}
}

// Audio then an outage: no retry, the error carries the unit id, the heard audio is closed.
func TestTTS_ErrorAfterAudioNoRetry(t *testing.T) {
	f, cc := newTTSFake(t, func(_ int32, _ context.Context, s rivapb.RivaSpeechSynthesis_SynthesizeOnlineServer) error {
		if err := s.Send(audio(tone(300, 22050, 4410))); err != nil {
			return err
		}
		return status.Error(codes.Unavailable, "gpu gone api_key=abcdefgh12345678")
	})
	h := startTTS(t, ttsCfg(cc))
	h.say(t, "partial", true)
	waitTTS(t, "stop", func() bool { return h.stopped() == 1 && len(h.errs()) == 1 })
	if f.opens.Load() != 1 {
		t.Fatalf("opens=%d, a unit that sent audio must not retry", f.opens.Load())
	}
	e := h.errs()[0]
	var pe *ProviderError
	if e.UnitID != 7 || !errors.As(e.Err, &pe) || pe.Code != "Unavailable" || strings.Contains(pe.Message, "abcdefgh12345678") {
		t.Fatalf("error frame: %+v", e)
	}
	// Error-before-TTSStopped emit order is ttsunit's (TestRetryOnlyBeforeAudio):
	// the sink cannot see it, ErrorFrame rides the control lane and overtakes.
	if a := h.audio(); !a[len(a)-1].IsFinal {
		t.Fatal("what was heard must be closed with a final frame")
	}
}

// Failures before any audio retry; the retry's audio is what plays.
func TestTTS_RetriesBeforeAudio(t *testing.T) {
	f, cc := newTTSFake(t, func(n int32, _ context.Context, s rivapb.RivaSpeechSynthesis_SynthesizeOnlineServer) error {
		if n < 3 {
			return status.Error(codes.Unavailable, "warming up")
		}
		return s.Send(audio(tone(300, 22050, 4410)))
	})
	h := startTTS(t, ttsCfg(cc))
	h.say(t, "retry me", true)
	waitTTS(t, "stop", func() bool { return h.stopped() == 1 })
	if f.opens.Load() != 3 || len(h.errs()) != 0 {
		t.Fatalf("opens=%d errs=%d", f.opens.Load(), len(h.errs()))
	}
	if _, _, retries, _ := h.proc.TTSCounts(); retries != 2 {
		t.Fatalf("retries=%d", retries)
	}
}

// A stream that ends without audio is a failed attempt, not a silent success.
func TestTTS_EmptyStreamIsError(t *testing.T) {
	f, cc := newTTSFake(t, func(int32, context.Context, rivapb.RivaSpeechSynthesis_SynthesizeOnlineServer) error { return nil })
	cfg := ttsCfg(cc)
	cfg.RetryPolicy.MaxAttempts = 2
	h := startTTS(t, cfg)
	h.say(t, "nothing", true)
	waitTTS(t, "error", func() bool { return len(h.errs()) == 1 })
	var pe *ProviderError
	if !errors.As(h.errs()[0].Err, &pe) || pe.Code != "BAD_RESPONSE" || f.opens.Load() != 2 {
		t.Fatalf("err=%v opens=%d", h.errs()[0].Err, f.opens.Load())
	}
}

// Caller faults (bad key, bad voice) neither retry nor trip the breaker; outages do.
func TestTTS_BreakerRules(t *testing.T) {
	for _, tc := range []struct {
		code     codes.Code
		wantOpen bool
		opens    int32
	}{
		{codes.Unauthenticated, false, 1}, {codes.PermissionDenied, false, 1}, {codes.InvalidArgument, false, 1},
		{codes.Unavailable, true, 2}, {codes.Internal, true, 2},
	} {
		f, cc := newTTSFake(t, func(int32, context.Context, rivapb.RivaSpeechSynthesis_SynthesizeOnlineServer) error {
			return status.Error(tc.code, "nope")
		})
		cfg := ttsCfg(cc)
		cfg.RetryPolicy.MaxAttempts = 2
		cfg.CircuitBreaker = resilience.NewBreaker(resilience.Config{FailureThreshold: 2, Cooldown: time.Hour})
		h := startTTS(t, cfg)
		h.say(t, "hello", true)
		waitTTS(t, "error", func() bool { return len(h.errs()) == 1 })
		var pe *ProviderError
		if !errors.As(h.errs()[0].Err, &pe) || pe.ClientError == tc.wantOpen {
			t.Fatalf("%v: %+v", tc.code, h.errs()[0].Err)
		}
		if f.opens.Load() != tc.opens || cfg.CircuitBreaker.WouldRefuse() != tc.wantOpen {
			t.Fatalf("%v: opens=%d breakerOpen=%v", tc.code, f.opens.Load(), cfg.CircuitBreaker.WouldRefuse())
		}
	}
}

// Barge-in cancels the RPC; the rest of the unit is never emitted.
func TestTTS_InterruptionCancelsStream(t *testing.T) {
	f, cc := newTTSFake(t, func(_ int32, ctx context.Context, s rivapb.RivaSpeechSynthesis_SynthesizeOnlineServer) error {
		_ = s.Send(audio(tone(300, 22050, 8820)))
		<-ctx.Done() // returns when the client hangs up
		return ctx.Err()
	})
	h := startTTS(t, ttsCfg(cc))
	h.say(t, "a long sentence", true)
	waitTTS(t, "audio", func() bool { return len(h.audio()) > 0 })
	_ = h.sess.Post(&core.InterruptionFrame{})
	select {
	case <-f.done:
	case <-time.After(3 * time.Second):
		t.Fatal("stream not cancelled on interruption")
	}
	time.Sleep(30 * time.Millisecond)
	if h.stopped() != 0 || len(h.errs()) != 0 {
		t.Fatal("interrupted unit must not complete or error")
	}
}

// Closing the processor cancels an in-flight stream and leaves no tasks behind.
func TestTTS_CloseCancelsStream(t *testing.T) {
	f, cc := newTTSFake(t, func(_ int32, ctx context.Context, s rivapb.RivaSpeechSynthesis_SynthesizeOnlineServer) error {
		<-ctx.Done()
		return ctx.Err()
	})
	h := startTTS(t, ttsCfg(cc))
	h.say(t, "hang", true)
	<-f.reqs
	if err := h.proc.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.done:
	case <-time.After(3 * time.Second):
		t.Fatal("stream not cancelled on Close")
	}
	h.say(t, "after close", true) // dropped, not an error
	time.Sleep(30 * time.Millisecond)
	if len(h.errs()) != 0 || f.opens.Load() != 1 {
		t.Fatalf("errs=%d opens=%d", len(h.errs()), f.opens.Load())
	}
	if err := h.proc.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestTTS_EmptyTextKeepsDrainBoundary(t *testing.T) {
	f, cc := newTTSFake(t, nil)
	h := startTTS(t, ttsCfg(cc))
	h.say(t, "  ", true)
	waitTTS(t, "stop", func() bool { return h.stopped() == 1 })
	a := h.audio()
	if len(a) != 1 || !a[0].IsFinal || !a[0].TurnFinal || f.opens.Load() != 0 {
		t.Fatalf("frames=%+v opens=%d", a, f.opens.Load())
	}
}

func TestNewTTSProcessor_HostedNeedsCredentials(t *testing.T) {
	cfg := DefaultTTSConfig()
	if _, err := NewTTSProcessor(cfg); !errors.Is(err, ErrMissingFunctionID) {
		t.Fatalf("err=%v", err)
	}
	cfg.FunctionID = "fn"
	if _, err := NewTTSProcessor(cfg); !errors.Is(err, ErrMissingAPIKey) {
		t.Fatalf("err=%v", err)
	}
	cfg.Endpoint, cfg.Insecure, cfg.AllowPrivate = "127.0.0.1:50051", true, true
	p, err := NewTTSProcessor(cfg)
	if err != nil {
		t.Fatalf("self-hosted needs no key: %v", err)
	}
	p.Close()
}

// Warm is a no-RPC channel connect and is skipped while the breaker is open.
func TestTTS_Warm(t *testing.T) {
	f, cc := newTTSFake(t, nil)
	cfg := ttsCfg(cc)
	cfg.CircuitBreaker = resilience.NewBreaker(resilience.Config{FailureThreshold: 1, Cooldown: time.Hour})
	p, err := NewTTSProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := p.Warm(ctx); err != nil || f.opens.Load() != 0 {
		t.Fatalf("warm: %v opens=%d", err, f.opens.Load())
	}
	cfg.CircuitBreaker.RecordFailure()
	if err := p.Warm(ctx); err != nil {
		t.Fatalf("open breaker: %v", err)
	}
}
