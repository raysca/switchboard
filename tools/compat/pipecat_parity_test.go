package compat_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"govoice/core"
)

type parityFixture struct {
	Reference struct {
		Repository string `json:"repository"`
		Revision   string `json:"revision"`
		File       string `json:"file"`
	} `json:"reference"`
	Cases []struct {
		Name          string   `json:"name"`
		ReferenceTest string   `json:"reference_test"`
		Expected      []string `json:"expected_turn_states"`
	} `json:"cases"`
}

type parityForwarder struct{}

func (parityForwarder) ProcessFrame(_ context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	return e.PushDirection(f, dir)
}

func TestPinnedPipecatTurnInterruptionPairedBehavior(t *testing.T) {
	data, err := os.ReadFile("fixtures/pipecat-turn-parity.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture parityFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	const revision = "2967e1c09ad484076089bc88427d305fe8f79fa9"
	if fixture.Reference.Repository != "pipecat-impl" || fixture.Reference.Revision != revision || fixture.Reference.File != "tests/test_turn_tracking_observer.py" || len(fixture.Cases) != 2 {
		t.Fatalf("unrecognized Pipecat fixture: %#v", fixture.Reference)
	}
	refDir := filepath.Join("..", "..", "pipecat-impl")
	head, err := exec.Command("git", "-C", refDir, "rev-parse", "HEAD").Output()
	if err != nil || strings.TrimSpace(string(head)) != revision {
		t.Fatalf("Pipecat reference revision differs from fixture: %q, %v", head, err)
	}
	source, err := os.ReadFile(filepath.Join(refDir, fixture.Reference.File))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.Cases {
		if !strings.Contains(string(source), "async def "+tc.ReferenceTest+"(") {
			t.Fatalf("fixture test %s absent from pinned source", tc.ReferenceTest)
		}
		validPair := (tc.Name == "user_interrupts_bot" && tc.ReferenceTest == "test_user_interrupts_bot") || (tc.Name == "cancel_flushes_active_turn" && tc.ReferenceTest == "test_cancel_frame_flushes_active_turn")
		if !validPair {
			t.Fatalf("fixture scenario %q does not match reference method %q", tc.Name, tc.ReferenceTest)
		}
		if tc.Name == "user_interrupts_bot" && !strings.Contains(string(source), "Turn 2 ended (interrupted: True)") {
			t.Fatal("pinned reference no longer records turn-2 interrupted on EndFrame")
		}
		t.Run(tc.Name, func(t *testing.T) {
			got := observedParityStates(t, tc.Name)
			if !reflect.DeepEqual(got, tc.Expected) {
				t.Fatalf("Go frame-derived events %v differ from Pipecat %v", got, tc.Expected)
			}
		})
	}
}

func observedParityStates(t *testing.T, name string) []string {
	t.Helper()
	pipe, err := core.NewPipeline(parityForwarder{})
	if err != nil {
		t.Fatal(err)
	}
	session, err := core.NewSession(123, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	frames := make(chan core.FrameType, 12)
	session.OnOutput = func(f core.Frame) { frames <- f.Type() }
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	select {
	case <-session.Ready():
	case <-time.After(time.Second):
		t.Fatal("session not ready")
	}
	sequence := []core.Frame{&core.UserStartedSpeakingFrame{}, &core.UserStoppedSpeakingFrame{}, &core.SpeakingStateFrame{State: core.SpeakingStarted, Turn: "turn-1", Gen: 1}}
	if name == "user_interrupts_bot" {
		sequence = append(sequence, &core.UserStartedSpeakingFrame{}, &core.EndFrame{})
	} else {
		sequence = append(sequence, &core.CancelFrame{})
	}
	observed := make([]core.FrameType, 0, len(sequence))
	for _, f := range sequence {
		if err := session.Post(f); err != nil {
			t.Fatal(err)
		}
		select {
		case kind := <-frames:
			observed = append(observed, kind)
		case <-time.After(time.Second):
			t.Fatalf("runtime did not deliver %T", f)
		}
	}
	var lifecycle core.TurnLifecycle
	states := []string{}
	turn := 0
	botSpeaking := false
	for _, kind := range observed {
		switch kind {
		case core.FrameUserStartedSpeaking:
			if botSpeaking && turn > 0 {
				id := core.TurnID(fmt.Sprintf("turn-%d", turn))
				if !lifecycle.Interrupt(id) {
					t.Fatalf("turn %d did not interrupt", turn)
				}
				states = append(states, string(id)+":interrupted")
				botSpeaking = false
			}
			turn++
			id := core.TurnID(fmt.Sprintf("turn-%d", turn))
			if !lifecycle.Start(id, core.Generation(turn)) {
				t.Fatalf("turn %d did not start", turn)
			}
			states = append(states, string(id)+":started")
		case core.FrameSpeakingState:
			botSpeaking = true
		case core.FrameEnd, core.FrameCancel:
			id := core.TurnID(fmt.Sprintf("turn-%d", turn))
			if !lifecycle.Interrupt(id) {
				t.Fatalf("terminal frame did not interrupt turn %d", turn)
			}
			states = append(states, string(id)+":interrupted")
		}
	}
	return states
}
