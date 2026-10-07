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
)

var errBoom = errors.New("503 upstream")

type failoverRig struct {
	sess     *core.Session
	rec      *core.PassthroughProcessor
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
	r := &failoverRig{rec: &core.PassthroughProcessor{}, events: make(chan core.DebugEvent, 16)}
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

func fake(chunks []string, failAfter int, err error) *core.FakeLLMProcessor {
	return core.NewFakeLLMProcessor(core.FakeLLMConfig{ResponseChunks: chunks, FailWith: err, FailAfter: failAfter})
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
	backup := core.NewFakeLLMProcessor(core.FakeLLMConfig{ResponseChunks: []string{"slow ", "answer"}, ChunkDelay: 300 * time.Millisecond})
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
