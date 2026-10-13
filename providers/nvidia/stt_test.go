package nvidia

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
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

// fakeRiva is an in-process Riva ASR server on a bufconn listener.
type fakeRiva struct {
	rivapb.UnimplementedRivaSpeechRecognitionServer
	handle func(ctx context.Context, s rivapb.RivaSpeechRecognition_StreamingRecognizeServer) error
	opens  atomic.Int32
	cfgs   chan *rivapb.StreamingRecognitionConfig
	md     chan metadata.MD
	done   chan struct{} // one send per finished handler
}

func (f *fakeRiva) StreamingRecognize(s rivapb.RivaSpeechRecognition_StreamingRecognizeServer) error {
	f.opens.Add(1)
	f.md <- func() metadata.MD { md, _ := metadata.FromIncomingContext(s.Context()); return md }()
	defer func() { f.done <- struct{}{} }()
	first, err := s.Recv()
	if err != nil {
		return err
	}
	f.cfgs <- first.GetStreamingConfig()
	return f.handle(s.Context(), s)
}

func newFake(t *testing.T, handle func(ctx context.Context, s rivapb.RivaSpeechRecognition_StreamingRecognizeServer) error) (*fakeRiva, *grpc.ClientConn) {
	t.Helper()
	f := &fakeRiva{handle: handle, cfgs: make(chan *rivapb.StreamingRecognitionConfig, 16), md: make(chan metadata.MD, 16), done: make(chan struct{}, 16)}
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	rivapb.RegisterRivaSpeechRecognitionServer(srv, f)
	go srv.Serve(lis)
	cc, err := grpc.NewClient("passthrough:///bufnet", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cc.Close(); srv.Stop() })
	return f, cc
}

func (f *fakeRiva) cfg(cc *grpc.ClientConn) STTConfig {
	c := DefaultSTTConfig()
	c.Conn, c.APIKey, c.FunctionID = cc, "nvapi-test-key-0123456789", "fn-123"
	c.DrainTimeout = 300 * time.Millisecond
	c.RetryPolicy = resilience.Policy{MaxAttempts: 2, InitialBackoff: time.Millisecond, AttemptTimeout: 2 * time.Second}
	return c
}

func result(text string, final bool, stab float32) *rivapb.StreamingRecognizeResponse {
	return &rivapb.StreamingRecognizeResponse{Results: []*rivapb.StreamingRecognitionResult{{
		Alternatives: []*rivapb.SpeechRecognitionAlternative{{Transcript: text}}, IsFinal: final, Stability: stab}}}
}

type harness struct {
	proc *STTProcessor
	sess *core.Session
	sink *coretest.PassthroughProcessor
}

func start(t *testing.T, cfg STTConfig) *harness {
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
	if err := sess.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	return &harness{proc, sess, sink}
}

func (h *harness) audio() {
	_ = h.sess.Post(&core.InputAudioFrame{Samples: make([]byte, 640), SampleRate: 16000, Channels: 1, Gen: 1})
}

func (h *harness) speak() {
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	h.audio()
}

func (h *harness) candidates() (out []*core.TranscriptCandidateFrame) {
	for _, f := range h.sink.GetFrames() {
		if c, ok := f.(*core.TranscriptCandidateFrame); ok {
			out = append(out, c)
		}
	}
	return
}

func (h *harness) errs() (out []error) {
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

// Interim, final, then the drain after the user stops: cumulative text, the
// config message and the NVCF metadata.
func TestSTT_InterimFinalDrain(t *testing.T) {
	f, cc := newFake(t, func(ctx context.Context, s rivapb.RivaSpeechRecognition_StreamingRecognizeServer) error {
		if _, err := s.Recv(); err != nil { // first audio
			return err
		}
		_ = s.Send(result("hel", false, 0.4))
		_ = s.Send(result("hello", true, 0))
		_ = s.Send(result("wor", false, 0.2))
		for { // until the client half-closes, then flush the last segment
			if _, err := s.Recv(); err != nil {
				break
			}
		}
		_ = s.Send(result("world", true, 0))
		return nil
	})
	h := start(t, f.cfg(cc))
	h.speak()
	waitFor(t, "interim", func() bool { return len(h.candidates()) >= 3 })
	c := h.candidates()
	if c[0].Text != "hel" || c[0].Final || !c[0].HasStability || c[0].Provider != "nvidia" {
		t.Fatalf("interim: %+v", c[0])
	}
	if c[1].Text != "hello" || !c[1].Final || c[1].EndOfUtterance {
		t.Fatalf("final: %+v", c[1])
	}
	if c[2].Text != "hello wor" || c[2].Final {
		t.Fatalf("cumulative interim: %+v", c[2])
	}
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{})
	waitFor(t, "drained final", func() bool { c := h.candidates(); return len(c) >= 4 && c[3].Final })
	if last := h.candidates()[3]; last.Text != "hello world" || !last.EndOfUtterance {
		t.Fatalf("drain: %+v", last)
	}

	cfg := <-f.cfgs
	rc := cfg.GetConfig()
	if rc.GetEncoding() != rivapb.AudioEncoding_LINEAR_PCM || rc.GetSampleRateHertz() != 16000 || rc.GetLanguageCode() != "en-US" ||
		!rc.GetEnableAutomaticPunctuation() || rc.GetMaxAlternatives() != 1 || !cfg.GetInterimResults() {
		t.Fatalf("config: %v", cfg)
	}
	md := <-f.md
	if md.Get("function-id")[0] != "fn-123" || md.Get("authorization")[0] != "Bearer nvapi-test-key-0123456789" {
		t.Fatalf("metadata: %v", md)
	}
}

// A plaintext channel never carries the bearer token.
func TestDial_NoBearerOverPlaintext(t *testing.T) {
	cfg := ConnConfig{Insecure: true, Endpoint: "127.0.0.1:1", APIKey: "nvapi-secret-0123456789", FunctionID: "f"}
	if _, err := Dial(cfg); err == nil {
		t.Fatal("plaintext without allow_private accepted")
	}
	cfg.AllowPrivate = true
	c, err := Dial(cfg)
	if err != nil {
		t.Fatal(err)
	}
	md, _ := metadata.FromOutgoingContext(c.Context(context.Background()))
	if len(md.Get("authorization")) != 0 || md.Get("function-id")[0] != "f" {
		t.Fatalf("md: %v", md)
	}
	if c.Close() != nil || c.Close() != nil { // idempotent on an owned channel
		t.Fatal("double close failed")
	}
	// Plaintext reaches only private addresses; TLS reaches public ones.
	pub, priv := netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("10.0.0.1")
	if dialAllowed(pub, true, true) || !dialAllowed(priv, true, true) || !dialAllowed(pub, false, false) || dialAllowed(priv, false, false) {
		t.Fatal("dialAllowed")
	}
}

// One response may split the interim into several results: they are joined,
// and the lowest stability is reported.
func TestSTT_MultiResultInterim(t *testing.T) {
	f, cc := newFake(t, func(ctx context.Context, s rivapb.RivaSpeechRecognition_StreamingRecognizeServer) error {
		if _, err := s.Recv(); err != nil {
			return err
		}
		_ = s.Send(&rivapb.StreamingRecognizeResponse{Results: []*rivapb.StreamingRecognitionResult{
			{Alternatives: []*rivapb.SpeechRecognitionAlternative{{Transcript: "hello there"}}, Stability: 0.9},
			{Alternatives: []*rivapb.SpeechRecognitionAlternative{{Transcript: "gen"}}, Stability: 0.1},
		}})
		<-ctx.Done()
		return nil
	})
	h := start(t, f.cfg(cc))
	h.speak()
	waitFor(t, "interim", func() bool { return len(h.candidates()) == 1 })
	if c := h.candidates()[0]; c.Text != "hello there gen" || c.Stability != float64(float32(0.1)) || c.Final {
		t.Fatalf("got %+v", c)
	}
}

// Speech resuming after the user stopped opens a new stream for the new VAD
// segment; the finalized stream's text does not leak into it (core/turn joins
// segments of one managed turn).
func TestSTT_ResumedSpeechNewStream(t *testing.T) {
	var n atomic.Int32
	f, cc := newFake(t, func(ctx context.Context, s rivapb.RivaSpeechRecognition_StreamingRecognizeServer) error {
		word := []string{"hello", "again"}[min(n.Add(1)-1, 1)]
		if _, err := s.Recv(); err != nil {
			return err
		}
		for {
			if _, err := s.Recv(); err != nil {
				break
			}
		}
		return s.Send(result(word, true, 0))
	})
	h := start(t, f.cfg(cc))
	h.speak()
	waitFor(t, "open", func() bool { return f.opens.Load() == 1 })
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{})
	waitFor(t, "first final", func() bool { return len(h.candidates()) == 1 })
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 2})
	h.audio()
	waitFor(t, "second open", func() bool { return f.opens.Load() == 2 })
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{})
	waitFor(t, "second final", func() bool { return len(h.candidates()) == 2 })
	c := h.candidates()
	if c[0].Text != "hello" || c[0].Turn != "turn-1-1" || !c[0].EndOfUtterance ||
		c[1].Text != "again" || c[1].Turn != "turn-1-2" || !c[1].EndOfUtterance {
		t.Fatalf("got %+v / %+v", c[0], c[1])
	}
}

func TestSTT_HostedNeedsFunctionIDAndKey(t *testing.T) {
	if _, err := NewSTTProcessor(STTConfig{APIKey: "k"}); !errors.Is(err, ErrMissingFunctionID) {
		t.Fatalf("got %v", err)
	}
	if _, err := NewSTTProcessor(STTConfig{FunctionID: "f"}); !errors.Is(err, ErrMissingAPIKey) {
		t.Fatalf("got %v", err)
	}
}

// Loopback endpoints are refused at dial time unless the operator opts in.
func TestDial_SSRFGuard(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	go func() {
		for {
			c, err := lis.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	cfg := DefaultSTTConfig()
	cfg.Endpoint = lis.Addr().String() // TLS: plaintext would need AllowPrivate
	cfg.RetryPolicy = resilience.Policy{MaxAttempts: 1, AttemptTimeout: time.Second}
	h := start(t, cfg)
	h.speak()
	waitFor(t, "open error", func() bool { return h.proc.Errors.Load() > 0 })
	if len(h.candidates()) != 0 {
		t.Fatal("unexpected candidates")
	}
	p2, err := NewSTTProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()
	_, err = p2.openOnce(context.Background())
	if pe := MapError("stt", err); pe == nil || pe.Code != "BLOCKED_ADDRESS" || !pe.ClientError {
		t.Fatalf("expected blocked address, got %v", err)
	}
	pe := MapError("stt", ErrBlockedAddress)
	if !pe.ClientError || pe.Retryable {
		t.Fatalf("blocked address must be a client error: %+v", pe)
	}
	for _, a := range []string{"169.254.169.254", "127.0.0.1", "10.0.0.1", "fd00:ec2::254"} {
		ip, _ := netipParse(a)
		if allowedIP(ip, false) && a != "" {
			t.Fatalf("%s allowed", a)
		}
	}
}

func TestMapError_Codes(t *testing.T) {
	for _, tc := range []struct {
		code   codes.Code
		client bool
	}{
		{codes.Unauthenticated, true}, {codes.PermissionDenied, true}, {codes.InvalidArgument, true},
		{codes.Unavailable, false}, {codes.DeadlineExceeded, false}, {codes.ResourceExhausted, false}, {codes.Internal, false},
	} {
		pe := MapError("stt", status.Error(tc.code, "boom Bearer nvapi-abcdefghijklmnop"))
		if pe.ClientError != tc.client || pe.Retryable == tc.client || pe.GRPCCode != tc.code {
			t.Errorf("%v: %+v", tc.code, pe)
		}
		if got := classify(pe).ClientError; got != tc.client {
			t.Errorf("%v: classify client=%v", tc.code, got)
		}
		if strings.Contains(pe.Error(), "nvapi-abcdef") {
			t.Errorf("secret leaked: %s", pe.Error())
		}
	}
	if !classify(context.Canceled).ClientError {
		t.Error("cancel must not trip the breaker")
	}
}

func TestSTT_BreakerOpensOnOutageNotOnAuth(t *testing.T) {
	code := codes.Unavailable
	f, cc := newFake(t, func(context.Context, rivapb.RivaSpeechRecognition_StreamingRecognizeServer) error {
		return status.Error(code, "nope")
	})
	cfg := f.cfg(cc)
	cfg.CircuitBreaker = resilience.NewBreaker(resilience.Config{FailureThreshold: 2, Cooldown: time.Minute})
	h := start(t, cfg)
	for i := 0; i < 2; i++ {
		before := len(h.errs())
		h.speak()
		waitFor(t, "stream error", func() bool { return len(h.errs()) > before })
		h.proc.finishActive("", 0) // let the next speech open a fresh stream
	}
	waitFor(t, "breaker open", func() bool { return cfg.CircuitBreaker.WouldRefuse() })
	if err := h.proc.ensureStream(context.Background()); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("got %v", err)
	}

	code = codes.Unauthenticated
	cfg2 := f.cfg(cc)
	cfg2.CircuitBreaker = resilience.NewBreaker(resilience.Config{FailureThreshold: 1, Cooldown: time.Minute})
	h2 := start(t, cfg2)
	h2.speak()
	waitFor(t, "auth error", func() bool { return len(h2.errs()) > 0 })
	if cfg2.CircuitBreaker.WouldRefuse() {
		t.Fatal("client error tripped the breaker")
	}
	var pe *ProviderError
	if !errors.As(h2.errs()[0], &pe) || !pe.ClientError {
		t.Fatalf("got %v", h2.errs()[0])
	}
}

// Close cancels the RPC and ends the receiver; a barge-in generation bump
// keeps the same stream.
func TestSTT_TeardownAndRebind(t *testing.T) {
	f, cc := newFake(t, func(ctx context.Context, s rivapb.RivaSpeechRecognition_StreamingRecognizeServer) error {
		<-ctx.Done()
		return ctx.Err()
	})
	h := start(t, f.cfg(cc))
	h.speak()
	waitFor(t, "open", func() bool { return f.opens.Load() == 1 })
	h.proc.beginTurn("turn-1-1", 2) // same turn, newer generation
	h.audio()
	time.Sleep(50 * time.Millisecond)
	if f.opens.Load() != 1 {
		t.Fatalf("rebind reopened the stream: %d opens", f.opens.Load())
	}
	if turn, gen := h.proc.active.owner(); turn != "turn-1-1" || gen != 2 {
		t.Fatalf("owner = %s/%d", turn, gen)
	}
	_ = h.proc.Close()
	select {
	case <-f.done:
	case <-time.After(3 * time.Second):
		t.Fatal("server RPC not cancelled on Close")
	}
	if err := h.proc.Close(); err != nil { // idempotent
		t.Fatal(err)
	}
}

func TestSTT_NewTurnFinalizesOldStream(t *testing.T) {
	var sawEOF atomic.Bool
	f, cc := newFake(t, func(ctx context.Context, s rivapb.RivaSpeechRecognition_StreamingRecognizeServer) error {
		for {
			if _, err := s.Recv(); errors.Is(err, io.EOF) {
				sawEOF.Store(true)
				return nil
			} else if err != nil {
				return err
			}
		}
	})
	h := start(t, f.cfg(cc))
	h.speak()
	waitFor(t, "open", func() bool { return f.opens.Load() == 1 })
	h.proc.beginTurn("turn-9-9", 9)
	waitFor(t, "half-close", sawEOF.Load)
}

func netipParse(s string) (netip.Addr, error) { return netip.ParseAddr(s) }

// A server that echoes the request metadata in its error must not leak the key
// through the error frame (non-nvapi keys are caught by the Bearer prefix).
func TestSTT_EchoedMetadataRedacted(t *testing.T) {
	for _, key := range []string{"nvapi-test-key-0123456789", "selfhosted-key-0123456789"} {
		f, cc := newFake(t, func(ctx context.Context, s rivapb.RivaSpeechRecognition_StreamingRecognizeServer) error {
			md, _ := metadata.FromIncomingContext(ctx)
			return status.Errorf(codes.Unauthenticated, "bad credentials: %v", md)
		})
		cfg := f.cfg(cc)
		cfg.APIKey = key
		h := start(t, cfg)
		h.speak()
		waitFor(t, "auth error", func() bool { return len(h.errs()) > 0 })
		msg := h.errs()[0].Error()
		if strings.Contains(msg, "0123456789") || !strings.Contains(msg, "[REDACTED]") {
			t.Fatalf("key leaked or not redacted: %s", msg)
		}
	}
}
