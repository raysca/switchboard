package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/coder/websocket"

	"govoice/server/flow"
	"govoice/server/storage"
)

// What a call leaves behind: who called and why it ended, for every way a call can end, over real
// WebSocket calls on the echo provider.

func (r *flowRigE2E) session(id string) storage.SessionRecord {
	r.t.Helper()
	rec, err := r.s.store.Sessions().Get(context.Background(), id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		r.t.Fatal(err)
	}
	return rec // zero until the row exists
}

func (r *flowRigE2E) waitEnded(id, reason string) storage.SessionRecord {
	r.t.Helper()
	var rec storage.SessionRecord
	eventually(r.t, "session "+id+" ended with "+reason, func() bool {
		rec = r.session(id)
		return rec.Status == "ended" && rec.EndReason == reason
	})
	return rec
}

func TestCallRecordsTheCallersLabelAndAHangUp(t *testing.T) {
	r := newFlowRigE2E(t, newProbe(), serverIdle(0, ""))
	id := behaviorFlow(t, r, "Plain", "plainvoice", ``)
	c := r.dial(id) // the token's principal is "alice"
	eventually(t, "the row", func() bool { return r.session(c.sessionID).ID != "" })
	if rec := r.session(c.sessionID); rec.Caller != "alice" || rec.EndReason != "" || rec.Status != "active" {
		t.Fatalf("while live: %+v", rec)
	}
	_ = c.conn.Close(websocket.StatusNormalClosure, "bye")
	rec := r.waitEnded(c.sessionID, storage.EndCallerHungUp)
	if rec.EndedAt.IsZero() {
		t.Fatal("no end time")
	}
}

func TestCallRecordsASilentCallerAndKeepsTheFirstCause(t *testing.T) {
	r := newFlowRigE2E(t, newProbe(), serverIdle(time.Minute, "server prompt"))
	quiet := behaviorFlow(t, r, "Quiet", "quietvoice", `,"idle":{"user_timeout_ms":1000,"max_prompts":1,"prompt_mode":"say"}`)
	c := r.dial(quiet)
	r.waitEnded(c.sessionID, storage.EndSilentCaller)
	_ = c.conn.CloseNow() // the socket closing after the server hung up must not rewrite the reason
	time.Sleep(200 * time.Millisecond)
	if got := r.session(c.sessionID).EndReason; got != storage.EndSilentCaller {
		t.Fatalf("reason became %q", got)
	}
}

func TestCallRecordsAFlowThatEndedTheCall(t *testing.T) {
	r := newFlowRigE2E(t, newProbe(), serverIdle(0, ""))
	a, err := r.s.flows.Create(t.Context(), flow.CreateRequest{Name: "Closer", Config: endCallFlowJSON(true, false)})
	if err != nil || a.Readiness.State != flow.ReadinessReady {
		t.Fatalf("flow: %+v err=%v", a.Readiness, err)
	}
	r.flowID = a.ID
	c := r.dial(a.ID)
	c.say("hello")
	c.waitReplies("[FRONT", 1)
	c.say("please end the call")
	r.waitEnded(c.sessionID, storage.EndedByFlow)
}

func TestCallRecordsTheTimeLimit(t *testing.T) {
	r := newFlowRigE2E(t, newProbe(), serverIdle(0, ""), func(c *Config) { c.Limits.MaxCallDuration = time.Second })
	c := r.dial(behaviorFlow(t, r, "Plain", "plainvoice", ``))
	r.waitEnded(c.sessionID, storage.EndTimeLimit)
}

func TestCallRecordsAServerShutdown(t *testing.T) {
	r := newFlowRigE2E(t, newProbe(), serverIdle(0, ""), func(c *Config) { c.Limits.DrainTimeout = 500 * time.Millisecond })
	c := r.dial(behaviorFlow(t, r, "Plain", "plainvoice", ``))
	eventually(t, "the call is live", func() bool { return r.activeSessions() == 1 })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = r.s.Shutdown(ctx) // the call outlives the drain timeout, so it is force-closed and Shutdown reports that
	// Shutdown closed the server's store: read the row through a fresh handle on the same file.
	st, err := storage.Open(context.Background(), storage.Config{Dialect: storage.SQLite, DSN: os.Getenv("GOVOICE_DATABASE_DSN")})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rec, err := st.Sessions().Get(context.Background(), c.sessionID)
	if err != nil || rec.Status != "ended" || rec.EndReason != storage.EndServerShutdown {
		t.Fatalf("%+v err %v", rec, err)
	}
}

func TestCallRecordsACallThatCouldNotStart(t *testing.T) {
	p := newProbe()
	p.failTTS = "failvoice"
	r := newFlowRigE2E(t, p, serverIdle(0, ""))
	id := behaviorFlow(t, r, "Broken", "failvoice", ``)
	c := r.dial(id)
	r.waitEnded(c.sessionID, storage.EndSetupFailed)
}

func TestTheOrphanSweepRecordsAServerRestart(t *testing.T) {
	r := newFlowRigE2E(t, newProbe(), serverIdle(0, ""))
	id := behaviorFlow(t, r, "Plain", "plainvoice", ``)
	pin, err := r.s.flows.StartSession(context.Background(), id, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.s.store.Sessions().EndOrphans(context.Background(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if rec := r.session(pin.ID); rec.EndReason != storage.EndServerRestart || rec.Status != "ended" {
		t.Fatalf("%+v", rec)
	}
}

func TestATypedTurnLeavesStageOffsetsAndAMedianInTheAPI(t *testing.T) {
	r := newFlowRigE2E(t, newProbe(), serverIdle(0, ""))
	c := r.dial(behaviorFlow(t, r, "Plain", "plainvoice", ``))
	c.say("hello")
	c.waitReplies("[Plain", 1)
	var l map[string]int64
	eventually(t, "a measured turn row", func() bool {
		turns, err := r.s.store.Sessions().Turns(context.Background(), c.sessionID)
		if err != nil || len(turns) == 0 {
			return false
		}
		l = nil
		return json.Unmarshal([]byte(turns[0].LatenciesJSON), &l) == nil && hasKeys(l, "llm_ttft_ms", "tts_ttfb_ms", "llm_start_ms", "tts_start_ms")
	})
	if l["tts_start_ms"] < l["llm_start_ms"] {
		t.Fatalf("the voice cannot start before the model: %v", l)
	}
	w := apiRequest(r.s, http.MethodGet, "/api/v1/sessions/"+c.sessionID, e2eMgmtKey, "")
	var out struct {
		Data struct {
			Caller        string `json:"caller"`
			MedianReplyMS *int   `json:"median_reply_ms"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.Data.Caller != "alice" || out.Data.MedianReplyMS == nil {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func hasKeys(m map[string]int64, keys ...string) bool {
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			return false
		}
	}
	return true
}

func TestCallRecordsAnAgentThatStoppedResponding(t *testing.T) {
	// The echo model's token delay is longer than the stuck-bot watchdog, so the reply never starts speaking.
	r := newFlowRigE2E(t, newProbe(), serverIdle(0, ""), func(c *Config) {
		c.Providers.Echo.LLMExtraDelay = 30 * time.Second
		c.Idle.BotTimeout, c.Idle.BotMaxStuck = 400*time.Millisecond, 1
	})
	c := r.dial(behaviorFlow(t, r, "Slow", "slowvoice", ``))
	c.say("hello there")
	r.waitEnded(c.sessionID, storage.EndAgentStuck)
}
