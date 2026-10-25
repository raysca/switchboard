package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// MaxPendingJobsPerAgent bounds an agent's pending plus running jobs; CreateRun refuses a run that would exceed it.
const MaxPendingJobsPerAgent = 200

// claimCandidates is how many due rows one Claim looks at before giving up.
const claimCandidates = 8

const (
	maxRunResults = 2000 // 200 jobs x the 5 built-in scorers, with headroom
	maxListRuns   = 200
)

var (
	// ErrLeaseLost: the job is no longer running under this claim (its lease expired and another worker took it, or it finished).
	ErrLeaseLost = errors.New("eval job lease lost")
	ErrQueueFull = errors.New("too many pending eval jobs for the agent")
	// ErrNoJobs: CreateRun was given no job, or only jobs whose idempotency key already exists. Such a run would stay
	// pending forever.
	ErrNoJobs = errors.New("eval run has no jobs")
	// ErrResultRunMismatch: a result handed to Complete names a run other than the job's.
	ErrResultRunMismatch = errors.New("eval result belongs to another run")
)

// EvalRun is one execution of a suite or one case against one agent version, or against a draft (AgentVersionID
// empty, ConfigHash the draft's, the draft config and tools kept so a worker can run them).
type EvalRun struct {
	ID, AgentID, AgentVersionID, ConfigHash string
	Draft                                   bool
	DraftConfigJSON, DraftToolsJSON         []byte
	Scope, CaseID, Providers                string
	Runs, MinPass                           int
	Status, TriggeredBy                     string
	PassedCases, TotalCases                 *int
	CreatedAt                               time.Time
	StartedAt, EndedAt                      *time.Time
}

// EvalJob is one queue row. Subject of a run_case job is "<caseID>#<repeat>".
type EvalJob struct {
	ID, Kind, AgentID, AgentVersionID, RunID, Subject, Scorer, IdempotencyKey, Status string
	Attempts, MaxAttempts                                                             int
	RunAfter                                                                          time.Time
	LeaseOwner                                                                        string
	LeaseExpiresAt                                                                    time.Time
	Payload                                                                           []byte
	LastErrorCategory                                                                 string
}

// EvalResult is one scorer's verdict on one subject. Its SubjectID is unique per (subject, scorer, VersionKey); for a
// case-run it is EvalCaseRunSubject(run, job subject) so that two runs of the same version never collide.
type EvalResult struct {
	SubjectKind, SubjectID, Scorer                                                      string
	ScorerVersion                                                                       int
	VersionKey, AgentID, AgentVersionID, ConfigHash, RunID, CaseID, CaseHash, SessionID string
	Score                                                                               float64
	Outcome, Reason, JudgeModel                                                         string
	CreatedAt                                                                           time.Time
}

// EvalCaseRunSubject is the result subject of a run_case job: the job's subject made unique per run. The result key is
// (subject, scorer, version key) and a version is run many times, so the bare "<caseID>#<repeat>" would make a second
// run's results collide with the first's and be dropped by ON CONFLICT DO NOTHING.
func EvalCaseRunSubject(runID, jobSubject string) string { return runID + "/" + jobSubject }

const evalRunCols = "id,agent_id,agent_version_id,config_hash,draft,draft_config_json,draft_tools_json,scope,case_id,providers,runs,min_pass,status,triggered_by,passed_cases,total_cases,created_at,started_at,ended_at"

func scanEvalRun(r rowScanner) (EvalRun, error) {
	var e EvalRun
	var ver, dcfg, dtools, caseID sql.NullString
	var passed, total sql.NullInt64
	var created string
	var started, ended sql.NullString
	var draft any
	if err := r.Scan(&e.ID, &e.AgentID, &ver, &e.ConfigHash, &draft, &dcfg, &dtools, &e.Scope, &caseID, &e.Providers, &e.Runs, &e.MinPass, &e.Status, &e.TriggeredBy, &passed, &total, &created, &started, &ended); err != nil {
		return EvalRun{}, err
	}
	switch d := draft.(type) {
	case bool:
		e.Draft = d
	case int64:
		e.Draft = d != 0
	}
	e.AgentVersionID, e.CaseID = ver.String, caseID.String
	if dcfg.Valid {
		e.DraftConfigJSON = []byte(dcfg.String)
	}
	if dtools.Valid {
		e.DraftToolsJSON = []byte(dtools.String)
	}
	if passed.Valid {
		p := int(passed.Int64)
		e.PassedCases = &p
	}
	if total.Valid {
		n := int(total.Int64)
		e.TotalCases = &n
	}
	e.CreatedAt = scanTime(created)
	if started.Valid {
		t := scanTime(started.String)
		e.StartedAt = &t
	}
	if ended.Valid {
		t := scanTime(ended.String)
		e.EndedAt = &t
	}
	return e, nil
}

const evalJobCols = "id,kind,agent_id,agent_version_id,run_id,subject,scorer,idempotency_key,status,attempts,max_attempts,run_after,lease_owner,lease_expires_at,payload,last_error_category"

func scanEvalJob(r rowScanner) (EvalJob, error) {
	var j EvalJob
	var ver, run, scorer, owner, lease, cat sql.NullString
	var after, payload string
	if err := r.Scan(&j.ID, &j.Kind, &j.AgentID, &ver, &run, &j.Subject, &scorer, &j.IdempotencyKey, &j.Status, &j.Attempts, &j.MaxAttempts, &after, &owner, &lease, &payload, &cat); err != nil {
		return EvalJob{}, err
	}
	j.AgentVersionID, j.RunID, j.Scorer, j.LeaseOwner, j.LastErrorCategory = ver.String, run.String, scorer.String, owner.String, cat.String
	j.RunAfter, j.Payload = scanTime(after), []byte(payload)
	if lease.Valid {
		j.LeaseExpiresAt = scanTime(lease.String)
	}
	return j, nil
}

// CreateRun stores the run and its jobs in one transaction. A job whose idempotency key already exists is skipped
// (ON CONFLICT DO NOTHING). The jobs inherit the run's agent, version and id; zero MaxAttempts becomes 3 and a zero
// RunAfter means due now. ErrQueueFull: the agent's pending and running jobs plus these would exceed
// MaxPendingJobsPerAgent, and nothing is written. ErrNoJobs: no job was inserted (none given, or every key already
// exists), and nothing is written.
func (r EvalRepository) CreateRun(ctx context.Context, run EvalRun, jobs []EvalJob) (EvalRun, error) {
	now := time.Now()
	if run.ID == "" {
		run.ID = NewID()
	}
	run.Status, run.CreatedAt = "pending", now
	err := r.s.WithTx(ctx, func(tx *sql.Tx) error {
		if err := lockAgent(ctx, tx, r.s, run.AgentID, "eval enqueue"); err != nil {
			return err
		}
		var open int
		if err := tx.QueryRowContext(ctx, r.s.bind("SELECT count(*) FROM eval_jobs WHERE agent_id=? AND status IN ('pending','running')"), run.AgentID).Scan(&open); err != nil {
			return fmt.Errorf("count open eval jobs: %w", err)
		}
		if open+len(jobs) > MaxPendingJobsPerAgent {
			return ErrQueueFull
		}
		if _, err := tx.ExecContext(ctx, r.s.bind("INSERT INTO eval_runs(id,agent_id,agent_version_id,config_hash,draft,draft_config_json,draft_tools_json,source,scope,case_id,providers,runs,min_pass,status,triggered_by,created_at) VALUES(?,?,?,?,?,?,?,'suite',?,?,?,?,?,'pending',?,?)"),
			run.ID, run.AgentID, nullable(run.AgentVersionID), run.ConfigHash, boolValue(r.s.dialect, run.Draft), nullableBytes(run.DraftConfigJSON), nullableBytes(run.DraftToolsJSON),
			run.Scope, nullable(run.CaseID), run.Providers, run.Runs, run.MinPass, run.TriggeredBy, utc(now)); err != nil {
			return fmt.Errorf("insert eval run: %w", err)
		}
		inserted := 0
		for _, j := range jobs {
			if j.ID == "" {
				j.ID = NewID()
			}
			if j.MaxAttempts == 0 {
				j.MaxAttempts = 3
			}
			if j.RunAfter.IsZero() {
				j.RunAfter = now
			}
			res, err := tx.ExecContext(ctx, r.s.bind("INSERT INTO eval_jobs(id,kind,agent_id,agent_version_id,run_id,subject,scorer,idempotency_key,status,attempts,max_attempts,run_after,payload,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,'pending',0,?,?,?,?,?) ON CONFLICT(idempotency_key) DO NOTHING"),
				j.ID, j.Kind, run.AgentID, nullable(run.AgentVersionID), run.ID, j.Subject, nullable(j.Scorer), j.IdempotencyKey, j.MaxAttempts, utc(j.RunAfter), string(j.Payload), utc(now), utc(now))
			if err != nil {
				return fmt.Errorf("insert eval job: %w", err)
			}
			n, _ := res.RowsAffected()
			inserted += int(n)
		}
		if inserted == 0 {
			return ErrNoJobs
		}
		return nil
	})
	if err != nil {
		return EvalRun{}, err
	}
	return run, nil
}

// lockAgent takes the agent row's lock for the rest of the transaction, so that two transactions that count the
// agent's rows and then insert (the queue and case caps) run one after the other instead of both counting before
// either inserts. The key columns do not change, so Postgres takes a FOR NO KEY UPDATE lock, which does not block the
// foreign-key checks of rows inserted meanwhile (the agents trigger fires only on UPDATE OF current_version_id). A call
// starting for the agent (Sessions().Create selects the agent FOR UPDATE) waits for the transaction, which is short.
func lockAgent(ctx context.Context, tx *sql.Tx, s *Store, agentID, what string) error {
	if _, err := tx.ExecContext(ctx, s.bind("UPDATE agents SET updated_at=updated_at WHERE id=?"), agentID); err != nil {
		return fmt.Errorf("lock agent for %s: %w", what, err)
	}
	return nil
}

func nullableBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

// GetRun returns sql.ErrNoRows when the agent has no such run.
func (r EvalRepository) GetRun(ctx context.Context, agentID, id string) (EvalRun, error) {
	return scanEvalRun(r.s.db.QueryRowContext(ctx, r.s.bind("SELECT "+evalRunCols+" FROM eval_runs WHERE id=? AND agent_id=?"), id, agentID))
}

// ListRuns returns the agent's runs, newest first; limit is clamped to 1..200.
func (r EvalRepository) ListRuns(ctx context.Context, agentID string, includeDrafts bool, limit, offset int) ([]EvalRun, error) {
	limit = min(max(limit, 1), maxListRuns)
	offset = max(offset, 0)
	q := "SELECT " + evalRunCols + " FROM eval_runs WHERE agent_id=?"
	if !includeDrafts {
		q += " AND NOT draft"
		if r.s.dialect == SQLite {
			q = strings.Replace(q, "NOT draft", "draft=0", 1)
		}
	}
	rows, err := r.s.db.QueryContext(ctx, r.s.bind(q+" ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?"), agentID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list eval runs: %w", err)
	}
	return collect(rows, make([]EvalRun, 0, limit), func(row rowScanner) (EvalRun, error) {
		e, err := scanEvalRun(row)
		if err != nil {
			return e, fmt.Errorf("scan eval run: %w", err)
		}
		return e, nil
	})
}

// RunJobs returns the run's jobs ordered by subject (a run has at most MaxPendingJobsPerAgent).
func (r EvalRepository) RunJobs(ctx context.Context, runID string) ([]EvalJob, error) {
	rows, err := r.s.db.QueryContext(ctx, r.s.bind("SELECT "+evalJobCols+" FROM eval_jobs WHERE run_id=? ORDER BY subject, id LIMIT ?"), runID, MaxPendingJobsPerAgent)
	if err != nil {
		return nil, fmt.Errorf("list eval jobs: %w", err)
	}
	return collect(rows, make([]EvalJob, 0, 16), func(row rowScanner) (EvalJob, error) {
		j, err := scanEvalJob(row)
		if err != nil {
			return j, fmt.Errorf("scan eval job: %w", err)
		}
		return j, nil
	})
}

const evalResultCols = "id,subject_kind,subject_id,scorer,scorer_version,version_key,agent_id,agent_version_id,config_hash,run_id,case_id,case_hash,session_id,score,outcome,reason,judge_model,created_at"

// RunResults returns the run's results ordered by subject and scorer, at most 2000.
func (r EvalRepository) RunResults(ctx context.Context, runID string) ([]EvalResult, error) {
	rows, err := r.s.db.QueryContext(ctx, r.s.bind("SELECT "+evalResultCols+" FROM eval_results WHERE run_id=? ORDER BY subject_id, scorer LIMIT ?"), runID, maxRunResults)
	if err != nil {
		return nil, fmt.Errorf("list eval results: %w", err)
	}
	return collect(rows, make([]EvalResult, 0, 16), func(row rowScanner) (EvalResult, error) {
		var e EvalResult
		var id string
		var ver, run, caseID, caseHash, judge sql.NullString
		var created string
		if err := row.Scan(&id, &e.SubjectKind, &e.SubjectID, &e.Scorer, &e.ScorerVersion, &e.VersionKey, &e.AgentID, &ver, &e.ConfigHash, &run, &caseID, &caseHash, &e.SessionID, &e.Score, &e.Outcome, &e.Reason, &judge, &created); err != nil {
			return e, fmt.Errorf("scan eval result: %w", err)
		}
		e.AgentVersionID, e.RunID, e.CaseID, e.CaseHash, e.JudgeModel, e.CreatedAt = ver.String, run.String, caseID.String, caseHash.String, judge.String, scanTime(created)
		return e, nil
	})
}

// MarkRunStarted moves a pending run to running; any other status is left as it is.
func (r EvalRepository) MarkRunStarted(ctx context.Context, runID string, now time.Time) error {
	if _, err := r.s.db.ExecContext(ctx, r.s.bind("UPDATE eval_runs SET status='running', started_at=? WHERE id=? AND status='pending'"), utc(now), runID); err != nil {
		return fmt.Errorf("mark eval run started: %w", err)
	}
	return nil
}

// ClaimEnds counts what a Claim ended besides the job it took: jobs whose lease expired after their last attempt
// (dead, lease_expired) and the runs that finished with them.
type ClaimEnds struct{ Dead, RunsFinished int }

// Claim takes one due job: pending and past run_after, or running with an expired lease. Candidates are read without a
// lock; each is then claimed with one conditional UPDATE ... RETURNING that repeats the claimability test, so when
// several workers race for one row the database lets exactly one UPDATE match it; the others get no row and move to
// the next candidate. The returned row is the one this UPDATE wrote, never a later re-read. now is the worker's clock:
// leases are compared against worker-supplied time, never the database's. A running job whose lease expired with no
// attempts left is ended as dead (category lease_expired) instead of claimed; ClaimEnds says how many Claim ended so, and
// how many runs that finished, so the caller can count them.
func (r EvalRepository) Claim(ctx context.Context, owner string, now time.Time, lease time.Duration) (EvalJob, bool, ClaimEnds, error) {
	var ends ClaimEnds
	n := utc(now)
	rows, err := r.s.db.QueryContext(ctx, r.s.bind("SELECT id FROM eval_jobs WHERE (status='pending' AND run_after<=?) OR (status='running' AND lease_expires_at<=?) ORDER BY run_after, id LIMIT ?"), n, n, claimCandidates)
	if err != nil {
		return EvalJob{}, false, ends, fmt.Errorf("select eval job candidates: %w", err)
	}
	ids := make([]string, 0, claimCandidates)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return EvalJob{}, false, ends, fmt.Errorf("scan eval job candidate: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return EvalJob{}, false, ends, err
	}
	rows.Close()
	for _, id := range ids {
		j, err := scanEvalJob(r.s.db.QueryRowContext(ctx, r.s.bind("UPDATE eval_jobs SET status='running', lease_owner=?, lease_expires_at=?, attempts=attempts+1, updated_at=? WHERE id=? AND attempts<max_attempts AND ((status='pending' AND run_after<=?) OR (status='running' AND lease_expires_at<=?)) RETURNING "+evalJobCols),
			owner, utc(now.Add(lease)), n, id, n, n))
		if err == nil {
			return j, true, ends, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return EvalJob{}, false, ends, fmt.Errorf("claim eval job: %w", err)
		}
		dead, finished, err := r.deadExhausted(ctx, id, now) // lost the race, or the job has no attempts left
		if err != nil {
			return EvalJob{}, false, ends, err
		}
		if dead {
			ends.Dead++
		}
		if finished {
			ends.RunsFinished++
		}
	}
	return EvalJob{}, false, ends, nil
}

// deadExhausted ends a running job whose lease expired after its last attempt; the guard makes it a no-op for any other state.
func (r EvalRepository) deadExhausted(ctx context.Context, id string, now time.Time) (dead, runFinished bool, err error) {
	err = r.s.WithTx(ctx, func(tx *sql.Tx) error {
		n := utc(now)
		var run sql.NullString
		err := tx.QueryRowContext(ctx, r.s.bind("UPDATE eval_jobs SET status='dead', lease_owner=NULL, lease_expires_at=NULL, last_error_category='lease_expired', updated_at=? WHERE id=? AND status='running' AND lease_expires_at<=? AND attempts>=max_attempts RETURNING run_id"), n, id, n).Scan(&run)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("end exhausted eval job: %w", err)
		}
		dead = true
		if !run.Valid {
			return nil
		}
		runFinished, err = r.finishRunIfDone(ctx, tx, run.String, now)
		return err
	})
	return dead && err == nil, runFinished && err == nil, err
}

// A write by a worker is fenced by the claimed attempt: lease_owner and attempts must both still be the claim's. The
// owner alone is not enough, because an owner string can be reused (a restart, a shared slot name) and a stale holder of
// an earlier attempt would then pass for the current one. attempts only grows, on a claim.

// Heartbeat extends the lease of a claimed job still running under its claim; ErrLeaseLost when it is not.
func (r EvalRepository) Heartbeat(ctx context.Context, job EvalJob, now time.Time, lease time.Duration) error {
	res, err := r.s.db.ExecContext(ctx, r.s.bind("UPDATE eval_jobs SET lease_expires_at=?, updated_at=? WHERE id=? AND status='running' AND lease_owner=? AND attempts=?"), utc(now.Add(lease)), utc(now), job.ID, job.LeaseOwner, job.Attempts)
	if err != nil {
		return fmt.Errorf("extend eval job lease: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrLeaseLost
	}
	return nil
}

// Complete marks the claimed job done and stores its results (ON CONFLICT DO NOTHING on the result key, so a replay
// never double-counts) in one transaction, then finishes the run when this was its last open job. ErrLeaseLost, with
// nothing written, when the job is no longer running under the claim. For a run_case job the storage layer, not the
// worker, sets each result's run and subject (EvalCaseRunSubject of the job's own run and subject): a result that
// names another run is refused (ErrResultRunMismatch), and the run summary can never miss a failure for a subject
// written in another shape. now stamps the rows.
func (r EvalRepository) Complete(ctx context.Context, job EvalJob, now time.Time, results []EvalResult) (runFinished bool, err error) {
	err = r.s.WithTx(ctx, func(tx *sql.Tx) error {
		var run sql.NullString
		var subject, kind string
		err := tx.QueryRowContext(ctx, r.s.bind("UPDATE eval_jobs SET status='done', lease_owner=NULL, lease_expires_at=NULL, updated_at=? WHERE id=? AND status='running' AND lease_owner=? AND attempts=? RETURNING run_id, subject, kind"),
			utc(now), job.ID, job.LeaseOwner, job.Attempts).Scan(&run, &subject, &kind)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrLeaseLost
		}
		if err != nil {
			return fmt.Errorf("complete eval job: %w", err)
		}
		gone := map[string]bool{} // session id -> erased
		for _, e := range results {
			if e.Reason != "" && e.SessionID != "" {
				if _, seen := gone[e.SessionID]; !seen {
					if gone[e.SessionID], err = r.sessionGone(ctx, tx, e.SessionID); err != nil {
						return err
					}
				}
				if gone[e.SessionID] {
					e.Reason = "" // the call was erased while it was scored; its reason can quote it
				}
			}
			if run.Valid && kind == "run_case" {
				if e.RunID != "" && e.RunID != run.String {
					return ErrResultRunMismatch
				}
				e.RunID, e.SubjectID = run.String, EvalCaseRunSubject(run.String, subject)
			}
			if _, err := tx.ExecContext(ctx, r.s.bind("INSERT INTO eval_results("+evalResultCols+") VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(subject_id,scorer,version_key) DO NOTHING"),
				NewID(), e.SubjectKind, e.SubjectID, e.Scorer, e.ScorerVersion, e.VersionKey, e.AgentID, nullable(e.AgentVersionID), e.ConfigHash, nullable(e.RunID), nullable(e.CaseID), nullable(e.CaseHash), e.SessionID, e.Score, e.Outcome, e.Reason, nullable(e.JudgeModel), utc(now)); err != nil {
				return fmt.Errorf("insert eval result: %w", err)
			}
		}
		if !run.Valid {
			return nil
		}
		runFinished, err = r.finishRunIfDone(ctx, tx, run.String, now)
		return err
	})
	return runFinished && err == nil, err
}

// sessionGone reports whether the scored call no longer exists (an erasure removed it). On Postgres it share-locks
// the row, so an erasure either finishes first (the row is gone) or waits for this transaction and then blanks the
// reasons it stored; SQLite has one writer, so the two never interleave.
func (r EvalRepository) sessionGone(ctx context.Context, tx *sql.Tx, id string) (bool, error) {
	query := "SELECT 1 FROM sessions WHERE id=?"
	if r.s.dialect == Postgres {
		query += " FOR SHARE"
	}
	var one int
	err := tx.QueryRowContext(ctx, r.s.bind(query), id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("check scored call: %w", err)
	}
	return false, nil
}

// Fail records a failed attempt of a claimed job. With final, or when the attempts are used up, the job is dead;
// otherwise it goes back to pending, due after retryAfter. category is a short error class, never content.
// ErrLeaseLost when the job is no longer running under the claim.
func (r EvalRepository) Fail(ctx context.Context, job EvalJob, now time.Time, category string, retryAfter time.Duration, final bool) (dead, runFinished bool, err error) {
	err = r.s.WithTx(ctx, func(tx *sql.Tx) error {
		var status string
		var run sql.NullString
		err := tx.QueryRowContext(ctx, r.s.bind("UPDATE eval_jobs SET status=CASE WHEN ? OR attempts>=max_attempts THEN 'dead' ELSE 'pending' END, run_after=?, lease_owner=NULL, lease_expires_at=NULL, last_error_category=?, updated_at=? WHERE id=? AND status='running' AND lease_owner=? AND attempts=? RETURNING status, run_id"),
			boolValue(r.s.dialect, final), utc(now.Add(retryAfter)), category, utc(now), job.ID, job.LeaseOwner, job.Attempts).Scan(&status, &run)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrLeaseLost
		}
		if err != nil {
			return fmt.Errorf("fail eval job: %w", err)
		}
		dead = status == "dead"
		if dead && run.Valid {
			runFinished, err = r.finishRunIfDone(ctx, tx, run.String, now)
		}
		return err
	})
	if err != nil {
		return false, false, err
	}
	return dead, runFinished, nil
}

// finishRunIfDone ends the run when none of its jobs is pending or running, and reports whether this call ended it.
// A case-run passes when its job is done and none of its results failed or errored (a dead job is a failure); a case
// passes when at least min_pass of its case-runs pass. The first statement writes the run row to take its row lock:
// two last jobs finishing in concurrent transactions would otherwise each still see the other as running and neither
// would finish the run. The lock serialises them so the second one counts after the first has committed.
func (r EvalRepository) finishRunIfDone(ctx context.Context, tx *sql.Tx, runID string, now time.Time) (bool, error) {
	var minPass int
	if err := tx.QueryRowContext(ctx, r.s.bind("UPDATE eval_runs SET status=status WHERE id=? RETURNING min_pass"), runID).Scan(&minPass); err != nil {
		return false, fmt.Errorf("lock eval run: %w", err)
	}
	var open int
	if err := tx.QueryRowContext(ctx, r.s.bind("SELECT count(*) FROM eval_jobs WHERE run_id=? AND status IN ('pending','running')"), runID).Scan(&open); err != nil {
		return false, fmt.Errorf("count open eval jobs: %w", err)
	}
	if open > 0 {
		return false, nil
	}
	rows, err := tx.QueryContext(ctx, r.s.bind(`SELECT j.subject, j.status, EXISTS(SELECT 1 FROM eval_results e WHERE e.run_id=j.run_id AND e.subject_id=j.run_id || '/' || j.subject AND e.outcome IN ('fail','error'))
		FROM eval_jobs j WHERE j.run_id=? LIMIT ?`), runID, MaxPendingJobsPerAgent)
	if err != nil {
		return false, fmt.Errorf("read eval run jobs: %w", err)
	}
	passing := map[string]int{} // case id -> passing case-runs (a key with 0 is a case with no passing run)
	for rows.Next() {
		var subject, status string
		var failed any
		if err := rows.Scan(&subject, &status, &failed); err != nil {
			rows.Close()
			return false, fmt.Errorf("scan eval run job: %w", err)
		}
		caseID, _, _ := strings.Cut(subject, "#")
		if _, ok := passing[caseID]; !ok {
			passing[caseID] = 0
		}
		if status == "done" && !truthy(failed) {
			passing[caseID]++
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, err
	}
	rows.Close()
	passed := 0
	for _, n := range passing {
		if n >= minPass {
			passed++
		}
	}
	res, err := tx.ExecContext(ctx, r.s.bind("UPDATE eval_runs SET status='done', started_at=COALESCE(started_at,?), ended_at=?, passed_cases=?, total_cases=? WHERE id=? AND status<>'done'"), utc(now), utc(now), passed, len(passing), runID)
	if err != nil {
		return false, fmt.Errorf("finish eval run: %w", err)
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// truthy reads a boolean column: Postgres returns bool, SQLite an integer.
func truthy(v any) bool {
	switch b := v.(type) {
	case bool:
		return b
	case int64:
		return b != 0
	}
	return false
}
