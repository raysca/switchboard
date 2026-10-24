package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"govoice/server/storage"
)

// A recorded call can be held and erased through the management API: the recording is named by the stored call, it
// is complete by the time the call reads as ended, a hold protects both, and an erasure removes both.
func TestCallRecordsHoldAndErasureCoverTheRecording(t *testing.T) {
	dir := t.TempDir()
	r := newAgentRigE2E(t, newProbe(), serverIdle(0, ""), func(c *Config) { c.Recording.Enabled, c.Recording.Dir = true, dir })
	id := behaviorAgent(t, r, "Recorded", "recvoice", `,"recording":{"enabled":true,"disclosure_confirmed":true}`)
	c := r.dial(id)
	c.say("hello")
	eventually(t, "the agent spoke", func() bool { return r.spokenBy("recvoice", "hello") })
	_ = c.conn.Close(websocket.StatusNormalClosure, "bye")
	r.waitEnded(c.sessionID, storage.EndCallerHungUp)

	// The call is ended, so its recording is finished and named by the stored call.
	wav, sidecar := filepath.Join(dir, c.sessionID+".wav"), filepath.Join(dir, c.sessionID+".json")
	for _, f := range []string{wav, sidecar} {
		if fi, err := os.Stat(f); err != nil || fi.Size() == 0 {
			t.Fatalf("%s: %v", filepath.Base(f), err)
		}
	}
	if raw, _ := os.ReadFile(sidecar); !strings.Contains(string(raw), c.sessionID) {
		t.Fatalf("the sidecar does not name the stored call: %s", raw)
	}

	api := func(method, path, body string) *httptestResponse {
		w := apiRequest(r.s, method, path, e2eMgmtKey, body)
		return &httptestResponse{w.Code, w.Body.String()}
	}
	base := "/api/v1/sessions/" + c.sessionID
	if got := api(http.MethodPut, base+"/hold", `{"reason":"legal"}`); got.code != 200 || !strings.Contains(got.body, `"held":true`) {
		t.Fatalf("hold: %d %s", got.code, got.body)
	}
	if got := api(http.MethodDelete, base, ""); got.code != http.StatusConflict || !strings.Contains(got.body, "session_held") {
		t.Fatalf("erase while held: %d %s", got.code, got.body)
	}
	for _, f := range []string{wav, sidecar} {
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("a refused erasure removed %s", filepath.Base(f))
		}
	}
	if got := api(http.MethodDelete, base+"/hold", ""); got.code != 200 {
		t.Fatalf("release: %d %s", got.code, got.body)
	}
	got := api(http.MethodDelete, base, "")
	if got.code != 200 || !strings.Contains(got.body, `"recordings_removed":2`) {
		t.Fatalf("erase: %d %s", got.code, got.body)
	}
	for _, f := range []string{wav, sidecar} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Fatalf("%s survived the erasure: %v", filepath.Base(f), err)
		}
	}
	if got := api(http.MethodGet, base, ""); got.code != http.StatusNotFound {
		t.Fatalf("erased call still readable: %d", got.code)
	}
	// the status shows the three audited actions and no hold left
	status := api(http.MethodGet, "/api/v1/retention", "")
	if status.code != 200 || !strings.Contains(status.body, `"held_sessions":0`) || !strings.Contains(status.body, `"erase_call"`) || strings.Contains(status.body, "legal") {
		t.Fatalf("status: %d %s", status.code, status.body)
	}
}

type httptestResponse struct {
	code int
	body string
}
