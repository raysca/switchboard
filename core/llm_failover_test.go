package core_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/coretest"
)

var errBoom = errors.New("503 upstream")

type failoverRig struct {
	w        *core.LLMFailover
	sess     *core.Session
	rec      *coretest.PassthroughProcessor
	events   chan core.DebugEvent
	mu       sync.Mutex
	switches []string
}

func (r *failoverRig) switched() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.switches...)
}

func newFailoverRig(t *testing.T, cfg core.LLMFailoverConfig) *failoverRig {
	t.Helper()
	r := &failoverRig{rec: &coretest.PassthroughProcessor{}, events: make(chan core.DebugEvent, 16)}
	cfg.OnFailover = func(from, to, class string) {
		r.mu.Lock()
		r.switches = append(r.switches, from+">"+to+":"+class)
		r.mu.Unlock()
	}
	if cfg.Classify == nil {
		cfg.Classify = func(error) string { return "server_error" }
	}
	w, err := core.NewLLMFailover(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.w = w
	pipe, err := core.NewPipeline(w, r.rec)
	if err != nil {
		t.Fatal(err)
	}
	r.sess, err = core.NewSession(900, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.sess.Start(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.sess.Subscribe(16, func(e core.DebugEvent) {
		if e.Name == core.DebugLLMFailover {
			r.events <- e
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = r.sess.Close()
		if n := r.sess.LeakedTasks(); n != 0 {
			t.Errorf("leaked %d tasks", n)
		}
	})
	return r
}

func (r *failoverRig) say(t *testing.T, text string) {
	t.Helper()
	if err := r.sess.Post(&core.UserMessageFrame{Turn: "t1", Content: text, Gen: r.sess.CurrentGeneration(), Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
}

// summary: users, concatenated deltas, final texts, errors.
func (r *failoverRig) summary() (users int, deltas string, finals, errs int) {
	var b strings.Builder
	for _, f := range r.rec.GetFrames() {
		switch v := f.(type) {
		case *core.UserMessageFrame:
			users++
		case *core.LLMTextDeltaFrame:
			b.WriteString(v.Delta)
		case *core.LLMTextFrame:
			finals++
		case *core.ErrorFrame:
			errs++
		}
	}
	return users, b.String(), finals, errs
}

func (r *failoverRig) waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("condition not met; frames=%d", r.rec.Count())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func fake(chunks []string, failAfter int, err error) *coretest.LLMProcessor {
	return coretest.NewLLMProcessor(coretest.LLMConfig{ResponseChunks: chunks, FailWith: err, FailAfter: failAfter})
}

func TestFailoverBeforeFirstTokenAnswersOnceFromFallback(t *testing.T) {
	primary, backup := fake([]string{"never"}, 0, errBoom), fake([]string{"hel", "lo"}, 0, nil)
	r := newFailoverRig(t, core.LLMFailoverConfig{Candidates: []core.LLMCandidate{
		{Provider: "a", Proc: primary}, {Provider: "b", Proc: backup}}})
	r.say(t, "hi")
	r.waitFor(t, func() bool { _, _, f, _ := r.summary(); return f == 1 })
	users, deltas, finals, errs := r.summary()
	if users != 1 || deltas != "hello" || finals != 1 || errs != 0 {
		t.Fatalf("users=%d deltas=%q finals=%d errs=%d", users, deltas, finals, errs)
	}
	if got := r.switched(); len(got) != 1 || got[0] != "a>b:server_error" {
		t.Fatalf("switches %v", got)
	}
	select {
	case e := <-r.events:
		if e.TurnID != "t1" || e.Generation != 1 {
			t.Fatalf("event turn=%q gen=%d", e.TurnID, e.Generation)
		}
		if a := e.Attributes(); a["component"] != "a" || a["provider"] != "b" || a["reason"] != "server_error" {
			t.Fatalf("event attrs %v", a)
		}
	case <-time.After(time.Second):
		t.Fatal("no llm.failover debug event")
	}
}

func TestNoFailoverAfterFirstToken(t *testing.T) {
	primary, backup := fake([]string{"par", "tial"}, 1, errBoom), fake([]string{"other"}, 0, nil)
	r := newFailoverRig(t, core.LLMFailoverConfig{Candidates: []core.LLMCandidate{
		{Provider: "a", Proc: primary}, {Provider: "b", Proc: backup}}})
	r.say(t, "hi")
	r.waitFor(t, func() bool { _, _, _, e := r.summary(); return e == 1 })
	time.Sleep(50 * time.Millisecond)
	users, deltas, finals, errs := r.summary()
	if users != 1 || deltas != "par" || finals != 0 || errs != 1 || backup.Started.Load() != 0 || len(r.switched()) != 0 {
		t.Fatalf("users=%d deltas=%q finals=%d errs=%d backupStarted=%d", users, deltas, finals, errs, backup.Started.Load())
	}
}

func TestAllCandidatesFailGivesOneError(t *testing.T) {
	r := newFailoverRig(t, core.LLMFailoverConfig{Candidates: []core.LLMCandidate{
		{Provider: "a", Proc: fake(nil, 0, errBoom)}, {Provider: "b", Proc: fake(nil, 0, errBoom)}, {Provider: "c", Proc: fake(nil, 0, errBoom)}}})
	r.say(t, "hi")
	r.waitFor(t, func() bool { _, _, _, e := r.summary(); return e >= 1 })
	time.Sleep(50 * time.Millisecond)
	users, deltas, finals, errs := r.summary()
	if users != 1 || deltas != "" || finals != 0 || errs != 1 || len(r.switched()) != 2 {
		t.Fatalf("users=%d deltas=%q finals=%d errs=%d switches=%v", users, deltas, finals, errs, r.switched())
	}
}

func TestDisallowedClassDoesNotFailOver(t *testing.T) {
	backup := fake([]string{"x"}, 0, nil)
	r := newFailoverRig(t, core.LLMFailoverConfig{
		Classify:   func(error) string { return "client_error" },
		Allow:      func(c string) bool { return c != "client_error" },
		Candidates: []core.LLMCandidate{{Provider: "a", Proc: fake(nil, 0, errBoom)}, {Provider: "b", Proc: backup}}})
	r.say(t, "hi")
	r.waitFor(t, func() bool { _, _, _, e := r.summary(); return e == 1 })
	if backup.Started.Load() != 0 {
		t.Fatal("failed over on a disallowed class")
	}
}

func TestToolSideEffectsBlockFailover(t *testing.T) {
	var effects atomic.Uint64
	backup := fake([]string{"x"}, 0, nil)
	r := newFailoverRig(t, core.LLMFailoverConfig{
		SideEffects: func() uint64 { return effects.Add(1) }, // a tool ran between start and failure
		Candidates:  []core.LLMCandidate{{Provider: "a", Proc: fake(nil, 0, errBoom)}, {Provider: "b", Proc: backup}}})
	r.say(t, "hi")
	r.waitFor(t, func() bool { _, _, _, e := r.summary(); return e == 1 })
	if backup.Started.Load() != 0 {
		t.Fatal("replayed a generation that had executed tools")
	}
}

func TestOpenBreakerCandidateIsTriedLast(t *testing.T) {
	primary, backup := fake([]string{"ok"}, 0, nil), fake([]string{"fb"}, 0, nil)
	r := newFailoverRig(t, core.LLMFailoverConfig{Candidates: []core.LLMCandidate{
		{Provider: "a", Proc: primary, Open: func() bool { return true }}, {Provider: "b", Proc: backup}}})
	r.say(t, "hi")
	r.waitFor(t, func() bool { _, _, f, _ := r.summary(); return f == 1 })
	if _, deltas, _, _ := r.summary(); deltas != "fb" || primary.Started.Load() != 0 {
		t.Fatalf("deltas=%q primaryStarted=%d", deltas, primary.Started.Load())
	}
}

func TestBargeInCancelsTheRunningCandidate(t *testing.T) {
	primary := fake(nil, 0, errBoom)
	backup := coretest.NewLLMProcessor(coretest.LLMConfig{ResponseChunks: []string{"slow ", "answer"}, ChunkDelay: 300 * time.Millisecond})
	r := newFailoverRig(t, core.LLMFailoverConfig{Candidates: []core.LLMCandidate{
		{Provider: "a", Proc: primary}, {Provider: "b", Proc: backup}}})
	r.say(t, "hi")
	r.waitFor(t, func() bool { return backup.Started.Load() == 1 })
	if err := r.sess.Post(&core.InterruptionFrame{}); err != nil {
		t.Fatal(err)
	}
	r.waitFor(t, func() bool { return backup.CanceledCount.Load() == 1 })
	time.Sleep(400 * time.Millisecond)
	if _, deltas, finals, _ := r.summary(); deltas != "" || finals != 0 {
		t.Fatalf("output after barge-in: %q finals=%d", deltas, finals)
	}
}

// straggler fails, then (after the failover) re-enters a late delta under a
// newer generation than the one it failed in.
type straggler struct {
	s   core.SessionController
	idx int
}

func (p *straggler) Attach(s core.SessionController, i int) error { p.s, p.idx = s, i; return nil }
func (p *straggler) ProcessFrame(_ context.Context, f core.Frame, d core.Direction, e core.Emitter) error {
	um, ok := f.(*core.UserMessageFrame)
	if !ok {
		return e.PushDirection(f, d)
	}
	if err := e.Push(f); err != nil {
		return err
	}
	gen := um.Gen
	return p.s.Go(func(context.Context) {
		_ = p.s.Reenter(&core.ErrorFrame{Err: errBoom}, core.Downstream, p.idx+1, gen)
		time.Sleep(30 * time.Millisecond)
		_ = p.s.Reenter(&core.LLMTextDeltaFrame{Delta: "STALE", Turn: um.Turn, Gen: gen + 1}, core.Downstream, p.idx+1, gen+1)
	})
}

func TestAbandonedCandidateOutputIsDropped(t *testing.T) {
	r := newFailoverRig(t, core.LLMFailoverConfig{Candidates: []core.LLMCandidate{
		{Provider: "a", Proc: &straggler{}}, {Provider: "b", Proc: fake([]string{"ok"}, 0, nil)}}})
	r.say(t, "hi")
	r.waitFor(t, func() bool { _, _, f, _ := r.summary(); return f == 1 })
	time.Sleep(80 * time.Millisecond)
	if _, deltas, _, errs := r.summary(); deltas != "ok" || errs != 0 {
		t.Fatalf("deltas=%q errs=%d", deltas, errs)
	}
}

// silent starts a generation (forwarding the user message) and then says
// nothing until released, like a hung provider. Interruption cancels it; once
// released it speaks "late" (a recovering primary).
type silent struct {
	s       core.SessionController
	idx     int
	release chan struct{}
	started atomic.Uint64
	stopped atomic.Uint64
}

func newSilent() *silent { return &silent{release: make(chan struct{})} }

func (p *silent) Attach(s core.SessionController, i int) error { p.s, p.idx = s, i; return nil }
func (p *silent) ProcessFrame(ctx context.Context, f core.Frame, d core.Direction, e core.Emitter) error {
	switch fr := f.(type) {
	case *core.UserMessageFrame:
		if err := e.PushDirection(f, d); err != nil {
			return err
		}
		p.started.Add(1)
		gen := fr.Gen
		return p.s.Go(func(c context.Context) {
			select {
			case <-p.release:
				_ = p.s.Reenter(&core.LLMTextDeltaFrame{Delta: "LATE", Turn: fr.Turn, Gen: gen}, core.Downstream, p.idx+1, gen)
				_ = p.s.Reenter(&core.LLMTextFrame{Text: "LATE", Turn: fr.Turn, Gen: gen}, core.Downstream, p.idx+1, gen)
			case <-c.Done():
			}
		})
	case *core.InterruptionFrame:
		p.stopped.Add(1)
	}
	return e.PushDirection(f, d)
}

func TestHungPrimaryFailsOverWithinFirstTokenDeadline(t *testing.T) {
	primary := newSilent()
	var timedOut atomic.Bool
	r := newFailoverRig(t, core.LLMFailoverConfig{
		Candidates: []core.LLMCandidate{
			{Provider: "a", Proc: primary, OnFirstTokenTimeout: func() { timedOut.Store(true) }},
			{Provider: "b", Proc: fake([]string{"hel", "lo"}, 0, nil)}},
		Classify: func(err error) string {
			return map[bool]string{true: "timeout"}[errors.Is(err, core.ErrFirstTokenTimeout)]
		},
		Budget:            5 * time.Second,
		FirstTokenTimeout: 60 * time.Millisecond,
	})
	start := time.Now()
	r.say(t, "hi")
	r.waitFor(t, func() bool { _, _, f, _ := r.summary(); return f == 1 })
	if d := time.Since(start); d > time.Second {
		t.Fatalf("failover took %v", d)
	}
	if users, deltas, finals, errs := r.summary(); users != 1 || deltas != "hello" || finals != 1 || errs != 0 {
		t.Fatalf("users=%d deltas=%q finals=%d errs=%d", users, deltas, finals, errs)
	}
	if got := r.switched(); len(got) != 1 || got[0] != "a>b:timeout" {
		t.Fatalf("switches %v", got)
	}
	if !timedOut.Load() || primary.stopped.Load() != 1 {
		t.Fatalf("breaker hook=%v primary cancels=%d", timedOut.Load(), primary.stopped.Load())
	}
}

func TestRecoveringPrimaryDoesNotDoubleSpeak(t *testing.T) {
	primary := newSilent()
	r := newFailoverRig(t, core.LLMFailoverConfig{
		Candidates:        []core.LLMCandidate{{Provider: "a", Proc: primary}, {Provider: "b", Proc: fake([]string{"ok"}, 0, nil)}},
		FirstTokenTimeout: 40 * time.Millisecond,
	})
	r.say(t, "hi")
	r.waitFor(t, func() bool { _, _, f, _ := r.summary(); return f == 1 })
	close(primary.release) // the abandoned primary wakes up and answers
	time.Sleep(100 * time.Millisecond)
	if _, deltas, finals, errs := r.summary(); deltas != "ok" || finals != 1 || errs != 0 {
		t.Fatalf("deltas=%q finals=%d errs=%d", deltas, finals, errs)
	}
}

func TestSlowHealthyGenerationIsNotCut(t *testing.T) {
	// first token inside the deadline, whole answer ~8x longer than it
	slow := coretest.NewLLMProcessor(coretest.LLMConfig{ResponseChunks: []string{"a", "b", "c", "d", "e", "f", "g", "h"}, ChunkDelay: 40 * time.Millisecond})
	backup := fake([]string{"BACKUP"}, 0, nil)
	r := newFailoverRig(t, core.LLMFailoverConfig{
		Candidates:        []core.LLMCandidate{{Provider: "a", Proc: slow}, {Provider: "b", Proc: backup}},
		FirstTokenTimeout: 100 * time.Millisecond,
	})
	r.say(t, "hi")
	r.waitFor(t, func() bool { _, _, f, _ := r.summary(); return f == 1 })
	if _, deltas, _, errs := r.summary(); deltas != "abcdefgh" || errs != 0 || backup.Started.Load() != 0 {
		t.Fatalf("deltas=%q errs=%d backup started=%d", deltas, errs, backup.Started.Load())
	}
}

func TestHungPrimaryWithoutFallbackBudgetIsLeftAlone(t *testing.T) {
	// the budget has already closed when the deadline fires: no failover
	primary, backup := newSilent(), fake([]string{"ok"}, 0, nil)
	r := newFailoverRig(t, core.LLMFailoverConfig{
		Candidates:        []core.LLMCandidate{{Provider: "a", Proc: primary}, {Provider: "b", Proc: backup}},
		Budget:            20 * time.Millisecond,
		FirstTokenTimeout: 60 * time.Millisecond,
	})
	r.say(t, "hi")
	time.Sleep(200 * time.Millisecond)
	if backup.Started.Load() != 0 || primary.stopped.Load() != 0 {
		t.Fatalf("backup=%d primary cancels=%d", backup.Started.Load(), primary.stopped.Load())
	}
}

func TestFirstTokenAtTheDeadlineSpeaksOnce(t *testing.T) {
	// the primary's first token and the watchdog land together: whichever wins
	// under the lock answers, the other is dropped; never both, never neither.
	for i := 0; i < 20; i++ {
		primary := newSilent()
		r := newFailoverRig(t, core.LLMFailoverConfig{
			Candidates:        []core.LLMCandidate{{Provider: "a", Proc: primary}, {Provider: "b", Proc: fake([]string{"ok"}, 0, nil)}},
			FirstTokenTimeout: 20 * time.Millisecond,
		})
		r.say(t, "hi")
		r.waitFor(t, func() bool { return primary.started.Load() == 1 })
		time.AfterFunc(20*time.Millisecond, func() { close(primary.release) })
		r.waitFor(t, func() bool { _, _, f, _ := r.summary(); return f >= 1 })
		time.Sleep(40 * time.Millisecond)
		if _, deltas, finals, errs := r.summary(); (deltas != "ok" && deltas != "LATE") || finals != 1 || errs != 0 {
			t.Fatalf("run %d: deltas=%q finals=%d errs=%d", i, deltas, finals, errs)
		}
	}
}

func TestToolStartedBeforeDeadlineBlocksWatchdog(t *testing.T) {
	// a tool-only first round: no text, but a tool ran, so the slow silence
	// that follows (the tool, then round two) must not replay the turn
	var effects atomic.Uint64
	primary, backup := newSilent(), fake([]string{"x"}, 0, nil)
	r := newFailoverRig(t, core.LLMFailoverConfig{
		SideEffects:       effects.Load,
		Candidates:        []core.LLMCandidate{{Provider: "a", Proc: primary}, {Provider: "b", Proc: backup}},
		FirstTokenTimeout: 40 * time.Millisecond,
	})
	r.say(t, "hi")
	r.waitFor(t, func() bool { return primary.started.Load() == 1 })
	effects.Add(1)
	time.Sleep(120 * time.Millisecond)
	if backup.Started.Load() != 0 || primary.stopped.Load() != 0 {
		t.Fatalf("backup=%d primary cancels=%d", backup.Started.Load(), primary.stopped.Load())
	}
}

func TestBargeInBeforeDeadlineStopsWatchdog(t *testing.T) {
	primary, backup := newSilent(), fake([]string{"x"}, 0, nil)
	r := newFailoverRig(t, core.LLMFailoverConfig{
		Candidates:        []core.LLMCandidate{{Provider: "a", Proc: primary}, {Provider: "b", Proc: backup}},
		FirstTokenTimeout: 40 * time.Millisecond,
	})
	r.say(t, "hi")
	r.waitFor(t, func() bool { return primary.started.Load() == 1 })
	if err := r.sess.Post(&core.InterruptionFrame{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(120 * time.Millisecond)
	if backup.Started.Load() != 0 {
		t.Fatal("watchdog failed over an interrupted generation")
	}
}

func TestCloseStopsFirstTokenWatchdog(t *testing.T) {
	primary, backup := newSilent(), fake([]string{"x"}, 0, nil)
	r := newFailoverRig(t, core.LLMFailoverConfig{
		Candidates:        []core.LLMCandidate{{Provider: "a", Proc: primary}, {Provider: "b", Proc: backup}},
		FirstTokenTimeout: 200 * time.Millisecond,
	})
	r.say(t, "hi")
	r.waitFor(t, func() bool { return primary.started.Load() == 1 })
	if err := r.w.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	if backup.Started.Load() != 0 {
		t.Fatal("watchdog started a fallback after Close")
	}
}

// toolCaller announces one tool call and, once released, settles it at the
// generation it ran in, like a provider's tool loop.
type toolCaller struct {
	s       core.SessionController
	idx     int
	release chan struct{}
	started atomic.Bool
	late    bool // announce only after release too (an attempt abandoned before its call frame)
}

func (p *toolCaller) Attach(s core.SessionController, i int) error { p.s, p.idx = s, i; return nil }
func (p *toolCaller) ProcessFrame(_ context.Context, f core.Frame, d core.Direction, e core.Emitter) error {
	um, ok := f.(*core.UserMessageFrame)
	if !ok {
		return e.PushDirection(f, d)
	}
	if err := e.Push(f); err != nil {
		return err
	}
	gen := um.Gen
	call := core.ToolCall{ID: "call-1", Name: "lookup", SessionID: "s", TurnID: core.TurnID(um.Turn), Generation: core.Generation(gen)}
	return p.s.Go(func(ctx context.Context) {
		if !p.late {
			_ = p.s.Reenter(&core.ToolCallFrame{Call: call}, core.Downstream, p.idx+1, gen)
		}
		p.started.Store(true)
		select {
		case <-p.release:
		case <-ctx.Done():
			return
		}
		if p.late {
			_ = p.s.Reenter(&core.ToolCallFrame{Call: call}, core.Downstream, p.idx+1, gen)
		}
		_ = p.s.Reenter(&core.ToolResultFrame{Turn: call.TurnID, Gen: call.Generation, Result: core.ToolResult{CallID: call.ID, Status: core.ToolCancelled}}, core.Downstream, p.idx+1, gen)
	})
}

func countOf[T core.Frame](frames []core.Frame) int {
	n := 0
	for _, f := range frames {
		if _, ok := f.(T); ok {
			n++
		}
	}
	return n
}

func TestToolResultOfAnInterruptedCandidateStillReachesTheSession(t *testing.T) {
	primary := &toolCaller{release: make(chan struct{})}
	r := newFailoverRig(t, core.LLMFailoverConfig{Candidates: []core.LLMCandidate{
		{Provider: "a", Proc: primary}, {Provider: "b", Proc: fake([]string{"x"}, 0, nil)}}})
	r.say(t, "hi")
	r.waitFor(t, func() bool { return primary.started.Load() && countOf[*core.ToolCallFrame](r.rec.GetFrames()) == 1 })
	if err := r.sess.Post(&core.InterruptionFrame{}); err != nil {
		t.Fatal(err)
	}
	r.waitFor(t, func() bool { return countOf[*core.InterruptionFrame](r.rec.GetFrames()) >= 1 })
	close(primary.release)
	r.waitFor(t, func() bool { return countOf[*core.ToolResultFrame](r.rec.GetFrames()) == 1 })
}

func TestToolCallOfAnAbandonedCandidateStillReachesTheSession(t *testing.T) {
	primary := &toolCaller{release: make(chan struct{}), late: true}
	r := newFailoverRig(t, core.LLMFailoverConfig{Candidates: []core.LLMCandidate{
		{Provider: "a", Proc: primary}, {Provider: "b", Proc: fake([]string{"x"}, 0, nil)}}})
	r.say(t, "hi")
	r.waitFor(t, func() bool { return primary.started.Load() })
	if err := r.sess.Post(&core.InterruptionFrame{}); err != nil {
		t.Fatal(err)
	}
	r.waitFor(t, func() bool { return countOf[*core.InterruptionFrame](r.rec.GetFrames()) >= 1 })
	close(primary.release) // the attempt is done: its loop announced the call before it saw the interruption
	r.waitFor(t, func() bool {
		f := r.rec.GetFrames()
		return countOf[*core.ToolCallFrame](f) == 1 && countOf[*core.ToolResultFrame](f) == 1
	})
}

// Pin (passes before this task too): a tool call is the candidate's first
// output, so the first-token watchdog never replays a turn whose tool runs long.
func TestToolCallIsTheFirstOutput(t *testing.T) {
	primary, backup := &toolCaller{release: make(chan struct{})}, fake([]string{"x"}, 0, nil)
	r := newFailoverRig(t, core.LLMFailoverConfig{
		Candidates:        []core.LLMCandidate{{Provider: "a", Proc: primary}, {Provider: "b", Proc: backup}},
		FirstTokenTimeout: 40 * time.Millisecond,
	})
	r.say(t, "hi")
	r.waitFor(t, func() bool { return primary.started.Load() })
	time.Sleep(120 * time.Millisecond) // past the deadline: asserts an absence
	if backup.Started.Load() != 0 {
		t.Fatal("a turn whose model called a tool was replayed on the fallback")
	}
}

// syncFailLLM refuses a user message synchronously, as a provider whose request cannot start does;
// push says whether it forwarded the message first (every production LLM does).
type syncFailLLM struct {
	err   error
	push  bool
	calls atomic.Int32
}

func (p *syncFailLLM) ProcessFrame(_ context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	if _, ok := f.(*core.UserMessageFrame); !ok {
		return e.PushDirection(f, dir)
	}
	p.calls.Add(1)
	if p.push {
		if err := e.PushDirection(f, dir); err != nil {
			return err
		}
	}
	return p.err
}

func TestSyncErrorFromThePrimaryFailsOver(t *testing.T) {
	for _, tc := range []struct {
		name string
		push bool
	}{{"after forwarding the turn", true}, {"before forwarding the turn", false}} {
		t.Run(tc.name, func(t *testing.T) {
			primary, backup := &syncFailLLM{err: errBoom, push: tc.push}, fake([]string{"hel", "lo"}, 0, nil)
			r := newFailoverRig(t, core.LLMFailoverConfig{Candidates: []core.LLMCandidate{
				{Provider: "a", Proc: primary}, {Provider: "b", Proc: backup}}})
			r.say(t, "hi")
			r.waitFor(t, func() bool { _, _, f, _ := r.summary(); return f == 1 })
			users, deltas, finals, errs := r.summary()
			if users != 1 || deltas != "hello" || finals != 1 || errs != 0 {
				t.Fatalf("users=%d deltas=%q finals=%d errs=%d", users, deltas, finals, errs)
			}
			if err := r.sess.Err(); err != nil {
				t.Fatalf("a synchronous provider error failed the session: %v", err)
			}
			if got := r.switched(); len(got) != 1 || got[0] != "a>b:server_error" {
				t.Fatalf("switches %v", got)
			}
		})
	}
}

// The session's own task budget is not a provider outage: no jump, the error stays the session's.
func TestLocalSyncErrorNeverFailsOver(t *testing.T) {
	primary, backup := &syncFailLLM{err: core.ErrTooManyTasks, push: true}, fake([]string{"x"}, 0, nil)
	r := newFailoverRig(t, core.LLMFailoverConfig{
		Candidates: []core.LLMCandidate{{Provider: "a", Proc: primary}, {Provider: "b", Proc: backup}},
		Classify: func(err error) string {
			if errors.Is(err, core.ErrTooManyTasks) {
				return ""
			}
			return "server_error"
		},
	})
	r.say(t, "hi")
	r.waitFor(t, func() bool { return r.sess.Err() != nil })
	if !errors.Is(r.sess.Err(), core.ErrTooManyTasks) || backup.Started.Load() != 0 || len(r.switched()) != 0 {
		t.Fatalf("err=%v backupStarted=%d switches=%v", r.sess.Err(), backup.Started.Load(), r.switched())
	}
}

var errRefused = errors.New("a later stage refused the turn")

// refuseUser is a stage after the LLM that refuses every user message.
type refuseUser struct{}

func (refuseUser) ProcessFrame(_ context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	if _, ok := f.(*core.UserMessageFrame); ok {
		return errRefused
	}
	return e.PushDirection(f, dir)
}

// A later stage refusing the user message is not the candidate's failure (pin: true before and after).
func TestADownstreamRefusalIsNotTheCandidatesFailure(t *testing.T) {
	backup := fake([]string{"x"}, 0, nil)
	w, err := core.NewLLMFailover(core.LLMFailoverConfig{Classify: func(error) string { return "server_error" },
		Candidates: []core.LLMCandidate{{Provider: "a", Proc: fake([]string{"never"}, 0, nil)}, {Provider: "b", Proc: backup}}})
	if err != nil {
		t.Fatal(err)
	}
	pipe, err := core.NewPipeline(w, refuseUser{})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := core.NewSession(901, pipe, core.DefaultResourceBudget())
	if err != nil || sess.Start() != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()
	if err := sess.Post(&core.UserMessageFrame{Turn: "t1", Content: "hi", Gen: sess.CurrentGeneration(), Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(2 * time.Second); sess.Err() == nil; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("the refusal did not reach the session")
		}
	}
	if !errors.Is(sess.Err(), errRefused) || backup.Started.Load() != 0 {
		t.Fatalf("err=%v backupStarted=%d", sess.Err(), backup.Started.Load())
	}
}
