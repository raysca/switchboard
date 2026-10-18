package sttunit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
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
	// closeGate, when set, blocks every Close until it is closed; closeEntered
	// is closed when the first Close begins. panicClose makes the first Close panic.
	closeGate    chan struct{}
	closeEntered chan struct{}
	panicClose   atomic.Bool
	closes       atomic.Int32
	// recvHold, when set, makes Recv ignore ctx until it is closed, so a test
	// can keep the stream's run goroutine (and its deferred teardown) parked.
	recvHold chan struct{}
	recvs    atomic.Int32 // Recv calls so far (a call after a result means it was processed)
	results  chan recvResult
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
	if c.recvHold != nil {
		<-c.recvHold
		return Result{}, ctx.Err()
	}
	select {
	case r := <-c.results:
		return r.res, r.err
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
}

func (c *fakeConn) Close() {
	if c.closes.Add(1) == 1 && c.closeEntered != nil {
		close(c.closeEntered)
	}
	if c.closeGate != nil {
		<-c.closeGate
	}
	if c.panicClose.CompareAndSwap(true, false) {
		panic("provider Close panicked")
	}
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
	return newHarnessTraced(t, cfg, budget, nil)
}

func newHarnessTraced(t *testing.T, cfg Config, budget core.ResourceBudget, tracer core.Tracer) *harness {
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
	if tracer != nil {
		if err := h.sess.SetTracer(tracer, core.TracePolicy{}); err != nil {
			t.Fatal(err)
		}
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
	if !errors.Is(ef.Err, core.ErrSTTUnavailable) {
		t.Fatalf("dial failure %v does not wrap core.ErrSTTUnavailable", ef.Err)
	}
	if _, _, n := h.r.STTCounts(); n != 1 {
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
	if _, _, n := h.r.STTCounts(); n != 0 {
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
	if _, _, n := h.r.STTCounts(); n != 0 {
		t.Fatalf("a cancelled dial counted %d provider errors", n)
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
	h.waitFor("fenced", func() bool { return h.sess.StaleDiscarded.Load() > 0 })
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
	if _, _, n := h.r.STTCounts(); n != 1 {
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
	if _, _, n := h.r.STTCounts(); n != 0 {
		t.Fatalf("Close during a flush counted %d provider errors", n)
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
	if errors.Is(ef.Err, core.ErrSTTUnavailable) {
		t.Fatalf("the local task budget is not an unreachable STT: %v", ef.Err)
	}
	if _, _, n := h.r.STTCounts(); n != 0 {
		t.Fatalf("the local task limit counted %d provider errors", n)
	}
	h.waitFor("frames passed", func() bool {
		return len(framesOf[*core.UserStartedSpeakingFrame](h)) == 2 && h.audioPassed() == 1
	})
	if err := h.sess.Err(); err != nil {
		t.Fatalf("the task limit ended the call: %v", err)
	}
}

// The provider closes the stream cleanly while a live Send is about to follow:
// the stream is stopped before its conn closes, so the racing Send is quiet:
// no error returned, none counted, nothing for the breaker, no ErrorFrame.
func TestRunner_ProviderCloseRacingSendIsQuiet(t *testing.T) {
	first, second := newFakeConn(t), newFakeConn(t)
	first.closeGate, first.closeEntered = make(chan struct{}), make(chan struct{})
	var n atomic.Int32
	h := newHarness(t, Config{Dial: func(context.Context) (Conn, error) {
		if n.Add(1) == 1 {
			return first, nil
		}
		return second, nil
	}})
	t.Cleanup(func() { close(first.closeGate) }) // runs before the harness cleanup
	h.audio(1)                                   // unbound stream: a provider end owes nobody a transcript (A8 applies to bound turns)
	h.waitFor("open", func() bool { return first.recvs.Load() >= 1 })
	first.setSendErr(errors.New("write: use of closed connection"))
	first.results <- recvResult{err: ErrStreamEnded}
	<-first.closeEntered
	err := h.r.ProcessFrame(context.Background(), &core.InputAudioFrame{Samples: []byte{1, 0}}, core.Downstream, discard{})
	if err != nil {
		t.Fatalf("ProcessFrame err = %v, want nil", err)
	}
	if _, _, errs := h.r.STTCounts(); errs != 0 {
		t.Fatalf("errors counted = %d", errs)
	}
	if len(h.errorFrames()) != 0 {
		t.Fatal("a clean provider close raced by a Send was reported")
	}
}

// The bound variant: the provider's end loses the turn (A8: one invalidation, one
// ErrorFrame, one count), and a Send racing that close adds nothing and tells the
// breaker nothing.
func TestRunner_ProviderCloseRacingSendOnBoundStreamIsOnlyA8(t *testing.T) {
	first := newFakeConn(t)
	first.closeGate, first.closeEntered = make(chan struct{}), make(chan struct{})
	br := resilience.NewBreaker(resilience.Config{FailureThreshold: 1, Cooldown: time.Hour})
	h := newHarness(t, Config{Dial: dialConn(first), Breaker: br, Classify: func(error) resilience.Verdict { return resilience.Verdict{Retryable: true} }})
	t.Cleanup(func() { close(first.closeGate) })
	h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	h.audio(1)
	h.waitFor("open", func() bool { return first.recvs.Load() >= 1 })
	first.setSendErr(errors.New("write: use of closed connection"))
	first.results <- recvResult{err: ErrStreamEnded}
	<-first.closeEntered
	err := h.r.ProcessFrame(context.Background(), &core.InputAudioFrame{Samples: []byte{1, 0}}, core.Downstream, discard{})
	if err != nil {
		t.Fatalf("ProcessFrame err = %v, want nil", err)
	}
	h.waitFor("invalidation and error", func() bool { return len(h.invalidations()) == 1 && len(h.errorFrames()) == 1 })
	time.Sleep(20 * time.Millisecond)
	if _, _, n := h.r.STTCounts(); n != 1 {
		t.Fatalf("errors counted = %d, want 1 (the A8 invalidation only)", n)
	}
	if len(h.errorFrames()) != 1 || br.WouldRefuse() {
		t.Fatalf("error frames = %d, breaker open = %v", len(h.errorFrames()), br.WouldRefuse())
	}
}

// A panicking provider Close on the drain timer goroutine must not take the
// process down; the stream is still removed.
func TestRunner_DrainTimeoutRecoversProviderClosePanic(t *testing.T) {
	conn := newFakeConn(t)
	conn.panicClose.Store(true)
	var logs syncBuf
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	tr := &spanTracer{}
	h := newHarnessTraced(t, Config{Dial: dialConn(conn), DrainTimeout: 20 * time.Millisecond}, core.DefaultResourceBudget(), tr)
	h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	h.audio(1)
	h.waitFor("open", func() bool { return conn.recvs.Load() >= 1 })
	h.post(&core.UserStoppedSpeakingFrame{})
	h.waitFor("drain timeout closed the stream", func() bool { return h.r.Streams() == 0 && conn.closes.Load() >= 1 })
	h.waitFor("recovery logged", func() bool { return strings.Contains(logs.String(), "provider Close panicked") })
	if out := logs.String(); !strings.Contains(out, "provider=fake") || !strings.Contains(out, "turn=turn-1-1") {
		t.Fatalf("log lacks provider/turn: %s", out)
	}
	h.waitFor("stt.stream span ended despite the panic", func() bool { return tr.ended("stt.stream") })
	if err := h.sess.Err(); err != nil {
		t.Fatalf("session failed: %v", err)
	}
}

func TestRunner_DialReturningNilConnIsAFailedDial(t *testing.T) {
	h := newHarness(t, Config{Dial: func(context.Context) (Conn, error) { return nil, nil }})
	h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	h.waitFor("error frame", func() bool { return len(h.errorFrames()) == 1 })
	if !errors.Is(h.errorFrames()[0].Err, errNilConn) {
		t.Fatalf("err = %v", h.errorFrames()[0].Err)
	}
	if err := h.sess.Err(); err != nil {
		t.Fatalf("session failed: %v", err)
	}
}

func TestRunner_DialErrorWithConnClosesIt(t *testing.T) {
	conn := newFakeConn(t)
	boom := errors.New("handshake failed")
	h := newHarness(t, Config{Dial: func(context.Context) (Conn, error) { return conn, boom }})
	h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	h.waitFor("error frame", func() bool { return len(h.errorFrames()) == 1 })
	h.waitFor("conn closed", conn.isClosed)
}

type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// spanTracer records which named spans have ended.
type spanTracer struct {
	mu   sync.Mutex
	done map[string]bool
}

func (t *spanTracer) StartSpan(_ context.Context, ev core.TraceEvent) core.Span {
	return &recSpan{t: t, name: ev.Name}
}

func (t *spanTracer) ended(name string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.done[name]
}

type recSpan struct {
	t    *spanTracer
	name string
}

func (*recSpan) SetAttribute(string, any) {}
func (*recSpan) AddEvent(core.TraceEvent) {}
func (s *recSpan) End(error) {
	s.t.mu.Lock()
	defer s.t.mu.Unlock()
	if s.t.done == nil {
		s.t.done = map[string]bool{}
	}
	s.t.done[s.name] = true
}

// stop ends the span even when the provider's Close panics. The run goroutine
// is parked in Recv, so its own deferred teardown cannot end the span first:
// only stop's deferred endTrace can.
func TestRunner_StopEndsSpanWhenClosePanics(t *testing.T) {
	conn := newFakeConn(t)
	conn.recvHold = make(chan struct{})
	conn.panicClose.Store(true)
	tr := &spanTracer{}
	h := newHarnessTraced(t, Config{Dial: dialConn(conn)}, core.DefaultResourceBudget(), tr)
	t.Cleanup(func() { close(conn.recvHold) }) // runs before the harness cleanup
	h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	h.audio(1)
	h.waitFor("open", func() bool { return conn.recvs.Load() >= 1 })
	h.r.mu.Lock()
	st := h.r.active
	h.r.mu.Unlock()
	func() {
		defer func() {
			if recover() == nil {
				t.Error("expected the provider Close panic to propagate out of stop")
			}
		}()
		h.r.stop(st, nil)
	}()
	// The session exports spans through an async queue: wait for the End.
	h.waitFor("stt.stream span ended by stop despite the panic", func() bool { return tr.ended("stt.stream") })
}

func (h *harness) invalidations() []*core.TranscriptInvalidationFrame {
	return framesOf[*core.TranscriptInvalidationFrame](h)
}

// A8: the provider ends the stream while the turn still owes its transcript
// (no Finish sent yet): the turn is invalidated, then an error raised, so an
// STT failover replays it. A clean end is not an outage: the breaker is not told.
func TestRunner_MidUtteranceDrop_InvalidatesTurn(t *testing.T) {
	conn := newFakeConn(t)
	br := resilience.NewBreaker(resilience.Config{FailureThreshold: 1, Cooldown: time.Hour})
	h := newHarness(t, Config{Dial: dialConn(conn), Breaker: br, Classify: func(error) resilience.Verdict { return resilience.Verdict{Retryable: true} }})
	h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	h.audio(1)
	conn.results <- recvResult{res: Result{Text: "hel"}}
	h.waitFor("interim", func() bool { return len(h.candidates()) == 1 })
	conn.results <- recvResult{err: fmt.Errorf("close 1000: %w", ErrStreamEnded)}
	h.waitFor("invalidation and error", func() bool { return len(h.invalidations()) == 1 && len(h.errorFrames()) == 1 })

	inv, ef := h.invalidations()[0], h.errorFrames()[0]
	if inv.Turn != "turn-1-1" || inv.Gen != 1 || ef.Turn != "turn-1-1" || ef.Gen != 1 {
		t.Fatalf("invalidation %+v error %+v", inv, ef)
	}
	if !errors.Is(ef.Err, core.ErrTranscriptInvalidated) || !errors.Is(ef.Err, ErrStreamEnded) {
		t.Fatalf("error %v must wrap ErrTranscriptInvalidated and the cause", ef.Err)
	}
	frames := h.sink.GetFrames()
	invAt := slices.IndexFunc(frames, func(f core.Frame) bool { _, ok := f.(*core.TranscriptInvalidationFrame); return ok })
	errAt := slices.IndexFunc(frames, func(f core.Frame) bool { _, ok := f.(*core.ErrorFrame); return ok })
	if invAt > errAt {
		t.Fatal("the error overtook the invalidation")
	}
	if br.WouldRefuse() {
		t.Fatal("a clean provider end was reported to the breaker")
	}
	if _, _, n := h.r.STTCounts(); n != 1 {
		t.Fatalf("errors counted = %d", n)
	}
}

func TestRunner_ProviderEndIsQuietWhenNothingIsOwed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script func(h *harness, conn *fakeConn)
	}{
		{"unbound", func(h *harness, conn *fakeConn) { h.audio(1) }},
		{"after the utterance final", func(h *harness, conn *fakeConn) {
			h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
			h.audio(1)
			h.post(&core.UserStoppedSpeakingFrame{})
			conn.results <- recvResult{res: Result{Text: "done", Final: true, EndOfUtterance: true}}
			h.waitFor("final", func() bool { return len(h.candidates()) == 1 })
		}},
		{"after finish with nothing heard", func(h *harness, conn *fakeConn) {
			h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
			h.audio(1)
			h.post(&core.UserStoppedSpeakingFrame{})
			h.waitFor("finish sent", func() bool { return slices.Contains(conn.snapshot(), "finish") })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := newFakeConn(t)
			h := newHarness(t, Config{Dial: dialConn(conn)})
			tc.script(h, conn)
			h.waitFor("open", func() bool { return conn.recvs.Load() >= 1 })
			conn.results <- recvResult{err: ErrStreamEnded}
			h.waitFor("released", func() bool { return h.r.Streams() == 0 })
			time.Sleep(20 * time.Millisecond)
			if n := len(h.errorFrames()) + len(h.invalidations()); n != 0 {
				t.Fatalf("a clean end with nothing owed raised %d frames", n)
			}
		})
	}
}

// After Finish the provider may still drop before the final: words were heard
// and no utterance-final candidate came, so the turn is lost (xai M3-09 rule).
func TestRunner_EndAfterFinishWithWordsButNoFinalInvalidates(t *testing.T) {
	conn := newFakeConn(t)
	h := newHarness(t, Config{Dial: dialConn(conn)})
	h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	h.audio(1)
	conn.results <- recvResult{res: Result{Text: "hel"}}
	h.waitFor("interim", func() bool { return len(h.candidates()) == 1 })
	h.post(&core.UserStoppedSpeakingFrame{})
	h.waitFor("finish sent", func() bool { return slices.Contains(conn.snapshot(), "finish") })
	conn.results <- recvResult{err: ErrStreamEnded}
	h.waitFor("invalidation and error", func() bool { return len(h.invalidations()) == 1 && len(h.errorFrames()) == 1 })
}

// A Final candidate commits all heard text even without EndOfUtterance (Riva
// answers CloseSend with no further result after an earlier final): the
// provider's end after Finish is then quiet, not a lost turn.
func TestRunner_FinalBeforeFinishThenEndIsQuiet(t *testing.T) {
	conn := newFakeConn(t)
	h := newHarness(t, Config{Dial: dialConn(conn)})
	h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	h.audio(1)
	conn.results <- recvResult{res: Result{Text: "hello", Final: true}}
	h.waitFor("final", func() bool { return len(h.candidates()) == 1 })
	h.post(&core.UserStoppedSpeakingFrame{})
	h.waitFor("finish sent", func() bool { return slices.Contains(conn.snapshot(), "finish") })
	conn.results <- recvResult{err: ErrStreamEnded}
	h.waitFor("released", func() bool { return h.r.Streams() == 0 })
	time.Sleep(20 * time.Millisecond)
	if n := len(h.errorFrames()) + len(h.invalidations()); n != 0 {
		t.Fatalf("a committed turn was called lost: %d frames", n)
	}
}

// An empty Final (noise retracted) settles the turn: nothing heard remains
// uncommitted, so the stream's end is not a lost utterance.
func TestRunner_EmptyFinalSettlesTheTurn(t *testing.T) {
	conn := newFakeConn(t)
	h := newHarness(t, Config{Dial: dialConn(conn)})
	h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	h.audio(1)
	conn.results <- recvResult{res: Result{Text: "uh"}}
	h.waitFor("interim", func() bool { return len(h.candidates()) == 1 })
	conn.results <- recvResult{res: Result{Final: true}}
	h.post(&core.UserStoppedSpeakingFrame{})
	h.waitFor("finish sent", func() bool { return slices.Contains(conn.snapshot(), "finish") })
	conn.results <- recvResult{err: ErrStreamEnded}
	h.waitFor("released", func() bool { return h.r.Streams() == 0 })
	time.Sleep(20 * time.Millisecond)
	if n := len(h.errorFrames()) + len(h.invalidations()); n != 0 || len(h.candidates()) != 1 {
		t.Fatalf("retracted noise called lost: %d frames, %d candidates", n, len(h.candidates()))
	}
}

// A bound stream whose pre-roll flush fails lost the turn's held audio: the
// turn is invalidated (so failover replays it) and nothing redials this turn.
func TestRunner_FlushSendFailureLosesBoundTurn(t *testing.T) {
	release := make(chan struct{})
	conn := newFakeConn(t)
	conn.setSendErr(errors.New("broken pipe"))
	h := newHarness(t, Config{Dial: gatedDial(release, conn)})
	h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	h.audio(1)
	h.waitFor("held", func() bool { return h.audioPassed() == 1 })
	close(release)
	h.waitFor("invalidation and error", func() bool { return len(h.invalidations()) == 1 && len(h.errorFrames()) == 1 })
	if ef := h.errorFrames()[0]; !errors.Is(ef.Err, core.ErrTranscriptInvalidated) || ef.Turn != "turn-1-1" {
		t.Fatalf("error frame %+v", ef)
	}
	h.audio(2)
	h.waitFor("audio passed", func() bool { return h.audioPassed() == 2 })
	if n := h.dials.Load(); n != 1 {
		t.Fatalf("dials = %d: no redial for a lost turn", n)
	}
}

// One stalled stream is one failure: a Send that times out while Recv fails
// concurrently (the adapter cancelled its RPC) counts, reports to the breaker
// and raises an ErrorFrame once, whichever side notices first.
func TestRunner_StalledSendAndFailingRecvReportOnce(t *testing.T) {
	conn := newFakeConn(t)
	conn.gate, conn.started = make(chan struct{}), make(chan struct{}) // Send blocks until its SendTimeout
	var reports atomic.Int32
	second := make(chan struct{})
	br := resilience.NewBreaker(resilience.Config{FailureThreshold: 100, Cooldown: time.Hour})
	h := newHarness(t, Config{
		Dial: dialConn(conn), Breaker: br, SendTimeout: 100 * time.Millisecond,
		Classify: func(error) resilience.Verdict {
			// Hold the first report until a second one shows up (the bug) or 400 ms pass, so
			// the other side runs while this one is mid-report and the stream not yet stopped.
			if reports.Add(1) == 1 {
				select {
				case <-second:
				case <-time.After(400 * time.Millisecond):
				}
			} else {
				close(second)
			}
			return resilience.Verdict{Retryable: true}
		},
	})
	h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	h.waitFor("open", func() bool { return conn.recvs.Load() >= 1 })
	go func() {
		<-conn.started
		conn.results <- recvResult{err: errors.New("rpc: context canceled")}
	}()
	err := h.r.ProcessFrame(context.Background(), &core.InputAudioFrame{Samples: []byte{1, 0}}, core.Downstream, discard{})
	h.waitFor("error frame", func() bool { return len(h.errorFrames()) >= 1 })
	h.waitFor("released", func() bool { return h.r.Streams() == 0 })
	time.Sleep(30 * time.Millisecond)
	if err != nil {
		t.Fatalf("ProcessFrame err = %v: the receive side already reported this stall", err)
	}
	if _, _, n := h.r.STTCounts(); n != 1 {
		t.Fatalf("errors counted = %d, want 1", n)
	}
	if n := reports.Load(); n != 1 {
		t.Fatalf("breaker reports = %d, want 1", n)
	}
	if n := len(h.errorFrames()); n != 1 {
		t.Fatalf("error frames = %d, want 1", n)
	}
}

// A stream reports one failure whichever path found it: the dial, the
// pre-roll flush (unbound and bound), or a failure after the first report.
func TestRunner_EachFailurePathReportsOnce(t *testing.T) {
	broken := errors.New("broken pipe")
	for _, tc := range []struct {
		name   string
		dial   func(release chan struct{}) func(context.Context) (Conn, error)
		script func(h *harness)
		frames int // error frames expected (an invalidation brings its own)
	}{
		{"dial", func(chan struct{}) func(context.Context) (Conn, error) {
			return func(context.Context) (Conn, error) { return nil, broken }
		}, func(h *harness) {
			h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
			h.audio(1)
		}, 1},
		{"flush unbound", func(release chan struct{}) func(context.Context) (Conn, error) {
			c := newFakeConn(t)
			c.setSendErr(broken)
			return gatedDial(release, c)
		}, func(h *harness) { h.audio(1) }, 1},
		{"flush bound", func(release chan struct{}) func(context.Context) (Conn, error) {
			c := newFakeConn(t)
			c.setSendErr(broken)
			return gatedDial(release, c)
		}, func(h *harness) {
			h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
			h.audio(1)
		}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release := make(chan struct{})
			var reports atomic.Int32
			br := resilience.NewBreaker(resilience.Config{FailureThreshold: 100, Cooldown: time.Hour})
			h := newHarness(t, Config{
				Dial: tc.dial(release), Breaker: br,
				Classify: func(error) resilience.Verdict { reports.Add(1); return resilience.Verdict{Retryable: true} },
			})
			tc.script(h)
			h.waitFor("held", func() bool { return h.audioPassed() == 1 })
			close(release)
			h.waitFor("error frame", func() bool { return len(h.errorFrames()) >= 1 })
			h.waitFor("released", func() bool { return h.r.Streams() == 0 })
			time.Sleep(30 * time.Millisecond)
			if n := len(h.errorFrames()); n != tc.frames {
				t.Fatalf("error frames = %d, want %d", n, tc.frames)
			}
			if _, _, n := h.r.STTCounts(); n != 1 {
				t.Fatalf("errors counted = %d, want 1", n)
			}
		})
	}
}

// While the provider hangs, every short turn's dial would hold a session task for
// the whole dial timeout (and the idle audio after it opens another). Unbounded,
// that stacks up to the session's MaxAsyncTasks and starves LLM and TTS tasks, so
// a runner holds at most maxStreams; a turn past it loses its transcript with a
// local, non-fatal error, not an "unreachable STT" and not a provider error.
func TestRunner_ProviderHangDoesNotStackSessionTasks(t *testing.T) {
	var live, peak atomic.Int32
	h := newHarness(t, Config{Dial: func(ctx context.Context) (Conn, error) {
		n := live.Add(1)
		defer live.Add(-1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		<-ctx.Done() // a provider-wide hang
		return nil, ctx.Err()
	}})
	const turns = 30
	for i := 1; i <= turns; i++ {
		h.post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: uint64(i)})
		h.audio(1)
		h.post(&core.UserStoppedSpeakingFrame{})
		h.audio(2) // idle audio after the turn: opens an unbound stream
		time.Sleep(5 * time.Millisecond)
	}
	h.waitFor("frames through", func() bool { return h.audioPassed() == 2*turns })
	if p := peak.Load(); p > maxStreams {
		t.Fatalf("peak concurrent dials (session tasks held) = %d, want <= %d", p, maxStreams)
	}
	if n := h.r.Streams(); n > maxStreams {
		t.Fatalf("open streams = %d, want <= %d", n, maxStreams)
	}
	h.waitFor("local errors", func() bool { return len(h.errorFrames()) >= turns-maxStreams })
	for _, ef := range h.errorFrames() {
		if ef.Fatal || !errors.Is(ef.Err, core.ErrTooManyTasks) || errors.Is(ef.Err, core.ErrSTTUnavailable) {
			t.Fatalf("error frame %+v: a local limit is non-fatal and not an unreachable STT", ef)
		}
	}
	if _, _, n := h.r.STTCounts(); n != 0 {
		t.Fatalf("the local limit counted %d provider errors", n)
	}
	if err := h.sess.Err(); err != nil {
		t.Fatalf("the call failed: %v", err)
	}
}
