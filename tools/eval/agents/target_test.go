package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"govoice/server/api"
	"govoice/server/eval"
	"govoice/tools/internal/stack"
)

func fixture[T any](t *testing.T, name string) (v T) {
	t.Helper()
	b, err := os.ReadFile("testdata/api/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return v
}

// The API's shapes convert to the storage rows the engine reads, field for field.
func TestToStored(t *testing.T) {
	sess := fixture[api.SessionDTO](t, "session.json")
	tr := fixture[api.SessionTracesDTO](t, "traces-ok.json")
	turns := []api.TurnDTO{{ID: "u1", Ordinal: 1, Transcript: "hi", AssistantText: "hello", Interrupted: true}}
	st := toStored(sess, turns, tr)
	s := st.Session
	if s.ID != "s1" || s.AgentVersionID != "v7" || s.ConfigHash != "abc123" || s.EndReason != "caller_hung_up" || s.TurnCount != 3 ||
		!s.Draft || s.Caller != "agent-eval" || s.EndedAt.IsZero() || s.StartedAt.IsZero() {
		t.Errorf("session = %+v", s)
	}
	if len(st.Turns) != 1 || st.Turns[0].Transcript != "hi" || st.Turns[0].AssistantText != "hello" || !st.Turns[0].Interrupted || st.Turns[0].Ordinal != 1 {
		t.Errorf("turns = %+v", st.Turns)
	}
	if len(st.Spans) != 3 || st.Spans[1].Name != "handoff.active" || st.Spans[1].AttributesJSON != `{"from_step":"front","to_step":"sales"}` {
		t.Errorf("spans = %+v", st.Spans)
	}
	if len(st.Tools) != 1 || st.Tools[0].RemoteName != "transfer_to_sales" || st.Tools[0].Status != "succeeded" || !st.Tools[0].EndedAt.IsZero() {
		t.Errorf("tools = %+v", st.Tools)
	}
	open := sess
	open.EndedAt = nil
	if !toStored(open, nil, tr).Session.EndedAt.IsZero() {
		t.Error("a live session has an end time")
	}
}

// The target's reads end with the call's context, and a failed read names its endpoint.
func TestHTTPTargetHonoursContext(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	tg := httpTarget{&stack.Server{Admin: srv.URL, Key: "k"}}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := tg.Stored(ctx, "s1")
	if err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("Stored: %v after %v", err, time.Since(start))
	}
	if want := "GET sessions/s1"; !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want it to name %q", err, want)
	}
	if _, err := tg.DraftToken(ctx, eval.Agent{ID: "a"}); err == nil {
		t.Error("DraftToken ignored the cancelled context")
	}
}
