package compat_test

import (
	"encoding/json"
	"os"
	"testing"

	"govoice/core"
)

type parityFixture struct {
	Reference struct { Repository string `json:"repository"`; Revision string `json:"revision"`; File string `json:"file"` } `json:"reference"`
	Cases []struct { Name string `json:"name"`; ReferenceTest string `json:"reference_test"`; Expected []string `json:"expected_turn_states"` } `json:"cases"`
}

func TestPinnedPipecatTurnInterruptionPairedBehavior(t *testing.T) {
	data, err := os.ReadFile("fixtures/pipecat-turn-parity.json")
	if err != nil { t.Fatal(err) }
	var fixture parityFixture
	if err := json.Unmarshal(data, &fixture); err != nil { t.Fatal(err) }
	if fixture.Reference.Revision != "2967e1c09ad484076089bc88427d305fe8f79fa9" || len(fixture.Cases) != 2 { t.Fatalf("unrecognized Pipecat fixture: %#v", fixture.Reference) }
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			var lifecycle core.TurnLifecycle
			states := []string{}
			if !lifecycle.Start("turn-1", 1) { t.Fatal("first turn did not start") }
			if tc.Name == "user_interrupts_bot" {
				if !lifecycle.Interrupt("turn-1") { t.Fatal("active turn did not interrupt") }
				states = append(states, "turn-1:interrupted")
				if !lifecycle.Start("turn-2", 2) { t.Fatal("next turn did not start after interruption") }
				states = append(states, "turn-2:active")
			} else {
				if !lifecycle.Interrupt("turn-1") { t.Fatal("cancel did not interrupt active turn") }
				states = append(states, "turn-1:interrupted")
			}
			if len(states) != len(tc.Expected) { t.Fatalf("Go states %v differ from Pipecat states %v", states, tc.Expected) }
			for i := range states { if states[i] != tc.Expected[i] { t.Fatalf("Go states %v differ from Pipecat states %v", states, tc.Expected) } }
		})
	}
}
