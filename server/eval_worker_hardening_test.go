package server

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"govoice/server/agent"
	"govoice/server/eval"
	"govoice/server/storage"
)

type safeBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// captureLogs sends the server's log to a buffer. Call it before any worker starts.
func captureLogs(s *Server) *safeBuf {
	b := &safeBuf{}
	s.logger = slog.New(slog.NewTextHandler(b, nil))
	return b
}

const judgeVerdict = `{"choices":[{"message":{"role":"assistant","content":"{\"score\":1,\"reason\":\"ok\"}"}}]}`

// judgeServer is a server whose openai provider points at baseURL; live sets GOVOICE_LIVE=1 in its environment.
func judgeServer(t *testing.T, baseURL string, live bool) *Server {
	t.Helper()
	s := completerServer(t, baseURL, "judge-key-12345")
	if live {
		vars := testProviderVars(t)
		vars["OPENAI_API_KEY"], vars["OPENAI_BASE_URL"], vars["GOVOICE_LIVE"] = "judge-key-12345", baseURL, "1"
		s.env = mapEnv(vars)
	}
	return s
}

func judgedInput() eval.Input {
	return eval.Input{Turns: []eval.Line{{Who: "caller", Text: "hi"}, {Who: "agent", Text: "hello"}}, TurnCount: 1}
}

func scoreOf(t *testing.T, rs []eval.Result, scorer string) eval.Result {
	t.Helper()
	for _, r := range rs {
		if r.Scorer == scorer {
			return r
		}
	}
	t.Fatalf("no result for %s", scorer)
	return eval.Result{}
}

// A paid judge costs credit like a paid agent does: without GOVOICE_LIVE=1 it is never called and the result is an
// error (not a skip, which would read as a pass of nothing).
func TestJudgeForPaidJudgeNeedsLive(t *testing.T) {
	url, _, calls := fakeJudge(t, 200, judgeVerdict)
	suite := storage.EvalSuite{JudgeProvider: "openai"}

	s := judgeServer(t, url, false)
	judges, closeJudges := s.judgeFor(t.Context(), suite, nil)
	r := scoreOf(t, eval.RunScorers(t.Context(), judgedInput(), judges, nil), "correctness")
	closeJudges()
	if r.Outcome != eval.Errored || !strings.Contains(r.Reason, "GOVOICE_LIVE") || calls.Load() != 0 {
		t.Fatalf("no live: outcome %s reason %q, %d requests; want an error and no request", r.Outcome, r.Reason, calls.Load())
	}

	s = judgeServer(t, url, true)
	judges, closeJudges = s.judgeFor(t.Context(), suite, nil)
	defer closeJudges()
	r = scoreOf(t, eval.RunScorers(t.Context(), judgedInput(), judges, nil), "correctness")
	if r.Outcome != eval.Pass || calls.Load() == 0 {
		t.Fatalf("live: outcome %s (%s), %d requests; want the judge called", r.Outcome, r.Reason, calls.Load())
	}
	if !strings.HasPrefix(r.JudgeModel, "openai/") || r.JudgeModel == "openai/" {
		t.Fatalf("judge model %q, want the provider and the catalog's resolved model", r.JudgeModel)
	}
}

func TestJudgeForResolution(t *testing.T) {
	url, _, _ := fakeJudge(t, 200, judgeVerdict)
	s := judgeServer(t, url, true)
	suite := storage.EvalSuite{JudgeProvider: "openai", JudgeModel: "model-a"}
	settings := []storage.EvalScorerSetting{{Scorer: "correctness", JudgeProvider: "openai", JudgeModel: "model-b"}}
	judges, closeJudges := s.judgeFor(t.Context(), suite, settings)
	defer closeJudges()
	if got := judges("answer_relevancy"); got == nil || got.Model != "openai/model-a" {
		t.Fatalf("suite default judge = %+v, want openai/model-a", got)
	}
	if got := judges("correctness"); got == nil || got.Model != "openai/model-b" {
		t.Fatalf("scorer override judge = %+v, want openai/model-b", got)
	}

	none, closeNone := s.judgeFor(t.Context(), storage.EvalSuite{}, nil)
	defer closeNone()
	if none("correctness") != nil {
		t.Fatal("no judge configured must give no judge")
	}
	r := scoreOf(t, eval.RunScorers(t.Context(), judgedInput(), none, nil), "correctness")
	if r.Outcome != eval.Skipped || r.Reason != "no judge configured" {
		t.Fatalf("no judge: %s %q, want skipped", r.Outcome, r.Reason)
	}

	bad, closeBad := s.judgeFor(t.Context(), storage.EvalSuite{JudgeProvider: "nope"}, nil)
	defer closeBad()
	r = scoreOf(t, eval.RunScorers(t.Context(), judgedInput(), bad, nil), "correctness")
	if r.Outcome != eval.Errored || !strings.Contains(r.Reason, "nope") {
		t.Fatalf("unbuildable judge: %s %q, want an error naming the provider", r.Outcome, r.Reason)
	}
}

func TestJudgeForAppliesADeadlinePerAsk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()
	old := evalJudgeWait
	evalJudgeWait = 100 * time.Millisecond
	defer func() { evalJudgeWait = old }()
	s := judgeServer(t, srv.URL+"/v1", true)
	judges, closeJudges := s.judgeFor(t.Context(), storage.EvalSuite{JudgeProvider: "openai"}, nil)
	defer closeJudges()
	start := time.Now()
	_, err := judges("correctness").Ask(t.Context(), "r", "e")
	if err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("ask returned %v after %v, want an error at the per-ask deadline", err, time.Since(start))
	}
}

func TestSecretLiteralsKeepShortValuesOut(t *testing.T) {
	got := secretLiterals([]byte(`{"api_key":"abcdefgh12345","region":"us","base_url":"https://x.example"}`))
	slices.Sort(got)
	if len(got) != 2 || got[0] != "abcdefgh12345" || got[1] != "https://x.example" {
		t.Fatalf("literals = %q, want the long values only", got)
	}
	if got := secretLiterals([]byte("  ")); len(got) != 0 {
		t.Fatalf("a blank key gave %q", got)
	}
	if got := secretLiterals([]byte(" sk-longenoughkey \n")); len(got) != 1 || got[0] != "sk-longenoughkey" {
		t.Fatalf("a bare key gave %q", got)
	}
}

// runnerRig is a started server whose own worker is stopped, so a test calls runCaseJob on a job it queued itself.
func runnerRig(t *testing.T, mods ...func(*Config)) (*Server, agent.Agent) {
	t.Helper()
	s := evalTestServer(t, mods...)
	s.mgmt.Load().eval.Load().stop(context.Background())
	a, err := s.agents.Create(t.Context(), agent.CreateRequest{Name: "Dealership", Config: agentConfigJSON("SALES", "salesvoice")})
	if err != nil || a.Readiness.State != agent.ReadinessReady {
		t.Fatalf("agent: %+v err=%v", a.Readiness, err)
	}
	return s, a
}

func queueJob(t *testing.T, s *Server, a agent.Agent, mod func(*storage.EvalRun)) storage.EvalJob {
	t.Helper()
	run, jobs := caseJobs(t, a.ID, a.Version.ID, scriptCase("c", "hello"))
	if mod != nil {
		mod(&run)
	}
	run, err := s.store.Eval().CreateRun(t.Context(), run, jobs)
	if err != nil {
		t.Fatal(err)
	}
	return mustJobs(t, s.store, run.ID)[0]
}

// A call cancelled while it was being scored has verdicts that may be cut short: none of them is stored.
func TestRunCaseJobStoresNothingWhenCancelledWhileScoring(t *testing.T) {
	var cancel atomic.Pointer[context.CancelFunc]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c := cancel.Load(); c != nil {
			(*c)()
		}
		_, _ = w.Write([]byte(judgeVerdict))
	}))
	defer srv.Close()
	s, a := runnerRig(t)
	vars := testProviderVars(t)
	vars["OPENAI_API_KEY"], vars["OPENAI_BASE_URL"], vars["GOVOICE_LIVE"] = "judge-key-12345", srv.URL+"/v1", "1"
	s.env = mapEnv(vars)
	s.cfg.Providers.OpenAILLM.AllowPrivateBaseURL = true
	if err := s.store.Eval().PutSuite(t.Context(), storage.EvalSuite{AgentID: a.ID, Runs: 1, MinPass: 1, PassThreshold: 0.5, JudgeProvider: "openai"}); err != nil {
		t.Fatal(err)
	}
	job := queueJob(t, s, a, nil)
	ctx, c := context.WithCancel(t.Context())
	defer c()
	cancel.Store(&c)
	results, category, final, err := s.runCaseJob(ctx, job)
	if err == nil || category != "timeout" || final || results != nil {
		t.Fatalf("got %d results, category %q final %v err %v; want none, category timeout, retryable", len(results), category, final, err)
	}
}

func TestRunCaseJobGatesTheRewrittenConfig(t *testing.T) {
	s, _ := runnerRig(t)
	a := paidAgent(t, s)
	// The agent's own models are paid, but this run rewrites them to the free echo model: no GOVOICE_LIVE needed.
	job := queueJob(t, s, a, func(r *storage.EvalRun) { r.Providers = eval.ModeEcho })
	results, category, _, err := s.runCaseJob(t.Context(), job)
	if err != nil || category != "" || len(results) == 0 {
		t.Fatalf("echo-mode run of a paid agent: %d results, category %q, err %v; want it to run", len(results), category, err)
	}
	// The same agent kept as it is would spend credit.
	job = queueJob(t, s, a, func(r *storage.EvalRun) { r.Providers = eval.ModeKeep })
	if _, category, final, _ := s.runCaseJob(t.Context(), job); category != "live_required" || !final {
		t.Fatalf("keep-mode run: category %q final %v, want live_required and final", category, final)
	}
}

func TestRunCaseJobFinalCategories(t *testing.T) {
	s, a := runnerRig(t)
	job := queueJob(t, s, a, func(r *storage.EvalRun) {
		r.Draft, r.AgentVersionID, r.DraftConfigJSON = true, "", []byte(`{"start":"x"}`)
	})
	if _, category, final, err := s.runCaseJob(t.Context(), job); err == nil || category != "rewrite" || !final {
		t.Fatalf("unrewritable config: category %q final %v err %v, want rewrite and final", category, final, err)
	}
	off := false
	d, err := s.agents.Create(t.Context(), agent.CreateRequest{Name: "Off", Config: agentConfigJSON("SALES", "v"), Enabled: &off})
	if err != nil {
		t.Fatal(err)
	}
	job = queueJob(t, s, d, nil)
	if _, category, final, err := s.runCaseJob(t.Context(), job); err == nil || category != "agent_gone" || !final {
		t.Fatalf("disabled agent: category %q final %v err %v, want agent_gone and final", category, final, err)
	}
	if n := sessionCount(t, s, d.ID); n != 0 {
		t.Fatalf("%d calls were made for a disabled agent", n)
	}
}

// stop waits for the slots, reports a slot that will not end, and gives up when its caller's context ends.
func TestEvalWorkerStopWaitsForSlots(t *testing.T) {
	q := newQueueRig(t)
	q.enqueue(t, 1)
	var finished atomic.Bool
	started := make(chan struct{})
	release := make(chan struct{})
	w := q.worker(t, q.s.store, 1, func(ctx context.Context, _ storage.EvalJob) ([]storage.EvalResult, string, bool, error) {
		close(started)
		<-release // ignores ctx: a slot that does not quit on its own
		time.Sleep(150 * time.Millisecond)
		finished.Store(true)
		return nil, "call", false, errors.New("late")
	})
	<-started
	short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if w.stop(short) {
		t.Fatal("stop reported the slots stopped while one still runs")
	}
	if finished.Load() {
		t.Fatal("the slot finished before it was released")
	}
	close(release)
	if !w.stop(context.Background()) || !finished.Load() {
		t.Fatalf("stop returned before the slot ended (finished=%v)", finished.Load())
	}
}

func TestStopLeavesTheStoreOpenUnderALiveSlot(t *testing.T) {
	q := newQueueRig(t)
	q.enqueue(t, 1)
	logs := captureLogs(q.s)
	started, release := make(chan struct{}), make(chan struct{})
	w := q.worker(t, q.s.store, 1, func(context.Context, storage.EvalJob) ([]storage.EvalResult, string, bool, error) {
		close(started)
		<-release
		return nil, "call", false, errors.New("late")
	})
	<-started
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Shutdown's budget is already spent
	start := time.Now()
	if w.stop(ctx) {
		t.Fatal("stop reported success under a live slot")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("stop took %v with its context already ended, want it to give up at once", d)
	}
	if !strings.Contains(logs.String(), "eval worker still running at stop") {
		t.Fatalf("no log of the stuck stop: %s", logs.String())
	}
}

// A job longer than its lease keeps it through the heartbeat and runs exactly once.
func TestEvalWorkerHeartbeatKeepsALongJob(t *testing.T) {
	q := newQueueRig(t)
	run, jobs := q.enqueue(t, 1)
	var runs, stolen atomic.Int32
	q.worker(t, q.s.store, 1, func(ctx context.Context, _ storage.EvalJob) ([]storage.EvalResult, string, bool, error) {
		runs.Add(1)
		select {
		case <-time.After(800 * time.Millisecond):
		case <-ctx.Done():
			return nil, "call", false, ctx.Err()
		}
		return []storage.EvalResult{passResult(run, q.version, "expectations", "ok")}, "", false, nil
	}, func(w *evalWorker) { w.lease, w.heartbeat = 200*time.Millisecond, 40*time.Millisecond })
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for tick := time.NewTicker(20 * time.Millisecond); ; {
			select {
			case <-stop:
				return
			case <-tick.C:
				if _, ok, _, err := q.s.store.Eval().Claim(context.Background(), "thief", time.Now(), 200*time.Millisecond); err == nil && ok {
					stolen.Add(1)
				}
			}
		}
	}()
	waitRun(t, q.s.store, q.agentID, run.ID)
	close(stop)
	wg.Wait()
	if j := mustJob(t, q.s.store, run.ID, jobs[0].ID); runs.Load() != 1 || stolen.Load() != 0 || j.Status != "done" || j.Attempts != 1 {
		t.Fatalf("ran %d times, stolen %d times, job %+v; want one run under its extended lease", runs.Load(), stolen.Load(), j)
	}
}

// A worker that learns its lease is gone stops the call promptly (within about two heartbeats), not at its end.
func TestEvalWorkerLostLeaseCancelsTheCall(t *testing.T) {
	q := newQueueRig(t)
	q.enqueue(t, 1)
	started := make(chan struct{})
	returned := make(chan time.Time, 1)
	q.worker(t, q.s.store, 1, func(ctx context.Context, _ storage.EvalJob) ([]storage.EvalResult, string, bool, error) {
		close(started)
		<-ctx.Done()
		returned <- time.Now()
		return nil, "call", false, ctx.Err()
	}, func(w *evalWorker) { w.heartbeat = 40 * time.Millisecond })
	<-started
	_, ok, _, err := q.s.store.Eval().Claim(t.Context(), "thief", time.Now().Add(evalLease+time.Second), evalLease)
	if err != nil || !ok {
		t.Fatalf("thief claim: %v %v", ok, err)
	}
	took := time.Now()
	select {
	case at := <-returned:
		if d := at.Sub(took); d > 300*time.Millisecond {
			t.Fatalf("the call was cancelled %v after the lease was lost, want about two heartbeats", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the call kept running after its lease was lost")
	}
}

// A stop writes nothing, even when the store would still accept a write on a cancelled context: the check, not the
// database, keeps a stopped worker quiet.
func TestEvalWorkerStopWritesNothing(t *testing.T) {
	for name, outcome := range map[string]func(run storage.EvalRun, version string) ([]storage.EvalResult, string, bool, error){
		"results": func(run storage.EvalRun, version string) ([]storage.EvalResult, string, bool, error) {
			return []storage.EvalResult{passResult(run, version, "expectations", "late")}, "", false, nil
		},
		"failure": func(storage.EvalRun, string) ([]storage.EvalResult, string, bool, error) {
			return nil, "call", false, errors.New("cut off")
		},
	} {
		t.Run(name, func(t *testing.T) {
			q := newQueueRig(t)
			logs := captureLogs(q.s)
			run, jobs := q.enqueue(t, 1)
			started := make(chan struct{})
			w := q.worker(t, q.s.store, 1, func(ctx context.Context, _ storage.EvalJob) ([]storage.EvalResult, string, bool, error) {
				close(started)
				<-ctx.Done()
				return outcome(run, q.version)
			})
			<-started
			w.stop(context.Background())
			if j := mustJob(t, q.s.store, run.ID, jobs[0].ID); j.Status != "running" || j.LastErrorCategory != "" {
				t.Fatalf("job after stop = %+v, want left running with no failure", j)
			}
			if out := logs.String(); strings.Contains(out, "eval complete failed") || strings.Contains(out, "eval fail write failed") {
				t.Fatalf("a stopped worker tried to write:\n%s", out)
			}
		})
	}
}

// The wait after a failure comes from the attempt number: it grows with the attempts.
func TestEvalWorkerBacksOffByAttempt(t *testing.T) {
	q := newQueueRig(t)
	run, _ := q.enqueue(t, 1)
	var mu sync.Mutex
	var seen []int
	q.worker(t, q.s.store, 1, func(context.Context, storage.EvalJob) ([]storage.EvalResult, string, bool, error) {
		return nil, "call", false, errors.New("boom")
	}, func(w *evalWorker) {
		w.backoff = func(n int) time.Duration {
			mu.Lock()
			seen = append(seen, n)
			mu.Unlock()
			return 0
		}
	})
	waitRun(t, q.s.store, q.agentID, run.ID)
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(seen, []int{1, 2, 3}) {
		t.Fatalf("backoff asked for attempts %v, want [1 2 3]", seen)
	}
}

// A slot survives a panic outside the job function (a claim or a write): it is logged and the slot goes on.
func TestEvalWorkerSlotSurvivesAPanic(t *testing.T) {
	q := newQueueRig(t)
	logs := captureLogs(q.s)
	w := newEvalWorker(q.s, nil) // a nil store panics inside every pass
	w.idle = 5 * time.Millisecond
	w.start(1)
	eventually(t, "the slot to panic twice and carry on", func() bool { return strings.Count(logs.String(), "panic in eval worker") >= 2 })
	if !w.stop(context.Background()) {
		t.Fatal("the slot did not stop")
	}
}

// Jobs the claim ends as dead (lease expired after the last attempt) are counted, with the runs that finished.
func TestEvalWorkerCountsJobsEndedByTheClaim(t *testing.T) {
	q := newQueueRig(t)
	at := time.Now().Add(-time.Hour)
	run, jobs := caseJobs(t, q.agentID, q.version, scriptCase("c", "hi"))
	jobs[0].RunAfter = at.Add(-time.Minute)
	run, err := q.s.store.Eval().CreateRun(t.Context(), run, jobs)
	if err != nil {
		t.Fatal(err)
	}
	jobs = mustJobs(t, q.s.store, run.ID)
	for i := 0; i < 3; i++ { // three owners that crashed, long ago
		if _, ok, _, err := q.s.store.Eval().Claim(t.Context(), "gone", at, time.Minute); err != nil || !ok {
			t.Fatalf("claim %d: %v %v", i, ok, err)
		}
		at = at.Add(3 * time.Minute)
	}
	q.worker(t, q.s.store, 1, func(context.Context, storage.EvalJob) ([]storage.EvalResult, string, bool, error) {
		t.Error("an exhausted job must not run")
		return nil, "call", false, errors.New("no")
	})
	waitRun(t, q.s.store, q.agentID, run.ID)
	if j := mustJob(t, q.s.store, run.ID, jobs[0].ID); j.Status != "dead" || j.LastErrorCategory != "lease_expired" {
		t.Fatalf("job = %+v", j)
	}
	if got := q.s.metrics.evalJobs[evalDead].Load(); got != 1 {
		t.Errorf("dead jobs counted = %d, want 1", got)
	}
	if got := q.s.metrics.evalRunsFinished.Load(); got != 1 {
		t.Errorf("runs finished counted = %d, want 1", got)
	}
}

func TestStartEvalWorkerTwiceKeepsOnePool(t *testing.T) {
	s := evalTestServer(t)
	m := s.mgmt.Load()
	w, addr := m.eval.Load(), s.evalAddr
	s.startEvalWorker()
	if m.eval.Load() != w || s.evalAddr != addr {
		t.Fatal("a second start replaced the pool or rebound the listener")
	}
}

// Closing the management plane under a slot that will not stop leaves the store open: closing it would turn a slow
// stop into a crash inside the slot.
func TestManagementCloseKeepsTheStoreUnderALiveSlot(t *testing.T) {
	q := newQueueRig(t)
	logs := captureLogs(q.s)
	q.enqueue(t, 1)
	old := evalStopWait
	evalStopWait = 100 * time.Millisecond
	defer func() { evalStopWait = old }()
	m := q.s.mgmt.Load()
	m.logger = q.s.logger // the capture came after the plane was enabled
	started, release := make(chan struct{}), make(chan struct{})
	w := q.worker(t, m.store, 1, func(context.Context, storage.EvalJob) ([]storage.EvalResult, string, bool, error) {
		close(started)
		<-release
		return nil, "call", false, errors.New("late")
	})
	m.eval.Store(w)
	<-started
	q.s.mgmt.Swap(nil).close()
	if err := m.store.DB().PingContext(t.Context()); err != nil {
		t.Fatalf("the store was closed under a live slot: %v", err)
	}
	if !strings.Contains(logs.String(), "store left open") {
		t.Fatalf("no log of the open store: %s", logs.String())
	}
	close(release)
	if !w.stop(context.Background()) {
		t.Fatal("the slot did not end")
	}
	_ = m.store.Close()
}
