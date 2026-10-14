package server

import (
	"encoding/json"
	"testing"

	"govoice/server/flow"
)

func TestAReplyIsStoredWithItsLatencies(t *testing.T) {
	r := newFlowRigE2E(t, newProbe())
	f, err := r.s.flows.Create(t.Context(), flow.CreateRequest{Name: "Latency", Config: json.RawMessage(echoFlowConfig("LAT"))})
	if err != nil || f.Readiness.State != flow.ReadinessReady {
		t.Fatalf("flow: %+v err %v", f.Readiness, err)
	}
	c := r.dial(f.ID)
	c.say("hello")
	c.waitReplies("[LAT", 1)
	eventually(t, "a turn row with latencies", func() bool {
		turns, err := r.s.store.Sessions().Turns(t.Context(), c.sessionID)
		if err != nil || len(turns) == 0 {
			return false
		}
		// A typed turn has no end of speech, so no voice_to_voice; the model and voice stages are
		// there (the echo provider is instant, so they may be 0 ms).
		var got map[string]int64
		if json.Unmarshal([]byte(turns[0].LatenciesJSON), &got) != nil {
			return false
		}
		_, llm := got["llm_ttft_ms"]
		_, tts := got["tts_ttfb_ms"]
		return llm && tts
	})
}
