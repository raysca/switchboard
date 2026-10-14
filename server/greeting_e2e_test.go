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

func TestAGreetingThatCannotBeQueuedIsCountedAndNeverFailsTheCall(t *testing.T) {
	var m Metrics
	queueGreeting(func(string) error { return errors.New("session closing") }, "hi", &m.GreetingFailedTotal, nil, "s1")
	if m.GreetingFailedTotal.Load() != 1 {
		t.Fatalf("counter %d", m.GreetingFailedTotal.Load())
	}
	var spoken atomic.Value
	queueGreeting(func(s string) error { spoken.Store(s); return nil }, "hi", &m.GreetingFailedTotal, nil, "s1")
	if spoken.Load() != "hi" || m.GreetingFailedTotal.Load() != 1 {
		t.Fatalf("spoken %v counter %d", spoken.Load(), m.GreetingFailedTotal.Load())
	}
}

func TestTheCallerCanInterruptTheGreeting(t *testing.T) {
	long := "Welcome to Smile Dental. We are open every weekday from eight until six. Our hygienists can see you today. " +
		"If this is an emergency, please say so now. Otherwise tell me what brings you in. We also take walk-ins on Saturdays."
	r := newFlowRigE2E(t, newProbe(), func(c *Config) { c.Providers.Echo.Audible = true })
	c := r.dial(greetingFlow(t, r, "BARGE", long))
	time.Sleep(150 * time.Millisecond) // the first sentence is playing
	c.say("actually I have a question")
	c.waitReplies("[BARGE", 1) // the caller's words get a normal reply
	eventually(t, "the greeting recorded as interrupted", func() bool {
		turns, err := r.s.store.Sessions().Turns(t.Context(), c.sessionID)
		return err == nil && len(turns) >= 2 && turns[0].AssistantText != "" && turns[0].Interrupted && len(turns[0].AssistantText) < len(long)
	})
}
