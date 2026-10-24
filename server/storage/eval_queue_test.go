package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testLease = 2 * time.Minute

// resetEval empties the eval tables. Claim takes any due job (a worker serves every agent), so a test that claims must
// start from an empty queue; the Postgres database is shared by the tests of this package, which run one at a time.
func resetEval(t *testing.T, s *Store) {
	t.Helper()
	for _, tbl := range []string{"eval_results", "eval_jobs", "eval_runs"} {
		if _, err := s.DB().ExecContext(context.Background(), "DELETE FROM "+tbl); err != nil {
			t.Fatal(err)
		}
	}
}

// seedRun creates a published-version run with n run_case jobs (one case each, repeat 0), all due at t0.
func seedRun(t *testing.T, s *Store, n int, t0 time.Time) (EvalRun, []EvalJob, string) {
	t.Helper()
	agentID, versionID := evalAgent(t, s)
	run := EvalRun{ID: NewID(), AgentID: agentID, AgentVersionID: versionID, ConfigHash: "h", Scope: "suite", Providers: "offline", Runs: 1, MinPass: 1, TriggeredBy: "api"}
	jobs := make([]EvalJob, n)
	for i := range jobs {
		subject := fmt.Sprintf("%s#0", NewID())
		jobs[i] = EvalJob{Kind: "run_case", Subject: subject, IdempotencyKey: "run:" + run.ID + ":" + subject, RunAfter: t0, Payload: []byte(`{}`)}
	}
	run, err := s.Eval().CreateRun(context.Background(), run, jobs)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Eval().RunJobs(context.Background(), run.ID)
	if err != nil || len(got) != n {
		t.Fatalf("seeded jobs: %v %d", err, len(got))
	}
	return run, got, versionID
}

func resultFor(run EvalRun, job EvalJob, versionID, scorer, outcome string) EvalResult {
	return EvalResult{SubjectKind: "case_run", SubjectID: EvalCaseRunSubject(run.ID, job.Subject), Scorer: scorer, ScorerVersion: 1, VersionKey: versionID,
		AgentID: run.AgentID, AgentVersionID: versionID, ConfigHash: "h", RunID: run.ID, SessionID: NewID(), Score: 1, Outcome: outcome, Reason: "r"}
}

func jobByID(t *testing.T, s *Store, run EvalRun, id string) EvalJob {
	t.Helper()
	jobs, err := s.Eval().RunJobs(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		if j.ID == id {
			return j
		}
	}
	t.Fatalf("job %s not found", id)
	return EvalJob{}
}

// The claim is a conditional UPDATE, so exactly one of several claimers wins each job. On SQLite the single connection
// serialises each statement but not the select-then-update pair of two claimers, so this still interleaves there; the
// proof that the database itself arbitrates (row locks, READ COMMITTED re-check of the WHERE) is the Postgres run
// (make test-postgres). Dropping the status predicate from the claim UPDATE makes this test fail on both.
func TestEvalClaimRaceExactlyOnce(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		resetEval(t, s)
		ctx := context.Background()
		t0 := time.Now()
		run, jobs, versionID := seedRun(t, s, 40, t0.Add(-time.Second))
		byID := make(map[string]EvalJob, len(jobs))
		for _, j := range jobs {
			byID[j.ID] = j
		}
		var mu sync.Mutex
		claims := map[string]int{}
		var finished atomic.Int64
		start := make(chan struct{})
		var wg sync.WaitGroup
		deadline := time.Now().Add(60 * time.Second)
		for w := 0; w < 4; w++ {
			owner := fmt.Sprintf("w%d", w)
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for finished.Load() < int64(len(jobs)) && time.Now().Before(deadline) {
					j, ok, err := s.Eval().Claim(ctx, owner, t0, testLease)
					if err != nil {
						t.Errorf("claim: %v", err)
						return
					}
					if !ok {
						time.Sleep(time.Millisecond) // candidates were all taken by the others; look again
						continue
					}
					mu.Lock()
					claims[j.ID]++
					mu.Unlock()
					if j.Attempts != 1 || j.LeaseOwner != owner || j.Status != "running" {
						t.Errorf("claimed job %s: attempts=%d owner=%q status=%q", j.ID, j.Attempts, j.LeaseOwner, j.Status)
					}
					if _, err := s.Eval().Complete(ctx, j.ID, owner, t0, []EvalResult{resultFor(run, byID[j.ID], versionID, "expectations", "pass")}); err != nil {
						t.Errorf("complete: %v", err)
						return
					}
					finished.Add(1)
				}
			}()
		}
		close(start)
		wg.Wait()
		for _, j := range jobs {
			if claims[j.ID] != 1 {
				t.Errorf("job %s claimed %d times, want exactly once", j.ID, claims[j.ID])
			}
		}
		got, err := s.Eval().RunResults(ctx, run.ID)
		if err != nil || len(got) != len(jobs) {
			t.Fatalf("results: %v %d", err, len(got))
		}
		r, err := s.Eval().GetRun(ctx, run.AgentID, run.ID)
		if err != nil || r.Status != "done" || r.PassedCases == nil || *r.PassedCases != 40 || *r.TotalCases != 40 {
			t.Fatalf("run: %v %+v", err, r)
		}
	})
}

func TestEvalLeaseExpiryFencesOldOwner(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		resetEval(t, s)
		ctx := context.Background()
		e := s.Eval()
		t0 := time.Now()
		run, jobs, versionID := seedRun(t, s, 1, t0.Add(-time.Second))
		res := []EvalResult{resultFor(run, jobs[0], versionID, "expectations", "pass")}

		a, ok, err := e.Claim(ctx, "A", t0, testLease)
		if err != nil || !ok || a.Attempts != 1 {
			t.Fatalf("A claim: %v %v %+v", err, ok, a)
		}
		if _, ok, _ := e.Claim(ctx, "B", t0.Add(time.Minute), testLease); ok {
			t.Fatal("B claimed a job whose lease is still live")
		}
		if err := e.Heartbeat(ctx, a.ID, "A", t0.Add(time.Minute), testLease); err != nil {
			t.Fatalf("A heartbeat while it owns the lease: %v", err)
		}
		// A's heartbeat moved the lease to t0+3m, so B must wait past that.
		if _, ok, _ := e.Claim(ctx, "B", t0.Add(3*time.Minute-time.Second), testLease); ok {
			t.Fatal("heartbeat did not extend the lease")
		}
		b, ok, err := e.Claim(ctx, "B", t0.Add(3*time.Minute+time.Second), testLease)
		if err != nil || !ok || b.ID != a.ID || b.Attempts != 2 || b.LeaseOwner != "B" {
			t.Fatalf("B reclaim: %v %v %+v", err, ok, b)
		}
		// A lost the lease: it can neither extend it nor complete the job, and its results are not written.
		if err := e.Heartbeat(ctx, a.ID, "A", t0.Add(4*time.Minute), testLease); !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("A heartbeat after losing the lease: %v", err)
		}
		if _, err := e.Complete(ctx, a.ID, "A", t0.Add(4*time.Minute), res); !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("A complete after losing the lease: %v", err)
		}
		if _, _, err := e.Fail(ctx, a.ID, "A", t0.Add(4*time.Minute), "x", time.Second, false); !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("A fail after losing the lease: %v", err)
		}
		if got, _ := e.RunResults(ctx, run.ID); len(got) != 0 {
			t.Fatalf("a fenced worker wrote %d results", len(got))
		}
		if j := jobByID(t, s, run, a.ID); j.Status != "running" || j.LeaseOwner != "B" {
			t.Fatalf("job after fenced writes: %+v", j)
		}
		finished, err := e.Complete(ctx, b.ID, "B", t0.Add(4*time.Minute), res)
		if err != nil || !finished {
			t.Fatalf("B complete: %v finished=%v", err, finished)
		}
		if j := jobByID(t, s, run, a.ID); j.Status != "done" || j.LeaseOwner != "" {
			t.Fatalf("done job: %+v", j)
		}
	})
}

func TestEvalFailBackoffThenDead(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		resetEval(t, s)
		ctx := context.Background()
		e := s.Eval()
		t0 := time.Now()
		run, jobs, _ := seedRun(t, s, 1, t0.Add(-time.Second))
		now := t0
		for attempt := 1; attempt <= 3; attempt++ {
			j, ok, err := e.Claim(ctx, "w", now, testLease)
			if err != nil || !ok || j.Attempts != attempt {
				t.Fatalf("claim %d: %v %v %+v", attempt, err, ok, j)
			}
			dead, finished, err := e.Fail(ctx, j.ID, "w", now, "dial", 30*time.Second, false)
			if err != nil {
				t.Fatal(err)
			}
			if attempt < 3 {
				if dead || finished {
					t.Fatalf("attempt %d went dead=%v finished=%v", attempt, dead, finished)
				}
				got := jobByID(t, s, run, jobs[0].ID)
				if got.Status != "pending" || got.LeaseOwner != "" || got.LastErrorCategory != "dial" || got.RunAfter.Sub(now).Round(time.Second) != 30*time.Second {
					t.Fatalf("after failure %d: %+v", attempt, got)
				}
				if _, ok, _ := e.Claim(ctx, "w", now.Add(29*time.Second), testLease); ok {
					t.Fatal("claimed before run_after")
				}
				now = now.Add(31 * time.Second)
				continue
			}
			if !dead || !finished {
				t.Fatalf("last attempt dead=%v finished=%v", dead, finished)
			}
		}
		if got := jobByID(t, s, run, jobs[0].ID); got.Status != "dead" || got.LastErrorCategory != "dial" {
			t.Fatalf("dead job: %+v", got)
		}
		if r, _ := e.GetRun(ctx, run.AgentID, run.ID); r.Status != "done" || *r.PassedCases != 0 || *r.TotalCases != 1 {
			t.Fatalf("run with a dead job: %+v", r)
		}
		// final=true goes dead at once, however many attempts are left.
		_, jobs2, _ := seedRun(t, s, 1, t0.Add(-time.Second))
		j, ok, _ := e.Claim(ctx, "w", t0, testLease)
		if !ok || j.ID != jobs2[0].ID {
			t.Fatalf("claim for final: %v %+v", ok, j)
		}
		if dead, finished, err := e.Fail(ctx, j.ID, "w", t0, "persona_unavailable", 0, true); err != nil || !dead || !finished {
			t.Fatalf("final fail: %v dead=%v finished=%v", err, dead, finished)
		}
	})
}

// A replayed Complete (the second claimer finishing a job whose first owner had already written results before it lost
// the lease) leaves one row per (subject, scorer, version_key). Replacing ON CONFLICT DO NOTHING with a plain insert
// makes this fail.
func TestEvalResultsInsertIdempotent(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		resetEval(t, s)
		ctx := context.Background()
		e := s.Eval()
		t0 := time.Now()
		run, jobs, versionID := seedRun(t, s, 1, t0.Add(-time.Second))
		first := resultFor(run, jobs[0], versionID, "expectations", "pass")
		second := resultFor(run, jobs[0], versionID, "expectations", "fail") // same key, a later attempt's verdict
		a, _, _ := e.Claim(ctx, "A", t0, testLease)
		// Simulate "A wrote its results, then the process died before the job row was observed done": the results exist, the job is reclaimed.
		if _, err := e.Complete(ctx, a.ID, "A", t0, []EvalResult{first}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB().ExecContext(ctx, s.bind("UPDATE eval_jobs SET status='running', lease_owner='A', lease_expires_at=? WHERE id=?"), utc(t0.Add(time.Minute)), a.ID); err != nil {
			t.Fatal(err)
		}
		b, ok, err := e.Claim(ctx, "B", t0.Add(3*time.Minute), testLease)
		if err != nil || !ok || b.ID != a.ID {
			t.Fatalf("reclaim: %v %v", err, ok)
		}
		if _, err := e.Complete(ctx, b.ID, "B", t0.Add(3*time.Minute), []EvalResult{second, resultFor(run, jobs[0], versionID, "tool_call_accuracy", "pass")}); err != nil {
			t.Fatalf("replayed complete must not fail on the duplicate: %v", err)
		}
		got, err := e.RunResults(ctx, run.ID)
		if err != nil || len(got) != 2 {
			t.Fatalf("results: %v %d, want 2 (one per scorer)", err, len(got))
		}
		for _, r := range got {
			if r.Scorer == "expectations" && r.Outcome != "pass" {
				t.Fatalf("the first write must win, got %q", r.Outcome)
			}
		}
	})
}

func TestFinishRunCountsDeadCaseRunAsFailed(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		resetEval(t, s)
		ctx := context.Background()
		e := s.Eval()
		t0 := time.Now()
		run, jobs, versionID := seedRun(t, s, 3, t0.Add(-time.Second))
		outcomes := map[string]string{} // job id -> "pass" | "fail" | "dead"
		for i, j := range jobs {
			outcomes[j.ID] = []string{"pass", "fail", "dead"}[i]
		}
		for range jobs {
			j, ok, err := e.Claim(ctx, "w", t0, testLease)
			if err != nil || !ok {
				t.Fatalf("claim: %v %v", err, ok)
			}
			var finished bool
			switch o := outcomes[j.ID]; o {
			case "dead":
				_, finished, err = e.Fail(ctx, j.ID, "w", t0, "dial", 0, true)
			default:
				finished, err = e.Complete(ctx, j.ID, "w", t0, []EvalResult{resultFor(run, j, versionID, "expectations", o)})
			}
			if err != nil {
				t.Fatal(err)
			}
			delete(outcomes, j.ID)
			if len(outcomes) == 0 && !finished {
				t.Fatal("the last job did not finish the run")
			}
		}
		r, err := e.GetRun(ctx, run.AgentID, run.ID)
		if err != nil || r.Status != "done" || *r.PassedCases != 1 || *r.TotalCases != 3 || r.EndedAt == nil {
			t.Fatalf("run: %v %+v (one pass, one fail, one dead: 1 of 3)", err, r)
		}
	})
}

// A case with runs=3 and min_pass=2 passes when two of its three case-runs pass.
func TestFinishRunMinPassAcrossRepeats(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		resetEval(t, s)
		ctx := context.Background()
		e := s.Eval()
		t0 := time.Now()
		agentID, versionID := evalAgent(t, s)
		run := EvalRun{ID: NewID(), AgentID: agentID, AgentVersionID: versionID, ConfigHash: "h", Scope: "case", Providers: "offline", Runs: 3, MinPass: 2, TriggeredBy: "console"}
		caseID := NewID()
		var jobs []EvalJob
		for i := 0; i < 3; i++ {
			sub := fmt.Sprintf("%s#%d", caseID, i)
			jobs = append(jobs, EvalJob{Kind: "run_case", Subject: sub, IdempotencyKey: "run:" + run.ID + ":" + sub, RunAfter: t0.Add(-time.Second), Payload: []byte(`{}`)})
		}
		run, err := e.CreateRun(ctx, run, jobs)
		if err != nil {
			t.Fatal(err)
		}
		verdicts := map[int]string{0: "pass", 1: "fail", 2: "pass"}
		for range jobs {
			j, ok, err := e.Claim(ctx, "w", t0, testLease)
			if err != nil || !ok {
				t.Fatalf("claim: %v %v", err, ok)
			}
			var idx int
			fmt.Sscanf(j.Subject[len(caseID)+1:], "%d", &idx)
			if _, err := e.Complete(ctx, j.ID, "w", t0, []EvalResult{resultFor(run, j, versionID, "expectations", verdicts[idx])}); err != nil {
				t.Fatal(err)
			}
		}
		if r, _ := e.GetRun(ctx, agentID, run.ID); r.Status != "done" || *r.PassedCases != 1 || *r.TotalCases != 1 {
			t.Fatalf("2 of 3 passed with min_pass 2 must pass the case: %+v", r)
		}
	})
}

// Two jobs of one run completing at the same moment must still finish the run: each transaction counts the open jobs,
// and only a lock on the run row makes the later one see the earlier one's commit. Needs Postgres to be meaningful
// (read-committed transactions overlap there); on SQLite it only checks the happy path.
func TestEvalRunFinishesWhenLastJobsCompleteConcurrently(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		resetEval(t, s)
		ctx := context.Background()
		e := s.Eval()
		t0 := time.Now()
		for round := 0; round < 10; round++ {
			run, jobs, versionID := seedRun(t, s, 8, t0.Add(-time.Second))
			var claimed []EvalJob
			for range jobs {
				j, ok, err := e.Claim(ctx, "w", t0, testLease)
				if err != nil || !ok {
					t.Fatalf("claim: %v %v", err, ok)
				}
				claimed = append(claimed, j)
			}
			var wg sync.WaitGroup
			start := make(chan struct{})
			for _, j := range claimed {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					if _, err := e.Complete(ctx, j.ID, "w", t0, []EvalResult{resultFor(run, j, versionID, "expectations", "pass")}); err != nil {
						t.Errorf("complete: %v", err)
					}
				}()
			}
			close(start)
			wg.Wait()
			if r, err := e.GetRun(ctx, run.AgentID, run.ID); err != nil || r.Status != "done" || *r.PassedCases != 8 {
				t.Fatalf("round %d: the run was left unfinished: %v %+v", round, err, r)
			}
		}
	})
}

// An expired lease on a job that has used all its attempts ends the job instead of running it a fourth time.
func TestEvalClaimDeadsExhaustedExpiredLease(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		resetEval(t, s)
		ctx := context.Background()
		e := s.Eval()
		t0 := time.Now()
		run, jobs, _ := seedRun(t, s, 1, t0.Add(-time.Second))
		now := t0
		for i := 0; i < 3; i++ { // three claims whose owners crash
			if _, ok, err := e.Claim(ctx, "w", now, testLease); err != nil || !ok {
				t.Fatalf("claim %d: %v %v", i, err, ok)
			}
			now = now.Add(3 * time.Minute)
		}
		if _, ok, err := e.Claim(ctx, "w", now, testLease); err != nil || ok {
			t.Fatalf("a fourth claim must not run the job: %v %v", err, ok)
		}
		if j := jobByID(t, s, run, jobs[0].ID); j.Status != "dead" || j.LastErrorCategory != "lease_expired" || j.Attempts != 3 {
			t.Fatalf("job: %+v", j)
		}
		if r, _ := e.GetRun(ctx, run.AgentID, run.ID); r.Status != "done" || *r.PassedCases != 0 {
			t.Fatalf("run: %+v", r)
		}
	})
}

func TestCreateRunQueueFull(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		resetEval(t, s)
		ctx := context.Background()
		e := s.Eval()
		agentID, versionID := evalAgent(t, s)
		mk := func(n int) (EvalRun, []EvalJob) {
			run := EvalRun{ID: NewID(), AgentID: agentID, AgentVersionID: versionID, ConfigHash: "h", Scope: "suite", Providers: "offline", Runs: 1, MinPass: 1, TriggeredBy: "api"}
			jobs := make([]EvalJob, n)
			for i := range jobs {
				k := NewID()
				jobs[i] = EvalJob{Kind: "run_case", Subject: k + "#0", IdempotencyKey: "run:" + run.ID + ":" + k, Payload: []byte(`{}`)}
			}
			return run, jobs
		}
		r, j := mk(MaxPendingJobsPerAgent + 1)
		if _, err := e.CreateRun(ctx, r, j); !errors.Is(err, ErrQueueFull) {
			t.Fatalf("201 jobs: %v", err)
		}
		if _, err := e.GetRun(ctx, agentID, r.ID); err == nil {
			t.Fatal("a refused run left its row behind")
		}
		r, j = mk(150)
		if _, err := e.CreateRun(ctx, r, j); err != nil {
			t.Fatal(err)
		}
		r, j = mk(51)
		if _, err := e.CreateRun(ctx, r, j); !errors.Is(err, ErrQueueFull) {
			t.Fatalf("150 + 51 jobs: %v", err)
		}
		r, j = mk(50)
		if _, err := e.CreateRun(ctx, r, j); err != nil {
			t.Fatalf("150 + 50 jobs fit: %v", err)
		}
	})
}

func TestCreateRunIdempotencyKey(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		resetEval(t, s)
		ctx := context.Background()
		e := s.Eval()
		agentID, versionID := evalAgent(t, s)
		run := EvalRun{ID: NewID(), AgentID: agentID, AgentVersionID: versionID, ConfigHash: "h", Scope: "suite", Providers: "offline", Runs: 1, MinPass: 1, TriggeredBy: "api"}
		job := EvalJob{Kind: "run_case", Subject: "c#0", IdempotencyKey: "run:" + run.ID + ":c#0", Payload: []byte(`{}`)}
		if _, err := e.CreateRun(ctx, run, []EvalJob{job, job}); err != nil {
			t.Fatal(err)
		}
		if got, _ := e.RunJobs(ctx, run.ID); len(got) != 1 {
			t.Fatalf("the same key twice made %d jobs", len(got))
		}
		// A second run reusing the first run's key does not get a job of its own.
		run2 := run
		run2.ID = NewID()
		if _, err := e.CreateRun(ctx, run2, []EvalJob{job}); err != nil {
			t.Fatal(err)
		}
		if got, _ := e.RunJobs(ctx, run2.ID); len(got) != 0 {
			t.Fatalf("a repeated key made a job for the second run: %d", len(got))
		}
	})
}

func TestEvalRunListingAndStart(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		resetEval(t, s)
		ctx := context.Background()
		e := s.Eval()
		agentID, versionID := evalAgent(t, s)
		mk := func(draft bool) EvalRun {
			r := EvalRun{ID: NewID(), AgentID: agentID, ConfigHash: "h", Draft: draft, DraftConfigJSON: nil, Scope: "suite", Providers: "offline", Runs: 1, MinPass: 1, TriggeredBy: "api"}
			if !draft {
				r.AgentVersionID = versionID
			}
			if draft {
				r.DraftConfigJSON, r.DraftToolsJSON = []byte(`{"a":1}`), []byte(`[]`)
			}
			got, err := e.CreateRun(ctx, r, nil)
			if err != nil {
				t.Fatal(err)
			}
			return got
		}
		pub := mk(false)
		time.Sleep(2 * time.Millisecond)
		dr := mk(true)
		got, err := e.ListRuns(ctx, agentID, true, 10, 0)
		if err != nil || len(got) != 2 || got[0].ID != dr.ID || !got[0].Draft || string(got[0].DraftConfigJSON) != `{"a":1}` || got[0].AgentVersionID != "" {
			t.Fatalf("with drafts, newest first: %v %+v", err, got)
		}
		got, err = e.ListRuns(ctx, agentID, false, 10, 0)
		if err != nil || len(got) != 1 || got[0].ID != pub.ID || got[0].AgentVersionID != versionID {
			t.Fatalf("without drafts: %v %+v", err, got)
		}
		now := time.Now()
		if err := e.MarkRunStarted(ctx, pub.ID, now); err != nil {
			t.Fatal(err)
		}
		first, _ := e.GetRun(ctx, agentID, pub.ID)
		if first.Status != "running" || first.StartedAt == nil {
			t.Fatalf("started: %+v", first)
		}
		if err := e.MarkRunStarted(ctx, pub.ID, now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if again, _ := e.GetRun(ctx, agentID, pub.ID); !again.StartedAt.Equal(*first.StartedAt) {
			t.Fatal("a second start moved started_at")
		}
	})
}

// finishedRun makes a run of one case whose job completed at `at` with one result (all stamps use `at`).
func finishedRun(t *testing.T, s *Store, at time.Time) (EvalRun, EvalJob) {
	t.Helper()
	ctx := context.Background()
	run, jobs, versionID := seedRun(t, s, 1, at.Add(-time.Second))
	j, ok, err := s.Eval().Claim(ctx, "w", at, testLease)
	if err != nil || !ok || j.ID != jobs[0].ID {
		t.Fatalf("claim: %v %v", err, ok)
	}
	if _, err := s.Eval().Complete(ctx, j.ID, "w", at, []EvalResult{resultFor(run, jobs[0], versionID, "expectations", "pass")}); err != nil {
		t.Fatal(err)
	}
	return run, jobs[0]
}

func countRows(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRowContext(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// eval_results points at its call by session id with no foreign key, so purging the ended session leaves the result.
// Adding a foreign key to sessions makes PurgeMetadata fail here.
func TestPurgeMetadataWithEvalResults(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		resetEval(t, s)
		ctx := context.Background()
		old := time.Now().Add(-48 * time.Hour)
		sessionID := seedSession(t, s, old, "ended")
		if _, err := s.DB().ExecContext(ctx, s.bind("UPDATE sessions SET ended_at=? WHERE id=?"), utc(old), sessionID); err != nil {
			t.Fatal(err)
		}
		run, job := finishedRun(t, s, time.Now())
		if _, err := s.DB().ExecContext(ctx, s.bind("UPDATE eval_results SET session_id=? WHERE run_id=?"), sessionID, run.ID); err != nil {
			t.Fatal(err)
		}
		stats, err := s.Retention().PurgeMetadata(ctx, time.Now().Add(-24*time.Hour), 100)
		if err != nil {
			t.Fatalf("purge with an eval result pointing at the session: %v", err)
		}
		if stats.SessionsDeleted < 1 {
			t.Fatalf("session not purged: %+v", stats)
		}
		got, err := s.Eval().RunResults(ctx, run.ID)
		if err != nil || len(got) != 1 || got[0].SessionID != sessionID || got[0].SubjectID != EvalCaseRunSubject(run.ID, job.Subject) {
			t.Fatalf("the result must survive its session: %v %+v", err, got)
		}
	})
}

func TestPurgeEval(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		resetEval(t, s)
		ctx := context.Background()
		now := time.Now()
		r := s.Retention()
		cut := func(d time.Duration) time.Time { return now.Add(-d) }
		day := 24 * time.Hour

		oldRun, _ := finishedRun(t, s, cut(100*day)) // finished 100 days ago
		newRun, _ := finishedRun(t, s, cut(time.Hour))
		openRun, _, _ := seedRun(t, s, 1, cut(200*day)) // old but still pending: never purged

		// Jobs at 30 days, results at 180: the old run's job goes, its result and run stay.
		st, err := r.PurgeEval(ctx, cut(180*day), cut(30*day), 100)
		if err != nil || st != (PurgeEvalStats{Jobs: 1}) {
			t.Fatalf("jobs only: %v %+v", err, st)
		}
		if got, _ := s.Eval().RunResults(ctx, oldRun.ID); len(got) != 1 {
			t.Fatalf("the result must outlive its job: %d", len(got))
		}
		if _, err := s.Eval().GetRun(ctx, oldRun.AgentID, oldRun.ID); err != nil {
			t.Fatalf("a run with results is kept: %v", err)
		}
		// Results at 50 days: now the old run's result and, in the same call, the emptied run go.
		st, err = r.PurgeEval(ctx, cut(50*day), cut(30*day), 100)
		if err != nil || st != (PurgeEvalStats{Results: 1, Runs: 1}) {
			t.Fatalf("results and run: %v %+v", err, st)
		}
		if _, err := s.Eval().GetRun(ctx, oldRun.AgentID, oldRun.ID); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("old run still stored: %v", err)
		}
		if got, _ := s.Eval().RunResults(ctx, newRun.ID); len(got) != 1 {
			t.Fatal("a recent result was purged")
		}
		if got, _ := s.Eval().RunJobs(ctx, openRun.ID); len(got) != 1 || got[0].Status != "pending" {
			t.Fatalf("a pending job was purged: %+v", got)
		}
		if _, err := s.Eval().GetRun(ctx, openRun.AgentID, openRun.ID); err != nil {
			t.Fatalf("an unfinished run was purged: %v", err)
		}
		// A zero cutoff leaves that pass out.
		if st, err = r.PurgeEval(ctx, time.Time{}, time.Time{}, 100); err != nil || st != (PurgeEvalStats{}) {
			t.Fatalf("zero cutoffs: %v %+v", err, st)
		}
	})
}

// Each call deletes at most `limit` rows per table, so a backlog is worked off in bounded batches.
func TestPurgeEvalBatches(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		resetEval(t, s)
		ctx := context.Background()
		old := time.Now().Add(-100 * 24 * time.Hour)
		for i := 0; i < 3; i++ {
			finishedRun(t, s, old.Add(time.Duration(i)*time.Second))
		}
		cutoff := time.Now().Add(-time.Hour)
		var jobs, results, runs int64
		for calls := 0; ; calls++ {
			st, err := s.Retention().PurgeEval(ctx, cutoff, cutoff, 2)
			if err != nil {
				t.Fatal(err)
			}
			if st.Jobs > 2 || st.Results > 2 || st.Runs > 2 {
				t.Fatalf("a batch exceeded the limit: %+v", st)
			}
			jobs, results, runs = jobs+st.Jobs, results+st.Results, runs+st.Runs
			if st == (PurgeEvalStats{}) {
				break
			}
			if calls > 10 {
				t.Fatal("purge does not drain")
			}
		}
		if jobs != 3 || results != 3 || runs != 3 || countRows(t, s, "eval_jobs") != 0 || countRows(t, s, "eval_runs") != 0 {
			t.Fatalf("drained %d/%d/%d", jobs, results, runs)
		}
	})
}

// queueRun is a published-version run of n run_case jobs for the agent, none of them due yet.
func queueRun(agentID, versionID string, n int) (EvalRun, []EvalJob) {
	run := EvalRun{ID: NewID(), AgentID: agentID, AgentVersionID: versionID, ConfigHash: "h", Scope: "suite", Providers: "offline", Runs: 1, MinPass: 1, TriggeredBy: "api"}
	jobs := make([]EvalJob, n)
	for i := range jobs {
		k := NewID()
		jobs[i] = EvalJob{Kind: "run_case", Subject: k + "#0", IdempotencyKey: "run:" + run.ID + ":" + k, Payload: []byte(`{}`)}
	}
	return run, jobs
}

// Two CreateRuns of 150 jobs for one agent racing in READ COMMITTED transactions both count 0 open jobs unless the
// agent row is locked first: the second one then leaves 300 pending jobs. Dropping the lock makes this fail on
// Postgres (SQLite serialises its single connection).
func TestCreateRunCapHoldsUnderConcurrency(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		resetEval(t, s)
		ctx := context.Background()
		for round := 0; round < 10; round++ {
			agentID, versionID := evalAgent(t, s)
			var ok, full atomic.Int32
			start := make(chan struct{})
			var wg sync.WaitGroup
			for g := 0; g < 2; g++ {
				run, jobs := queueRun(agentID, versionID, 150)
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					switch _, err := s.Eval().CreateRun(ctx, run, jobs); {
					case err == nil:
						ok.Add(1)
					case errors.Is(err, ErrQueueFull):
						full.Add(1)
					default:
						t.Errorf("create run: %v", err)
					}
				}()
			}
			close(start)
			wg.Wait()
			var open int
			if err := s.DB().QueryRowContext(ctx, s.bind("SELECT count(*) FROM eval_jobs WHERE agent_id=? AND status IN ('pending','running')"), agentID).Scan(&open); err != nil {
				t.Fatal(err)
			}
			if ok.Load() != 1 || full.Load() != 1 || open != 150 {
				t.Fatalf("round %d: %d accepted, %d refused, %d open jobs (want 1, 1, 150)", round, ok.Load(), full.Load(), open)
			}
		}
	})
}

// Same for the saved cases: the 200 cap is exact, not "by one batch".
func TestCreateCasesCapHoldsUnderConcurrency(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		resetEval(t, s)
		ctx := context.Background()
		for round := 0; round < 10; round++ {
			agentID, _ := evalAgent(t, s)
			var ok, full atomic.Int32
			start := make(chan struct{})
			var wg sync.WaitGroup
			for g := 0; g < 2; g++ {
				cs := make([]EvalCase, 150)
				for i := range cs {
					cs[i] = EvalCase{Name: fmt.Sprintf("g%d-%d", g, i), CallerJSON: []byte(`{}`), ExpectJSON: []byte(`{}`)}
				}
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					switch _, err := s.Eval().CreateCases(ctx, agentID, cs); {
					case err == nil:
						ok.Add(1)
					case errors.Is(err, ErrTooManyCases):
						full.Add(1)
					default:
						t.Errorf("create cases: %v", err)
					}
				}()
			}
			close(start)
			wg.Wait()
			if got, _ := s.Eval().ListCases(ctx, agentID); ok.Load() != 1 || full.Load() != 1 || len(got) != 150 {
				t.Fatalf("round %d: %d accepted, %d refused, %d cases (want 1, 1, 150)", round, ok.Load(), full.Load(), len(got))
			}
		}
	})
}

// The agent row lock of an enqueue must not block inserts that only reference the agent (their foreign-key check takes
// a KEY SHARE lock, which a NO KEY UPDATE on non-key columns does not conflict with). Sessions().Create does take
// SELECT ... FOR UPDATE on the agent and so waits for the enqueue transaction, which lasts milliseconds.
func TestAgentLockDoesNotBlockForeignKeyInserts(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		if s.dialect != Postgres {
			t.Skip("SQLite has one connection and no row locks")
		}
		agentID, _ := evalAgent(t, s)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		tx, err := s.DB().BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if err := lockAgent(ctx, tx, s, agentID, "test"); err != nil {
			t.Fatal(err)
		}
		if err := s.Eval().PutSuite(ctx, EvalSuite{AgentID: agentID, Runs: 1, MinPass: 1, PassThreshold: 1}); err != nil {
			t.Fatalf("a row referencing the agent waited on the lock: %v", err)
		}
	})
}
