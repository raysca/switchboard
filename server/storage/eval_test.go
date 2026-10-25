package storage

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"testing"
	"time"
)

// evalAgent makes an agent with one published version and returns both ids.
func evalAgent(t *testing.T, s *Store) (agentID, versionID string) {
	t.Helper()
	ctx := context.Background()
	a := Agent{ID: NewID(), Name: "A", Enabled: true, CreatedAt: time.Now()}
	if err := s.Agents().Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	v, err := publishTestVersion(ctx, s, a.ID, []byte(`{}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	return a.ID, v.ID
}

func TestEvalCasesCRUD(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		agentID, _ := evalAgent(t, s)
		e := s.Eval()
		mk := func(name string) EvalCase {
			return EvalCase{Name: name, CallerJSON: []byte(`{"script":["hi"]}`), ExpectJSON: []byte(`{}`), ExpectedAnswer: "ok"}
		}
		got, err := e.CreateCases(ctx, agentID, []EvalCase{mk("b"), mk("a")})
		if err != nil || len(got) != 2 || !IsID(got[0].ID) || got[0].AgentID != agentID {
			t.Fatalf("create: %v %+v", err, got)
		}
		// A duplicate name anywhere in the batch writes nothing, including the new, valid name.
		if _, err := e.CreateCases(ctx, agentID, []EvalCase{mk("c"), mk("a")}); !errors.Is(err, ErrDuplicateCaseName) {
			t.Fatalf("duplicate: %v", err)
		}
		list, err := e.ListCases(ctx, agentID)
		if err != nil || len(list) != 2 || list[0].Name != "a" || list[1].Name != "b" {
			t.Fatalf("list (ordered by name, no partial write): %v %+v", err, list)
		}
		c := list[0]
		c.Name, c.ExpectedAnswer = "z", ""
		up, err := e.UpdateCase(ctx, c)
		if err != nil || up.Name != "z" || up.ExpectedAnswer != "" || !up.UpdatedAt.After(c.CreatedAt.Add(-time.Second)) {
			t.Fatalf("update: %v %+v", err, up)
		}
		// Renaming onto an existing name is the same duplicate error.
		c.Name = "b"
		if _, err := e.UpdateCase(ctx, c); !errors.Is(err, ErrDuplicateCaseName) {
			t.Fatalf("rename onto duplicate: %v", err)
		}
		if g, err := e.GetCase(ctx, agentID, c.ID); err != nil || g.Name != "z" || string(g.CallerJSON) != `{"script":["hi"]}` {
			t.Fatalf("get: %v %+v", err, g)
		}
		if _, err := e.GetCase(ctx, agentID, NewID()); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("get missing: %v", err)
		}
		if err := e.DeleteCase(ctx, agentID, c.ID); err != nil {
			t.Fatal(err)
		}
		if err := e.DeleteCase(ctx, agentID, c.ID); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("delete missing: %v", err)
		}
	})
}

func TestEvalCasesCap(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		agentID, _ := evalAgent(t, s)
		batch := make([]EvalCase, MaxEvalCases)
		for i := range batch {
			batch[i] = EvalCase{Name: time.Duration(i).String(), CallerJSON: []byte(`{}`), ExpectJSON: []byte(`{}`)}
		}
		if _, err := s.Eval().CreateCases(ctx, agentID, batch); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Eval().CreateCases(ctx, agentID, []EvalCase{{Name: "one more", CallerJSON: []byte(`{}`), ExpectJSON: []byte(`{}`)}}); !errors.Is(err, ErrTooManyCases) {
			t.Fatalf("over the cap: %v", err)
		}
	})
}

func TestEvalSuiteDefaultsAndUpsert(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		agentID, _ := evalAgent(t, s)
		e := s.Eval()
		d, err := e.Suite(ctx, agentID)
		if err != nil || d.AgentID != agentID || d.Runs != 1 || d.MinPass != 1 || d.PassThreshold != 1.0 || d.JudgeProvider != "" {
			t.Fatalf("defaults: %v %+v", err, d)
		}
		if err := e.PutSuite(ctx, EvalSuite{AgentID: agentID, Runs: 3, MinPass: 2, PassThreshold: 0.8, JudgeProvider: "offline", JudgeModel: "m"}); err != nil {
			t.Fatal(err)
		}
		if err := e.PutSuite(ctx, EvalSuite{AgentID: agentID, Runs: 3, MinPass: 3, PassThreshold: 0.9}); err != nil {
			t.Fatal(err)
		}
		g, err := e.Suite(ctx, agentID)
		if err != nil || g.Runs != 3 || g.MinPass != 3 || g.PassThreshold != 0.9 || g.JudgeProvider != "" {
			t.Fatalf("upsert replaces: %v %+v", err, g)
		}
		// The CHECKs refuse an impossible suite.
		if err := e.PutSuite(ctx, EvalSuite{AgentID: agentID, Runs: 2, MinPass: 3, PassThreshold: 1}); err == nil {
			t.Fatal("min_pass above runs accepted")
		}
		if err := e.PutSuite(ctx, EvalSuite{AgentID: agentID, Runs: 6, MinPass: 1, PassThreshold: 1}); err == nil {
			t.Fatal("runs above 5 accepted")
		}
	})
}

func TestEvalScorerSettings(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		agentID, _ := evalAgent(t, s)
		e := s.Eval()
		th := 0.5
		if err := e.PutScorerSetting(ctx, EvalScorerSetting{AgentID: agentID, Scorer: "correctness", Threshold: &th, JudgeProvider: "offline", JudgeModel: "m"}); err != nil {
			t.Fatal(err)
		}
		if err := e.PutScorerSetting(ctx, EvalScorerSetting{AgentID: agentID, Scorer: "toxicity"}); err != nil {
			t.Fatal(err)
		}
		if err := e.PutScorerSetting(ctx, EvalScorerSetting{AgentID: agentID, Scorer: "correctness", Threshold: nil}); err != nil {
			t.Fatal(err)
		}
		got, err := e.ScorerSettings(ctx, agentID)
		if err != nil || len(got) != 2 || got[0].Scorer != "correctness" || got[1].Scorer != "toxicity" {
			t.Fatalf("list: %v %+v", err, got)
		}
		if got[0].Threshold != nil || got[0].JudgeProvider != "" {
			t.Fatalf("upsert must replace, not merge: %+v", got[0])
		}
	})
}

// A run is either a draft (no version) or a published version's run (a version): never both, never neither.
func TestEvalRunDraftCheck(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		agentID, versionID := evalAgent(t, s)
		insert := func(draft bool, version string) error {
			_, err := s.DB().ExecContext(ctx, s.bind(`INSERT INTO eval_runs(id,agent_id,agent_version_id,config_hash,draft,source,scope,providers,runs,min_pass,status,triggered_by,created_at)
				VALUES(?,?,?,?,?,'suite','suite','offline',1,1,'pending','api',?)`), NewID(), agentID, nullable(version), "h", boolValue(s.dialect, draft), utc(time.Now()))
			return err
		}
		if insert(false, "") == nil {
			t.Fatal("non-draft run without a version accepted")
		}
		if insert(true, versionID) == nil {
			t.Fatal("draft run with a version accepted")
		}
		if err := insert(false, versionID); err != nil {
			t.Fatal(err)
		}
		if err := insert(true, ""); err != nil {
			t.Fatal(err)
		}
	})
}

// insertRun writes an eval_runs row directly with the given shape. ended is also the created stamp.
func insertRun(t *testing.T, s *Store, agentID, versionID string, scope string, draft bool, status string, passed, total int, ended time.Time) string {
	t.Helper()
	id := NewID()
	ver, tmpl := any(versionID), any(nil)
	if draft {
		ver, tmpl = nil, `{}`
	}
	_, err := s.DB().ExecContext(context.Background(), s.bind("INSERT INTO eval_runs(id,agent_id,agent_version_id,config_hash,draft,draft_config_json,draft_tools_json,source,scope,providers,runs,min_pass,status,triggered_by,passed_cases,total_cases,created_at,started_at,ended_at) VALUES(?,?,?,'h',?,?,?,'suite',?,'offline',1,1,?,'api',?,?,?,?,?)"),
		id, agentID, ver, boolValue(s.dialect, draft), tmpl, tmpl, scope, status, passed, total, utc(ended), utc(ended), utc(ended))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func fleetAgent(t *testing.T, s *Store, name string) (agentID, versionID string) {
	t.Helper()
	agentID, versionID = evalAgent(t, s)
	if _, err := s.DB().ExecContext(context.Background(), s.bind("UPDATE agents SET name=? WHERE id=?"), name, agentID); err != nil {
		t.Fatal(err)
	}
	return agentID, versionID
}

func rowFor(t *testing.T, p FleetPage, id string) FleetRow {
	t.Helper()
	for _, f := range p.Agents {
		if f.AgentID == id {
			return f
		}
	}
	t.Fatalf("agent %s not on the page %+v", id, p)
	return FleetRow{}
}

func TestFleetExcludesDraftAndCaseScopedRuns(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		resetEval(t, s)
		ctx := context.Background()
		a, v := fleetAgent(t, s, "fleet-excl")
		t0 := time.Now().Add(-time.Hour)
		suite := insertRun(t, s, a, v, "suite", false, "done", 4, 4, t0)
		insertRun(t, s, a, v, "suite", true, "done", 0, 4, t0.Add(time.Minute))   // draft
		insertRun(t, s, a, v, "case", false, "done", 0, 1, t0.Add(2*time.Minute)) // one case
		insertRun(t, s, a, v, "suite", false, "running", 0, 4, t0.Add(3*time.Minute))
		page, err := s.Eval().Fleet(ctx, 200, 0)
		if err != nil {
			t.Fatal(err)
		}
		f := rowFor(t, page, a)
		if f.Latest == nil || f.Latest.RunID != suite || f.Latest.Passed != 4 || f.Latest.Total != 4 || len(f.Trend) != 1 {
			t.Fatalf("latest must be the suite run: %+v", f)
		}
		// The newer draft and case runs fail everything: if either leaked into the count this agent would be below.
		if f.Below || page.BelowThreshold != 0 {
			t.Fatalf("below: %+v %d", f, page.BelowThreshold)
		}
	})
}

func TestFleetTrendAndThreshold(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		resetEval(t, s)
		ctx := context.Background()
		a, v := fleetAgent(t, s, "fleet-a")
		b, vb := fleetAgent(t, s, "fleet-b")
		never, _ := fleetAgent(t, s, "fleet-never")
		arch, va := fleetAgent(t, s, "fleet-archived")
		t0 := time.Now().Add(-48 * time.Hour)
		for i := 0; i < 12; i++ { // 12 runs, passed = i of 12 cases: only the last 10 count, oldest first
			insertRun(t, s, a, v, "suite", false, "done", i, 12, t0.Add(time.Duration(i)*time.Minute))
		}
		if err := s.Eval().PutSuite(ctx, EvalSuite{AgentID: a, Runs: 1, MinPass: 1, PassThreshold: 0.95}); err != nil {
			t.Fatal(err)
		}
		insertRun(t, s, b, vb, "suite", false, "done", 4, 5, t0) // 0.8 meets a 0.8 threshold exactly
		if err := s.Eval().PutSuite(ctx, EvalSuite{AgentID: b, Runs: 1, MinPass: 1, PassThreshold: 0.8}); err != nil {
			t.Fatal(err)
		}
		insertRun(t, s, arch, va, "suite", false, "done", 0, 5, t0) // would be below, but archived
		if err := s.Agents().Archive(ctx, arch); err != nil {
			t.Fatal(err)
		}
		page, err := s.Eval().Fleet(ctx, 200, 0)
		if err != nil {
			t.Fatal(err)
		}
		fa := rowFor(t, page, a)
		if len(fa.Trend) != 10 || fa.Trend[0] != 2.0/12 || fa.Trend[9] != 11.0/12 || fa.Latest.Passed != 11 || fa.Latest.Version != 1 || fa.Threshold != 0.95 || !fa.Below {
			t.Fatalf("a: %+v", fa)
		}
		if fb := rowFor(t, page, b); fb.Below || fb.Threshold != 0.8 {
			t.Fatalf("b meets its threshold: %+v", fb)
		}
		if fn := rowFor(t, page, never); fn.Latest != nil || fn.Below || len(fn.Trend) != 0 || fn.Threshold != 1.0 {
			t.Fatalf("no run is never below: %+v", fn)
		}
		for _, f := range page.Agents {
			if f.AgentID == arch {
				t.Fatal("archived agent listed")
			}
		}
		if page.BelowThreshold != 1 {
			t.Fatalf("only a is below (archived excluded): %d", page.BelowThreshold)
		}
	})
}

func TestFleetPagingCountsAllAgents(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		resetEval(t, s)
		ctx := context.Background()
		before, err := s.Eval().Fleet(ctx, 200, 0)
		if err != nil {
			t.Fatal(err)
		}
		t0 := time.Now().Add(-time.Hour)
		var ids []string
		for _, n := range []string{"page-1", "page-2", "page-3"} {
			a, v := fleetAgent(t, s, n)
			insertRun(t, s, a, v, "suite", false, "done", 0, 2, t0) // all three are below the default 1.0
			ids = append(ids, a)
		}
		all, _ := s.Eval().Fleet(ctx, 200, 0)
		if all.Total != before.Total+3 || all.BelowThreshold != before.BelowThreshold+3 {
			t.Fatalf("totals: before %+v after %d %d", before, all.Total, all.BelowThreshold)
		}
		idx := -1
		for i, f := range all.Agents {
			if f.AgentID == ids[0] {
				idx = i
			}
		}
		p, err := s.Eval().Fleet(ctx, 1, idx+1) // the agent after page-1 by name
		if err != nil || len(p.Agents) != 1 || p.Agents[0].AgentID != all.Agents[idx+1].AgentID {
			t.Fatalf("offset: %v %+v", err, p)
		}
		if p.Total != all.Total || p.BelowThreshold != all.BelowThreshold {
			t.Fatalf("counts cover every agent, not the page: %+v vs %+v", p, all)
		}
		if z, err := s.Eval().Fleet(ctx, 0, -5); err != nil || len(z.Agents) != 1 {
			t.Fatalf("limit and offset clamp: %v %d", err, len(z.Agents))
		}
	})
}

func TestHistoryAggregates(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		resetEval(t, s)
		ctx := context.Background()
		a, v := fleetAgent(t, s, "hist")
		t0 := time.Now().Add(-time.Hour)
		old := insertRun(t, s, a, v, "suite", false, "done", 1, 1, t0)
		run := insertRun(t, s, a, v, "suite", false, "done", 1, 2, t0.Add(time.Minute))
		insertRun(t, s, a, v, "suite", true, "done", 0, 1, t0.Add(2*time.Minute))
		insertRun(t, s, a, v, "case", false, "done", 0, 1, t0.Add(3*time.Minute))
		add := func(runID, scorer, outcome string, score float64) {
			id := NewID()
			if _, err := s.DB().ExecContext(ctx, s.bind("INSERT INTO eval_results(id,subject_kind,subject_id,scorer,scorer_version,version_key,agent_id,agent_version_id,config_hash,run_id,session_id,score,outcome,reason,created_at) VALUES(?,'case_run',?,?,1,?,?,?,'h',?,?,?,?,'r',?)"),
				id, id, scorer, v, a, v, runID, NewID(), score, outcome, utc(time.Now())); err != nil {
				t.Fatal(err)
			}
		}
		add(run, "expectations", "pass", 1)
		add(run, "expectations", "fail", 0.5)
		add(run, "expectations", "skipped", 0.9) // counted, not averaged
		add(run, "expectations", "error", 0.9)
		add(run, "judge", "skipped", 0)
		add(old, "expectations", "pass", 1)
		h, err := s.Eval().History(ctx, a, 50)
		if err != nil || len(h) != 2 || h[0].RunID != run || h[1].RunID != old {
			t.Fatalf("newest first, qualifying only: %v %+v", err, h)
		}
		if h[0].Version != 1 || h[0].Passed != 1 || h[0].Total != 2 || h[0].EndedAt.IsZero() || len(h[0].Scorers) != 2 {
			t.Fatalf("row: %+v", h[0])
		}
		if e := h[0].Scorers[0]; e.Scorer != "expectations" || e.Mean != 0.75 || e.Pass != 1 || e.Fail != 1 || e.Skipped != 1 || e.Errors != 1 {
			t.Fatalf("expectations: %+v", e)
		}
		if j := h[0].Scorers[1]; j.Scorer != "judge" || j.Mean != 0 || j.Skipped != 1 || j.Pass != 0 {
			t.Fatalf("judge: %+v", j)
		}
		if n, err := s.Eval().History(ctx, NewID(), 50); err != nil || len(n) != 0 {
			t.Fatalf("unknown agent: %v %v", err, n)
		}
	})
}

func TestHistoryLimitClamps(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		resetEval(t, s)
		ctx := context.Background()
		a, v := fleetAgent(t, s, "hist-clamp")
		t0 := time.Now().Add(-24 * time.Hour)
		for i := 0; i < MaxEvalHistory+1; i++ {
			insertRun(t, s, a, v, "suite", false, "done", 1, 1, t0.Add(time.Duration(i)*time.Second))
		}
		if h, err := s.Eval().History(ctx, a, 10_000); err != nil || len(h) != MaxEvalHistory {
			t.Fatalf("over the maximum: %v %d", err, len(h))
		}
		if h, err := s.Eval().History(ctx, a, 0); err != nil || len(h) != 1 {
			t.Fatalf("below one: %v %d", err, len(h))
		}
	})
}

// A suite run with no cases (total 0) neither errors the below-threshold count nor counts as below.
func TestFleetZeroTotalRunIsNotBelow(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		resetEval(t, s)
		ctx := context.Background()
		a, v := fleetAgent(t, s, "fleet-zero")
		insertRun(t, s, a, v, "suite", false, "done", 0, 0, time.Now().Add(-time.Hour))
		page, err := s.Eval().Fleet(ctx, 200, 0)
		if err != nil {
			t.Fatal(err)
		}
		if f := rowFor(t, page, a); f.Below || page.BelowThreshold != 0 {
			t.Fatalf("an empty run is not below: %+v %d", f, page.BelowThreshold)
		}
	})
}

// Runs that ended at the same instant order by id, newest id first, in the fleet's latest run and in the history.
func TestFleetAndHistoryBreakEndedAtTiesByID(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		resetEval(t, s)
		ctx := context.Background()
		a, v := fleetAgent(t, s, "fleet-tie")
		at := time.Now().Add(-time.Hour)
		// Inserted in an order that is neither ascending nor descending by id, so only the tiebreak yields 3, 2, 1.
		ids := []string{"00000000-0000-7000-8000-000000000002", "00000000-0000-7000-8000-000000000003", "00000000-0000-7000-8000-000000000001"}
		for i, id := range ids {
			run := insertRun(t, s, a, v, "suite", false, "done", i, 3, at)
			if _, err := s.DB().ExecContext(ctx, s.bind("UPDATE eval_runs SET id=? WHERE id=?"), id, run); err != nil {
				t.Fatal(err)
			}
		}
		sort.Sort(sort.Reverse(sort.StringSlice(ids)))
		page, err := s.Eval().Fleet(ctx, 200, 0)
		if err != nil {
			t.Fatal(err)
		}
		if f := rowFor(t, page, a); f.Latest == nil || f.Latest.RunID != ids[0] {
			t.Fatalf("latest must be the highest id %s: %+v", ids[0], f.Latest)
		}
		h, err := s.Eval().History(ctx, a, 50)
		if err != nil || len(h) != 3 {
			t.Fatalf("history: %v %+v", err, h)
		}
		for i, row := range h {
			if row.RunID != ids[i] {
				t.Fatalf("history[%d] = %s, want %s", i, row.RunID, ids[i])
			}
		}
	})
}
