package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"govoice/server/agent"
)

const greetingText = "Welcome to Smile Dental, how can I help?"

// greetingConfig is the flat echo agent with a greeting.
func greetingConfig(prompt, greeting string) string {
	g, _ := json.Marshal(greeting)
	return `{"stt":{"provider":"echo","model":"s"},"llm":{"provider":"echo","model":"m","system_prompt":"` + prompt + `"},"tts":{"provider":"echo","voice":"v"},"greeting":` + string(g) + `}`
}

func greetingAgent(t *testing.T, r *agentRigE2E, prompt, greeting string) string {
	t.Helper()
	f, err := r.s.agents.Create(t.Context(), agent.CreateRequest{Name: "Greeter " + prompt, Config: json.RawMessage(greetingConfig(prompt, greeting))})
	if err != nil || f.Readiness.State != agent.ReadinessReady {
		t.Fatalf("agent: %+v err %v", f.Readiness, err)
	}
	r.agentID = f.ID
	return f.ID
}

// spoken counts the bot messages heard so far.
func (c *e2eCall) spoken() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.texts)
}

func TestTheStartStepsGreetingIsSpokenFirstAndRecordedAsTurnOne(t *testing.T) {
	r := newAgentRigE2E(t, newProbe())
	c := r.dial(greetingAgent(t, r, "GREET", greetingText))
	c.waitReplies(greetingText, 1) // nothing was typed: the agent spoke first
	eventually(t, "the greeting's turn row", func() bool {
		turns, err := r.s.store.Sessions().Turns(t.Context(), c.sessionID)
		return err == nil && len(turns) == 1 && turns[0].AssistantText == greetingText && turns[0].Transcript == "" && turns[0].Ordinal == 1
	})
	if n := c.replies("[GREET"); n != 0 { // the model is not asked to greet
		t.Fatalf("the model spoke before the caller did")
	}
	for _, ev := range r.timeline() { // a greeting is not a caller turn or a model reply in the timeline
		if ev.Type == EventSTTResult || ev.Type == EventLLMFirstToken || ev.Type == EventLLMComplete {
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

func TestAnAgentWithoutAGreetingStaysSilentUntilTheCallerSpeaks(t *testing.T) {
	r := newAgentRigE2E(t, newProbe())
	f, err := r.s.agents.Create(t.Context(), agent.CreateRequest{Name: "Quiet", Config: json.RawMessage(echoAgentConfig("QUIET"))})
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

func TestADraftCallPlaysTheDraftsGreetingAndThePublishedAgentStaysSilent(t *testing.T) {
	r := newAgentRigE2E(t, newProbe())
	f, err := r.s.agents.Create(t.Context(), agent.CreateRequest{Name: "Draft greeting", Config: json.RawMessage(echoAgentConfig("P"))})
	if err != nil {
		t.Fatal(err)
	}
	r.agentID = f.ID
	code, token, body := issueDraftToken(r, f.ID, greetingConfig("P", greetingText))
	if code != http.StatusCreated {
		t.Fatalf("%d %s", code, body)
	}
	c := r.dialToken(token)
	c.waitReplies(greetingText, 1)
	c2 := r.dial(f.ID) // the published agent has no greeting
	time.Sleep(1200 * time.Millisecond)
	if n := c2.spoken(); n != 0 {
		t.Fatalf("the published agent spoke first")
	}
}

func TestTheGreetingReleasesTheUntilFirstBotCompleteMute(t *testing.T) {
	r := newAgentRigE2E(t, newProbe(), func(c *Config) { c.Turn.Mute = []string{"until_first_bot_complete"} })
	c := r.dial(greetingAgent(t, r, "MUTE", greetingText))
	c.waitReplies(greetingText, 1)
	time.Sleep(500 * time.Millisecond)
	c.say("hello")
	c.waitReplies("[MUTE", 1) // the caller is heard once the greeting is done
}
