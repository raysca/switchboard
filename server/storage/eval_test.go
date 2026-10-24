package storage

import (
	"context"
	"database/sql"
	"errors"
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
