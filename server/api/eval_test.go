package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"govoice/server/agent"
	"govoice/server/eval"
	"govoice/server/storage"
)

const (
	paidConfig = `{"stt":{"provider":"google","model":"s"},"llm":{"provider":"google","model":"m"},"tts":{"provider":"google","voice":"v"}}`
	freeConfig = `{"stt":{"provider":"echo","model":"s"},"llm":{"provider":"echo","model":"m"},"tts":{"provider":"echo","voice":"v"}}`
)

// evalRig is a real sqlite store, the real agent service and the real eval handler. live stands for GOVOICE_LIVE=1
// on the serving process.
type evalRig struct {
	t             *testing.T
	store         *storage.Store
	svc           *agent.Service
	h             http.Handler
	live          atomic.Bool
	remoteOffline atomic.Bool // providers.offline.llm_endpoint is not on this host
	down          atomic.Bool // every provider service is unavailable: no agent is ready
	paid, free    agent.Agent
}

type rigAvail struct{ r *evalRig }

func (a rigAvail) Check(provider, _ string) agent.Unavailable {
	if a.r.down.Load() && provider != "echo" && provider != "offline" && provider != agent.SilenceProvider { // the free providers need no key
		return agent.Unavailable{Reason: "no key", MissingEnv: []string{"KEY"}}
	}
	return agent.Unavailable{}
}

func newEvalRig(t *testing.T) *evalRig {
	t.Helper()
	store, err := storage.Open(context.Background(), storage.Config{Dialect: storage.SQLite, DSN: filepath.Join(t.TempDir(), "eval-api.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	r := &evalRig{t: t, store: store}
	r.svc = agent.NewService(store, rigAvail{r})
	r.h = NewEvalHandler(r.svc, store, EvalPolicy{
		LiveRequired:        func(c json.RawMessage) bool { return eval.NeedsLive(c, true) && !r.live.Load() },
		JudgeLiveRequired:   func(p string) bool { return p != "offline" && p != "echo" && !r.live.Load() },
		PersonaLiveRequired: func() bool { return r.remoteOffline.Load() && !r.live.Load() },
		JudgeProvider: func(p string) error {
			if p == "offline" || p == "echo" || p == "openai" {
				return nil
			}
			return errors.New("no such llm provider")
		},
	})
	r.paid = r.newAgent("Paid", paidConfig)
	r.free = r.newAgent("Free", freeConfig)
	return r
}

func (r *evalRig) newAgent(name, config string) agent.Agent {
	r.t.Helper()
	a, err := r.svc.Create(context.Background(), agent.CreateRequest{Name: name, Config: json.RawMessage(config)})
	if err != nil {
		r.t.Fatal(err)
	}
	return a
}

func (r *evalRig) do(method, path, body string, hdr ...string) *httptest.ResponseRecorder {
	r.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	r.h.ServeHTTP(w, req)
	return w
}

func (r *evalRig) path(a agent.Agent, rest string) string {
	return "/api/v1/agents/" + a.ID + "/eval" + rest
}

func jsonBody(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// expect fails unless w has the status and, for an error, the code.
func expect(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status %d, want %d: %s", w.Code, status, w.Body)
	}
	if code == "" {
		return
	}
	var e ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil || e.Error.Code != code {
		t.Fatalf("error code %q (%v), want %q: %s", e.Error.Code, err, code, w.Body)
	}
}

func decodeData[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var env struct {
		Data T `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode %s: %v", w.Body, err)
	}
	return env.Data
}

func scriptYAML(names ...string) string {
	var b strings.Builder
	for i, n := range names {
		if i > 0 {
			b.WriteString("---\n")
		}
		fmt.Fprintf(&b, "name: %s\ncaller:\n  script: [\"hello\", \"bye\"]\nexpect:\n  say: [\"hello\"]\n", n)
	}
	return b.String()
}

func (r *evalRig) addCases(a agent.Agent, names ...string) []EvalCaseDTO {
	r.t.Helper()
	w := r.do(http.MethodPost, r.path(a, "/cases"), jsonBody(map[string]string{"yaml": scriptYAML(names...)}))
	expect(r.t, w, http.StatusCreated, "")
	return decodeData[[]EvalCaseDTO](r.t, w)
}

func TestEvalCasesYAMLRoundTrip(t *testing.T) {
	r := newEvalRig(t)
	in := `name: buy
agent: Some other name
caller:
  persona: {who: Jane, goal: buy a car}
expect:
  path: [front, sales]
  tools_called: [find, "book:failed"]
  tools_ordered: true
  never_say: ["(?i)as an ai"]
  end_reason: caller_hung_up
  max_turns: 6
expected_answer: offers a test drive
---
` + scriptYAML("greet")
	w := r.do(http.MethodPost, r.path(r.free, "/cases"), jsonBody(map[string]string{"yaml": in}))
	expect(t, w, http.StatusCreated, "")
	created := decodeData[[]EvalCaseDTO](t, w)
	if len(created) != 2 || created[0].ID == "" || created[0].YAML == "" {
		t.Fatalf("created %+v", created)
	}

	w = r.do(http.MethodGet, r.path(r.free, "/cases/export"), "")
	expect(t, w, http.StatusOK, "")
	out := decodeData[struct {
		YAML string `json:"yaml"`
	}](t, w).YAML
	got, err := eval.ParseCases([]byte(out), "export")
	if err != nil {
		t.Fatalf("the CLI loader refuses the export: %v\n%s", err, out)
	}
	want, err := eval.ParseCases([]byte(in), "in")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("export holds %d cases:\n%s", len(got), out)
	}
	byName := map[string]eval.Case{}
	for _, c := range got {
		if c.Agent != r.free.Name {
			t.Errorf("case %q exported with agent %q, want %q", c.Name, c.Agent, r.free.Name)
		}
		c.Agent, c.File = "", ""
		byName[c.Name] = c
	}
	for _, c := range want {
		c.Agent, c.File = "", ""
		if !reflect.DeepEqual(byName[c.Name], c) {
			t.Errorf("case %q changed in the round trip:\n got %+v\nwant %+v", c.Name, byName[c.Name], c)
		}
	}
	// A case's own YAML loads too, and a stored case never keeps the agent it was imported with.
	one := decodeData[EvalCaseDTO](t, r.do(http.MethodGet, r.path(r.free, "/cases/"+created[0].ID), ""))
	cs, err := eval.ParseCases([]byte(one.YAML), "one")
	if err != nil || len(cs) != 1 || cs[0].Agent != "" {
		t.Fatalf("case yaml %q: %v %+v", one.YAML, err, cs)
	}
}

func TestEvalCaseInvalidYAMLNamesDocument(t *testing.T) {
	r := newEvalRig(t)
	cases := map[string]string{
		"cases.yaml#2": scriptYAML("ok") + "---\nname: bad\ncaller:\n  script: [\"x\"]\n  persona: {who: a, goal: b}\n",
		"expect.say":   "name: x\ncaller: {script: [\"hi\"]}\nexpect: {say: [\"(\"]}\n",
		"unknown":      "name: x\ncaller: {script: [\"hi\"]}\nbogus: 1\n",
		"empty":        "",
	}
	for want, y := range cases {
		w := r.do(http.MethodPost, r.path(r.free, "/cases"), jsonBody(map[string]string{"yaml": y}))
		expect(t, w, http.StatusBadRequest, "invalid_case")
		if !strings.Contains(w.Body.String(), want) && want != "unknown" && want != "empty" {
			t.Errorf("message does not name %q: %s", want, w.Body)
		}
	}
	if got := decodeData[[]EvalCaseDTO](t, r.do(http.MethodGet, r.path(r.free, "/cases"), "")); len(got) != 0 {
		t.Fatalf("a refused import saved %d cases", len(got))
	}
}

func TestEvalCaseLimits(t *testing.T) {
	r := newEvalRig(t)
	long := strings.Repeat("n", 201)
	w := r.do(http.MethodPost, r.path(r.free, "/cases"), jsonBody(map[string]string{"yaml": scriptYAML(long)}))
	expect(t, w, http.StatusBadRequest, "invalid_case")
	big := "name: big\ncaller: {persona: {who: \"" + strings.Repeat("w", 20000) + "\", goal: g}}\n"
	w = r.do(http.MethodPost, r.path(r.free, "/cases"), jsonBody(map[string]string{"yaml": big}))
	expect(t, w, http.StatusBadRequest, "invalid_case")
	// A whole request over the YAML limit is 413 and never parsed.
	w = r.do(http.MethodPost, r.path(r.free, "/cases"), jsonBody(map[string]string{"yaml": strings.Repeat("# c\n", 300<<10)}))
	expect(t, w, http.StatusRequestEntityTooLarge, "payload_too_large")
	// Unknown JSON fields and a wrong content type are refused.
	expect(t, r.do(http.MethodPost, r.path(r.free, "/cases"), `{"yaml":"x","extra":1}`), http.StatusBadRequest, "invalid_request")
	req := httptest.NewRequest(http.MethodPost, r.path(r.free, "/cases"), strings.NewReader(`{"yaml":"x"}`))
	w = httptest.NewRecorder()
	r.h.ServeHTTP(w, req)
	expect(t, w, http.StatusBadRequest, "invalid_request")
}

func TestEvalCaseNamesAreUnique(t *testing.T) {
	r := newEvalRig(t)
	r.addCases(r.free, "one")
	// Against a saved case, and within one batch: nothing is written.
	expect(t, r.do(http.MethodPost, r.path(r.free, "/cases"), jsonBody(map[string]string{"yaml": scriptYAML("two", "one")})), http.StatusConflict, "duplicate_case_name")
	expect(t, r.do(http.MethodPost, r.path(r.free, "/cases"), jsonBody(map[string]string{"yaml": scriptYAML("x", "x")})), http.StatusBadRequest, "invalid_case")
	if got := decodeData[[]EvalCaseDTO](t, r.do(http.MethodGet, r.path(r.free, "/cases"), "")); len(got) != 1 {
		t.Fatalf("%d cases saved, want 1", len(got))
	}
	// Another agent may reuse the name; renaming onto an existing name is a conflict.
	r.addCases(r.paid, "one")
	two := r.addCases(r.free, "two")[0]
	expect(t, r.do(http.MethodPut, r.path(r.free, "/cases/"+two.ID), jsonBody(map[string]string{"yaml": scriptYAML("one")})), http.StatusConflict, "duplicate_case_name")
}

func TestEvalCaseCap(t *testing.T) {
	r := newEvalRig(t)
	names := make([]string, storage.MaxEvalCases)
	for i := range names {
		names[i] = fmt.Sprintf("c%03d", i)
	}
	r.addCases(r.free, names...)
	expect(t, r.do(http.MethodPost, r.path(r.free, "/cases"), jsonBody(map[string]string{"yaml": scriptYAML("one-more")})), http.StatusConflict, "too_many_cases")
	over := append(names, "x")
	expect(t, r.do(http.MethodPost, r.path(r.paid, "/cases"), jsonBody(map[string]string{"yaml": scriptYAML(over...)})), http.StatusConflict, "too_many_cases")
}

func TestEvalCaseGetPutDelete(t *testing.T) {
	r := newEvalRig(t)
	c := r.addCases(r.free, "a")[0]
	w := r.do(http.MethodPut, r.path(r.free, "/cases/"+c.ID), jsonBody(map[string]string{"yaml": "name: renamed\ncaller: {script: [\"hi\"]}\nexpected_answer: fine\n"}))
	expect(t, w, http.StatusOK, "")
	got := decodeData[EvalCaseDTO](t, w)
	if got.ID != c.ID || got.Name != "renamed" || got.ExpectedAnswer != "fine" || !got.UpdatedAt.After(c.CreatedAt.Add(-time.Second)) {
		t.Fatalf("%+v", got)
	}
	expect(t, r.do(http.MethodPut, r.path(r.free, "/cases/"+c.ID), jsonBody(map[string]string{"yaml": scriptYAML("a", "b")})), http.StatusBadRequest, "invalid_case")
	expect(t, r.do(http.MethodDelete, r.path(r.free, "/cases/"+c.ID), ""), http.StatusNoContent, "")
	expect(t, r.do(http.MethodGet, r.path(r.free, "/cases/"+c.ID), ""), http.StatusNotFound, "not_found")
	expect(t, r.do(http.MethodDelete, r.path(r.free, "/cases/"+c.ID), ""), http.StatusNotFound, "not_found")
	expect(t, r.do(http.MethodGet, r.path(r.free, "/cases/nope"), ""), http.StatusNotFound, "not_found")
}

func TestEvalCrossAgentIsolation(t *testing.T) {
	r := newEvalRig(t)
	mine := r.addCases(r.free, "mine")[0]
	// The other agent cannot read, change, delete or run a case it does not own.
	expect(t, r.do(http.MethodGet, r.path(r.paid, "/cases/"+mine.ID), ""), http.StatusNotFound, "not_found")
	expect(t, r.do(http.MethodPut, r.path(r.paid, "/cases/"+mine.ID), jsonBody(map[string]string{"yaml": scriptYAML("hijack")})), http.StatusNotFound, "not_found")
	expect(t, r.do(http.MethodDelete, r.path(r.paid, "/cases/"+mine.ID), ""), http.StatusNotFound, "not_found")
	expect(t, r.do(http.MethodPost, r.path(r.paid, "/runs"), jsonBody(map[string]any{"case_id": mine.ID, "providers": "offline"})), http.StatusNotFound, "not_found")
	if got := decodeData[EvalCaseDTO](t, r.do(http.MethodGet, r.path(r.free, "/cases/"+mine.ID), "")); got.Name != "mine" {
		t.Fatalf("the owner's case changed: %+v", got)
	}
	// A run of one agent is invisible under another.
	r.addCases(r.paid, "p")
	w := r.do(http.MethodPost, r.path(r.paid, "/runs"), jsonBody(map[string]any{"providers": "offline"}))
	expect(t, w, http.StatusAccepted, "")
	run := decodeData[EvalRunDTO](t, w)
	expect(t, r.do(http.MethodGet, r.path(r.free, "/runs/"+run.ID), ""), http.StatusNotFound, "not_found")
	// An unknown agent is 404 on every endpoint.
	for _, p := range []string{"/cases", "/cases/export", "/suite", "/scorers", "/runs"} {
		expect(t, r.do(http.MethodGet, "/api/v1/agents/nope/eval"+p, ""), http.StatusNotFound, "not_found")
	}
}

func TestEvalSuiteSettings(t *testing.T) {
	r := newEvalRig(t)
	got := decodeData[EvalSuiteDTO](t, r.do(http.MethodGet, r.path(r.free, "/suite"), ""))
	if got.Runs != 1 || got.MinPass != 1 || got.PassThreshold != 1 || got.Judge != nil {
		t.Fatalf("defaults %+v", got)
	}
	w := r.do(http.MethodPut, r.path(r.free, "/suite"), `{"runs":3,"min_pass":2,"pass_threshold":0.8,"judge":{"provider":"offline","model":"m"}}`)
	expect(t, w, http.StatusOK, "")
	want := EvalSuiteDTO{Runs: 3, MinPass: 2, PassThreshold: 0.8, Judge: &EvalJudgeDTO{Provider: "offline", Model: "m"}}
	if got := decodeData[EvalSuiteDTO](t, r.do(http.MethodGet, r.path(r.free, "/suite"), "")); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
	// null clears the judge. Another agent is untouched.
	expect(t, r.do(http.MethodPut, r.path(r.free, "/suite"), `{"runs":1,"min_pass":1,"pass_threshold":1,"judge":null}`), http.StatusOK, "")
	if got := decodeData[EvalSuiteDTO](t, r.do(http.MethodGet, r.path(r.free, "/suite"), "")); got.Judge != nil {
		t.Fatalf("judge kept: %+v", got)
	}
	for name, body := range map[string]string{
		"runs 0":         `{"runs":0,"min_pass":1,"pass_threshold":1}`,
		"runs 6":         `{"runs":6,"min_pass":1,"pass_threshold":1}`,
		"min_pass>runs":  `{"runs":2,"min_pass":3,"pass_threshold":1}`,
		"threshold>1":    `{"runs":1,"min_pass":1,"pass_threshold":1.5}`,
		"threshold<0":    `{"runs":1,"min_pass":1,"pass_threshold":-0.1}`,
		"missing field":  `{"runs":1,"min_pass":1}`,
		"unknown field":  `{"runs":1,"min_pass":1,"pass_threshold":1,"x":1}`,
		"empty provider": `{"runs":1,"min_pass":1,"pass_threshold":1,"judge":{"provider":""}}`,
		"long model":     `{"runs":1,"min_pass":1,"pass_threshold":1,"judge":{"provider":"offline","model":"` + strings.Repeat("m", 129) + `"}}`,
		"unknown judge":  `{"runs":1,"min_pass":1,"pass_threshold":1,"judge":{"provider":"nowhere"}}`,
	} {
		w := r.do(http.MethodPut, r.path(r.free, "/suite"), body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d: %s", name, w.Code, w.Body)
		}
	}
}

func TestEvalPaidJudgeNeedsLive(t *testing.T) {
	r := newEvalRig(t)
	paidSuite := `{"runs":1,"min_pass":1,"pass_threshold":1,"judge":{"provider":"openai","model":"gpt"}}`
	w := r.do(http.MethodPut, r.path(r.free, "/suite"), paidSuite)
	expect(t, w, http.StatusForbidden, "eval_live_required")
	if !strings.Contains(w.Body.String(), "GOVOICE_LIVE=1") {
		t.Fatalf("message does not name GOVOICE_LIVE=1: %s", w.Body)
	}
	if got := decodeData[EvalSuiteDTO](t, r.do(http.MethodGet, r.path(r.free, "/suite"), "")); got.Judge != nil {
		t.Fatalf("a refused save was stored: %+v", got)
	}
	expect(t, r.do(http.MethodPut, r.path(r.free, "/scorers/correctness"), `{"judge":{"provider":"openai"}}`), http.StatusForbidden, "eval_live_required")
	// Free judges never need it.
	expect(t, r.do(http.MethodPut, r.path(r.free, "/scorers/correctness"), `{"judge":{"provider":"offline"}}`), http.StatusOK, "")
	expect(t, r.do(http.MethodPut, r.path(r.free, "/suite"), `{"runs":1,"min_pass":1,"pass_threshold":1,"judge":{"provider":"echo"}}`), http.StatusOK, "")
	// With the live switch both are accepted. An unknown provider is still a 400.
	r.live.Store(true)
	expect(t, r.do(http.MethodPut, r.path(r.free, "/suite"), paidSuite), http.StatusOK, "")
	expect(t, r.do(http.MethodPut, r.path(r.free, "/scorers/correctness"), `{"judge":{"provider":"openai"}}`), http.StatusOK, "")
	expect(t, r.do(http.MethodPut, r.path(r.free, "/suite"), `{"runs":1,"min_pass":1,"pass_threshold":1,"judge":{"provider":"nowhere"}}`), http.StatusBadRequest, "invalid_judge")
}

func TestEvalScorers(t *testing.T) {
	r := newEvalRig(t)
	list := decodeData[[]EvalScorerDTO](t, r.do(http.MethodGet, r.path(r.free, "/scorers"), ""))
	if len(list) != len(eval.Builtins()) {
		t.Fatalf("%d scorers, want %d", len(list), len(eval.Builtins()))
	}
	for i, s := range eval.Builtins() {
		if list[i].Name != s.Name() || list[i].Judged != s.Judged() || list[i].DefaultThreshold != s.DefaultThreshold() || list[i].Threshold != nil || list[i].Judge != nil {
			t.Fatalf("entry %d: %+v", i, list[i])
		}
	}
	w := r.do(http.MethodPut, r.path(r.free, "/scorers/correctness"), `{"threshold":0.4,"judge":{"provider":"offline","model":"m"}}`)
	expect(t, w, http.StatusOK, "")
	got := decodeData[EvalScorerDTO](t, w)
	if got.Name != "correctness" || got.Threshold == nil || *got.Threshold != 0.4 || got.Judge == nil || got.Judge.Model != "m" || !got.Judged {
		t.Fatalf("%+v", got)
	}
	var merged EvalScorerDTO
	for _, s := range decodeData[[]EvalScorerDTO](t, r.do(http.MethodGet, r.path(r.free, "/scorers"), "")) {
		if s.Name == "correctness" {
			merged = s
		}
	}
	if !reflect.DeepEqual(merged, got) {
		t.Fatalf("list %+v differs from put %+v", merged, got)
	}
	// A put replaces the setting: absent fields go back to the default.
	expect(t, r.do(http.MethodPut, r.path(r.free, "/scorers/correctness"), `{}`), http.StatusOK, "")
	for _, s := range decodeData[[]EvalScorerDTO](t, r.do(http.MethodGet, r.path(r.free, "/scorers"), "")) {
		if s.Threshold != nil || s.Judge != nil {
			t.Fatalf("setting kept: %+v", s)
		}
	}
	expect(t, r.do(http.MethodPut, r.path(r.free, "/scorers/nope"), `{}`), http.StatusNotFound, "not_found")
	expect(t, r.do(http.MethodPut, r.path(r.free, "/scorers/correctness"), `{"threshold":1.2}`), http.StatusBadRequest, "invalid_request")
	expect(t, r.do(http.MethodPut, r.path(r.free, "/scorers/correctness"), `{"threshold":0.5,"x":1}`), http.StatusBadRequest, "invalid_request")
	// A deterministic scorer has no judge to set.
	expect(t, r.do(http.MethodPut, r.path(r.free, "/scorers/expectations"), `{"judge":{"provider":"offline"}}`), http.StatusBadRequest, "invalid_request")
	expect(t, r.do(http.MethodPut, r.path(r.free, "/scorers/expectations"), `{"threshold":0.5}`), http.StatusOK, "")
}

func TestEvalRunFanOut(t *testing.T) {
	r := newEvalRig(t)
	cs := r.addCases(r.free, "alpha", "beta")
	expect(t, r.do(http.MethodPut, r.path(r.free, "/suite"), `{"runs":3,"min_pass":2,"pass_threshold":1}`), http.StatusOK, "")
	w := r.do(http.MethodPost, r.path(r.free, "/runs"), `{}`)
	expect(t, w, http.StatusAccepted, "")
	run := decodeData[EvalRunDTO](t, w)
	if run.Status != "pending" || run.Scope != "suite" || run.Providers != "offline" || run.Draft || run.Runs != 3 || run.MinPass != 2 || run.TriggeredBy != "api" ||
		run.Version == nil || *run.Version != 1 || run.CaseID != "" {
		t.Fatalf("%+v", run)
	}
	row, err := r.store.Eval().GetRun(context.Background(), r.free.ID, run.ID)
	if err != nil || row.AgentVersionID != r.free.Version.ID || row.ConfigHash != r.free.Version.ConfigHash || row.Draft || len(row.DraftConfigJSON) != 0 {
		t.Fatalf("run row %+v err %v", row, err)
	}
	jobs, err := r.store.Eval().RunJobs(context.Background(), run.ID)
	if err != nil || len(jobs) != 6 {
		t.Fatalf("%d jobs, err %v", len(jobs), err)
	}
	seen := map[string]bool{}
	for _, j := range jobs {
		var cj eval.CaseJob
		if err := json.Unmarshal(j.Payload, &cj); err != nil {
			t.Fatal(err)
		}
		if j.Kind != "run_case" || j.MaxAttempts != 3 || j.Status != "pending" || j.AgentVersionID != r.free.Version.ID {
			t.Errorf("job %+v", j)
		}
		if j.Subject != fmt.Sprintf("%s#%d", cj.CaseID, cj.Repeat) || j.IdempotencyKey != "run:"+run.ID+":"+j.Subject {
			t.Errorf("subject %q key %q payload %+v", j.Subject, j.IdempotencyKey, cj)
		}
		if cj.Case.Name == "" || cj.Case.Agent != "" || cj.CaseHash == "" || len(cj.CaseHash) != 64 || len(cj.Case.Caller.Script) != 2 {
			t.Errorf("payload %+v", cj)
		}
		seen[j.Subject] = true
	}
	for _, c := range cs {
		for rep := range 3 {
			if !seen[fmt.Sprintf("%s#%d", c.ID, rep)] {
				t.Errorf("no job for %s#%d", c.ID, rep)
			}
		}
	}
	// The snapshot: a later edit does not change a queued run.
	expect(t, r.do(http.MethodPut, r.path(r.free, "/cases/"+cs[0].ID), jsonBody(map[string]string{"yaml": "name: edited\ncaller: {script: [\"changed\"]}\n"})), http.StatusOK, "")
	again, _ := r.store.Eval().RunJobs(context.Background(), run.ID)
	for _, j := range again {
		var cj eval.CaseJob
		_ = json.Unmarshal(j.Payload, &cj)
		if cj.Case.Name == "edited" {
			t.Fatal("a queued job followed a later edit")
		}
	}
}

func TestEvalRunOneCase(t *testing.T) {
	r := newEvalRig(t)
	cs := r.addCases(r.free, "alpha", "beta")
	expect(t, r.do(http.MethodPut, r.path(r.free, "/suite"), `{"runs":2,"min_pass":1,"pass_threshold":1}`), http.StatusOK, "")
	w := r.do(http.MethodPost, r.path(r.free, "/runs"), jsonBody(map[string]any{"case_id": cs[1].ID, "providers": "echo"}))
	expect(t, w, http.StatusAccepted, "")
	run := decodeData[EvalRunDTO](t, w)
	if run.Scope != "case" || run.CaseID != cs[1].ID || run.Providers != "echo" {
		t.Fatalf("%+v", run)
	}
	jobs, _ := r.store.Eval().RunJobs(context.Background(), run.ID)
	if len(jobs) != 2 || !strings.HasPrefix(jobs[0].Subject, cs[1].ID+"#") {
		t.Fatalf("jobs %+v", jobs)
	}
	expect(t, r.do(http.MethodPost, r.path(r.free, "/runs"), `{"case_id":"nope"}`), http.StatusNotFound, "not_found")
}

func TestEvalRunRefusesVersionAndDraft(t *testing.T) {
	r := newEvalRig(t)
	r.addCases(r.free, "a")
	w := r.do(http.MethodPost, r.path(r.free, "/runs"), `{"version":1,"draft":{"config":`+freeConfig+`}}`)
	expect(t, w, http.StatusBadRequest, "invalid_request")
	for name, body := range map[string]string{
		"negative version": `{"version":-1}`,
		"bad providers":    `{"providers":"paid"}`,
		"unknown field":    `{"x":1}`,
		"empty draft":      `{"draft":{}}`,
		"bad draft config": `{"draft":{"config":{"llm":"x"}}}`,
	} {
		if w := r.do(http.MethodPost, r.path(r.free, "/runs"), body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d: %s", name, w.Code, w.Body)
		}
	}
	expect(t, r.do(http.MethodPost, r.path(r.free, "/runs"), `{"version":9}`), http.StatusNotFound, "not_found")
	if runs := decodeData[[]EvalRunDTO](t, r.do(http.MethodGet, r.path(r.free, "/runs?include_drafts=true"), "")); len(runs) != 0 {
		t.Fatalf("%d runs were created by refused requests", len(runs))
	}
}

func TestEvalRunNoCases(t *testing.T) {
	r := newEvalRig(t)
	expect(t, r.do(http.MethodPost, r.path(r.free, "/runs"), `{}`), http.StatusConflict, "no_cases")
	if runs := decodeData[[]EvalRunDTO](t, r.do(http.MethodGet, r.path(r.free, "/runs"), "")); len(runs) != 0 {
		t.Fatalf("an empty suite left %d runs", len(runs))
	}
}

func TestEvalRunSpecificVersion(t *testing.T) {
	r := newEvalRig(t)
	r.addCases(r.free, "a")
	v1 := r.free.Version
	if _, err := r.svc.Update(context.Background(), r.free.ID, agent.UpdateRequest{Config: json.RawMessage(strings.Replace(freeConfig, `"model":"m"`, `"model":"m2"`, 1))}); err != nil {
		t.Fatal(err)
	}
	w := r.do(http.MethodPost, r.path(r.free, "/runs"), `{"version":1}`)
	expect(t, w, http.StatusAccepted, "")
	run := decodeData[EvalRunDTO](t, w)
	row, _ := r.store.Eval().GetRun(context.Background(), r.free.ID, run.ID)
	if row.AgentVersionID != v1.ID || row.ConfigHash != v1.ConfigHash || run.Version == nil || *run.Version != 1 {
		t.Fatalf("row %+v dto %+v, want version 1 (%s)", row, run, v1.ID)
	}
	cur := decodeData[EvalRunDTO](t, r.do(http.MethodPost, r.path(r.free, "/runs"), `{"version":0}`))
	if cur.Version == nil || *cur.Version != 2 {
		t.Fatalf("version 0 should mean the current version: %+v", cur)
	}
}

func TestEvalRunDraftHasNoVersion(t *testing.T) {
	r := newEvalRig(t)
	r.addCases(r.free, "a")
	draft := strings.Replace(freeConfig, `"model":"m"`, `"model":"draft-model"`, 1)
	w := r.do(http.MethodPost, r.path(r.free, "/runs"), `{"draft":{"config":`+draft+`,"mcp_tools":[]},"providers":"echo"}`)
	expect(t, w, http.StatusAccepted, "")
	run := decodeData[EvalRunDTO](t, w)
	if !run.Draft || run.Version != nil || run.AgentVersionID != "" || run.ConfigHash == "" {
		t.Fatalf("%+v", run)
	}
	row, err := r.store.Eval().GetRun(context.Background(), r.free.ID, run.ID)
	if err != nil || !row.Draft || row.AgentVersionID != "" || !strings.Contains(string(row.DraftConfigJSON), "draft-model") || string(row.DraftToolsJSON) != "[]" {
		t.Fatalf("row %+v err %v", row, err)
	}
	_, canonical, _ := storage.ConfigHash([]byte(draft))
	wantHash, _, _ := storage.ConfigHash(canonical)
	if row.ConfigHash != wantHash || row.ConfigHash == r.free.Version.ConfigHash {
		t.Fatalf("config hash %q, want the draft's %q", row.ConfigHash, wantHash)
	}
	jobs, _ := r.store.Eval().RunJobs(context.Background(), run.ID)
	if len(jobs) != 1 || jobs[0].AgentVersionID != "" {
		t.Fatalf("jobs %+v", jobs)
	}
	// Drafts are listed only on request.
	if got := decodeData[[]EvalRunDTO](t, r.do(http.MethodGet, r.path(r.free, "/runs"), "")); len(got) != 0 {
		t.Fatalf("draft listed by default: %+v", got)
	}
	if got := decodeData[[]EvalRunDTO](t, r.do(http.MethodGet, r.path(r.free, "/runs?include_drafts=true"), "")); len(got) != 1 || got[0].ID != run.ID {
		t.Fatalf("draft not listed on request: %+v", got)
	}
	// Without mcp_tools the draft stores the base version's selection (here none): an empty column would read as no tools.
	w = r.do(http.MethodPost, r.path(r.free, "/runs"), `{"draft":{"config":`+freeConfig+`}}`)
	expect(t, w, http.StatusAccepted, "")
	row, _ = r.store.Eval().GetRun(context.Background(), r.free.ID, decodeData[EvalRunDTO](t, w).ID)
	if len(row.DraftToolsJSON) == 0 {
		t.Fatal("draft tools column is empty")
	}
	// A draft the service rejects is a 400, one for an unknown agent a 404.
	expect(t, r.do(http.MethodPost, r.path(r.free, "/runs"), `{"draft":{"config":{"start":"a","steps":[{"id":"a","name":"A","tools":["srv/x"],"config":`+freeConfig+`}],"handoffs":[]}}}`), http.StatusBadRequest, "")
}

func TestEvalRunLiveGate(t *testing.T) {
	r := newEvalRig(t)
	r.addCases(r.paid, "a")
	r.addCases(r.free, "a")
	count := func(a agent.Agent) int {
		runs, err := r.store.Eval().ListRuns(context.Background(), a.ID, true, 100, 0)
		if err != nil {
			t.Fatal(err)
		}
		return len(runs)
	}
	// keep on a paid agent without GOVOICE_LIVE: refused, nothing queued.
	w := r.do(http.MethodPost, r.path(r.paid, "/runs"), `{"providers":"keep"}`)
	expect(t, w, http.StatusForbidden, "eval_live_required")
	if !strings.Contains(w.Body.String(), "GOVOICE_LIVE=1") || count(r.paid) != 0 {
		t.Fatalf("message %s, %d runs", w.Body, count(r.paid))
	}
	// A paid draft is refused the same way.
	expect(t, r.do(http.MethodPost, r.path(r.paid, "/runs"), `{"providers":"keep","draft":{"config":`+paidConfig+`}}`), http.StatusForbidden, "eval_live_required")
	// Free modes never need it, whatever the agent's own models are; the default mode is offline.
	for _, body := range []string{`{}`, `{"providers":"offline"}`, `{"providers":"echo"}`} {
		expect(t, r.do(http.MethodPost, r.path(r.paid, "/runs"), body), http.StatusAccepted, "")
	}
	// A keyless agent in keep mode needs no live switch.
	expect(t, r.do(http.MethodPost, r.path(r.free, "/runs"), `{"providers":"keep"}`), http.StatusAccepted, "")
	// With the switch on the process, keep on a paid agent is accepted.
	r.live.Store(true)
	w = r.do(http.MethodPost, r.path(r.paid, "/runs"), `{"providers":"keep"}`)
	expect(t, w, http.StatusAccepted, "")
	if decodeData[EvalRunDTO](t, w).Providers != "keep" {
		t.Fatalf("providers: %s", w.Body)
	}
	if got := count(r.paid); got != 4 {
		t.Fatalf("%d paid runs, want 4", got)
	}
}

// A version run rewritten to offline or echo does not need the agent's real providers ready; keep does.
func TestEvalVersionRunReadinessByMode(t *testing.T) {
	r := newEvalRig(t)
	r.addCases(r.paid, "a")
	r.live.Store(true)
	r.down.Store(true)
	expect(t, r.do(http.MethodPost, r.path(r.paid, "/runs"), `{"providers":"keep"}`), http.StatusConflict, "agent_not_ready")
	for _, m := range []string{"offline", "echo"} {
		expect(t, r.do(http.MethodPost, r.path(r.paid, "/runs"), `{"providers":"`+m+`"}`), http.StatusAccepted, "")
	}
}

func TestEvalRunAgentNotReady(t *testing.T) {
	r := newEvalRig(t)
	r.addCases(r.free, "a")
	off := false
	if _, err := r.svc.Update(context.Background(), r.free.ID, agent.UpdateRequest{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	w := r.do(http.MethodPost, r.path(r.free, "/runs"), `{}`)
	expect(t, w, http.StatusConflict, "agent_not_ready")
	if !strings.Contains(w.Body.String(), `"state"`) {
		t.Fatalf("no readiness details: %s", w.Body)
	}
	expect(t, r.do(http.MethodPost, r.path(r.free, "/runs"), `{"draft":{"config":`+freeConfig+`}}`), http.StatusConflict, "agent_not_ready")
}

// A draft run follows the version rule: offline and echo need no ready providers, keep does.
func TestEvalDraftRunReadinessByMode(t *testing.T) {
	r := newEvalRig(t)
	r.addCases(r.paid, "a")
	r.live.Store(true)
	r.down.Store(true)
	draft := `"draft":{"config":` + paidConfig + `}`
	expect(t, r.do(http.MethodPost, r.path(r.paid, "/runs"), `{"providers":"keep",`+draft+`}`), http.StatusBadRequest, "agent_not_attached")
	for _, m := range []string{"offline", "echo"} {
		expect(t, r.do(http.MethodPost, r.path(r.paid, "/runs"), `{"providers":"`+m+`",`+draft+`}`), http.StatusAccepted, "")
	}
}

// A persona case reaches the offline provider's model, so it follows that provider's paid rule.
func TestEvalRunPersonaLiveGate(t *testing.T) {
	r := newEvalRig(t)
	persona := "name: p\ncaller:\n  persona: {who: a buyer, goal: buy}\n"
	expect(t, r.do(http.MethodPost, r.path(r.free, "/cases"), jsonBody(map[string]string{"yaml": persona})), http.StatusCreated, "")
	expect(t, r.do(http.MethodPost, r.path(r.free, "/runs"), `{"providers":"echo"}`), http.StatusAccepted, "")
	r.remoteOffline.Store(true)
	expect(t, r.do(http.MethodPost, r.path(r.free, "/runs"), `{"providers":"echo"}`), http.StatusForbidden, "eval_live_required")
	r.live.Store(true)
	expect(t, r.do(http.MethodPost, r.path(r.free, "/runs"), `{"providers":"echo"}`), http.StatusAccepted, "")
}

func TestEvalRunQueueFull(t *testing.T) {
	r := newEvalRig(t)
	names := make([]string, storage.MaxPendingJobsPerAgent)
	for i := range names {
		names[i] = fmt.Sprintf("c%03d", i)
	}
	r.addCases(r.free, names...)
	expect(t, r.do(http.MethodPost, r.path(r.free, "/runs"), `{}`), http.StatusAccepted, "")
	expect(t, r.do(http.MethodPost, r.path(r.free, "/runs"), `{}`), http.StatusTooManyRequests, "eval_queue_full")
	runs, _ := r.store.Eval().ListRuns(context.Background(), r.free.ID, true, 100, 0)
	if len(runs) != 1 {
		t.Fatalf("%d runs, want the first only", len(runs))
	}
	// 200 cases x 2 repeats can never fit: not a queue to wait for, a suite to shrink.
	expect(t, r.do(http.MethodPut, r.path(r.paid, "/suite"), `{"runs":2,"min_pass":1,"pass_threshold":1}`), http.StatusOK, "")
	r.addCases(r.paid, names...)
	w := r.do(http.MethodPost, r.path(r.paid, "/runs"), `{"providers":"echo"}`)
	expect(t, w, http.StatusConflict, "suite_too_large")
	if !strings.Contains(w.Body.String(), "exceeds 200") {
		t.Fatalf("message: %s", w.Body)
	}
}

func TestEvalRunTriggeredBy(t *testing.T) {
	r := newEvalRig(t)
	r.addCases(r.free, "a")
	api := decodeData[EvalRunDTO](t, r.do(http.MethodPost, r.path(r.free, "/runs"), `{}`))
	ui := decodeData[EvalRunDTO](t, r.do(http.MethodPost, r.path(r.free, "/runs"), `{}`, "X-Switchboard-UI", "1"))
	other := decodeData[EvalRunDTO](t, r.do(http.MethodPost, r.path(r.free, "/runs"), `{}`, "X-Switchboard-UI", "yes"))
	if api.TriggeredBy != "api" || ui.TriggeredBy != "console" || other.TriggeredBy != "api" {
		t.Fatalf("api=%q ui=%q other=%q", api.TriggeredBy, ui.TriggeredBy, other.TriggeredBy)
	}
}

// finishJob claims the next job and ends it: results make it done, none (and final) make it dead.
func (r *evalRig) finishJob(results []storage.EvalResult, category string) storage.EvalJob {
	r.t.Helper()
	ctx := context.Background()
	job, ok, _, err := r.store.Eval().Claim(ctx, "test", time.Now(), time.Minute)
	if err != nil || !ok {
		r.t.Fatalf("claim: %v %v", ok, err)
	}
	if results != nil {
		for i := range results {
			results[i].SubjectKind, results[i].SubjectID, results[i].AgentID = "case_run", job.Subject, job.AgentID
			results[i].VersionKey, results[i].AgentVersionID, results[i].ConfigHash = job.AgentVersionID, job.AgentVersionID, "h"
			r.ensureSession(job.AgentID, results[i].SessionID)
		}
		if _, err := r.store.Eval().Complete(ctx, job, time.Now(), results); err != nil {
			r.t.Fatal(err)
		}
		return job
	}
	if _, _, err := r.store.Eval().Fail(ctx, job, time.Now(), category, time.Minute, true); err != nil {
		r.t.Fatal(err)
	}
	return job
}

// call ids of the results the tests store
var testSess1, testS1, testS2, testS3 = storage.NewID(), storage.NewID(), storage.NewID(), storage.NewID()

// ensureSession stores an ended call with id, as the worker's case runs have: Complete drops the reason of a result
// whose call is gone.
func (r *evalRig) ensureSession(agentID, id string) {
	r.t.Helper()
	ctx := context.Background()
	if id == "" {
		return
	}
	var n int
	if err := r.store.DB().QueryRowContext(ctx, "SELECT count(*) FROM sessions WHERE id=?", id).Scan(&n); err != nil || n > 0 {
		return
	}
	var version, hash string
	if err := r.store.DB().QueryRowContext(ctx, "SELECT id, config_hash FROM agent_versions WHERE agent_id=? LIMIT 1", agentID).Scan(&version, &hash); err != nil {
		r.t.Fatalf("a version of %s: %v", agentID, err)
	}
	if err := r.store.Sessions().Create(ctx, storage.SessionRecord{ID: id, AgentID: agentID, AgentVersionID: version, ConfigHash: hash, Status: "ended", StartedAt: time.Now().UTC()}); err != nil {
		r.t.Fatal(err)
	}
}

func TestEvalRunListAndDetail(t *testing.T) {
	r := newEvalRig(t)
	cs := r.addCases(r.free, "alpha", "beta")
	run := decodeData[EvalRunDTO](t, r.do(http.MethodPost, r.path(r.free, "/runs"), `{}`))

	detail := decodeData[EvalRunDetailDTO](t, r.do(http.MethodGet, r.path(r.free, "/runs/"+run.ID), ""))
	if detail.Run.ID != run.ID || len(detail.CaseRuns) != 2 || detail.CaseRuns[0].Status != "pending" || detail.CaseRuns[0].Passed || len(detail.CaseRuns[0].Scores) != 0 {
		t.Fatalf("pending detail %+v", detail)
	}

	passed := r.finishJob([]storage.EvalResult{
		{Scorer: "expectations", Outcome: "pass", Score: 1, Reason: "ok", SessionID: testSess1},
		{Scorer: "correctness", Outcome: "skipped", Reason: "no judge configured", SessionID: testSess1},
	}, "")
	dead := r.finishJob(nil, "dial")
	detail = decodeData[EvalRunDetailDTO](t, r.do(http.MethodGet, r.path(r.free, "/runs/"+run.ID), ""))
	if detail.Run.Status != "done" || detail.Run.PassedCases == nil || *detail.Run.PassedCases != 1 || detail.Run.TotalCases == nil || *detail.Run.TotalCases != 2 || detail.Run.EndedAt == nil {
		t.Fatalf("run %+v", detail.Run)
	}
	byCase := map[string]EvalCaseRunDTO{}
	for _, c := range detail.CaseRuns {
		byCase[c.CaseID] = c
	}
	p, d := byCase[strings.SplitN(passed.Subject, "#", 2)[0]], byCase[strings.SplitN(dead.Subject, "#", 2)[0]]
	if p.CaseName == "" || p.Status != "done" || !p.Passed || p.SessionID != testSess1 || p.Repeat != 0 || len(p.Scores) != 2 || p.Scores[0].Scorer != "correctness" || p.Scores[0].Outcome != "skipped" {
		t.Fatalf("passed case-run %+v", p)
	}
	if d.Status != "dead" || d.Passed || d.ErrorCategory != "dial" || d.SessionID != "" || len(d.Scores) != 0 || d.CaseName == "" {
		t.Fatalf("dead case-run %+v", d)
	}

	// A failing score makes the case-run fail; reasons are bounded on the way out.
	long := strings.Repeat("r", 3000)
	run2 := decodeData[EvalRunDTO](t, r.do(http.MethodPost, r.path(r.free, "/runs"), `{"case_id":"`+cs[0].ID+`"}`))
	r.finishJob([]storage.EvalResult{{Scorer: "expectations", Outcome: "fail", Score: 0, Reason: long, SessionID: testS2, JudgeModel: "offline/m"}}, "")
	detail = decodeData[EvalRunDetailDTO](t, r.do(http.MethodGet, r.path(r.free, "/runs/"+run2.ID), ""))
	c := detail.CaseRuns[0]
	if c.Passed || len(c.Scores) != 1 || c.Scores[0].JudgeModel != "offline/m" || len([]rune(c.Scores[0].Reason)) > 1000 || !strings.HasSuffix(c.Scores[0].Reason, "...") {
		t.Fatalf("failed case-run %+v", c)
	}

	list := decodeData[[]EvalRunDTO](t, r.do(http.MethodGet, r.path(r.free, "/runs?limit=1"), ""))
	if len(list) != 1 || list[0].ID != run2.ID {
		t.Fatalf("newest first, limit 1: %+v", list)
	}
	list = decodeData[[]EvalRunDTO](t, r.do(http.MethodGet, r.path(r.free, "/runs?limit=1&offset=1"), ""))
	if len(list) != 1 || list[0].ID != run.ID || list[0].Version == nil || *list[0].Version != 1 {
		t.Fatalf("offset: %+v", list)
	}
	// A judge that errored is not a pass.
	run3 := decodeData[EvalRunDTO](t, r.do(http.MethodPost, r.path(r.free, "/runs"), `{"case_id":"`+cs[1].ID+`"}`))
	r.finishJob([]storage.EvalResult{{Scorer: "correctness", Outcome: "error", Reason: "judge down", SessionID: testS3}}, "")
	detail = decodeData[EvalRunDetailDTO](t, r.do(http.MethodGet, r.path(r.free, "/runs/"+run3.ID), ""))
	if detail.CaseRuns[0].Status != "done" || detail.CaseRuns[0].Passed {
		t.Fatalf("errored case-run %+v", detail.CaseRuns[0])
	}
	expect(t, r.do(http.MethodGet, r.path(r.free, "/runs/nope"), ""), http.StatusNotFound, "not_found")
}

func TestEvalUnknownQueryKeyRefused(t *testing.T) {
	r := newEvalRig(t)
	for _, p := range []string{"/runs?agent=x", "/cases?x=1", "/suite?x=1", "/scorers?x=1", "/cases/export?x=1", "/runs?include_drafts=maybe", "/runs?limit=0", "/runs?limit=101", "/runs?offset=-1"} {
		expect(t, r.do(http.MethodGet, r.path(r.free, p), ""), http.StatusBadRequest, "invalid_request")
	}
	expect(t, r.do(http.MethodPost, r.path(r.free, "/runs?x=1"), `{}`), http.StatusBadRequest, "invalid_request")
}

func TestEvalMethodsAndPaths(t *testing.T) {
	r := newEvalRig(t)
	c := r.addCases(r.free, "a")[0]
	for _, tc := range []struct{ method, path, allow string }{
		{http.MethodDelete, "/cases", "GET, POST"},
		{http.MethodPost, "/cases/export", "GET"},
		{http.MethodPost, "/cases/" + c.ID, "GET, PUT, DELETE"},
		{http.MethodPost, "/suite", "GET, PUT"},
		{http.MethodPut, "/scorers", "GET"},
		{http.MethodGet, "/scorers/correctness", "PUT"},
		{http.MethodPut, "/runs", "GET, POST"},
		{http.MethodPost, "/runs/x", "GET"},
	} {
		w := r.do(tc.method, r.path(r.free, tc.path), "")
		expect(t, w, http.StatusMethodNotAllowed, "method_not_allowed")
		if w.Header().Get("Allow") != tc.allow {
			t.Errorf("%s %s: Allow %q, want %q", tc.method, tc.path, w.Header().Get("Allow"), tc.allow)
		}
	}
	for _, p := range []string{"/nope", "/cases/a/b", "/runs/a/b", "/suite/x"} {
		expect(t, r.do(http.MethodGet, r.path(r.free, p), ""), http.StatusNotFound, "not_found")
	}
	expect(t, r.do(http.MethodGet, "/api/v1/agents/"+r.free.ID+"/evaluation", ""), http.StatusNotFound, "not_found")
	expect(t, r.do(http.MethodGet, "/api/v1/agents/"+r.free.ID, ""), http.StatusNotFound, "not_found")
}

// A draft without mcp_tools runs with the agent's attached tools, not with none.
func TestEvalDraftWithoutToolsKeepsAttachedTools(t *testing.T) {
	r := newEvalRig(t)
	ctx := context.Background()
	m := storage.MCPServer{ID: storage.NewID(), Name: "crm", Endpoint: "https://mcp.example/rpc", Transport: "streamable-http", Enabled: true, ValidationState: "healthy"}
	if err := r.store.MCP().CreateServer(ctx, m); err != nil {
		t.Fatal(err)
	}
	d, err := r.store.MCP().AddDiscovery(ctx, m.ID, []storage.MCPTool{{RemoteName: "echo", SchemaJSON: []byte(`{"type":"object"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	key := m.ID + "/echo"
	cfg := `{"start":"a","steps":[{"id":"a","name":"A","tools":["` + key + `"],"config":` + freeConfig + `}],"handoffs":[]}`
	tools := []agent.ToolSelection{{ServerID: m.ID, RemoteName: "echo", SchemaHash: d.Tools[0].SchemaHash}}
	a, err := r.svc.Create(ctx, agent.CreateRequest{Name: "Tooled", Config: json.RawMessage(cfg), MCPTools: tools})
	if err != nil {
		t.Fatal(err)
	}
	r.addCases(a, "c")
	// The draft references the attached tool and names no selection: accepted, and the run carries the attached one.
	w := r.do(http.MethodPost, r.path(a, "/runs"), `{"providers":"echo","draft":{"config":`+cfg+`}}`)
	expect(t, w, http.StatusAccepted, "")
	row, err := r.store.Eval().GetRun(ctx, a.ID, decodeData[EvalRunDTO](t, w).ID)
	if err != nil {
		t.Fatal(err)
	}
	var got []agent.ToolSelection
	if err := json.Unmarshal(row.DraftToolsJSON, &got); err != nil || len(got) != 1 || got[0].RemoteName != "echo" || got[0].ServerID != m.ID {
		t.Fatalf("draft tools %q (%v), want the attached echo", row.DraftToolsJSON, err)
	}
}

func TestEvalFleetEnvelope(t *testing.T) {
	r := newEvalRig(t)
	r.addCases(r.free, "alpha", "beta")
	expect(t, r.do(http.MethodPut, r.path(r.free, "/suite"), `{"runs":1,"min_pass":1,"pass_threshold":0.8}`), http.StatusOK, "")
	run := decodeData[EvalRunDTO](t, r.do(http.MethodPost, r.path(r.free, "/runs"), `{}`))
	r.finishJob([]storage.EvalResult{{Scorer: "expectations", Outcome: "pass", Score: 1, Reason: "ok", SessionID: testS1}}, "")
	r.finishJob([]storage.EvalResult{{Scorer: "expectations", Outcome: "fail", Score: 0, Reason: "no", SessionID: testS2}}, "")

	fleet := decodeData[EvalFleetDTO](t, r.do(http.MethodGet, "/api/v1/eval/fleet", ""))
	if fleet.Total != 2 || fleet.BelowThreshold != 1 || len(fleet.Agents) != 2 {
		t.Fatalf("fleet %+v", fleet)
	}
	var free, paid EvalFleetAgentDTO
	for _, a := range fleet.Agents {
		if a.AgentID == r.free.ID {
			free = a
		} else {
			paid = a
		}
	}
	if free.Name != "Free" || free.Threshold != 0.8 || !free.BelowThreshold || len(free.Trend) != 1 || free.Trend[0] != 0.5 ||
		free.Latest == nil || free.Latest.RunID != run.ID || free.Latest.Version != 1 || free.Latest.Passed != 1 || free.Latest.Total != 2 ||
		free.Latest.PassRate == nil || *free.Latest.PassRate != 0.5 {
		t.Fatalf("free %+v", free)
	}
	if _, err := time.Parse(time.RFC3339, free.Latest.EndedAt); err != nil {
		t.Fatalf("ended_at: %v", err)
	}
	if paid.Latest != nil || paid.BelowThreshold || paid.Threshold != 1 || paid.Trend == nil {
		t.Fatalf("an agent that never ran a suite is never below, with an empty (not null) trend: %+v", paid)
	}
	raw := r.do(http.MethodGet, "/api/v1/eval/fleet", "").Body.String()
	if !strings.Contains(raw, `"trend":[]`) || strings.Contains(raw, `"latest":null`) {
		t.Fatalf("wire shape: %s", raw)
	}

	page := decodeData[EvalFleetDTO](t, r.do(http.MethodGet, "/api/v1/eval/fleet?limit=1&offset=1", ""))
	if len(page.Agents) != 1 || page.Total != 2 || page.BelowThreshold != 1 || page.Agents[0].AgentID != fleet.Agents[1].AgentID {
		t.Fatalf("page %+v", page)
	}
	for _, p := range []string{"?x=1", "?limit=0", "?limit=201", "?limit=a", "?offset=-1"} {
		expect(t, r.do(http.MethodGet, "/api/v1/eval/fleet"+p, ""), http.StatusBadRequest, "invalid_request")
	}
	expect(t, r.do(http.MethodGet, "/api/v1/eval/fleet?limit=200", ""), http.StatusOK, "")
	expect(t, r.do(http.MethodPost, "/api/v1/eval/fleet", ""), http.StatusMethodNotAllowed, "method_not_allowed")
	expect(t, r.do(http.MethodGet, "/api/v1/eval/nope", ""), http.StatusNotFound, "not_found")
}

func TestEvalHistoryEnvelope(t *testing.T) {
	r := newEvalRig(t)
	r.addCases(r.free, "alpha")
	run := decodeData[EvalRunDTO](t, r.do(http.MethodPost, r.path(r.free, "/runs"), `{}`))
	r.finishJob([]storage.EvalResult{{Scorer: "expectations", Outcome: "pass", Score: 1, Reason: "ok", SessionID: testS1}}, "")
	h := decodeData[[]EvalHistoryDTO](t, r.do(http.MethodGet, r.path(r.free, "/history"), ""))
	if len(h) != 1 || h[0].RunID != run.ID || h[0].Version != 1 || h[0].Passed != 1 || h[0].Total != 1 || h[0].PassRate == nil || *h[0].PassRate != 1 ||
		len(h[0].Scorers) != 1 || h[0].Scorers[0].Scorer != "expectations" || h[0].Scorers[0].Mean != 1 || h[0].Scorers[0].Pass != 1 {
		t.Fatalf("history %+v", h)
	}
	if _, err := time.Parse(time.RFC3339, h[0].EndedAt); err != nil {
		t.Fatalf("ended_at: %v", err)
	}
	if e := decodeData[[]EvalHistoryDTO](t, r.do(http.MethodGet, r.path(r.paid, "/history"), "")); e == nil || len(e) != 0 {
		t.Fatalf("an agent with no run has an empty list: %#v", e)
	}
	for _, p := range []string{"?x=1", "?limit=0", "?limit=101", "?limit=a", "?offset=1"} {
		expect(t, r.do(http.MethodGet, r.path(r.free, "/history"+p), ""), http.StatusBadRequest, "invalid_request")
	}
	expect(t, r.do(http.MethodGet, r.path(r.free, "/history?limit=100"), ""), http.StatusOK, "")
	expect(t, r.do(http.MethodPost, r.path(r.free, "/history"), ""), http.StatusMethodNotAllowed, "method_not_allowed")
	expect(t, r.do(http.MethodGet, "/api/v1/agents/"+storage.NewID()+"/eval/history", ""), http.StatusNotFound, "not_found")
}
