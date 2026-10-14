package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"govoice/server/flow"
)

const greetingText = "Welcome to Smile Dental, how can I help?"

// greetingConfig is the flat echo flow with a greeting.
func greetingConfig(prompt, greeting string) string {
	g, _ := json.Marshal(greeting)
	return `{"stt":{"provider":"echo","model":"s"},"llm":{"provider":"echo","model":"m","system_prompt":"` + prompt + `"},"tts":{"provider":"echo","voice":"v"},"greeting":` + string(g) + `}`
}

func greetingFlow(t *testing.T, r *flowRigE2E, prompt, greeting string) string {
	t.Helper()
	f, err := r.s.flows.Create(t.Context(), flow.CreateRequest{Name: "Greeter " + prompt, Config: json.RawMessage(greetingConfig(prompt, greeting))})
	if err != nil || f.Readiness.State != flow.ReadinessReady {
		t.Fatalf("flow: %+v err %v", f.Readiness, err)
	}
	r.flowID = f.ID
	return f.ID
}

// spoken counts the bot messages heard so far.
func (c *e2eCall) spoken() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.texts)
}

func TestTheStartStepsGreetingIsSpokenFirstAndRecordedAsTurnOne(t *testing.T) {
	r := newFlowRigE2E(t, newProbe())
	c := r.dial(greetingFlow(t, r, "GREET", greetingText))
	c.waitReplies(greetingText, 1) // nothing was typed: the agent spoke first
	eventually(t, "the greeting's turn row", func() bool {
		turns, err := r.s.store.Sessions().Turns(t.Context(), c.sessionID)
		return err == nil && len(turns) == 1 && turns[0].AssistantText == greetingText && turns[0].Transcript == "" && turns[0].Ordinal == 1
	})
	if n := c.replies("[GREET"); n != 0 { // the model is not asked to greet
		t.Fatalf("the model spoke before the caller did")
	}
	for _, ev := range r.timeline() { // a greeting is not a caller turn or a model reply in the timeline
		if ev.Type == EventSTTResult || ev.Type == EventLLMFirstToken {
			t.Fatalf("the greeting recorded a %s event", ev.Type)
		}
	}
	c.say("hello")
	c.waitReplies("[GREET", 1) // the model answers the caller as usual
	eventually(t, "the caller's turn after the greeting", func() bool {
		turns, _ := r.s.store.Sessions().Turns(t.Context(), c.sessionID)
		return len(turns) == 2 && turns[1].Transcript == "hello" && turns[1].Ordinal == 2
	})
	if n := c.replies(greetingText); n != 1 {
		t.Fatalf("the greeting was spoken %d times", n)
	}
}

func TestAFlowWithoutAGreetingStaysSilentUntilTheCallerSpeaks(t *testing.T) {
	r := newFlowRigE2E(t, newProbe())
	f, err := r.s.flows.Create(t.Context(), flow.CreateRequest{Name: "Quiet", Config: json.RawMessage(echoFlowConfig("QUIET"))})
	if err != nil {
		t.Fatal(err)
	}
	c := r.dial(f.ID)
	time.Sleep(1500 * time.Millisecond)
	if n := c.spoken(); n != 0 {
		t.Fatalf("the agent spoke first without a greeting")
	}
	turns, _ := r.s.store.Sessions().Turns(t.Context(), c.sessionID)
	if len(turns) != 0 {
		t.Fatalf("turn rows without a greeting: %+v", turns)
	}
}

func TestADraftCallPlaysTheDraftsGreetingAndThePublishedFlowStaysSilent(t *testing.T) {
	r := newFlowRigE2E(t, newProbe())
	f, err := r.s.flows.Create(t.Context(), flow.CreateRequest{Name: "Draft greeting", Config: json.RawMessage(echoFlowConfig("P"))})
	if err != nil {
		t.Fatal(err)
	}
	r.flowID = f.ID
	code, token, body := issueDraftToken(r, f.ID, greetingConfig("P", greetingText))
	if code != http.StatusCreated {
		t.Fatalf("%d %s", code, body)
	}
	c := r.dialToken(token)
	c.waitReplies(greetingText, 1)
	c2 := r.dial(f.ID) // the published flow has no greeting
	time.Sleep(1200 * time.Millisecond)
	if n := c2.spoken(); n != 0 {
		t.Fatalf("the published flow spoke first")
	}
}

func TestTheGreetingReleasesTheUntilFirstBotCompleteMute(t *testing.T) {
	r := newFlowRigE2E(t, newProbe(), func(c *Config) { c.Turn.Mute = []string{"until_first_bot_complete"} })
	c := r.dial(greetingFlow(t, r, "MUTE", greetingText))
	c.waitReplies(greetingText, 1)
	time.Sleep(500 * time.Millisecond)
	c.say("hello")
	c.waitReplies("[MUTE", 1) // the caller is heard once the greeting is done
}

func TestAGreetingThatCannotBeQueuedIsReportedAndNeverFailsTheCall(t *testing.T) {
	var failed []error
	queueGreeting(func(string) error { return errors.New("session closing") }, "hi", func(err error) { failed = append(failed, err) })
	if len(failed) != 1 {
		t.Fatalf("failures %v", failed)
	}
	var spoken atomic.Value
	queueGreeting(func(s string) error { spoken.Store(s); return nil }, "hi", func(err error) { failed = append(failed, err) })
	if spoken.Load() != "hi" || len(failed) != 1 {
		t.Fatalf("spoken %v failures %v", spoken.Load(), failed)
	}
	queueGreeting(func(string) error { return errors.New("x") }, "hi", nil) // no handler: still no panic
}
