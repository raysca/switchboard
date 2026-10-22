package server

import (
	"encoding/json"
	"testing"
	"time"

	"govoice/server/flow"
)

// silenceFlow is a text-only flow: silence for both speech stages, the echo model for the reply.
func silenceFlow(t *testing.T, r *flowRigE2E, name, extra string) string {
	t.Helper()
	cfg := `{"stt":{"provider":"silence","model":"text","language":"en-US"},"llm":{"provider":"echo","model":"m","system_prompt":"` + name +
		`"},"tts":{"provider":"silence","voice":"none"}` + extra + `}`
	f, err := r.s.flows.Create(t.Context(), flow.CreateRequest{Name: name, Config: json.RawMessage(cfg)})
	if err != nil || f.Readiness.State != flow.ReadinessReady {
		t.Fatalf("%s: %+v err %v", name, f.Readiness, err)
	}
	return f.ID
}

func TestSilenceFlowAnswersATypedTurnInText(t *testing.T) {
	r := newFlowRigE2E(t, newProbe())
	c := r.dial(silenceFlow(t, r, "TXT", ""))
	c.say("hello there")
	c.waitReplies("hello there", 1) // the model's text reached the client
	eventually(t, "the typed turn is stored", func() bool {
		turns, _ := r.s.store.Sessions().Turns(t.Context(), c.sessionID)
		return len(turns) == 1 && turns[0].Transcript == "hello there" && turns[0].AssistantText != ""
	})
	if c.ended() {
		t.Fatal("the call ended after a normal turn")
	}
}

func TestSilenceFlowIsNotHungUpOnForTypingSlowly(t *testing.T) {
	r := newFlowRigE2E(t, newProbe(), serverIdle(time.Second, "server prompt"))
	c := r.dial(silenceFlow(t, r, "QUIET", ""))
	time.Sleep(2500 * time.Millisecond) // more than twice the server's idle wait, with nobody typing
	if c.ended() || r.s.metrics.UserIdlePromptsTotal.Load() != 0 || r.s.metrics.UserIdleHangupsTotal.Load() != 0 {
		t.Fatalf("a silence flow was nudged: ended=%v prompts=%d hangups=%d", c.ended(), r.s.metrics.UserIdlePromptsTotal.Load(), r.s.metrics.UserIdleHangupsTotal.Load())
	}
}

func TestSilenceFlowWithExplicitIdleSettingsStillHangsUp(t *testing.T) {
	r := newFlowRigE2E(t, newProbe(), serverIdle(time.Minute, "server prompt"))
	id := silenceFlow(t, r, "EXPLICIT", `,"idle":{"user_timeout_ms":1000,"max_prompts":1,"prompt":"Still there?","prompt_mode":"say","goodbye":"Goodbye."}`)
	c := r.dial(id)
	eventually(t, "the hang-up the flow asked for", c.ended)
	eventually(t, "the hang-up is counted", func() bool { return r.s.metrics.UserIdleHangupsTotal.Load() == 1 })
}

// silenceRoutedFlow is the Dealership shape with a text-only start step: the front step (the only one that
// can name speech-to-text) listens with silence, and each step's voice is given as its tts stage JSON.
func silenceRoutedFlow(t *testing.T, r *flowRigE2E, frontTTS, salesTTS, frontExtra string) string {
	t.Helper()
	cfg := `{"start":"front","steps":[` +
		`{"id":"front","name":"Front desk","config":{"stt":{"provider":"silence","model":"text","language":"en-US"},"llm":{"provider":"echo","model":"m","system_prompt":"FRONT"},"tts":` + frontTTS + frontExtra + `}},` +
		`{"id":"sales","name":"Sales","config":{"llm":{"provider":"echo","model":"m","system_prompt":"SALES"},"tts":` + salesTTS + `}}],` +
		`"routes":[{"id":"e1","from":"front","to":"sales","condition":"The caller wants to buy."}]}`
	f, err := r.s.flows.Create(t.Context(), flow.CreateRequest{Name: "Text dealership", Config: json.RawMessage(cfg)})
	if err != nil || f.Readiness.State != flow.ReadinessReady {
		t.Fatalf("routed silence flow: %+v err %v", f.Readiness, err)
	}
	r.flowID = f.ID
	return f.ID
}

const (
	ttsSilence   = `{"provider":"silence","voice":"none"}`
	ttsEchoFront = `{"provider":"echo","voice":"frontvoice"}`
	ttsEchoSales = `{"provider":"echo","voice":"salesvoice"}`
)

// A routed flow keeps working in text when the steps differ in whether they make audio: the speech stages are
// rebuilt at each route, and the call-level (start step) speech-to-text stays silence throughout.
func TestSilenceRoutedFlowMovesBetweenStepsInText(t *testing.T) {
	for name, tts := range map[string][2]string{
		"silence on both steps":          {ttsSilence, ttsSilence},
		"silent front, audio sales":      {ttsSilence, ttsEchoSales},
		"audio front, silent sales step": {ttsEchoFront, ttsSilence},
	} {
		t.Run(name, func(t *testing.T) {
			r := newFlowRigE2E(t, newProbe())
			c := r.dial(silenceRoutedFlow(t, r, tts[0], tts[1], ""))
			eventually(t, "session", func() bool { return r.activeSessions() == 1 })

			c.say("hello")
			c.waitReplies("[FRONT", 1)

			c.say("please transfer to sales")
			c.waitReplies("Connecting you.", 1)
			c.waitReplies("[SALES", 1) // the new step greets once the call is idle

			front := c.replies("[FRONT")
			c.say("what do you sell")
			c.waitReplies("[SALES", 2)
			if c.replies("[FRONT") != front {
				t.Fatal("the front step answered after the route")
			}
			if !r.has(EventRoute, "front->sales") || !r.has(EventStepActive, "sales") || !r.has(EventStepClosed, "front") {
				t.Fatalf("timeline = %+v", r.timeline())
			}
			if r.s.metrics.route.total.Load() != 1 || r.s.metrics.route.failures.Load() != 0 {
				t.Fatalf("metrics: routes=%d failures=%d", r.s.metrics.route.total.Load(), r.s.metrics.route.failures.Load())
			}
			if c.ended() {
				t.Fatal("the call ended")
			}
		})
	}
}

// The idle default belongs to the call (the start step's speech-to-text), so it survives the route: a caller
// who is reading the second step's answer is not nudged or hung up on.
func TestSilenceRoutedFlowIsNotNudgedBeforeOrAfterTheRoute(t *testing.T) {
	r := newFlowRigE2E(t, newProbe(), serverIdle(time.Second, "server prompt"))
	c := r.dial(silenceRoutedFlow(t, r, ttsSilence, ttsSilence, ""))
	c.say("please transfer to sales")
	c.waitReplies("[SALES", 1)
	time.Sleep(2500 * time.Millisecond) // more than twice the server's idle wait, after the route
	if c.ended() || r.s.metrics.UserIdlePromptsTotal.Load() != 0 || r.s.metrics.UserIdleHangupsTotal.Load() != 0 {
		t.Fatalf("a text-only routed call was nudged: ended=%v prompts=%d hangups=%d", c.ended(), r.s.metrics.UserIdlePromptsTotal.Load(), r.s.metrics.UserIdleHangupsTotal.Load())
	}
}

// Idle settings are call-level, so they sit on the start step; set there they apply to a text-only routed flow.
func TestSilenceRoutedFlowHonoursIdleSettingsOnItsStartStep(t *testing.T) {
	r := newFlowRigE2E(t, newProbe(), serverIdle(time.Minute, "server prompt"))
	c := r.dial(silenceRoutedFlow(t, r, ttsSilence, ttsSilence, `,"idle":{"user_timeout_ms":1000,"max_prompts":1,"prompt":"Still there?","prompt_mode":"say","goodbye":"Goodbye."}`))
	eventually(t, "the hang-up the start step asked for", c.ended)
	eventually(t, "the hang-up is counted", func() bool { return r.s.metrics.UserIdleHangupsTotal.Load() == 1 })
}
