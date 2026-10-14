package flow

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// twoStepFlow is a front-desk entry (the standard valid config) and a sales
// step whose LLM uses salesProvider.
func twoStepFlow(salesProvider string) json.RawMessage {
	return json.RawMessage(`{"start":"front","steps":[` +
		`{"id":"front","name":"Front desk","config":` + string(validConfig()) + `},` +
		`{"id":"sales","name":"Sales","config":{"llm":{"provider":"` + salesProvider + `","model":"google.llm"},"tts":{"provider":"google","voice":"google.tts"}}}],` +
		`"routes":[{"id":"e1","from":"front","to":"sales","condition":"wants to buy"}]}`)
}

func TestCreateAcceptsFlowAndReportsStepProblems(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()
	a, err := svc.Create(ctx, CreateRequest{Name: "two node", Config: twoStepFlow("google")})
	if err != nil {
		t.Fatal(err)
	}
	if a.Readiness.State != ReadinessReady {
		t.Fatalf("want ready, got %+v", a.Readiness)
	}
	svc = testServiceWith(t, fakeAvail{"acme/llm": {Reason: "unknown_provider"}})
	_, err = svc.Create(ctx, CreateRequest{Name: "bad", Config: twoStepFlow("acme")})
	if !errors.Is(err, ErrNotAttached) || !strings.Contains(err.Error(), "sales:") {
		t.Fatalf("want ErrNotAttached naming the node, got %v", err)
	}
}

func TestStepToolsMustBeSelected(t *testing.T) {
	g, err := Decode(json.RawMessage(`{"start":"a","steps":[{"id":"a","name":"A","tools":["srv/lookup"],"config":` + string(validConfig()) + `}],"routes":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := checkStepTools(g, nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("a node tool that is not selected on the version must be rejected, got %v", err)
	}
	if err := checkStepTools(g, []ToolSelection{{ServerID: "srv", RemoteName: "lookup"}}); err != nil {
		t.Fatal(err)
	}
}

func TestCreateRejectsFlowStepToolNotSelected(t *testing.T) {
	svc := testService(t)
	raw := json.RawMessage(`{"start":"a","steps":[{"id":"a","name":"A","tools":["srv/lookup"],"config":` + string(validConfig()) + `}],"routes":[]}`)
	if _, err := svc.Create(context.Background(), CreateRequest{Name: "t", Config: raw}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("want ErrInvalidConfig, got %v", err)
	}
}

func TestFlatConfigStillReadyAndReadsAsOneStep(t *testing.T) {
	svc := testService(t)
	a, err := svc.Create(context.Background(), CreateRequest{Name: "flat", Config: validConfig()})
	if err != nil || a.Readiness.State != ReadinessReady {
		t.Fatalf("err=%v readiness=%+v", err, a.Readiness)
	}
	if g, err := Decode(a.Version.Config); err != nil || len(g.Steps) != 1 {
		t.Fatalf("a flat agent must read back as a one-node graph: %v", err)
	}
}

func TestUpdateFlowPublishesNewVersionKeepingOldPinned(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()
	a, err := svc.Create(ctx, CreateRequest{Name: "flat", Config: validConfig()})
	if err != nil {
		t.Fatal(err)
	}
	u, err := svc.Update(ctx, a.ID, UpdateRequest{Config: twoStepFlow("google")})
	if err != nil || u.Version.Number != 2 {
		t.Fatalf("err=%v version=%+v", err, u.Version)
	}
	old, err := svc.store.Flows().Version(ctx, a.Version.ID)
	if err != nil || strings.Contains(string(old.ConfigJSON), `"steps"`) {
		t.Fatalf("version 1 must stay flat: err=%v", err)
	}
}

// A config with the pre-rename keys is refused on write like any unknown field.
func TestCreateRefusesTheOldConfigKeys(t *testing.T) {
	svc := testService(t)
	old := strings.NewReplacer(`"start"`, `"entry"`, `"steps"`, `"nodes"`, `"routes"`, `"edges"`).Replace(string(twoStepFlow("google")))
	if _, err := svc.Create(context.Background(), CreateRequest{Name: "old keys", Config: json.RawMessage(old)}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("got %v", err)
	}
}
