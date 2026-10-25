package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// MaxEvalCases bounds the saved cases of one agent.
const MaxEvalCases = 200

// Bounds of the dashboard reads: agents per fleet page, runs per history and runs in one trend.
const (
	MaxFleetPage    = 200
	MaxEvalHistory  = 100
	fleetTrendRuns  = 10
	defaultPassRate = 1.0 // the threshold of an agent with no saved suite
)

var (
	ErrDuplicateCaseName = errors.New("an eval case with this name already exists for the agent")
	ErrTooManyCases      = errors.New("too many eval cases for the agent")
)

// EvalCase is one saved scenario of an agent (not of a version). CallerJSON and ExpectJSON are opaque to storage.
type EvalCase struct {
	ID, AgentID, Name      string
	CallerJSON, ExpectJSON []byte
	ExpectedAnswer         string
	CreatedAt, UpdatedAt   time.Time
}

// EvalSuite is the per-agent suite setting: repeats per case, how many must pass, the pass-rate threshold and the
// default judge (a provider and model from the configured LLM providers; empty means none).
type EvalSuite struct {
	AgentID                   string
	Runs, MinPass             int
	PassThreshold             float64
	JudgeProvider, JudgeModel string
	UpdatedAt                 time.Time
}

// EvalScorerSetting overrides one scorer for one agent. A nil Threshold means the scorer's default.
type EvalScorerSetting struct {
	AgentID, Scorer           string
	Threshold                 *float64
	JudgeProvider, JudgeModel string
	UpdatedAt                 time.Time
}

type EvalRepository struct{ s *Store }

func (s *Store) Eval() EvalRepository { return EvalRepository{s} }

// isUniqueViolation reports a UNIQUE or primary key violation in either dialect: Postgres SQLSTATE 23505, SQLite's
// SQLITE_CONSTRAINT_UNIQUE or SQLITE_CONSTRAINT_PRIMARYKEY extended result codes.
func isUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code == "23505"
	}
	var lite *sqlite.Error
	return errors.As(err, &lite) && (lite.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE || lite.Code() == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY)
}

func scanTime(s string) time.Time {
	t, _ := parseTime(s)
	return t
}

const evalCaseCols = "id,agent_id,name,caller_json,expect_json,expected_answer,created_at,updated_at"

type rowScanner interface{ Scan(dest ...any) error }

func scanEvalCase(r rowScanner) (EvalCase, error) {
	var c EvalCase
	var caller, expect, created, updated string
	var answer sql.NullString
	if err := r.Scan(&c.ID, &c.AgentID, &c.Name, &caller, &expect, &answer, &created, &updated); err != nil {
		return EvalCase{}, err
	}
	c.CallerJSON, c.ExpectJSON, c.ExpectedAnswer = []byte(caller), []byte(expect), answer.String
	c.CreatedAt, c.UpdatedAt = scanTime(created), scanTime(updated)
	return c, nil
}

// CreateCases saves the cases in one transaction under fresh ids: a duplicate name (within the batch or against the
// agent's saved cases) writes nothing. The agent row is locked first, so concurrent batches are counted one after
// the other and the cap is exact.
func (r EvalRepository) CreateCases(ctx context.Context, agentID string, cs []EvalCase) ([]EvalCase, error) {
	out := make([]EvalCase, 0, len(cs))
	err := r.s.WithTx(ctx, func(tx *sql.Tx) error {
		if err := lockAgent(ctx, tx, r.s, agentID, "eval cases"); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRowContext(ctx, r.s.bind("SELECT count(*) FROM eval_cases WHERE agent_id=?"), agentID).Scan(&n); err != nil {
			return fmt.Errorf("count eval cases: %w", err)
		}
		if n+len(cs) > MaxEvalCases {
			return ErrTooManyCases
		}
		now := time.Now()
		for _, c := range cs {
			c.ID, c.AgentID, c.CreatedAt, c.UpdatedAt = NewID(), agentID, now, now
			_, err := tx.ExecContext(ctx, r.s.bind("INSERT INTO eval_cases("+evalCaseCols+") VALUES(?,?,?,?,?,?,?,?)"),
				c.ID, agentID, c.Name, string(c.CallerJSON), string(c.ExpectJSON), nullable(c.ExpectedAnswer), utc(now), utc(now))
			if isUniqueViolation(err) {
				return ErrDuplicateCaseName
			}
			if err != nil {
				return fmt.Errorf("insert eval case: %w", err)
			}
			out = append(out, c)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateCase replaces the case's name, caller, expectations and expected answer. sql.ErrNoRows: no such case.
func (r EvalRepository) UpdateCase(ctx context.Context, c EvalCase) (EvalCase, error) {
	res, err := r.s.db.ExecContext(ctx, r.s.bind("UPDATE eval_cases SET name=?,caller_json=?,expect_json=?,expected_answer=?,updated_at=? WHERE id=? AND agent_id=?"),
		c.Name, string(c.CallerJSON), string(c.ExpectJSON), nullable(c.ExpectedAnswer), utc(time.Now()), c.ID, c.AgentID)
	if isUniqueViolation(err) {
		return EvalCase{}, ErrDuplicateCaseName
	}
	if err != nil {
		return EvalCase{}, fmt.Errorf("update eval case: %w", err)
	}
	if err := oneRow(res, nil); err != nil {
		return EvalCase{}, err
	}
	return r.GetCase(ctx, c.AgentID, c.ID)
}

// DeleteCase returns sql.ErrNoRows when the case does not exist.
func (r EvalRepository) DeleteCase(ctx context.Context, agentID, id string) error {
	res, err := r.s.db.ExecContext(ctx, r.s.bind("DELETE FROM eval_cases WHERE id=? AND agent_id=?"), id, agentID)
	if err != nil {
		return fmt.Errorf("delete eval case: %w", err)
	}
	return oneRow(res, nil)
}

// GetCase returns sql.ErrNoRows when the agent has no such case.
func (r EvalRepository) GetCase(ctx context.Context, agentID, id string) (EvalCase, error) {
	return scanEvalCase(r.s.db.QueryRowContext(ctx, r.s.bind("SELECT "+evalCaseCols+" FROM eval_cases WHERE id=? AND agent_id=?"), id, agentID))
}

// ListCases returns the agent's cases ordered by name, at most MaxEvalCases.
func (r EvalRepository) ListCases(ctx context.Context, agentID string) ([]EvalCase, error) {
	rows, err := r.s.db.QueryContext(ctx, r.s.bind("SELECT "+evalCaseCols+" FROM eval_cases WHERE agent_id=? ORDER BY name LIMIT ?"), agentID, MaxEvalCases)
	if err != nil {
		return nil, fmt.Errorf("list eval cases: %w", err)
	}
	return collect(rows, make([]EvalCase, 0, 8), func(row rowScanner) (EvalCase, error) {
		c, err := scanEvalCase(row)
		if err != nil {
			return c, fmt.Errorf("scan eval case: %w", err)
		}
		return c, nil
	})
}

// Suite returns the agent's suite setting, or the defaults (1 run, 1 must pass, threshold 1.0, no judge) when none is saved.
func (r EvalRepository) Suite(ctx context.Context, agentID string) (EvalSuite, error) {
	su := EvalSuite{AgentID: agentID}
	var jp, jm sql.NullString
	var updated string
	err := r.s.db.QueryRowContext(ctx, r.s.bind("SELECT runs,min_pass,pass_threshold,judge_provider,judge_model,updated_at FROM eval_suites WHERE agent_id=?"), agentID).
		Scan(&su.Runs, &su.MinPass, &su.PassThreshold, &jp, &jm, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return EvalSuite{AgentID: agentID, Runs: 1, MinPass: 1, PassThreshold: defaultPassRate}, nil
	}
	if err != nil {
		return EvalSuite{}, fmt.Errorf("read eval suite: %w", err)
	}
	su.JudgeProvider, su.JudgeModel, su.UpdatedAt = jp.String, jm.String, scanTime(updated)
	return su, nil
}

// PutSuite creates the agent's suite setting or replaces it. The table's CHECKs refuse runs outside 1..5, a min_pass
// outside 1..runs and a threshold outside 0..1.
func (r EvalRepository) PutSuite(ctx context.Context, su EvalSuite) error {
	_, err := r.s.db.ExecContext(ctx, r.s.bind("INSERT INTO eval_suites(agent_id,runs,min_pass,pass_threshold,judge_provider,judge_model,updated_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(agent_id) DO UPDATE SET runs=excluded.runs, min_pass=excluded.min_pass, pass_threshold=excluded.pass_threshold, judge_provider=excluded.judge_provider, judge_model=excluded.judge_model, updated_at=excluded.updated_at"),
		su.AgentID, su.Runs, su.MinPass, su.PassThreshold, nullable(su.JudgeProvider), nullable(su.JudgeModel), utc(time.Now()))
	if err != nil {
		return fmt.Errorf("put eval suite: %w", err)
	}
	return nil
}

// ScorerSettings returns the agent's per-scorer overrides ordered by scorer name.
func (r EvalRepository) ScorerSettings(ctx context.Context, agentID string) ([]EvalScorerSetting, error) {
	rows, err := r.s.db.QueryContext(ctx, r.s.bind("SELECT scorer,threshold,judge_provider,judge_model,updated_at FROM eval_scorer_settings WHERE agent_id=? ORDER BY scorer LIMIT 100"), agentID)
	if err != nil {
		return nil, fmt.Errorf("list eval scorer settings: %w", err)
	}
	return collect(rows, make([]EvalScorerSetting, 0, 5), func(row rowScanner) (EvalScorerSetting, error) {
		st := EvalScorerSetting{AgentID: agentID}
		var th sql.NullFloat64
		var jp, jm sql.NullString
		var updated string
		if err := row.Scan(&st.Scorer, &th, &jp, &jm, &updated); err != nil {
			return st, fmt.Errorf("scan eval scorer setting: %w", err)
		}
		if th.Valid {
			st.Threshold = &th.Float64
		}
		st.JudgeProvider, st.JudgeModel, st.UpdatedAt = jp.String, jm.String, scanTime(updated)
		return st, nil
	})
}

// PutScorerSetting creates or replaces one scorer's override for the agent.
func (r EvalRepository) PutScorerSetting(ctx context.Context, st EvalScorerSetting) error {
	var th any
	if st.Threshold != nil {
		th = *st.Threshold
	}
	_, err := r.s.db.ExecContext(ctx, r.s.bind("INSERT INTO eval_scorer_settings(agent_id,scorer,threshold,judge_provider,judge_model,updated_at) VALUES(?,?,?,?,?,?) ON CONFLICT(agent_id,scorer) DO UPDATE SET threshold=excluded.threshold, judge_provider=excluded.judge_provider, judge_model=excluded.judge_model, updated_at=excluded.updated_at"),
		st.AgentID, st.Scorer, th, nullable(st.JudgeProvider), nullable(st.JudgeModel), utc(time.Now()))
	if err != nil {
		return fmt.Errorf("put eval scorer setting: %w", err)
	}
	return nil
}

// FleetRun is the qualifying run an agent's dashboard row stands on.
type FleetRun struct {
	RunID         string
	Version       int
	Passed, Total int
	EndedAt       time.Time
}

// FleetRow is one agent on the dashboard. Latest is nil when the agent has no qualifying run; Trend holds the pass
// rates of up to 10 latest qualifying runs, oldest first (a run with no cases has no rate and is left out).
type FleetRow struct {
	AgentID, Name string
	Threshold     float64
	Latest        *FleetRun
	Trend         []float64
	Below         bool
}

// FleetPage is one page of non-archived agents (by name) and the counts over all of them.
type FleetPage struct {
	Agents         []FleetRow
	Total          int
	BelowThreshold int
}

// qualifyingRuns is what the dashboard and history count: a finished, non-draft, whole-suite run.
const qualifyingRuns = "r.status='done' AND r.draft=? AND r.scope='suite' AND r.ended_at IS NOT NULL"

const rankedRuns = "ROW_NUMBER() OVER (PARTITION BY r.agent_id ORDER BY r.ended_at DESC, r.id DESC)"

// Fleet returns a page of non-archived agents with their latest qualifying suite run and trend, in four statements
// whatever the page size: the agents, their count, the ranked runs of the page, and the below-threshold count over
// every non-archived agent. limit is clamped to 1..MaxFleetPage.
func (r EvalRepository) Fleet(ctx context.Context, limit, offset int) (FleetPage, error) {
	limit = min(max(limit, 1), MaxFleetPage)
	offset = max(offset, 0)
	d := r.s.dialect
	notArchived, notDraft := boolValue(d, false), boolValue(d, false)
	var page FleetPage
	rows, err := r.s.db.QueryContext(ctx, r.s.bind("SELECT a.id,a.name,COALESCE(s.pass_threshold,?) FROM agents a LEFT JOIN eval_suites s ON s.agent_id=a.id WHERE a.archived=? ORDER BY a.name,a.id LIMIT ? OFFSET ?"),
		defaultPassRate, notArchived, limit, offset)
	if err != nil {
		return FleetPage{}, fmt.Errorf("list fleet agents: %w", err)
	}
	defer rows.Close()
	page.Agents = make([]FleetRow, 0, limit)
	byID := make(map[string]int, limit)
	args := make([]any, 0, limit+1)
	marks := make([]string, 0, limit)
	for rows.Next() {
		var f FleetRow
		if err := rows.Scan(&f.AgentID, &f.Name, &f.Threshold); err != nil {
			return FleetPage{}, fmt.Errorf("scan fleet agent: %w", err)
		}
		byID[f.AgentID] = len(page.Agents)
		page.Agents = append(page.Agents, f)
		args, marks = append(args, f.AgentID), append(marks, "?")
	}
	if err := rows.Err(); err != nil {
		return FleetPage{}, err
	}
	rows.Close()
	if err := r.s.db.QueryRowContext(ctx, r.s.bind("SELECT count(*) FROM agents WHERE archived=?"), notArchived).Scan(&page.Total); err != nil {
		return FleetPage{}, fmt.Errorf("count fleet agents: %w", err)
	}
	if len(args) > 0 {
		q := "SELECT agent_id,run_id,version,passed,total,ended_at FROM (SELECT r.agent_id AS agent_id, r.id AS run_id, v.version AS version, COALESCE(r.passed_cases,0) AS passed, COALESCE(r.total_cases,0) AS total, r.ended_at AS ended_at, " +
			rankedRuns + " AS rn FROM eval_runs r JOIN agent_versions v ON v.id=r.agent_version_id WHERE r.agent_id IN (" + strings.Join(marks, ",") + ") AND " + qualifyingRuns +
			") t WHERE rn<=? ORDER BY agent_id, rn DESC"
		runs, err := r.s.db.QueryContext(ctx, r.s.bind(q), append(args, notDraft, fleetTrendRuns)...)
		if err != nil {
			return FleetPage{}, fmt.Errorf("list fleet runs: %w", err)
		}
		defer runs.Close()
		for runs.Next() {
			var agentID, ended string
			var run FleetRun
			if err := runs.Scan(&agentID, &run.RunID, &run.Version, &run.Passed, &run.Total, &ended); err != nil {
				return FleetPage{}, fmt.Errorf("scan fleet run: %w", err)
			}
			run.EndedAt = scanTime(ended)
			f := &page.Agents[byID[agentID]]
			f.Latest = &run // rows arrive oldest first, so the last one stays
			if run.Total > 0 {
				f.Trend = append(f.Trend, float64(run.Passed)/float64(run.Total))
			}
		}
		if err := runs.Err(); err != nil {
			return FleetPage{}, err
		}
	}
	for i := range page.Agents {
		f := &page.Agents[i]
		f.Below = f.Latest != nil && f.Latest.Total > 0 && float64(f.Latest.Passed)/float64(f.Latest.Total) < f.Threshold
	}
	err = r.s.db.QueryRowContext(ctx, r.s.bind("SELECT count(*) FROM (SELECT r.agent_id AS agent_id, r.passed_cases AS passed, r.total_cases AS total, "+rankedRuns+
		" AS rn FROM eval_runs r JOIN agents a ON a.id=r.agent_id WHERE a.archived=? AND "+qualifyingRuns+") t LEFT JOIN eval_suites s ON s.agent_id=t.agent_id WHERE t.rn=1 AND CAST(t.passed AS DOUBLE PRECISION)/NULLIF(t.total,0) < COALESCE(s.pass_threshold,?)"),
		notArchived, notDraft, defaultPassRate).Scan(&page.BelowThreshold)
	if err != nil {
		return FleetPage{}, fmt.Errorf("count agents below threshold: %w", err)
	}
	return page, nil
}

// ScorerAgg is one scorer's outcomes in one run. Mean is over the pass and fail outcomes only; skipped and error
// results are counted but have no score to average.
type ScorerAgg struct {
	Scorer                      string
	Mean                        float64
	Pass, Fail, Skipped, Errors int
}

// HistoryRow is one qualifying suite run with its per-scorer aggregates.
type HistoryRow struct {
	RunID         string
	Version       int
	EndedAt       time.Time
	Passed, Total int
	Scorers       []ScorerAgg
}

// History returns the agent's latest qualifying suite runs, newest first; limit is clamped to 1..MaxEvalHistory.
func (r EvalRepository) History(ctx context.Context, agentID string, limit int) ([]HistoryRow, error) {
	limit = min(max(limit, 1), MaxEvalHistory)
	rows, err := r.s.db.QueryContext(ctx, r.s.bind("SELECT r.id,v.version,COALESCE(r.passed_cases,0),COALESCE(r.total_cases,0),r.ended_at FROM eval_runs r JOIN agent_versions v ON v.id=r.agent_version_id WHERE r.agent_id=? AND "+qualifyingRuns+" ORDER BY r.ended_at DESC, r.id DESC LIMIT ?"),
		agentID, boolValue(r.s.dialect, false), limit)
	if err != nil {
		return nil, fmt.Errorf("list eval history: %w", err)
	}
	defer rows.Close()
	out := make([]HistoryRow, 0, limit)
	byID := make(map[string]int, limit)
	args := make([]any, 0, limit)
	marks := make([]string, 0, limit)
	for rows.Next() {
		var h HistoryRow
		var ended string
		if err := rows.Scan(&h.RunID, &h.Version, &h.Passed, &h.Total, &ended); err != nil {
			return nil, fmt.Errorf("scan eval history: %w", err)
		}
		h.EndedAt, h.Scorers = scanTime(ended), []ScorerAgg{}
		byID[h.RunID] = len(out)
		out = append(out, h)
		args, marks = append(args, h.RunID), append(marks, "?")
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if len(args) == 0 {
		return out, nil
	}
	aggs, err := r.s.db.QueryContext(ctx, r.s.bind(`SELECT run_id,scorer,
		AVG(CASE WHEN outcome IN ('pass','fail') THEN score END),
		SUM(CASE WHEN outcome='pass' THEN 1 ELSE 0 END), SUM(CASE WHEN outcome='fail' THEN 1 ELSE 0 END),
		SUM(CASE WHEN outcome='skipped' THEN 1 ELSE 0 END), SUM(CASE WHEN outcome='error' THEN 1 ELSE 0 END)
		FROM eval_results WHERE run_id IN (`+strings.Join(marks, ",")+") GROUP BY run_id, scorer ORDER BY scorer"), args...)
	if err != nil {
		return nil, fmt.Errorf("aggregate eval history: %w", err)
	}
	defer aggs.Close()
	for aggs.Next() {
		var runID string
		var a ScorerAgg
		var mean sql.NullFloat64
		if err := aggs.Scan(&runID, &a.Scorer, &mean, &a.Pass, &a.Fail, &a.Skipped, &a.Errors); err != nil {
			return nil, fmt.Errorf("scan eval history scorer: %w", err)
		}
		a.Mean = mean.Float64
		h := &out[byID[runID]]
		h.Scorers = append(h.Scorers, a)
	}
	return out, aggs.Err()
}
