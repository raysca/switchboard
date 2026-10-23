package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// Operator control through a real call (onSessionReady wiring): a note with run starts a reply
// on the start step, still works after the call handoffs to the next step, and the call is gone
// (404) once it ends.
func TestContextInjectionAgentE2E(t *testing.T) {
	r := newAgentRigE2E(t, newProbe())
	c := r.dial(r.createHandoffAgent("salesvoice"))
	eventually(t, "session", func() bool { return r.activeSessions() == 1 })
	post := func(path, body string) int {
		return apiRequest(r.s, http.MethodPost, "/api/v1/sessions/"+c.sessionID+path, e2eMgmtKey, body).Code
	}

	c.say("hello")
	c.waitReplies("[FRONT", 1)
	time.Sleep(600 * time.Millisecond) // the reply has finished: the call is idle
	if code := post("/context", `{"messages":[{"content":"the caller is a VIP"}],"run":true}`); code != http.StatusAccepted {
		t.Fatalf("note on the start step: status %d", code)
	}
	c.waitReplies("the caller is a VIP", 1) // the requested reply (echo repeats the note the model saw), on the start step
	if c.replies("[FRONT") < 2 {
		t.Fatalf("the note's reply did not come from the start step (FRONT replies %d)", c.replies("[FRONT"))
	}

	time.Sleep(time.Second)
	c.say("please transfer to sales")
	c.waitReplies("[SALES", 1)
	time.Sleep(time.Second)
	sales := c.replies("[SALES")
	if code := post("/run", ""); code != http.StatusAccepted {
		t.Fatalf("run after the handoff: status %d", code)
	}
	c.waitReplies("run_llm", 1)
	eventually(t, "the reply from the step the call moved to", func() bool { return c.replies("[SALES") > sales })

	_ = c.conn.Close(websocket.StatusNormalClosure, "bye")
	eventually(t, "session ended", func() bool { return r.activeSessions() == 0 })
	if code := post("/context", `{"messages":[{"content":"too late"}]}`); code != http.StatusNotFound {
		t.Fatalf("note after the call ended: status %d, want 404", code)
	}
}
