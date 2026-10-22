package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"govoice/core"
	"govoice/server/agent"
)

const salesGreeting = "Sales here, what are you looking for?"

// routedGreetingConfig is the front/sales echo flow with a route each way and a greeting on either step.
func routedGreetingConfig(frontGreeting, salesGreeting, salesVoice string) string {
	g := func(s string) string {
		if s == "" {
			return ""
		}
		b, _ := json.Marshal(s)
		return `,"greeting":` + string(b)
	}
	st := func(extra string) string { return `"provider":"echo"` + extra }
	return `{"start":"front","steps":[` +
		`{"id":"front","name":"Front desk","config":{"stt":{` + st(`,"model":"s"`) + `},"llm":{` + st(`,"model":"m","system_prompt":"FRONT"`) + `},"tts":{` + st(`,"voice":"frontvoice"`) + `}` + g(frontGreeting) + `}},` +
		`{"id":"sales","name":"Sales","config":{"llm":{` + st(`,"model":"m","system_prompt":"SALES"`) + `},"tts":{` + st(`,"voice":"`+salesVoice+`"`) + `}` + g(salesGreeting) + `}}],` +
		`"routes":[{"id":"e1","from":"front","to":"sales","condition":"The caller wants to buy."},{"id":"e2","from":"sales","to":"front","condition":"The caller wants support."}]}`
}

func routedGreetingAgent(t *testing.T, r *agentRigE2E, frontGreeting, salesGreeting, salesVoice string) string {
	t.Helper()
	f, err := r.s.agents.Create(t.Context(), agent.CreateRequest{Name: "Routed greeting", Config: json.RawMessage(routedGreetingConfig(frontGreeting, salesGreeting, salesVoice))})
	if err != nil || f.Readiness.State != agent.ReadinessReady {
		t.Fatalf("flow: %+v err %v", f.Readiness, err)
	}
	r.agentID = f.ID
	return f.ID
}

func TestAStepsGreetingIsSpokenWordForWordInItsVoiceAndReplacesTheModelsGreeting(t *testing.T) {
	probe := newProbe()
	r := newAgentRigE2E(t, probe)
	c := r.dial(routedGreetingAgent(t, r, "", salesGreeting, "salesvoice"))
	c.say("please transfer to sales")
	c.waitReplies("Connecting you.", 1)
	c.waitReplies(salesGreeting, 1)
	// The text reaches the client as the units are queued; the voice stage counts them a moment later.
	eventually(t, "the greeting in the new step's voice", func() bool { return probe.spoke("salesvoice") > 0 })
	time.Sleep(800 * time.Millisecond)
	if n := c.replies("[SALES"); n != 0 {
		t.Fatalf("the model greeted as well (%d replies)", n)
	}
	c.say("what do you sell")
	c.waitReplies("[SALES", 1) // after the greeting the model answers the caller as usual
	eventually(t, "the greeting as its own turn", func() bool {
		turns, _ := r.s.store.Sessions().Turns(t.Context(), c.sessionID)
		for _, tr := range turns {
			if tr.AssistantText == salesGreeting && tr.Transcript == "" {
				return true
			}
		}
		return false
	})
}

func TestAReturnToTheStartStepDoesNotReplayTheOpeningGreeting(t *testing.T) {
	r := newAgentRigE2E(t, newProbe())
	c := r.dial(routedGreetingAgent(t, r, greetingText, salesGreeting, "salesvoice"))
	c.waitReplies(greetingText, 1) // the opening
	c.say("please transfer to sales")
	c.waitReplies(salesGreeting, 1)
	c.say("transfer to front")
	c.waitReplies("[FRONT", 1) // the start step gets the model's transfer greeting
	time.Sleep(500 * time.Millisecond)
	if n := c.replies(greetingText); n != 1 {
		t.Fatalf("the opening greeting was spoken %d times", n)
	}
}

func TestAGreetingOnAStepThatCannotBeBuiltIsNeverSpokenAndTheCallGoesOn(t *testing.T) {
	probe := newProbe()
	probe.failTTS = "brokenvoice"
	r := newAgentRigE2E(t, probe)
	c := r.dial(routedGreetingAgent(t, r, "", salesGreeting, "brokenvoice"))
	c.say("please transfer to sales")
	c.waitReplies("Connecting you.", 1)
	time.Sleep(1200 * time.Millisecond)
	if n := c.replies(salesGreeting); n != 0 {
		t.Fatal("a greeting was spoken for a step that failed to build")
	}
	c.say("hello")
	c.waitReplies("[FRONT", 1) // the caller stayed with the front step
}

func TestADraftCallPlaysTheDraftsRouteGreeting(t *testing.T) {
	r := newAgentRigE2E(t, newProbe())
	f, err := r.s.agents.Create(t.Context(), agent.CreateRequest{Name: "Draft routed", Config: json.RawMessage(routedGreetingConfig("", "", "salesvoice"))})
	if err != nil {
		t.Fatal(err)
	}
	r.agentID = f.ID
	code, token, body := issueDraftToken(r, f.ID, routedGreetingConfig("", salesGreeting, "salesvoice"))
	if code != http.StatusCreated {
		t.Fatalf("%d %s", code, body)
	}
	c := r.dialToken(token)
	c.say("please transfer to sales")
	c.waitReplies(salesGreeting, 1)
}

// greetStep is the route greeting's decision, tested without a call.
type greetLog struct {
	messages []string
	spoken   []string
}

func (g *greetLog) add(m core.Message) error { g.messages = append(g.messages, m.Content); return nil }
func (g *greetLog) speak(s string)           { g.spoken = append(g.spoken, s) }

func TestGreetStepKeepsTheNoteBeforeAFixedGreetingAndNeverShowsItToTheCaller(t *testing.T) {
	cf := &CallAgent{Start: "front"}
	cf.setNote("Ana needs a quote for a car")
	var l greetLog
	if err := cf.greetStep("sales", "Sales", salesGreeting, l.add, l.speak); err != nil {
		t.Fatal(err)
	}
	if len(l.messages) != 1 || !strings.Contains(l.messages[0], "Ana needs a quote") || strings.Contains(l.messages[0], "Greet them briefly") {
		t.Fatalf("history messages %q", l.messages)
	}
	if len(l.spoken) != 1 || l.spoken[0] != salesGreeting {
		t.Fatalf("spoken %q", l.spoken)
	}
}

func TestGreetStepWithoutANoteAddsNothingToTheHistoryBeforeAFixedGreeting(t *testing.T) {
	cf := &CallAgent{Start: "front"}
	var l greetLog
	if err := cf.greetStep("sales", "Sales", salesGreeting, l.add, l.speak); err != nil {
		t.Fatal(err)
	}
	if len(l.messages) != 0 || len(l.spoken) != 1 {
		t.Fatalf("messages %q spoken %q", l.messages, l.spoken)
	}
}

func TestGreetStepFallsBackToTheModelsGreetingWithoutAGreetingOrForTheStartStep(t *testing.T) {
	for name, tc := range map[string]struct{ step, greeting string }{
		"no greeting":     {"sales", ""},
		"the start step":  {"front", "Opening line."},
		"blank greeting?": {"sales", ""},
	} {
		cf := &CallAgent{Start: "front"}
		cf.setNote("a note")
		var l greetLog
		if err := cf.greetStep(tc.step, "Sales", tc.greeting, l.add, l.speak); err != nil {
			t.Fatal(err)
		}
		if len(l.spoken) != 0 || len(l.messages) != 1 || !strings.Contains(l.messages[0], "Greet them briefly") || !strings.Contains(l.messages[0], "a note") {
			t.Errorf("%s: messages %q spoken %q", name, l.messages, l.spoken)
		}
	}
}
