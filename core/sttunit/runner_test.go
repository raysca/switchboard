package sttunit

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/coretest"
	"govoice/core/resilience"
)

// fakeConn is a scripted provider stream: it records sends and finishes in
// order, and Recv returns what the test feeds into results. Send and Finish
// must never overlap (the Conn contract): any overlap fails the test.
type fakeConn struct {
	t        *testing.T
	inFlight atomic.Int32 // Send/Finish calls running now
	// gate, when set, holds the first Send until it is closed (or its ctx
	// ends); started is closed when that Send begins.
	gate    chan struct{}
	started chan struct{}
	gated   atomic.Bool

	mu      sync.Mutex
	ops     []string // "send:<first byte>" or "finish"
	closed  bool
	sendErr error
	recvs   atomic.Int32 // Recv calls so far (a call after a result means it was processed)
	results chan recvResult
}

type recvResult struct {
	res Result
	err error
}

func newFakeConn(t *testing.T) *fakeConn {
	return &fakeConn{t: t, results: make(chan recvResult, 16)}
}

// newGatedConn returns a fakeConn whose first Send blocks until gate closes.
func newGatedConn(t *testing.T) (c *fakeConn, gate chan struct{}) {
	c = newFakeConn(t)
	c.gate, c.started = make(chan struct{}), make(chan struct{})
	return c, c.gate
}

func (c *fakeConn) enter(op string) {
	if c.inFlight.Add(1) != 1 {
		c.t.Errorf("%s overlapped another Send/Finish", op)
	}
}

func (c *fakeConn) Send(ctx context.Context, pcm []byte) error {
	c.enter("Send")
	defer c.inFlight.Add(-1)
	if c.gate != nil && c.gated.CompareAndSwap(false, true) {
		close(c.started)
		select {
		case <-c.gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sendErr != nil {
		return c.sendErr
	}
	c.ops = append(c.ops, "send:"+strconv.Itoa(int(pcm[0])))
	return nil
}

func (c *fakeConn) Finish(context.Context) error {
	c.enter("Finish")
	defer c.inFlight.Add(-1)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ops = append(c.ops, "finish")
	return nil
}

func (c *fakeConn) Recv(ctx context.Context) (Result, error) {
	c.recvs.Add(1)
	select {
	case r := <-c.results:
		return r.res, r.err
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
}

func (c *fakeConn) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
}

func (c *fakeConn) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.ops)
}

func (c *fakeConn) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func (c *fakeConn) setSendErr(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sendErr = err
}

// gatedDial blocks until release is closed, then returns conn.
func gatedDial(release <-chan struct{}, conn Conn) func(context.Context) (Conn, error) {
	return func(ctx context.Context) (Conn, error) {
		select {
		case <-release:
			return conn, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func dialConn(conn Conn) func(context.Context) (Conn, error) {
	return func(context.Context) (Conn, error) { return conn, nil }
}

type harness struct {
	t     *testing.T
	r     *Runner
	sess  *core.Session
	sink  *coretest.PassthroughProcessor
	dials atomic.Int32
}

func newHarness(t *testing.T, cfg Config) *harness {
	t.Helper()
	return newHarnessWithBudget(t, cfg, core.DefaultResourceBudget())
}

func newHarnessWithBudget(t *testing.T, cfg Config, budget core.ResourceBudget) *harness {
	t.Helper()
	h := &harness{t: t, sink: &coretest.PassthroughProcessor{}}
	dial := cfg.Dial
	cfg.Dial = func(ctx context.Context) (Conn, error) {
		h.dials.Add(1)
		return dial(ctx)
	}
	if cfg.Provider == "" {
		cfg.Provider = "fake"
	}
	h.r = New(cfg)
	pipe, err := core.NewPipeline(h.r, h.sink)
	if err != nil {
		t.Fatal(err)
	}
	h.sess, err = core.NewSession(7, pipe, budget)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.sess.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = h.sess.Close()
		if n := h.sess.LeakedTasks(); n != 0 {
			t.Errorf("leaked %d session tasks", n)
		}
	})
	return h
}

func (h *harness) post(f core.Frame) {
	h.t.Helper()
	if err := h.sess.Post(f); err != nil {
		h.t.Fatal(err)
	}
}

// audio posts one 2-byte frame whose first byte names it.
func (h *harness) audio(b byte) {
	h.t.Helper()
	h.post(&core.InputAudioFrame{Samples: []byte{b, 0}, SampleRate: 16000, Channels: 1})
}

func (h *harness) waitFor(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func framesOf[T core.Frame](h *harness) (out []T) {
	for _, f := range h.sink.GetFrames() {
		if v, ok := f.(T); ok {
			out = append(out, v)
		}
	}
	return out
}

func (h *harness) candidates() []*core.TranscriptCandidateFrame {
	return framesOf[*core.TranscriptCandidateFrame](h)
}
func (h *harness) errorFrames() []*core.ErrorFrame { return framesOf[*core.ErrorFrame](h) }
func (h *harness) audioPassed() int                { return len(framesOf[*core.InputAudioFrame](h)) }

type discard struct{}

func (discard) Push(core.Frame) error                          { return nil }
func (discard) PushDirection(core.Frame, core.Direction) error { return nil }

// A1: ProcessFrame never waits for the dial, so a dial longer than the 2 s
// frame watchdog does not end the call, and the audio sent meanwhile reaches
// the stream once it opens.
func TestRunner_DialDoesNotBlockProcessFrame(t *testing.T) {
	conn := newFakeConn(t)
	h := newHarness(t, Config{Dial: func(ctx context.Context) (Conn, error) {
		select {
		case <-time.After(3 * time.Second):
			return conn, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}})
	// Through the run loop, so the real 2 s frame watchdog is armed.
	start := time.Now()
	h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	h.audio(1)
	h.audio(2)
	h.waitFor("frames through the runner", func() bool { return h.audioPassed() == 2 })
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("frames took %v to pass: ProcessFrame waited for the dial", d)
	}
	time.Sleep(2500 * time.Millisecond) // past the watchdog, the dial still in flight
	if err := h.sess.Err(); err != nil {
		t.Fatalf("session failed while the dial hung: %v", err)
	}
	h.waitFor("pre-roll flushed", func() bool { return len(conn.snapshot()) == 2 })
	if got := conn.snapshot(); !slices.Equal(got, []string{"send:1", "send:2"}) {
		t.Fatalf("sent %v", got)
	}
}

func TestRunner_PreRollFlushedInOrder(t *testing.T) {
	release := make(chan struct{})
	conn := newFakeConn(t)
	h := newHarness(t, Config{Dial: gatedDial(release, conn)})
	h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	for b := byte(1); b <= 5; b++ {
		h.audio(b)
	}
	h.waitFor("audio passed downstream while dialing", func() bool { return h.audioPassed() == 5 })
	close(release)
	h.audio(6)
	h.waitFor("all audio sent", func() bool { return len(conn.snapshot()) == 6 })
	want := []string{"send:1", "send:2", "send:3", "send:4", "send:5", "send:6"}
	if got := conn.snapshot(); !slices.Equal(got, want) {
		t.Fatalf("sent %v, want %v", got, want)
	}
}

func TestRunner_PreRollBounded(t *testing.T) {
	release := make(chan struct{})
	conn := newFakeConn(t)
	h := newHarness(t, Config{Dial: gatedDial(release, conn)})
	h.r.maxPreRoll = 3 * 2 // three 2-byte frames; set before any frame is posted
	h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	for b := byte(1); b <= 5; b++ {
		h.audio(b)
	}
	h.waitFor("audio passed downstream while dialing", func() bool { return h.audioPassed() == 5 })
	close(release)
	h.waitFor("held audio sent", func() bool { return len(conn.snapshot()) == 3 })
	if got := conn.snapshot(); !slices.Equal(got, []string{"send:3", "send:4", "send:5"}) {
		t.Fatalf("sent %v: the oldest audio must be dropped first", got)
	}
}

func TestRunner_FinishWhileDialingGoesAfterPreRoll(t *testing.T) {
	release := make(chan struct{})
	conn := newFakeConn(t)
	h := newHarness(t, Config{Dial: gatedDial(release, conn)})
	h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	h.audio(1)
	h.audio(2)
	h.post(&core.UserStoppedSpeakingFrame{})
	h.waitFor("stop passed downstream", func() bool { return len(framesOf[*core.UserStoppedSpeakingFrame](h)) == 1 })
	close(release)
	h.waitFor("finish", func() bool { return len(conn.snapshot()) == 3 })
	if got := conn.snapshot(); !slices.Equal(got, []string{"send:1", "send:2", "finish"}) {
		t.Fatalf("ops %v: Finish must follow the held audio", got)
	}
}

func TestRunner_DialFailureReportsErrorFrame(t *testing.T) {
	boom := errors.New("dial: connection refused")
	h := newHarness(t, Config{Dial: func(context.Context) (Conn, error) { return nil, boom }})
	h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	h.audio(1)
	h.waitFor("error frame", func() bool { return len(h.errorFrames()) == 1 })
	ef := h.errorFrames()[0]
	if !errors.Is(ef.Err, boom) || ef.Fatal || ef.Turn != "turn-1-1" || ef.Gen != 1 {
		t.Fatalf("error frame %+v", ef)
	}
	if _, _, _, n := h.r.STTCounts(); n != 1 {
		t.Fatalf("errors counted = %d, want 1", n)
	}
	// No redial for the rest of the turn: the audio passes through untranscribed.
	h.audio(2)
	h.audio(3)
	h.waitFor("audio passed", func() bool { return h.audioPassed() == 3 })
	if n := h.dials.Load(); n != 1 {
		t.Fatalf("dials = %d, want one per turn", n)
	}
	// The next turn dials again.
	h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 2})
	h.waitFor("second dial", func() bool { return h.dials.Load() == 2 })
	if err := h.sess.Err(); err != nil {
		t.Fatalf("a failed dial ended the call: %v", err)
	}
}

func TestRunner_CircuitOpenDialIsReportedNotCounted(t *testing.T) {
	h := newHarness(t, Config{Dial: func(context.Context) (Conn, error) {
		return nil, fmt.Errorf("fake: %w", resilience.ErrCircuitOpen)
	}})
	h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	h.waitFor("error frame", func() bool { return len(h.errorFrames()) == 1 })
	if !errors.Is(h.errorFrames()[0].Err, resilience.ErrCircuitOpen) {
		t.Fatalf("err = %v", h.errorFrames()[0].Err)
	}
	if _, _, _, n := h.r.STTCounts(); n != 0 {
		t.Fatalf("an open breaker counted as a provider error: %d", n)
	}
}

func TestRunner_CloseCancelsDialAndLeaksNothing(t *testing.T) {
	dialing := make(chan struct{})
	h := newHarness(t, Config{Dial: func(ctx context.Context) (Conn, error) {
		close(dialing)
		<-ctx.Done()
		return nil, ctx.Err()
	}})
	h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	h.audio(1)
	<-dialing
	if err := h.sess.Close(); err != nil {
		t.Fatal(err)
	}
	if n := h.sess.LeakedTasks(); n != 0 {
		t.Fatalf("leaked %d tasks", n)
	}
	if n := h.r.Streams(); n != 0 {
		t.Fatalf("streams after Close = %d", n)
	}
	if len(h.errorFrames()) != 0 {
		t.Fatal("a cancelled dial was reported as a failure")
	}
}

func TestRunner_UnboundStreamAdoptedByNextTurn(t *testing.T) {
	conn := newFakeConn(t)
	h := newHarness(t, Config{Dial: dialConn(conn)})
	h.audio(1) // idle audio opens an unbound stream
	conn.results <- recvResult{res: Result{Text: "hi", Final: true}}
	h.waitFor("result read", func() bool { return conn.recvs.Load() >= 2 })
	if len(h.candidates()) != 0 {
		t.Fatal("an unbound stream emitted a candidate")
	}
	h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	h.audio(2)
	// The turn start and audio 2 have been processed (the stream is bound)
	// before the result is fed; otherwise Recv can win the race and the
	// unbound stream drops it.
	h.waitFor("turn bound", func() bool { return h.audioPassed() == 2 })
	conn.results <- recvResult{res: Result{Text: "hi there"}}
	h.waitFor("adopted candidate", func() bool { return len(h.candidates()) == 1 })
	c := h.candidates()[0]
	if c.Text != "hi there" || c.Turn != "turn-1-1" || c.Gen != 1 || c.Provider != "fake" || h.dials.Load() != 1 {
		t.Fatalf("candidate %+v dials=%d", c, h.dials.Load())
	}
}

func TestRunner_LateResultOfOldGenerationIsFenced(t *testing.T) {
	conn := newFakeConn(t)
	h := newHarness(t, Config{Dial: dialConn(conn)})
	h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	h.audio(1)
	h.waitFor("open", func() bool { return conn.recvs.Load() >= 1 })
	h.sess.IncrementGeneration() // a barge-in moved on; its InterruptionFrame is not here yet
	conn.results <- recvResult{res: Result{Text: "stale", Final: true, EndOfUtterance: true}}
	h.waitFor("result read", func() bool { return conn.recvs.Load() >= 2 })
	time.Sleep(50 * time.Millisecond)
	if c := h.candidates(); len(c) != 0 {
		t.Fatalf("a generation-1 final reached the transcript stage: %+v", c)
	}
}

func TestRunner_SendFailureFailsTheFrameAndReleases(t *testing.T) {
	conn := newFakeConn(t)
	h := newHarness(t, Config{Dial: dialConn(conn)})
	h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	h.waitFor("open", func() bool { return conn.recvs.Load() >= 1 })
	broken := errors.New("broken pipe")
	conn.setSendErr(broken)
	err := h.r.ProcessFrame(context.Background(), &core.InputAudioFrame{Samples: []byte{1, 0}}, core.Downstream, discard{})
	if !errors.Is(err, broken) {
		t.Fatalf("ProcessFrame err = %v, want the send error (STTFailover.syncFailure needs it)", err)
	}
	if _, _, _, n := h.r.STTCounts(); n != 1 {
		t.Fatalf("errors counted = %d", n)
	}
	h.waitFor("released and closed", func() bool { return h.r.Streams() == 0 && conn.isClosed() })
}

// Live audio arriving while a flush pass is sending joins the pre-roll and
// goes in the next pass: order 1-4, never a live Send overlapping the flush.
func TestRunner_LiveAudioDuringFlushKeepsOrder(t *testing.T) {
	release := make(chan struct{})
	conn, gate := newGatedConn(t)
	h := newHarness(t, Config{Dial: gatedDial(release, conn), SendTimeout: 5 * time.Second})
	h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	h.audio(1)
	h.audio(2)
	h.waitFor("held", func() bool { return h.audioPassed() == 2 })
	close(release)
	<-conn.started // the flush is inside Send(1)
	h.audio(3)
	h.audio(4)
	h.waitFor("live audio passed", func() bool { return h.audioPassed() == 4 })
	close(gate)
	h.waitFor("all sent", func() bool { return len(conn.snapshot()) == 4 })
	if got := conn.snapshot(); !slices.Equal(got, []string{"send:1", "send:2", "send:3", "send:4"}) {
		t.Fatalf("sent %v", got)
	}
}

func TestRunner_FinishDuringFlushGoesLast(t *testing.T) {
	release := make(chan struct{})
	conn, gate := newGatedConn(t)
	h := newHarness(t, Config{Dial: gatedDial(release, conn), SendTimeout: 5 * time.Second})
	h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	h.audio(1)
	h.audio(2)
	h.waitFor("held", func() bool { return h.audioPassed() == 2 })
	close(release)
	<-conn.started
	h.post(&core.UserStoppedSpeakingFrame{})
	h.waitFor("stop passed", func() bool { return len(framesOf[*core.UserStoppedSpeakingFrame](h)) == 1 })
	close(gate)
	h.waitFor("finish", func() bool { return len(conn.snapshot()) == 3 })
	if got := conn.snapshot(); !slices.Equal(got, []string{"send:1", "send:2", "finish"}) {
		t.Fatalf("ops %v", got)
	}
}

func TestRunner_CloseDuringFlushIsQuiet(t *testing.T) {
	release := make(chan struct{})
	conn, _ := newGatedConn(t) // the gate never opens: Close must unblock the Send
	h := newHarness(t, Config{Dial: gatedDial(release, conn), SendTimeout: 5 * time.Second})
	h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	h.audio(1)
	h.waitFor("held", func() bool { return h.audioPassed() == 1 })
	close(release)
	<-conn.started
	if err := h.sess.Close(); err != nil {
		t.Fatal(err)
	}
	if n := h.sess.LeakedTasks(); n != 0 {
		t.Fatalf("leaked %d tasks", n)
	}
	if len(h.errorFrames()) != 0 {
		t.Fatal("Close during a flush was reported as a failure")
	}
}

// A flush Send that fails is a failed open: one error, no redial this turn.
// (Task 2.2 adds the bound-turn case, which invalidates.)
func TestRunner_FlushSendFailure(t *testing.T) {
	release := make(chan struct{})
	conn := newFakeConn(t)
	broken := errors.New("broken pipe")
	conn.setSendErr(broken)
	h := newHarness(t, Config{Dial: gatedDial(release, conn)})
	h.audio(1) // idle audio: an unbound stream, still active
	h.waitFor("held", func() bool { return h.audioPassed() == 1 })
	close(release)
	h.waitFor("error frame", func() bool { return len(h.errorFrames()) == 1 })
	if !errors.Is(h.errorFrames()[0].Err, broken) {
		t.Fatalf("err = %v", h.errorFrames()[0].Err)
	}
	h.audio(2)
	h.waitFor("audio passed", func() bool { return h.audioPassed() == 2 })
	if n := h.dials.Load(); n != 1 {
		t.Fatalf("dials = %d: a failed flush must not redial before the next turn", n)
	}
}

// ErrTooManyTasks loses the turn's transcript, not the call: a non-fatal
// error, the frame passes, the session lives.
func TestRunner_TaskLimitLosesTheTurnNotTheCall(t *testing.T) {
	budget := core.DefaultResourceBudget()
	budget.MaxAsyncTasks = 1
	release := make(chan struct{})
	defer close(release)
	h := newHarnessWithBudget(t, Config{Dial: gatedDial(release, newFakeConn(t))}, budget)
	h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1}) // its dial holds the only task
	h.post(&core.UserStoppedSpeakingFrame{})               // the dialing stream now finishes
	h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 2}) // no task left for this turn's stream
	h.audio(1)
	h.waitFor("error frame", func() bool { return len(h.errorFrames()) == 1 })
	ef := h.errorFrames()[0]
	if !errors.Is(ef.Err, core.ErrTooManyTasks) || ef.Fatal || ef.Turn != "turn-1-2" {
		t.Fatalf("error frame %+v", ef)
	}
	h.waitFor("frames passed", func() bool {
		return len(framesOf[*core.UserStartedSpeakingFrame](h)) == 2 && h.audioPassed() == 1
	})
	if err := h.sess.Err(); err != nil {
		t.Fatalf("the task limit ended the call: %v", err)
	}
}
