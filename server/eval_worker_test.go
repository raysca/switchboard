package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"govoice/server/agent"
	"govoice/server/eval"
	"govoice/server/storage"
)

// caseJobs builds the run and the jobs (one per case, repeat 0) the API will enqueue.
func caseJobs(t *testing.T, agentID, versionID string, cases ...eval.Case) (storage.EvalRun, []storage.EvalJob) {
	t.Helper()
	run := storage.EvalRun{ID: storage.NewID(), AgentID: agentID, AgentVersionID: versionID, ConfigHash: "h", Scope: "suite", Providers: eval.ModeEcho, Runs: 1, MinPass: 1, TriggeredBy: "api"}
	jobs := make([]storage.EvalJob, 0, len(cases))
	for i, c := range cases {
		id := fmt.Sprintf("case%d", i)
		payload, err := json.Marshal(eval.CaseJob{CaseID: id, CaseHash: "ch", Case: c})
		if err != nil {
			t.Fatal(err)
		}
		subject := id + "#0"
		jobs = append(jobs, storage.EvalJob{Kind: "run_case", Subject: subject, IdempotencyKey: "run:" + run.ID + ":" + subject, Payload: payload})
	}
	return run, jobs
}

func scriptCase(name string, lines ...string) eval.Case {
	return eval.Case{Name: name, Caller: eval.Caller{Script: lines}, Expect: eval.Expect{Path: []string{"front"}}}
}

func waitRun(t *testing.T, st *storage.Store, agentID, runID string) storage.EvalRun {
	t.Helper()
	var r storage.EvalRun
	eventually(t, "the run to finish", func() bool {
		var err error
		if r, err = st.Eval().GetRun(context.Background(), agentID, runID); err != nil {
			t.Fatal(err)
		}
		return r.Status == "done"
	})
	return r
}

func TestEvalWorkerRunsQueuedCase(t *testing.T) {
	s := evalTestServer(t)
	a, err := s.agents.Create(t.Context(), agent.CreateRequest{Name: "Dealership", Config: agentConfigJSON("SALES", "salesvoice")})
	if err != nil || a.Readiness.State != agent.ReadinessReady {
		t.Fatalf("agent: %+v err=%v", a.Readiness, err)
	}
	run, jobs := caseJobs(t, a.ID, a.Version.ID, scriptCase("greets", "hello"))
	run, err = s.store.Eval().CreateRun(t.Context(), run, jobs)
	if err != nil {
		t.Fatal(err)
	}
	r := waitRun(t, s.store, a.ID, run.ID)
	if r.PassedCases == nil || *r.PassedCases != 1 || *r.TotalCases != 1 {
		t.Fatalf("run = passed %v of %v, want 1 of 1", r.PassedCases, r.TotalCases)
	}
	rs, err := s.store.Eval().RunResults(t.Context(), run.ID)
	if err != nil || len(rs) != len(eval.Builtins()) {
		t.Fatalf("results = %d (%v), want one per built-in scorer", len(rs), err)
	}
	for _, e := range rs {
		if e.SubjectID != storage.EvalCaseRunSubject(run.ID, "case0#0") || e.VersionKey != a.Version.ID || e.AgentVersionID != a.Version.ID || e.SessionID == "" || e.CaseID != "case0" {
			t.Fatalf("result row = %+v", e)
		}
		if e.Outcome == "skipped" {
			if e.Reason != "no judge configured" && e.Reason != "no tool expectations" {
				t.Errorf("%s skipped for %q", e.Scorer, e.Reason)
			}
		} else if e.Outcome != "pass" {
			t.Errorf("%s = %s (%s), want pass", e.Scorer, e.Outcome, e.Reason)
		}
	}
	if got := s.metrics.evalJobs[evalDone].Load(); got != 1 {
		t.Errorf("done jobs counted = %d, want 1", got)
	}
	if got := s.metrics.EvalRunsFinished.Load(); got != 1 {
		t.Errorf("runs finished counted = %d, want 1", got)
	}
	if got := s.metrics.evalCallsActive.Load(); got != 0 {
		t.Errorf("calls active after the run = %d, want 0", got)
	}
}

func TestEvalWorkerStopsOnShutdown(t *testing.T) {
	before := goleak.IgnoreCurrent()
	s := evalTestServer(t)
	if s.mgmt.Load().eval.Load() == nil {
		t.Fatal("the worker did not start with the server")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	s.closeManagement()
	if err := goleak.Find(before); err != nil {
		t.Fatalf("goroutines left after Shutdown: %v", err)
	}
}

func TestEvalWorkerDisabledAtZeroCap(t *testing.T) {
	s := evalTestServer(t, func(c *Config) { c.Evaluation.MaxConcurrentCalls = 0 })
	if s.mgmt.Load().eval.Load() != nil {
		t.Fatal("MaxConcurrentCalls=0 must not start a worker")
	}
	a, err := s.agents.Create(t.Context(), agent.CreateRequest{Name: "A", Config: agentConfigJSON("SALES", "v")})
	if err != nil {
		t.Fatal(err)
	}
	run, jobs := caseJobs(t, a.ID, a.Version.ID, scriptCase("c", "hello"))
	if _, err := s.store.Eval().CreateRun(t.Context(), run, jobs); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2*evalIdlePoll + 300*time.Millisecond)
	got, err := s.store.Eval().RunJobs(t.Context(), run.ID)
	if err != nil || len(got) != 1 || got[0].Status != "pending" || got[0].Attempts != 0 {
		t.Fatalf("job = %+v (%v), want untouched pending", got, err)
	}
}

func TestEvalBackoff(t *testing.T) {
	for n, want := range map[int]time.Duration{0: 30 * time.Second, 1: 30 * time.Second, 2: time.Minute, 3: 2 * time.Minute, 5: 8 * time.Minute, 6: 10 * time.Minute, 40: 10 * time.Minute} {
		if got := evalBackoff(n); got != want {
			t.Errorf("evalBackoff(%d) = %v, want %v", n, got, want)
		}
	}
}

// queueRig is a server with a store and agents but no listeners: workers built on it run an injected job function.
type queueRig struct {
	s       *Server
	agentID string
	version string
}

func newQueueRig(t *testing.T) *queueRig {
	t.Helper()
	s := apiTestServer(t)
	enableTestManagement(t, s)
	a, err := s.agents.Create(t.Context(), agent.CreateRequest{Name: "Q", Config: agentConfigJSON("SALES", "v")})
	if err != nil {
		t.Fatal(err)
	}
	return &queueRig{s: s, agentID: a.ID, version: a.Version.ID}
}

// worker starts a pool of n slots on st running fn; fast timings, and the pool stops with the test.
func (q *queueRig) worker(t *testing.T, st *storage.Store, n int, fn evalRunFunc, mods ...func(*evalWorker)) *evalWorker {
	t.Helper()
	w := newEvalWorker(q.s, st)
	w.run, w.idle, w.backoff = fn, 5*time.Millisecond, func(int) time.Duration { return 0 }
	for _, m := range mods {
		m(w)
	}
	w.start(n)
	t.Cleanup(w.stop)
	return w
}

func (q *queueRig) enqueue(t *testing.T, n int) (storage.EvalRun, []storage.EvalJob) {
	t.Helper()
	cases := make([]eval.Case, n)
	for i := range cases {
		cases[i] = scriptCase(fmt.Sprintf("c%d", i), "hi")
	}
	run, jobs := caseJobs(t, q.agentID, q.version, cases...)
	run, err := q.s.store.Eval().CreateRun(t.Context(), run, jobs)
	if err != nil {
		t.Fatal(err)
	}
	got, err := q.s.store.Eval().RunJobs(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return run, got
}

func passResult(run storage.EvalRun, version, scorer, reason string) storage.EvalResult {
	return storage.EvalResult{SubjectKind: "case_run", SubjectID: "bare", Scorer: scorer, ScorerVersion: 1, VersionKey: version, AgentID: run.AgentID,
		AgentVersionID: version, ConfigHash: "h", SessionID: storage.NewID(), Score: 1, Outcome: "pass", Reason: reason}
}

func TestEvalWorkerRecoversPanic(t *testing.T) {
	q := newQueueRig(t)
	runA, jobsA := q.enqueue(t, 1)
	var calls atomic.Int32
	q.worker(t, q.s.store, 1, func(ctx context.Context, job storage.EvalJob) ([]storage.EvalResult, string, bool, error) {
		calls.Add(1)
		if job.RunID == runA.ID {
			panic("scorer exploded with caller text 4111 1111 1111 1111")
		}
		return []storage.EvalResult{passResult(runA, q.version, "expectations", "ok")}, "", false, nil
	}, func(w *evalWorker) { w.backoff = func(int) time.Duration { return time.Hour } })
	eventually(t, "the panicking job to return to pending", func() bool {
		j := mustJob(t, q.s.store, runA.ID, jobsA[0].ID)
		return j.Status == "pending" && j.LastErrorCategory == "panic" && j.Attempts == 1
	})
	// The pool survived: a second run is served by the same single slot.
	runB, _ := q.enqueue(t, 1)
	r := waitRun(t, q.s.store, q.agentID, runB.ID)
	if *r.PassedCases != 1 {
		t.Fatalf("run B passed %d, want 1", *r.PassedCases)
	}
	if got := q.s.metrics.evalJobs[evalRetry].Load(); got != 1 {
		t.Errorf("retries counted = %d, want 1", got)
	}
}

func mustJob(t *testing.T, st *storage.Store, runID, id string) storage.EvalJob {
	t.Helper()
	jobs, err := st.Eval().RunJobs(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		if j.ID == id {
			return j
		}
	}
	t.Fatalf("job %s not found", id)
	return storage.EvalJob{}
}

func TestEvalWorkerFailsWithCategoryBacksOffThenDies(t *testing.T) {
	q := newQueueRig(t)
	run, jobs := q.enqueue(t, 1)
	var calls atomic.Int32
	// The real backoff: the first failure parks the job about 30 s ahead.
	q.worker(t, q.s.store, 1, func(context.Context, storage.EvalJob) ([]storage.EvalResult, string, bool, error) {
		calls.Add(1)
		return nil, "token", false, errors.New("voice token: boom with secret-text")
	}, func(w *evalWorker) { w.backoff = evalBackoff })
	eventually(t, "the job to be parked", func() bool { return mustJob(t, q.s.store, run.ID, jobs[0].ID).LastErrorCategory == "token" })
	j := mustJob(t, q.s.store, run.ID, jobs[0].ID)
	if d := time.Until(j.RunAfter); j.Status != "pending" || d < 25*time.Second || d > 31*time.Second {
		t.Fatalf("job = %s, due in %v, want pending about 30 s ahead", j.Status, d)
	}
	if calls.Load() != 1 {
		t.Fatalf("ran %d times before its backoff, want 1", calls.Load())
	}

	// With no backoff, the attempts run out: dead, the run finishes with the case failed, only the category is kept.
	q2 := newQueueRig(t)
	run2, jobs2 := q2.enqueue(t, 1)
	q2.worker(t, q2.s.store, 1, func(context.Context, storage.EvalJob) ([]storage.EvalResult, string, bool, error) {
		return nil, "token", false, errors.New("voice token: boom")
	})
	r := waitRun(t, q2.s.store, q2.agentID, run2.ID)
	j2 := mustJob(t, q2.s.store, run2.ID, jobs2[0].ID)
	if j2.Status != "dead" || j2.Attempts != 3 || j2.LastErrorCategory != "token" || *r.PassedCases != 0 || *r.TotalCases != 1 {
		t.Fatalf("job %+v run %+v, want dead after 3 attempts with category token and 0 of 1 passed", j2, r)
	}
	if q2.s.metrics.evalJobs[evalDead].Load() != 1 || q2.s.metrics.evalJobs[evalRetry].Load() != 2 {
		t.Errorf("metrics dead=%d retry=%d, want 1 and 2", q2.s.metrics.evalJobs[evalDead].Load(), q2.s.metrics.evalJobs[evalRetry].Load())
	}
}

func TestEvalWorkerFinalFailureIsDeadAtOnce(t *testing.T) {
	q := newQueueRig(t)
	run, jobs := q.enqueue(t, 1)
	q.worker(t, q.s.store, 1, func(context.Context, storage.EvalJob) ([]storage.EvalResult, string, bool, error) {
		return nil, "persona_unavailable", true, errors.New("no offline endpoint")
	})
	waitRun(t, q.s.store, q.agentID, run.ID)
	if j := mustJob(t, q.s.store, run.ID, jobs[0].ID); j.Status != "dead" || j.Attempts != 1 || j.LastErrorCategory != "persona_unavailable" {
		t.Fatalf("job = %+v, want dead after one attempt", j)
	}
}

// A stop in the middle of a job writes nothing: the lease expires and the next claimer takes over as attempt 2.
func TestEvalWorkerStopMidJobLeavesTheLeaseToExpire(t *testing.T) {
	q := newQueueRig(t)
	before := goleak.IgnoreCurrent() // after the rig: its store and trace goroutines end with the test, the worker's must end with stop
	run, jobs := q.enqueue(t, 1)
	started := make(chan struct{})
	w := q.worker(t, q.s.store, 2, func(ctx context.Context, job storage.EvalJob) ([]storage.EvalResult, string, bool, error) {
		close(started)
		<-ctx.Done()
		return nil, "call", false, ctx.Err()
	})
	<-started
	w.stop()
	j := mustJob(t, q.s.store, run.ID, jobs[0].ID)
	if j.Status != "running" || j.LeaseOwner == "" || j.LastErrorCategory != "" {
		t.Fatalf("job after stop = %+v, want left running under its lease with no failure written", j)
	}
	if got := q.s.metrics.evalJobs[evalRetry].Load() + q.s.metrics.evalJobs[evalDead].Load(); got != 0 {
		t.Errorf("a stop counted %d failures", got)
	}
	next, ok, err := q.s.store.Eval().Claim(t.Context(), "next", time.Now().Add(evalLease+time.Second), evalLease)
	if err != nil || !ok || next.ID != j.ID || next.Attempts != 2 {
		t.Fatalf("claim after the lease expired = %+v %v %v, want attempt 2 of the same job", next, ok, err)
	}
	if err := goleak.Find(before); err != nil {
		t.Fatalf("goroutines left after stop: %v", err)
	}
}

// A worker that lost its lease must not write: not through Heartbeat noticing it, and not through the fence on Complete.
func TestEvalWorkerLostLeaseWritesNothing(t *testing.T) {
	for _, tc := range []struct {
		name      string
		heartbeat time.Duration
	}{{"heartbeat notices", 10 * time.Millisecond}, {"complete is fenced", time.Hour}} {
		t.Run(tc.name, func(t *testing.T) {
			q := newQueueRig(t)
			run, jobs := q.enqueue(t, 1)
			running, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			q.worker(t, q.s.store, 1, func(ctx context.Context, job storage.EvalJob) ([]storage.EvalResult, string, bool, error) {
				once.Do(func() { close(running) })
				if job.Attempts > 1 {
					return []storage.EvalResult{passResult(run, q.version, "expectations", "thief")}, "", false, nil
				}
				<-release // ignores ctx on purpose: a stale worker that still returns results
				return []storage.EvalResult{passResult(run, q.version, "expectations", "stale")}, "", false, nil
			}, func(w *evalWorker) { w.heartbeat = tc.heartbeat })
			<-running
			// Another instance takes the job after the lease expired (a worker-supplied clock, spec "Clock skew").
			thief, ok, err := q.s.store.Eval().Claim(t.Context(), "thief", time.Now().Add(evalLease+time.Second), evalLease)
			if err != nil || !ok || thief.ID != jobs[0].ID || thief.Attempts != 2 {
				t.Fatalf("thief claim = %+v %v %v", thief, ok, err)
			}
			if tc.heartbeat < time.Second {
				eventually(t, "the heartbeat to see the lost lease", func() bool { return q.s.metrics.evalJobs[evalLeaseLost].Load() == 1 })
			}
			close(release)
			if tc.heartbeat >= time.Second {
				eventually(t, "the fenced Complete to be counted", func() bool { return q.s.metrics.evalJobs[evalLeaseLost].Load() == 1 })
			}
			if _, err := q.s.store.Eval().Complete(t.Context(), thief, time.Now(), []storage.EvalResult{passResult(run, q.version, "expectations", "thief")}); err != nil {
				t.Fatalf("the thief's Complete: %v", err)
			}
			rs, err := q.s.store.Eval().RunResults(t.Context(), run.ID)
			if err != nil || len(rs) != 1 || rs[0].Reason != "thief" {
				t.Fatalf("results = %+v (%v), want only the thief's single row", rs, err)
			}
			if got := q.s.metrics.evalJobs[evalLeaseLost].Load(); got != 1 {
				t.Errorf("lease_lost counted %d times, want exactly 1", got)
			}
			if got := q.s.metrics.evalJobs[evalDone].Load(); got != 0 {
				t.Errorf("the stale worker counted %d done jobs", got)
			}
		})
	}
}

// Two pools on one database never run a job twice at once, nor twice at all when nothing fails.
func TestEvalWorkerTwoPoolsRunEachJobOnce(t *testing.T) {
	q := newQueueRig(t)
	stA, stB := q.s.store, q.s.store // SQLite: one connection, one store
	if dsn := os.Getenv("TEST_POSTGRES_DSN"); dsn != "" {
		open := func() *storage.Store {
			st, err := storage.Open(context.Background(), storage.Config{Dialect: storage.Postgres, DSN: dsn})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			return st
		}
		stA, stB = open(), open()
		for _, tbl := range []string{"eval_results", "eval_jobs", "eval_runs"} { // Claim serves every agent: start from an empty queue
			if _, err := stA.DB().ExecContext(context.Background(), "DELETE FROM "+tbl); err != nil {
				t.Fatal(err)
			}
		}
		// The agent lives in the server's SQLite store, so seed one in the shared database.
		a, err := agent.NewService(stA, q.s.availability).Create(t.Context(), agent.CreateRequest{Name: "PG", Config: agentConfigJSON("SALES", "v")})
		if err != nil {
			t.Fatal(err)
		}
		q.agentID, q.version = a.ID, a.Version.ID
	}
	q.s.store = stA
	run, jobs := q.enqueue(t, 40)
	var mu sync.Mutex
	inFlight, runs := map[string]int{}, map[string]int{}
	fn := func(ctx context.Context, job storage.EvalJob) ([]storage.EvalResult, string, bool, error) {
		mu.Lock()
		inFlight[job.ID]++
		runs[job.ID]++
		if inFlight[job.ID] > 1 {
			t.Errorf("job %s runs twice at once", job.ID)
		}
		mu.Unlock()
		time.Sleep(time.Duration(rand.IntN(4)) * time.Millisecond)
		mu.Lock()
		inFlight[job.ID]--
		mu.Unlock()
		return []storage.EvalResult{passResult(run, q.version, "expectations", "ok")}, "", false, nil
	}
	q.worker(t, stA, 3, fn)
	q.worker(t, stB, 3, fn)
	r := waitRun(t, stA, q.agentID, run.ID)
	if *r.PassedCases != 40 {
		t.Fatalf("passed %d of 40", *r.PassedCases)
	}
	for _, j := range jobs {
		if runs[j.ID] != 1 {
			t.Errorf("job %s ran %d times, want 1", j.ID, runs[j.ID])
		}
	}
	rs, err := stA.Eval().RunResults(t.Context(), run.ID)
	if err != nil || len(rs) != 40 {
		t.Fatalf("results = %d (%v), want 40", len(rs), err)
	}
}

func paidAgent(t *testing.T, s *Server) agent.Agent {
	t.Helper()
	a, err := s.agents.Create(t.Context(), agent.CreateRequest{Name: "Paid", Config: json.RawMessage(
		`{"stt":{"provider":"google","model":"chirp_2"},"llm":{"provider":"google","model":"gemini-2.0-flash"},"tts":{"provider":"google","voice":"en-US-Journey-F"}}`)})
	if err != nil || a.Readiness.State != agent.ReadinessReady {
		t.Fatalf("paid agent: %+v %v", a.Readiness, err)
	}
	return a
}

func sessionCount(t *testing.T, s *Server, agentID string) int {
	t.Helper()
	var n int
	if err := s.store.DB().QueryRowContext(context.Background(), "SELECT count(*) FROM sessions WHERE agent_id=?", agentID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The server gate belongs to the API; the worker enforces it again so that a job on the queue can never spend credit on its own.
func TestEvalWorkerRefusesPaidRunWithoutLive(t *testing.T) {
	s := evalTestServer(t)
	a := paidAgent(t, s)
	run, jobs := caseJobs(t, a.ID, a.Version.ID, scriptCase("c", "hello"))
	run.Providers = eval.ModeKeep
	run, err := s.store.Eval().CreateRun(t.Context(), run, jobs)
	if err != nil {
		t.Fatal(err)
	}
	r := waitRun(t, s.store, a.ID, run.ID)
	j := mustJob(t, s.store, run.ID, mustJobs(t, s.store, run.ID)[0].ID)
	if j.Status != "dead" || j.LastErrorCategory != "live_required" || j.Attempts != 1 || *r.PassedCases != 0 {
		t.Fatalf("job = %+v run passed %d, want dead at once with category live_required", j, *r.PassedCases)
	}
	if n := sessionCount(t, s, a.ID); n != 0 {
		t.Fatalf("%d calls were made for a run that needs GOVOICE_LIVE=1", n)
	}
}

func mustJobs(t *testing.T, st *storage.Store, runID string) []storage.EvalJob {
	t.Helper()
	js, err := st.Eval().RunJobs(context.Background(), runID)
	if err != nil || len(js) == 0 {
		t.Fatalf("jobs of %s: %v", runID, err)
	}
	return js
}

func TestEvalLiveGate(t *testing.T) {
	s := apiTestServer(t)
	paid := agentConfigJSONWith(t, "google")
	free := agentConfigJSON("SALES", "v")
	cases := []struct {
		name string
		live string
		cfg  json.RawMessage
		want bool
	}{
		{"paid without the env", "", paid, true},
		{"paid with =0", "0", paid, true},
		{"paid with =1", "1", paid, false},
		{"echo without the env", "", free, false},
	}
	for _, c := range cases {
		s.env = mapEnv(map[string]string{"GOVOICE_LIVE": c.live})
		if got := s.evalLiveRequired(c.cfg); got != c.want {
			t.Errorf("%s: evalLiveRequired = %v, want %v", c.name, got, c.want)
		}
	}
}

// agentConfigJSONWith is a single-step config whose LLM is the named provider.
func agentConfigJSONWith(t *testing.T, llm string) json.RawMessage {
	t.Helper()
	return json.RawMessage(`{"stt":{"provider":"echo","model":"s"},"llm":{"provider":"` + llm + `","model":"m"},"tts":{"provider":"echo","voice":"v"}}`)
}

func TestEvalWorkerSkipsArchivedAgent(t *testing.T) {
	s := evalTestServer(t)
	a, err := s.agents.Create(t.Context(), agent.CreateRequest{Name: "Gone", Config: agentConfigJSON("SALES", "v")})
	if err != nil {
		t.Fatal(err)
	}
	// Disable the worker's view for a moment: queue the run, archive, then let the pool see it.
	w := s.mgmt.Load().eval.Load()
	w.stop()
	run, jobs := caseJobs(t, a.ID, a.Version.ID, scriptCase("c", "hello"))
	if _, err := s.store.Eval().CreateRun(t.Context(), run, jobs); err != nil {
		t.Fatal(err)
	}
	if err := s.agents.Archive(t.Context(), a.ID); err != nil {
		t.Fatal(err)
	}
	w2 := newEvalWorker(s, s.store)
	w2.start(1)
	t.Cleanup(w2.stop)
	waitRun(t, s.store, a.ID, run.ID)
	j := mustJobs(t, s.store, run.ID)[0]
	if j.Status != "dead" || j.LastErrorCategory != "agent_gone" || j.Attempts != 1 {
		t.Fatalf("job = %+v, want dead with category agent_gone", j)
	}
	if n := sessionCount(t, s, a.ID); n != 0 {
		t.Fatalf("%d calls were made for an archived agent", n)
	}
}

func TestEvalHarnessCategory(t *testing.T) {
	for want, err := range map[string]error{
		"token":        fmt.Errorf("%w: %w", errVoiceToken, errors.New("too many drafts")),
		"turn_timeout": errors.New("turn 2: no settled reply within 45s"),
		"stored_rows":  errors.Join(errors.New("caller: x"), errors.New("session s did not end within 15s")),
		"caller":       errors.New("caller: persona model: boom"),
		"dial":         errors.New("call abc: dial tcp: refused"),
		"call":         errors.New("something else"),
	} {
		if got := evalHarnessCategory(err); got != want {
			t.Errorf("evalHarnessCategory(%v) = %q, want %q", err, got, want)
		}
	}
}

// A persona case needs the offline model; with no endpoint it can never run, so it is dead at once.
func TestEvalWorkerPersonaWithoutOfflineIsDeadAtOnce(t *testing.T) {
	s := evalTestServer(t, func(c *Config) { c.Providers.Offline.LLMEndpoint = "" })
	a, err := s.agents.Create(t.Context(), agent.CreateRequest{Name: "P", Config: agentConfigJSON("SALES", "v")})
	if err != nil {
		t.Fatal(err)
	}
	c := eval.Case{Name: "p", Caller: eval.Caller{Persona: &eval.Persona{Who: "a buyer", Goal: "buy", MaxTurns: 2}}}
	run, jobs := caseJobs(t, a.ID, a.Version.ID, c)
	if _, err := s.store.Eval().CreateRun(t.Context(), run, jobs); err != nil {
		t.Fatal(err)
	}
	waitRun(t, s.store, a.ID, run.ID)
	if j := mustJobs(t, s.store, run.ID)[0]; j.Status != "dead" || j.Attempts != 1 || j.LastErrorCategory != "persona_unavailable" {
		t.Fatalf("job = %+v, want dead after one attempt with category persona_unavailable", j)
	}
	if n := sessionCount(t, s, a.ID); n != 0 {
		t.Fatalf("%d calls were made", n)
	}
}
