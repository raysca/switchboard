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
	"govoice/core/turn"
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

type parityTurnProcessor struct {
	manager *turn.Manager
	clock   time.Time
	turn    int
}

func (p *parityTurnProcessor) ProcessFrame(_ context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	p.clock = p.clock.Add(time.Millisecond)
	var decisions []core.Frame
	var err error
	switch f.Type() {
	case core.FrameUserStartedSpeaking:
		p.turn++
		decisions, err = p.manager.Observe(turn.Observation{At: p.clock, TurnID: core.TurnID(fmt.Sprintf("turn-%d", p.turn)), Generation: core.Generation(p.turn), Speaking: true, Activity: true})
	case core.FrameUserStoppedSpeaking:
		decisions, err = p.manager.Observe(turn.Observation{At: p.clock, TurnID: core.TurnID(fmt.Sprintf("turn-%d", p.turn)), Generation: core.Generation(p.turn), Speaking: false})
	case core.FrameEnd:
		// production interruption path: a newer generation fences the active turn
		decisions, err = p.manager.Observe(turn.Observation{At: p.clock, TurnID: core.TurnID(fmt.Sprintf("turn-%d", p.turn)), Generation: core.Generation(p.turn + 1), Speaking: false})
	}
	if err != nil {
		return err
	}
	for _, decision := range decisions {
		if err := e.PushDirection(decision, core.Downstream); err != nil {
			return err
		}
	}
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

func TestParityDecisionsComeFromRuntimeTurnEvents(t *testing.T) {
	events := observedParityEvents(t, "user_interrupts_bot")
	want := []struct {
		turn core.TurnID
		kind core.TurnEventKind
	}{{"turn-1", core.TurnEventStarted}, {"turn-1", core.TurnEventInterrupted}, {"turn-2", core.TurnEventStarted}, {"turn-2", core.TurnEventInterrupted}}
	if len(events) != len(want) {
		t.Fatalf("got %d controller events, want %d", len(events), len(want))
	}
	for i, event := range events {
		if event.Turn != want[i].turn || event.Kind != want[i].kind {
			t.Fatalf("controller event %d: %#v", i, event)
		}
	}
}

func observedParityStates(t *testing.T, name string) []string {
	events := observedParityEvents(t, name)
	states := make([]string, 0, len(events))
	for _, event := range events {
		switch event.Kind {
		case core.TurnEventStarted:
			states = append(states, string(event.Turn)+":started")
		case core.TurnEventInterrupted:
			states = append(states, string(event.Turn)+":interrupted")
		}
	}
	return states
}

func observedParityEvents(t *testing.T, name string) []core.TurnEventFrame {
	t.Helper()
	cfg := turn.DefaultConfig()
	cfg.StartDebounce, cfg.StopDebounce, cfg.MinimumSpeechDuration = 0, 0, 0
	cfg.SilenceTimeout = 5 * time.Second
	manager, err := turn.NewManager(cfg, turn.SilenceDetector{})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	bridge := &parityTurnProcessor{manager: manager, clock: time.Unix(100, 0)}
	pipe, err := core.NewPipeline(bridge)
	if err != nil {
		t.Fatal(err)
	}
	session, err := core.NewSession(123, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	frames := make(chan core.Frame, 24)
	session.OnOutput = func(f core.Frame) { frames <- f }
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
		// cancel_flushes_active_turn: Pipecat's turn_tracking_observer.py:102 treats
		// EndFrame and the cancel frame alike (both end the active turn as interrupted).
		sequence = append(sequence, &core.EndFrame{})
	}
	observed := make([]core.TurnEventFrame, 0, 4)
	for _, f := range sequence {
		if err := session.Post(f); err != nil {
			t.Fatal(err)
		}
		for {
			select {
			case delivered := <-frames:
				if event, ok := delivered.(*core.TurnEventFrame); ok {
					if event.Kind == core.TurnEventStarted || event.Kind == core.TurnEventInterrupted {
						observed = append(observed, *event)
					}
				}
				if delivered.Type() == f.Type() {
					goto nextFrame
				}
			case <-time.After(time.Second):
				t.Fatalf("runtime did not deliver %T", f)
			}
		}
	nextFrame:
	}
	return observed
}
