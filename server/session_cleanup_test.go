package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"govoice/server/flow"
)

// A call that fails to set up after its pipeline is built must close every
// provider stage exactly once (no leaked clients per failed attempt).
func TestSetupFailureClosesProviders(t *testing.T) {
	for name, tc := range map[string]struct {
		async    int
		greeting bool
	}{
		"create_session": {async: 0},                 // budget rejected by core.NewSession
		"post_session":   {async: 1, greeting: true}, // observer takes the only task slot; greeting Go fails
	} {
		t.Run(name, func(t *testing.T) {
			probe := newProbe()
			r := newFlowRigE2E(t, probe)
			r.s.sessionBudget.MaxAsyncTasks = tc.async
			id := r.createRoutedFlow("salesvoice")
			if tc.greeting {
				raw := strings.Replace(string(flowConfigJSON("SALES", "salesvoice")), `"config":{`, `"config":{"greeting":"hello there",`, 1)
				a, err := r.s.flows.Create(r.t.Context(), flow.CreateRequest{Name: "Greeter", Config: json.RawMessage(raw)})
				if err != nil {
					t.Fatal(err)
				}
				id = a.ID
			}
			r.dial(id)
			eventually(t, "providers built", func() bool { return probe.builds("FRONT") == 1 })
			eventually(t, "providers closed", func() bool { return probe.closes("FRONT") >= 1 && probe.closes("frontvoice") >= 1 })
			time.Sleep(100 * time.Millisecond)
			if probe.closes("FRONT") != 1 || probe.closes("frontvoice") != 1 {
				t.Fatalf("closes llm=%d tts=%d, want 1 each", probe.closes("FRONT"), probe.closes("frontvoice"))
			}
		})
	}
}
