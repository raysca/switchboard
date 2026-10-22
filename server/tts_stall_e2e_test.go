package server

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
	"govoice/server/agent"
)

// stalledTTS is a TTS provider whose synthesis never answers (a wedged socket): it takes units and emits nothing.
type stalledTTS struct{ units atomic.Int32 }

func (p *stalledTTS) ProcessFrame(_ context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	if _, ok := f.(*core.SynthesisUnitFrame); ok {
		p.units.Add(1)
		return nil
	}
	return e.PushDirection(f, dir)
}

// The primary TTS stalls on the reply's first sentence: its watchdog expires (handled on the run loop), the
// sentence moves to the fallback voice, and every later sentence is spoken by the fallback too.
func TestCallBehaviorAStalledTTSUnitFailsOverAndLaterUnitsAreSpoken(t *testing.T) {
	r := newAgentRigE2E(t, newProbe())
	r.s.registry.ttsUnitTimeout = 200 * time.Millisecond // core's default is 30 s
	stalled := &stalledTTS{}
	healthy := r.s.registry.factories["echo.tts"]
	r.s.registry.Register("echo.tts", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		if spec.Voice == "stallvoice" {
			return stalled, nil
		}
		return healthy(cfg, spec)
	})
	f, err := r.s.agents.Create(t.Context(), agent.CreateRequest{Name: "Stall", Config: json.RawMessage(
		`{"stt":{"provider":"echo","model":"s"},"llm":{"provider":"echo","model":"m","system_prompt":"Stall"},` +
			`"tts":{"provider":"echo","voice":"stallvoice","fallbacks":[{"provider":"echo","voice":"okvoice"}]}}`)})
	if err != nil || f.Readiness.State != agent.ReadinessReady {
		t.Fatalf("flow: %+v err=%v", f.Readiness, err)
	}
	c := r.dial(f.ID)
	c.say("First sentence here. Second sentence here. Third sentence here.")
	for _, s := range []string{"First sentence", "Second sentence", "Third sentence"} {
		eventually(t, s+" spoken by the fallback", func() bool { return r.spokenBy("okvoice", s) })
	}
	if n := stalled.units.Load(); n != 1 {
		t.Fatalf("the stalled primary got %d units, want 1 (demoted for the rest of the turn)", n)
	}
	if c.ended() {
		t.Fatal("a stalled TTS unit ended the call")
	}
}
