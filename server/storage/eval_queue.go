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
	// ErrLeaseLost: the job is no longer running under this owner (its lease expired and another worker took it, or it finished).
	ErrLeaseLost = errors.New("eval job lease lost")
	ErrQueueFull = errors.New("too many pending eval jobs for the agent")
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
// MaxPendingJobsPerAgent, and nothing is written.
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
			if _, err := tx.ExecContext(ctx, r.s.bind("INSERT INTO eval_jobs(id,kind,agent_id,agent_version_id,run_id,subject,scorer,idempotency_key,status,attempts,max_attempts,run_after,payload,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,'pending',0,?,?,?,?,?) ON CONFLICT(idempotency_key) DO NOTHING"),
				j.ID, j.Kind, run.AgentID, nullable(run.AgentVersionID), run.ID, j.Subject, nullable(j.Scorer), j.IdempotencyKey, j.MaxAttempts, utc(j.RunAfter), string(j.Payload), utc(now), utc(now)); err != nil {
				return fmt.Errorf("insert eval job: %w", err)
			}
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
	defer rows.Close()
	out := make([]EvalRun, 0, limit)
	for rows.Next() {
		e, err := scanEvalRun(rows)
		if err != nil {
			return nil, fmt.Errorf("scan eval run: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// RunJobs returns the run's jobs ordered by subject (a run has at most MaxPendingJobsPerAgent).
func (r EvalRepository) RunJobs(ctx context.Context, runID string) ([]EvalJob, error) {
	rows, err := r.s.db.QueryContext(ctx, r.s.bind("SELECT "+evalJobCols+" FROM eval_jobs WHERE run_id=? ORDER BY subject, id LIMIT ?"), runID, MaxPendingJobsPerAgent)
	if err != nil {
		return nil, fmt.Errorf("list eval jobs: %w", err)
	}
	defer rows.Close()
	out := make([]EvalJob, 0, 16)
	for rows.Next() {
		j, err := scanEvalJob(rows)
		if err != nil {
			return nil, fmt.Errorf("scan eval job: %w", err)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

const evalResultCols = "id,subject_kind,subject_id,scorer,scorer_version,version_key,agent_id,agent_version_id,config_hash,run_id,case_id,case_hash,session_id,score,outcome,reason,judge_model,created_at"

// RunResults returns the run's results ordered by subject and scorer, at most 2000.
func (r EvalRepository) RunResults(ctx context.Context, runID string) ([]EvalResult, error) {
	rows, err := r.s.db.QueryContext(ctx, r.s.bind("SELECT "+evalResultCols+" FROM eval_results WHERE run_id=? ORDER BY subject_id, scorer LIMIT ?"), runID, maxRunResults)
	if err != nil {
		return nil, fmt.Errorf("list eval results: %w", err)
	}
	defer rows.Close()
	out := make([]EvalResult, 0, 16)
	for rows.Next() {
		var e EvalResult
		var id string
		var ver, run, caseID, caseHash, judge sql.NullString
		var created string
		if err := rows.Scan(&id, &e.SubjectKind, &e.SubjectID, &e.Scorer, &e.ScorerVersion, &e.VersionKey, &e.AgentID, &ver, &e.ConfigHash, &run, &caseID, &caseHash, &e.SessionID, &e.Score, &e.Outcome, &e.Reason, &judge, &created); err != nil {
			return nil, fmt.Errorf("scan eval result: %w", err)
		}
		e.AgentVersionID, e.RunID, e.CaseID, e.CaseHash, e.JudgeModel, e.CreatedAt = ver.String, run.String, caseID.String, caseHash.String, judge.String, scanTime(created)
		out = append(out, e)
	}
	return out, rows.Err()
}

// MarkRunStarted moves a pending run to running; any other status is left as it is.
func (r EvalRepository) MarkRunStarted(ctx context.Context, runID string, now time.Time) error {
	if _, err := r.s.db.ExecContext(ctx, r.s.bind("UPDATE eval_runs SET status='running', started_at=? WHERE id=? AND status='pending'"), utc(now), runID); err != nil {
		return fmt.Errorf("mark eval run started: %w", err)
	}
	return nil
}

// Claim takes one due job: pending and past run_after, or running with an expired lease. Candidates are read without a
// lock; each is then claimed with a conditional UPDATE that repeats the claimability test, so when several workers
// race for one row the database lets exactly one UPDATE match it (RowsAffected 1) and the others move to the next
// candidate. now is the worker's clock: leases are compared against worker-supplied time, never the database's.
// A running job whose lease expired with no attempts left is ended as dead (category lease_expired) instead of claimed.
func (r EvalRepository) Claim(ctx context.Context, owner string, now time.Time, lease time.Duration) (EvalJob, bool, error) {
	n := utc(now)
	rows, err := r.s.db.QueryContext(ctx, r.s.bind("SELECT id FROM eval_jobs WHERE (status='pending' AND run_after<=?) OR (status='running' AND lease_expires_at<=?) ORDER BY run_after, id LIMIT ?"), n, n, claimCandidates)
	if err != nil {
		return EvalJob{}, false, fmt.Errorf("select eval job candidates: %w", err)
	}
	ids := make([]string, 0, claimCandidates)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return EvalJob{}, false, fmt.Errorf("scan eval job candidate: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return EvalJob{}, false, err
	}
	rows.Close()
	for _, id := range ids {
		res, err := r.s.db.ExecContext(ctx, r.s.bind("UPDATE eval_jobs SET status='running', lease_owner=?, lease_expires_at=?, attempts=attempts+1, updated_at=? WHERE id=? AND attempts<max_attempts AND ((status='pending' AND run_after<=?) OR (status='running' AND lease_expires_at<=?))"),
			owner, utc(now.Add(lease)), n, id, n, n)
		if err != nil {
			return EvalJob{}, false, fmt.Errorf("claim eval job: %w", err)
		}
		if won, _ := res.RowsAffected(); won == 1 {
			j, err := scanEvalJob(r.s.db.QueryRowContext(ctx, r.s.bind("SELECT "+evalJobCols+" FROM eval_jobs WHERE id=?"), id))
			if err != nil {
				return EvalJob{}, false, fmt.Errorf("read claimed eval job: %w", err)
			}
			return j, true, nil
		}
		if err := r.deadExhausted(ctx, id, now); err != nil {
			return EvalJob{}, false, err
		}
	}
	return EvalJob{}, false, nil
}

// deadExhausted ends a running job whose lease expired after its last attempt; the guard makes it a no-op for any other state.
func (r EvalRepository) deadExhausted(ctx context.Context, id string, now time.Time) error {
	return r.s.WithTx(ctx, func(tx *sql.Tx) error {
		n := utc(now)
		res, err := tx.ExecContext(ctx, r.s.bind("UPDATE eval_jobs SET status='dead', lease_owner=NULL, lease_expires_at=NULL, last_error_category='lease_expired', updated_at=? WHERE id=? AND status='running' AND lease_expires_at<=? AND attempts>=max_attempts"), n, id, n)
		if err != nil {
			return fmt.Errorf("end exhausted eval job: %w", err)
		}
		if k, _ := res.RowsAffected(); k != 1 {
			return nil
		}
		var run sql.NullString
		if err := tx.QueryRowContext(ctx, r.s.bind("SELECT run_id FROM eval_jobs WHERE id=?"), id).Scan(&run); err != nil {
			return fmt.Errorf("read exhausted eval job: %w", err)
		}
		if !run.Valid {
			return nil
		}
		_, err = r.finishRunIfDone(ctx, tx, run.String, now)
		return err
	})
}

// Heartbeat extends the lease of a job still running under owner; ErrLeaseLost when it is not.
func (r EvalRepository) Heartbeat(ctx context.Context, id, owner string, now time.Time, lease time.Duration) error {
	res, err := r.s.db.ExecContext(ctx, r.s.bind("UPDATE eval_jobs SET lease_expires_at=?, updated_at=? WHERE id=? AND status='running' AND lease_owner=?"), utc(now.Add(lease)), utc(now), id, owner)
	if err != nil {
		return fmt.Errorf("extend eval job lease: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrLeaseLost
	}
	return nil
}

// Complete marks the job done and stores its results (ON CONFLICT DO NOTHING on the result key, so a replay never
// double-counts) in one transaction, then finishes the run when this was its last open job. ErrLeaseLost, with
// nothing written, when the job is not running under owner. now stamps the rows.
func (r EvalRepository) Complete(ctx context.Context, id, owner string, now time.Time, results []EvalResult) (runFinished bool, err error) {
	err = r.s.WithTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, r.s.bind("UPDATE eval_jobs SET status='done', lease_owner=NULL, lease_expires_at=NULL, updated_at=? WHERE id=? AND status='running' AND lease_owner=?"), utc(now), id, owner)
		if err != nil {
			return fmt.Errorf("complete eval job: %w", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrLeaseLost
		}
		for _, e := range results {
			if _, err := tx.ExecContext(ctx, r.s.bind("INSERT INTO eval_results("+evalResultCols+") VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(subject_id,scorer,version_key) DO NOTHING"),
				NewID(), e.SubjectKind, e.SubjectID, e.Scorer, e.ScorerVersion, e.VersionKey, e.AgentID, nullable(e.AgentVersionID), e.ConfigHash, nullable(e.RunID), nullable(e.CaseID), nullable(e.CaseHash), e.SessionID, e.Score, e.Outcome, e.Reason, nullable(e.JudgeModel), utc(now)); err != nil {
				return fmt.Errorf("insert eval result: %w", err)
			}
		}
		runFinished, err = r.finishJobRun(ctx, tx, id, now)
		return err
	})
	return runFinished && err == nil, err
}

// Fail records a failed attempt of a job running under owner. With final, or when the attempts are used up, the job
// is dead; otherwise it goes back to pending, due after retryAfter. category is a short error class, never content.
// ErrLeaseLost when the job is not running under owner.
func (r EvalRepository) Fail(ctx context.Context, id, owner string, now time.Time, category string, retryAfter time.Duration, final bool) (dead, runFinished bool, err error) {
	err = r.s.WithTx(ctx, func(tx *sql.Tx) error {
		var attempts, maxAttempts int
		if err := tx.QueryRowContext(ctx, r.s.bind("SELECT attempts,max_attempts FROM eval_jobs WHERE id=? AND status='running' AND lease_owner=?"), id, owner).Scan(&attempts, &maxAttempts); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrLeaseLost
			}
			return fmt.Errorf("read eval job: %w", err)
		}
		dead = final || attempts >= maxAttempts
		status, after := "pending", now.Add(retryAfter)
		if dead {
			status = "dead"
		}
		// The ownership guard is repeated: attempts only change under a new owner, but the write must still lose to one.
		res, err := tx.ExecContext(ctx, r.s.bind("UPDATE eval_jobs SET status=?, run_after=?, lease_owner=NULL, lease_expires_at=NULL, last_error_category=?, updated_at=? WHERE id=? AND status='running' AND lease_owner=?"),
			status, utc(after), category, utc(now), id, owner)
		if err != nil {
			return fmt.Errorf("fail eval job: %w", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrLeaseLost
		}
		if dead {
			runFinished, err = r.finishJobRun(ctx, tx, id, now)
		}
		return err
	})
	if err != nil {
		return false, false, err
	}
	return dead, runFinished, nil
}

func (r EvalRepository) finishJobRun(ctx context.Context, tx *sql.Tx, jobID string, now time.Time) (bool, error) {
	var run sql.NullString
	if err := tx.QueryRowContext(ctx, r.s.bind("SELECT run_id FROM eval_jobs WHERE id=?"), jobID).Scan(&run); err != nil {
		return false, fmt.Errorf("read eval job run: %w", err)
	}
	if !run.Valid {
		return false, nil
	}
	return r.finishRunIfDone(ctx, tx, run.String, now)
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
