package server

import (
	"net/http"
	"strings"
	"sync"
	"testing"
)

const testMgmtKey = "management-test-key-32-bytes-long-and-separate"

type recInjector struct {
	mu    sync.Mutex
	notes []string
	runs  []bool
	ran   int
	ready bool
}

func (r *recInjector) Note(c string, run bool) {
	r.mu.Lock()
	r.notes, r.runs = append(r.notes, c), append(r.runs, run)
	r.mu.Unlock()
}
func (r *recInjector) Run()        { r.mu.Lock(); r.ran++; r.mu.Unlock() }
func (r *recInjector) Ready() bool { return r.ready }

func TestLiveCallRoutesNeedTheManagementKey(t *testing.T) {
	s := apiTestServer(t)
	s.live.Put("sess1", &recInjector{ready: true})
	body := `{"messages":[{"content":"hi"}]}`
	for _, key := range []string{"", "wrong-key", s.cfg.Token.Secret} { // none, bad, the voice secret
		if w := apiRequest(s, http.MethodPost, "/api/v1/sessions/sess1/context", key, body); w.Code != http.StatusUnauthorized {
			t.Errorf("key %q: status = %d, want 401", key, w.Code)
		}
		if w := apiRequest(s, http.MethodPost, "/api/v1/sessions/sess1/run", key, ""); w.Code != http.StatusUnauthorized {
			t.Errorf("run, key %q: status = %d, want 401", key, w.Code)
		}
	}
}

func TestLiveCallContextAcceptsNotes(t *testing.T) {
	s := apiTestServer(t)
	inj := &recInjector{ready: true}
	s.live.Put("sess1", inj)
	w := apiRequest(s, http.MethodPost, "/api/v1/sessions/sess1/context", testMgmtKey, `{"messages":[{"content":" a "},{"content":"b"}],"run":true}`)
	if w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), `"accepted":2`) {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	if len(inj.notes) != 2 || inj.notes[0] != "a" || inj.notes[1] != "b" || !inj.runs[0] || !inj.runs[1] {
		t.Fatalf("notes = %v runs = %v, want trimmed a, b both with run", inj.notes, inj.runs)
	}
}

func TestLiveCallRunTriggersAReply(t *testing.T) {
	s := apiTestServer(t)
	inj := &recInjector{ready: true}
	s.live.Put("sess1", inj)
	if w := apiRequest(s, http.MethodPost, "/api/v1/sessions/sess1/run", testMgmtKey, ""); w.Code != http.StatusAccepted {
		t.Fatalf("status = %d", w.Code)
	}
	if inj.ran != 1 {
		t.Fatalf("Run calls = %d, want 1", inj.ran)
	}
}

func TestLiveCallRoutesRejectBadRequests(t *testing.T) {
	s := apiTestServer(t)
	s.live.Put("sess1", &recInjector{ready: true})
	long := strings.Repeat("a", maxNoteBytes+1)
	for name, body := range map[string]string{
		"no messages":   `{"messages":[]}`,
		"empty note":    `{"messages":[{"content":"  "}]}`,
		"too long":      `{"messages":[{"content":"` + long + `"}]}`,
		"unknown field": `{"messages":[{"content":"a"}],"role":"system"}`,
		"role in note":  `{"messages":[{"content":"a","role":"system"}]}`,
		"not json":      `nope`,
	} {
		w := apiRequest(s, http.MethodPost, "/api/v1/sessions/sess1/context", testMgmtKey, body)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid_request") {
			t.Errorf("%s: status = %d body = %s, want 400 invalid_request", name, w.Code, w.Body.String())
		}
	}
}

func TestLiveCallRoutesStatuses(t *testing.T) {
	s := apiTestServer(t)
	body := `{"messages":[{"content":"hi"}]}`
	if w := apiRequest(s, http.MethodPost, "/api/v1/sessions/nope/context", testMgmtKey, body); w.Code != http.StatusNotFound {
		t.Errorf("unknown session: status = %d, want 404", w.Code)
	}
	s.live.Put("starting", &recInjector{ready: false})
	if w := apiRequest(s, http.MethodPost, "/api/v1/sessions/starting/context", testMgmtKey, body); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "call_not_ready") {
		t.Errorf("starting session: status = %d body = %s, want 409 call_not_ready", w.Code, w.Body.String())
	}
	s.live.Put("sess1", &recInjector{ready: true})
	if w := apiRequest(s, http.MethodGet, "/api/v1/sessions/sess1/context", testMgmtKey, ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: status = %d, want 405", w.Code)
	}
	s.live.Delete("sess1")
	if w := apiRequest(s, http.MethodPost, "/api/v1/sessions/sess1/run", testMgmtKey, ""); w.Code != http.StatusNotFound {
		t.Errorf("ended session: status = %d, want 404", w.Code)
	}
}

func TestLiveCallRoutesAreRateLimited(t *testing.T) {
	s := apiTestServer(t)
	s.live.Put("sess1", &recInjector{ready: true})
	body := `{"messages":[{"content":"hi"}]}`
	var limited bool
	for i := 0; i < noteBurst+2; i++ {
		w := apiRequest(s, http.MethodPost, "/api/v1/sessions/sess1/context", testMgmtKey, body)
		if w.Code == http.StatusTooManyRequests {
			limited = strings.Contains(w.Body.String(), "rate_limited")
			break
		}
	}
	if !limited {
		t.Fatal("a burst past the bucket was never answered with 429 rate_limited")
	}
}

// The stored-session routes keep working: the new paths do not shadow them.
func TestLiveCallRoutesDoNotShadowSessionHistory(t *testing.T) {
	s := apiTestServer(t)
	if w := apiRequest(s, http.MethodGet, "/api/v1/sessions", testMgmtKey, ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("sessions list without storage: status = %d, want 503 as before", w.Code)
	}
}
