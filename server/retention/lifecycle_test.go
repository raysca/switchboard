package retention_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"govoice/server/api"
	"govoice/server/retention"
	"govoice/server/storage"
)

// fakeRecordings stands for the recording sink: files it "has" by call id, and ids whose removal fails.
type fakeRecordings struct {
	mu      sync.Mutex
	files   map[string]int
	failing map[string]bool
	asked   []string
}

func (f *fakeRecordings) Remove(id string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, id)
	if f.failing[id] {
		return 0, errors.New("/var/recordings/" + id + ".wav: permission denied")
	}
	n := f.files[id]
	delete(f.files, id)
	return n, nil
}

type fixture struct {
	t       *testing.T
	store   *storage.Store
	handler http.Handler
	status  http.Handler
	rec     *fakeRecordings
	logs    *bytes.Buffer
	erased  map[string]int
	agent   string
	version string
	hash    string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, store: newTestStore(t), rec: &fakeRecordings{files: map[string]int{}, failing: map[string]bool{}}, logs: &bytes.Buffer{}, erased: map[string]int{}}
	f.agent, f.version, f.hash = createTestAgentAndVersion(t, f.store)
	life := retention.NewLifecycle(f.store.Retention(), f.rec, slog.New(slog.NewTextHandler(f.logs, nil)), func(kind string, n int) { f.erased[kind] += n })
	f.handler = api.NewTracesHandler(f.store.Sessions(), f.store.Traces(), life)
	f.status = retention.Handler(retention.Settings{Enabled: true, Interval: time.Hour, Policy: retention.Policy{ContentRetention: 7 * day, MetadataRetention: 30 * day, BatchSize: 500}}, nil, f.store.Retention())
	return f
}

// call makes a stored call with data and a recording of caller, ended or not.
func (f *fixture) call(caller, status string) string {
	f.t.Helper()
	now := time.Now().UTC()
	ended := now.Add(-time.Minute)
	if status != "ended" {
		ended = time.Time{}
	}
	d := createSessionWithData(f.t, f.store, f.agent, f.version, f.hash, status, now.Add(-time.Hour), ended)
	if _, err := f.store.DB().ExecContext(context.Background(), "UPDATE sessions SET caller=? WHERE id=?", caller, d.sessionID); err != nil {
		f.t.Fatal(err)
	}
	f.rec.files[d.sessionID] = 2
	return d.sessionID
}

type response struct {
	Code int
	Body string
	ID   string
}

func (f *fixture) do(h http.Handler, method, path, body string) response {
	f.t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Request-ID", "client-chosen") // must be ignored
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return response{w.Code, w.Body.String(), w.Header().Get("X-Request-ID")}
}

func (r response) data(t *testing.T, dst any) {
	t.Helper()
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(r.Body), &env); err != nil {
		t.Fatalf("%v: %s", err, r.Body)
	}
	if err := json.Unmarshal(env.Data, dst); err != nil {
		t.Fatalf("%v: %s", err, r.Body)
	}
}

func (r response) errCode(t *testing.T) string {
	t.Helper()
	var env struct {
		Error struct{ Code string } `json:"error"`
	}
	_ = json.Unmarshal([]byte(r.Body), &env)
	return env.Error.Code
}

func TestHoldAndReleaseAreIdempotentAndShownOnTheCall(t *testing.T) {
	f := newFixture(t)
	id := f.call("alice", "ended")
	for i, reason := range []string{"legal case 42", "legal case 43"} {
		body := `{"reason":"` + reason + `"}`
		r := f.do(f.handler, http.MethodPut, "/api/v1/sessions/"+id+"/hold", body)
		var dto api.SessionDTO
		r.data(t, &dto)
		if r.Code != 200 || !dto.Held || dto.HoldReason != reason {
			t.Fatalf("hold %d: %d %s", i, r.Code, r.Body)
		}
	}
	if r := f.do(f.handler, http.MethodPut, "/api/v1/sessions/"+id+"/hold", ""); r.Code != 200 || !strings.Contains(r.Body, `"hold_reason":""`) {
		t.Fatalf("hold without a body: %d %s", r.Code, r.Body)
	}
	list := f.do(f.handler, http.MethodGet, "/api/v1/sessions?held=true", "")
	if !strings.Contains(list.Body, id) || strings.Contains(f.do(f.handler, http.MethodGet, "/api/v1/sessions?held=false", "").Body, id) {
		t.Fatalf("held filter: %s", list.Body)
	}
	for i := 0; i < 2; i++ {
		var dto api.SessionDTO
		r := f.do(f.handler, http.MethodDelete, "/api/v1/sessions/"+id+"/hold", "")
		r.data(t, &dto)
		if r.Code != 200 || dto.Held || dto.HoldReason != "" {
			t.Fatalf("release %d: %d %s", i, r.Code, r.Body)
		}
	}
	st := f.do(f.status, http.MethodGet, "/api/v1/retention", "")
	var status retention.Status
	st.data(t, &status)
	if status.HeldSessions != 0 || len(status.RecentActions) != 2 || status.RecentActions[0].Action != "release" || status.RecentActions[1].Action != "hold" {
		t.Fatalf("status = %+v", status)
	}
	// the audited request id is the server's, not the client's header
	if status.RecentActions[0].RequestID == "client-chosen" || status.RecentActions[0].RequestID == "" {
		t.Fatalf("request id = %q", status.RecentActions[0].RequestID)
	}
}

func TestHoldAndEraseRefuseBadRequests(t *testing.T) {
	f := newFixture(t)
	id := f.call("alice", "ended")
	unknown := storage.NewID()
	for name, tc := range map[string]struct {
		method, path, body string
		code               int
		err                string
	}{
		"hold unknown":                 {http.MethodPut, "/api/v1/sessions/" + unknown + "/hold", "", 404, "not_found"},
		"hold not an id":               {http.MethodPut, "/api/v1/sessions/nope/hold", "", 404, "not_found"},
		"release unknown":              {http.MethodDelete, "/api/v1/sessions/" + unknown + "/hold", "", 404, "not_found"},
		"long reason":                  {http.MethodPut, "/api/v1/sessions/" + id + "/hold", `{"reason":"` + strings.Repeat("x", 201) + `"}`, 400, "invalid_request"},
		"unknown body field":           {http.MethodPut, "/api/v1/sessions/" + id + "/hold", `{"why":"x"}`, 400, "invalid_request"},
		"hold wrong method":            {http.MethodPost, "/api/v1/sessions/" + id + "/hold", "", 405, "method_not_allowed"},
		"erase unknown":                {http.MethodDelete, "/api/v1/sessions/" + unknown, "", 404, "not_found"},
		"erase not an id":              {http.MethodDelete, "/api/v1/sessions/nope", "", 404, "not_found"},
		"GET erase is not an endpoint": {http.MethodGet, "/api/v1/sessions/erase", "", 404, "not_found"},
		"held filter value":            {http.MethodGet, "/api/v1/sessions?held=maybe", "", 400, "invalid_request"},
		"unknown filter":               {http.MethodGet, "/api/v1/sessions?holds=true", "", 400, "invalid_request"},
	} {
		r := f.do(f.handler, tc.method, tc.path, tc.body)
		if r.Code != tc.code || r.errCode(t) != tc.err {
			t.Errorf("%s: %d %s", name, r.Code, r.Body)
		}
	}
	// nothing above changed the call or audited anything
	var status retention.Status
	f.do(f.status, http.MethodGet, "/api/v1/retention", "").data(t, &status)
	if len(status.RecentActions) != 0 || f.rec.files[id] != 2 {
		t.Fatalf("a refused request changed something: %+v, files %d", status, f.rec.files[id])
	}
}

func TestEraseCallRemovesRowsAndRecordingAndRefusesHeldAndActiveCalls(t *testing.T) {
	f := newFixture(t)
	held, active, plain := f.call("a", "ended"), f.call("a", "active"), f.call("a", "ended")
	if r := f.do(f.handler, http.MethodPut, "/api/v1/sessions/"+held+"/hold", `{"reason":"keep"}`); r.Code != 200 {
		t.Fatal(r.Body)
	}
	if r := f.do(f.handler, http.MethodDelete, "/api/v1/sessions/"+held, ""); r.Code != 409 || r.errCode(t) != "session_held" {
		t.Fatalf("held: %d %s", r.Code, r.Body)
	}
	if r := f.do(f.handler, http.MethodDelete, "/api/v1/sessions/"+active, ""); r.Code != 409 || r.errCode(t) != "session_active" {
		t.Fatalf("active: %d %s", r.Code, r.Body)
	}
	if f.rec.files[held] != 2 || f.rec.files[active] != 2 {
		t.Fatal("a refused erasure removed a recording")
	}
	r := f.do(f.handler, http.MethodDelete, "/api/v1/sessions/"+plain, "")
	var out api.Erasure
	r.data(t, &out)
	if r.Code != 200 || out.Turns != 1 || out.ToolCalls != 1 || out.RecordingsRemoved != 2 {
		t.Fatalf("erase: %d %s", r.Code, r.Body)
	}
	if r := f.do(f.handler, http.MethodGet, "/api/v1/sessions/"+plain, ""); r.Code != 404 {
		t.Fatalf("erased call still readable: %d", r.Code)
	}
	if f.erased["call"] != 1 {
		t.Fatalf("metric hook = %v", f.erased)
	}
	// erased again: no rows and no files left, so the call is simply unknown
	if r := f.do(f.handler, http.MethodDelete, "/api/v1/sessions/"+plain, ""); r.Code != 404 {
		t.Fatalf("second erase: %d %s", r.Code, r.Body)
	}
	var status retention.Status
	f.do(f.status, http.MethodGet, "/api/v1/retention", "").data(t, &status)
	if status.HeldSessions != 1 || status.RecentActions[0].Action != "erase_call" || status.RecentActions[0].Subject != plain {
		t.Fatalf("status = %+v", status)
	}
}

// A file that cannot be removed leaves the rows erased, says so, and the same request finishes the job later.
func TestErasingAgainRetriesARecordingThatCouldNotBeRemoved(t *testing.T) {
	f := newFixture(t)
	id := f.call("a", "ended")
	f.rec.failing[id] = true
	r := f.do(f.handler, http.MethodDelete, "/api/v1/sessions/"+id, "")
	if r.Code != 500 || r.errCode(t) != "recordings_not_removed" || strings.Contains(r.Body, "/var/recordings") || strings.Contains(f.logs.String(), "/var/recordings") {
		t.Fatalf("failed removal: %d %s\nlogs: %s", r.Code, r.Body, f.logs)
	}
	if r := f.do(f.handler, http.MethodGet, "/api/v1/sessions/"+id, ""); r.Code != 404 {
		t.Fatal("the rows were not erased")
	}
	// still failing: the retry fails the same way and is audited
	if r := f.do(f.handler, http.MethodDelete, "/api/v1/sessions/"+id, ""); r.Code != 500 {
		t.Fatalf("retry while failing: %d %s", r.Code, r.Body)
	}
	delete(f.rec.failing, id)
	r = f.do(f.handler, http.MethodDelete, "/api/v1/sessions/"+id, "")
	var out api.Erasure
	r.data(t, &out)
	if r.Code != 200 || out.RecordingsRemoved != 2 {
		t.Fatalf("retry: %d %s", r.Code, r.Body)
	}
	if r := f.do(f.handler, http.MethodDelete, "/api/v1/sessions/"+id, ""); r.Code != 404 {
		t.Fatalf("after the files are gone: %d", r.Code)
	}
	var status retention.Status
	f.do(f.status, http.MethodGet, "/api/v1/retention", "").data(t, &status)
	var failed, removed int
	for _, a := range status.RecentActions {
		if strings.Contains(string(a.Detail), "recordings_failed") {
			failed++
		}
		if strings.Contains(string(a.Detail), `"recordings_removed":2`) {
			removed++
		}
	}
	if failed != 2 || removed != 1 {
		t.Fatalf("audit = %+v", status.RecentActions)
	}
}

// An id that is not a stored call never names a file: an older recording is not reachable through the API.
func TestEraseOnlyLooksForFilesOfStoredCallIds(t *testing.T) {
	f := newFixture(t)
	f.rec.files["agent-1-1700000000"] = 2
	f.do(f.handler, http.MethodDelete, "/api/v1/sessions/agent-1-1700000000", "")
	f.do(f.handler, http.MethodDelete, "/api/v1/sessions/erase", "")
	if len(f.rec.asked) != 0 || f.rec.files["agent-1-1700000000"] != 2 {
		t.Fatalf("recording asked for: %v", f.rec.asked)
	}
}

func TestEraseByCallerValidatesConfirmsCountsAndRepeats(t *testing.T) {
	f := newFixture(t)
	const label = "customer-77@example.com"
	var ids []string
	for i := 0; i < 3; i++ {
		ids = append(ids, f.call(label, "ended"))
	}
	held := f.call(label, "ended")
	f.do(f.handler, http.MethodPut, "/api/v1/sessions/"+held+"/hold", "")
	f.call(label, "active")
	other := f.call("someone else", "ended")

	post := func(body string) response { return f.do(f.handler, http.MethodPost, "/api/v1/sessions/erase", body) }
	for name, body := range map[string]string{
		"missing":          `{}`,
		"blank":            `{"caller":"  ","confirm_caller":"  "}`,
		"not confirmed":    `{"caller":"` + label + `"}`,
		"mismatch":         `{"caller":"` + label + `","confirm_caller":"customer-77"}`,
		"too long":         `{"caller":"` + strings.Repeat("x", 129) + `","confirm_caller":"` + strings.Repeat("x", 129) + `"}`,
		"unknown field":    `{"caller":"a","confirm_caller":"a","force":true}`,
		"a boolean alone":  `{"confirm":true}`,
		"not a json value": `nope`,
	} {
		if r := post(body); r.Code != 400 {
			t.Errorf("%s: %d %s", name, r.Code, r.Body)
		}
	}
	if len(f.rec.asked) != 0 {
		t.Fatal("a refused request touched recordings")
	}

	var dry api.CallerErasure
	r := post(`{"caller":"` + label + `","confirm_caller":"` + label + `","dry_run":true}`)
	r.data(t, &dry)
	if r.Code != 200 || dry.Erased != 0 || dry.Remaining != 3 || dry.Held != 1 || dry.Active != 1 || len(f.rec.asked) != 0 {
		t.Fatalf("dry run: %d %s", r.Code, r.Body)
	}

	var out api.CallerErasure
	r = post(`{"caller":"` + label + `","confirm_caller":"` + label + `"}`)
	r.data(t, &out)
	if r.Code != 200 || out.Erased != 3 || out.Remaining != 0 || out.Held != 1 || out.Active != 1 || out.RecordingsRemoved != 6 || out.Turns != 3 {
		t.Fatalf("erase: %d %s", r.Code, r.Body)
	}
	if f.erased["caller"] != 3 {
		t.Fatalf("metric hook = %v", f.erased)
	}
	for _, id := range ids {
		if f.rec.files[id] != 0 {
			t.Fatalf("recording of %s left", id)
		}
	}
	if f.rec.files[held] != 2 || f.rec.files[other] != 2 {
		t.Fatal("recordings of a held call or of another caller were removed")
	}
	var again api.CallerErasure
	post(`{"caller":"`+label+`","confirm_caller":"`+label+`"}`).data(t, &again)
	if again.Erased != 0 {
		t.Fatalf("a repeat erased %d", again.Erased)
	}

	// neither the logs, the audit trail nor the status carry the label or transcript text
	var status retention.Status
	st := f.do(f.status, http.MethodGet, "/api/v1/retention", "")
	st.data(t, &status)
	for _, text := range []string{f.logs.String(), st.Body} {
		if strings.Contains(text, label) || strings.Contains(text, "Hello world") {
			t.Fatalf("a label or transcript leaked: %s", text)
		}
	}
	if status.RecentActions[0].Action != "erase_caller" || status.RecentActions[0].Subject != storage.CallerHash(label) {
		t.Fatalf("audit = %+v", status.RecentActions)
	}
}

func TestEraseByCallerListsCallsWhoseRecordingCouldNotBeRemoved(t *testing.T) {
	f := newFixture(t)
	a, b := f.call("c", "ended"), f.call("c", "ended")
	f.rec.failing[a] = true
	var out api.CallerErasure
	r := f.do(f.handler, http.MethodPost, "/api/v1/sessions/erase", `{"caller":"c","confirm_caller":"c"}`)
	r.data(t, &out)
	if r.Code != 200 || out.Erased != 2 || len(out.RecordingsFailed) != 1 || out.RecordingsFailed[0] != a || out.RecordingsRemoved != 2 || f.rec.files[b] != 0 {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	if strings.Contains(f.logs.String(), "/var/recordings") {
		t.Fatal("a recording path was logged")
	}
}

func TestLifecycleWithoutRecordingsStillErasesRows(t *testing.T) {
	store := newTestStore(t)
	agent, version, hash := createTestAgentAndVersion(t, store)
	d := createSessionWithData(t, store, agent, version, hash, "ended", time.Now().Add(-time.Hour), time.Now().Add(-time.Minute))
	life := retention.NewLifecycle(store.Retention(), nil, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), nil)
	out, err := life.Erase(context.Background(), d.sessionID, "r")
	if err != nil || out.Turns != 1 || out.RecordingsRemoved != 0 {
		t.Fatalf("%+v %v", out, err)
	}
	if _, err := life.Erase(context.Background(), d.sessionID, "r"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("second erase: %v", err)
	}
}

// The store keeps up to 128 characters, so a label of 128 multibyte characters can be erased.
func TestEraseByCallerAcceptsALabelOfMultibyteCharacters(t *testing.T) {
	f := newFixture(t)
	label := strings.Repeat("é", 128)
	f.call(label, "ended")
	var out api.CallerErasure
	r := f.do(f.handler, http.MethodPost, "/api/v1/sessions/erase", `{"caller":"`+label+`","confirm_caller":"`+label+`"}`)
	r.data(t, &out)
	if r.Code != 200 || out.Erased != 1 {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	long := label + "é"
	if r := f.do(f.handler, http.MethodPost, "/api/v1/sessions/erase", `{"caller":"`+long+`","confirm_caller":"`+long+`"}`); r.Code != 400 {
		t.Fatalf("129 characters: %d", r.Code)
	}
}
