//go:build xaispeech

package server

import (
	"encoding/json"
	"testing"
	"time"

	"govoice/server/flow"
)

// Per-flow interruption settings against the real VAD: caller audio goes through Silero, the echo STT turns
// voiced audio into placeholder words (an interim every 400 ms, like Google's latest_long), and the turn
// processor decides whether that speech cuts the agent off. Typed turns bypass min words and mute, so
// these tests need real audio.

// interruptionRig is Google-like echo timing with audio recognition and the real VAD.
func interruptionRig(t *testing.T, mods ...func(*Config)) *flowRigE2E {
	t.Helper()
	r := newFlowRigE2E(t, newProbe(), append([]func(*Config){func(c *Config) {
		c.Providers.Echo.Mimic, c.Providers.Echo.STT = "google", "audio"
	}}, mods...)...)
	realVAD(t, r.s)
	return r
}

// interruptionFlow is a flow on echo's interim-producing speech model with the given turn settings.
func interruptionFlow(t *testing.T, r *flowRigE2E, name, turn string) string {
	t.Helper()
	extra := ""
	if turn != "" {
		extra = `,"turn":` + turn
	}
	cfg := `{"stt":{"provider":"echo","model":"latest_long"},"llm":{"provider":"echo","model":"m","system_prompt":"` + name + `"},"tts":{"provider":"echo","voice":"` + name + `"}` + extra + `}`
	f, err := r.s.flows.Create(t.Context(), flow.CreateRequest{Name: name, Config: json.RawMessage(cfg)})
	if err != nil || f.Readiness.State != flow.ReadinessReady {
		t.Fatalf("%s: %+v err %v", name, f.Readiness, err)
	}
	return f.ID
}

// callerBargesIn plays a long reply, then speaks over it with a recorded clip, and says whether the
// agent was cut off. The reply plays to its end first, so a late cut is caught as well.
func callerBargesIn(t *testing.T, r *flowRigE2E, flowID string) (interrupted bool) {
	t.Helper()
	c := r.dial(flowID)
	c.say(longReply)
	within(t, 20*time.Second, "the long reply to play", func() bool { return countOf(c.wireLog(), "audio") >= 10 })
	c.sendAudio(callerSpeech(t))
	within(t, 30*time.Second, "the reply to stop", func() bool {
		log := c.wireLog()
		return countOf(log, "msg:bot-interrupted") > 0 || countOf(log, "msg:bot-stopped-speaking") > 0
	})
	return countOf(c.wireLog(), "msg:bot-interrupted") > 0
}

// Measured with the server's own turn.min_words on this clip (callerSpeech) and echo's latest_long timing:
// the clip yields 7 interim words (an interrupt at 7 or fewer, none at 8 or more). The thresholds below
// keep clear of that edge: 1 and 5 interrupt, 10 does not.

func TestCallBehaviorAFlowThatMutesWhileTheAgentSpeaksIsNotInterrupted(t *testing.T) {
	r := interruptionRig(t)
	protected := interruptionFlow(t, r, "protected", `{"mute":["always"]}`)
	open := interruptionFlow(t, r, "open", ``)
	if !callerBargesIn(t, r, open) {
		t.Fatal("a flow with no mute was not interrupted by the caller's speech")
	}
	if callerBargesIn(t, r, protected) {
		t.Fatal("a flow that mutes while the agent speaks was interrupted")
	}
}

func TestCallBehaviorAFlowsMinWordsIgnoresAShortBackchannel(t *testing.T) {
	r := interruptionRig(t, func(c *Config) { c.Turn.MinWords = 5 }) // the server alone would let the clip through
	strict := interruptionFlow(t, r, "strict", `{"min_words":10}`)   // more words than the clip has
	eager := interruptionFlow(t, r, "eager", `{"min_words":1}`)
	if callerBargesIn(t, r, strict) {
		t.Fatal("a flow needing 10 words was interrupted by a 7-word clip")
	}
	if !callerBargesIn(t, r, eager) {
		t.Fatal("a flow needing 1 word was not interrupted")
	}
}
