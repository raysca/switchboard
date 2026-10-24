package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// MaxEvalCases bounds the saved cases of one agent.
const MaxEvalCases = 200

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
	if n, _ := res.RowsAffected(); n == 0 {
		return EvalCase{}, sql.ErrNoRows
	}
	return r.GetCase(ctx, c.AgentID, c.ID)
}

// DeleteCase returns sql.ErrNoRows when the case does not exist.
func (r EvalRepository) DeleteCase(ctx context.Context, agentID, id string) error {
	res, err := r.s.db.ExecContext(ctx, r.s.bind("DELETE FROM eval_cases WHERE id=? AND agent_id=?"), id, agentID)
	if err != nil {
		return fmt.Errorf("delete eval case: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
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
	defer rows.Close()
	out := make([]EvalCase, 0, 8)
	for rows.Next() {
		c, err := scanEvalCase(rows)
		if err != nil {
			return nil, fmt.Errorf("scan eval case: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Suite returns the agent's suite setting, or the defaults (1 run, 1 must pass, threshold 1.0, no judge) when none is saved.
func (r EvalRepository) Suite(ctx context.Context, agentID string) (EvalSuite, error) {
	su := EvalSuite{AgentID: agentID}
	var jp, jm sql.NullString
	var updated string
	err := r.s.db.QueryRowContext(ctx, r.s.bind("SELECT runs,min_pass,pass_threshold,judge_provider,judge_model,updated_at FROM eval_suites WHERE agent_id=?"), agentID).
		Scan(&su.Runs, &su.MinPass, &su.PassThreshold, &jp, &jm, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return EvalSuite{AgentID: agentID, Runs: 1, MinPass: 1, PassThreshold: 1.0}, nil
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
	defer rows.Close()
	out := make([]EvalScorerSetting, 0, 5)
	for rows.Next() {
		st := EvalScorerSetting{AgentID: agentID}
		var th sql.NullFloat64
		var jp, jm sql.NullString
		var updated string
		if err := rows.Scan(&st.Scorer, &th, &jp, &jm, &updated); err != nil {
			return nil, fmt.Errorf("scan eval scorer setting: %w", err)
		}
		if th.Valid {
			st.Threshold = &th.Float64
		}
		st.JudgeProvider, st.JudgeModel, st.UpdatedAt = jp.String, jm.String, scanTime(updated)
		out = append(out, st)
	}
	return out, rows.Err()
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
