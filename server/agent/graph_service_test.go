package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// twoNodeGraph is a front-desk entry (the standard valid config) and a sales
// node whose LLM uses salesCred (empty: the seeded google credential).
func twoNodeGraph(salesCred string) json.RawMessage {
	if salesCred == "" {
		salesCred = testCredentialID("google")
	}
	return json.RawMessage(`{"entry":"front","nodes":[` +
		`{"id":"front","name":"Front desk","config":` + string(validConfig()) + `},` +
		`{"id":"sales","name":"Sales","config":{"llm":{"provider":"google","model":"google.llm","credential_id":"` + salesCred + `"},"tts":{"provider":"google","voice":"google.tts","credential_id":"` + testCredentialID("google") + `"}}}],` +
		`"edges":[{"id":"e1","from":"front","to":"sales","condition":"wants to buy"}]}`)
}

func TestCreateAcceptsGraphAndReportsNodeProblems(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()
	a, err := svc.Create(ctx, CreateRequest{Name: "two node", Config: twoNodeGraph("")})
	if err != nil {
		t.Fatal(err)
	}
	if a.Readiness.State != ReadinessReady {
		t.Fatalf("want ready, got %+v", a.Readiness)
	}
	_, err = svc.Create(ctx, CreateRequest{Name: "bad", Config: twoNodeGraph("018f0000-0000-7000-8000-0000000000ff")})
	if !errors.Is(err, ErrNotAttached) || !strings.Contains(err.Error(), "sales:") {
		t.Fatalf("want ErrNotAttached naming the node, got %v", err)
	}
}

func TestNodeToolsMustBeSelected(t *testing.T) {
	g, err := DecodeGraph(json.RawMessage(`{"entry":"a","nodes":[{"id":"a","name":"A","tools":["srv/lookup"],"config":` + string(validConfig()) + `}],"edges":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := checkNodeTools(g, nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("a node tool that is not selected on the version must be rejected, got %v", err)
	}
	if err := checkNodeTools(g, []ToolSelection{{ServerID: "srv", RemoteName: "lookup"}}); err != nil {
		t.Fatal(err)
	}
}

func TestCreateRejectsGraphNodeToolNotSelected(t *testing.T) {
	svc := testService(t)
	raw := json.RawMessage(`{"entry":"a","nodes":[{"id":"a","name":"A","tools":["srv/lookup"],"config":` + string(validConfig()) + `}],"edges":[]}`)
	if _, err := svc.Create(context.Background(), CreateRequest{Name: "t", Config: raw}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("want ErrInvalidConfig, got %v", err)
	}
}

func TestFlatConfigStillReadyAndReadsAsOneNode(t *testing.T) {
	svc := testService(t)
	a, err := svc.Create(context.Background(), CreateRequest{Name: "flat", Config: validConfig()})
	if err != nil || a.Readiness.State != ReadinessReady {
		t.Fatalf("err=%v readiness=%+v", err, a.Readiness)
	}
	if g, err := DecodeGraph(a.Version.Config); err != nil || len(g.Nodes) != 1 {
		t.Fatalf("a flat agent must read back as a one-node graph: %v", err)
	}
}

func TestUpdateGraphPublishesNewVersionKeepingOldPinned(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()
	a, err := svc.Create(ctx, CreateRequest{Name: "flat", Config: validConfig()})
	if err != nil {
		t.Fatal(err)
	}
	u, err := svc.Update(ctx, a.ID, UpdateRequest{Config: twoNodeGraph("")})
	if err != nil || u.Version.Number != 2 {
		t.Fatalf("err=%v version=%+v", err, u.Version)
	}
	old, err := svc.store.Agents().Version(ctx, a.Version.ID)
	if err != nil || strings.Contains(string(old.ConfigJSON), `"nodes"`) {
		t.Fatalf("version 1 must stay flat: err=%v", err)
	}
}
