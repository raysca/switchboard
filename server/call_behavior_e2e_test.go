package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"govoice/server/flow"
)

// Per-flow quiet-caller and audio settings, end to end on the echo provider: a real server, real
// WebSocket calls, flows that set (or do not set) idle and audio. The echo TTS speaks silence, so what
// was said is read from the voice's synthesis units and audio energy is the background's.

func behaviorFlow(t *testing.T, r *flowRigE2E, name, voice, extra string) string {
	t.Helper()
	cfg := `{"stt":{"provider":"echo","model":"s"},"llm":{"provider":"echo","model":"m","system_prompt":"` + name + `"},"tts":{"provider":"echo","voice":"` + voice + `"}` + extra + `}`
	f, err := r.s.flows.Create(t.Context(), flow.CreateRequest{Name: name, Config: json.RawMessage(cfg)})
	if err != nil || f.Readiness.State != flow.ReadinessReady {
		t.Fatalf("%s: %+v err %v", name, f.Readiness, err)
	}
	return f.ID
}

func (r *flowRigE2E) spokenBy(voice, want string) bool {
	for _, s := range r.probe.sayings(voice) {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}

func (c *e2eCall) ended() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

func (c *e2eCall) maxPeak() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := 0
	for _, a := range c.audio {
		m = max(m, a.peak)
	}
	return m
}

func serverIdle(timeout time.Duration, prompt string) func(*Config) {
	return func(c *Config) {
		c.Idle.UserTimeout, c.Idle.MaxPrompts, c.Idle.PromptMode, c.Idle.Prompt = timeout, 1, "say", prompt
	}
}

func TestCallBehaviorAFlowsQuietCallerSettingsPromptThenEndTheCall(t *testing.T) {
	r := newFlowRigE2E(t, newProbe(), serverIdle(time.Minute, "server prompt"))
	quiet := behaviorFlow(t, r, "Quiet", "quietvoice", `,"idle":{"user_timeout_ms":1000,"max_prompts":1,"prompt":"Still there for the test?","prompt_mode":"say","goodbye":"Goodbye for now."}`)
	plain := behaviorFlow(t, r, "Plain", "plainvoice", ``)
	c := r.dial(quiet)
	c2 := r.dial(plain)
	eventually(t, "the exact prompt", func() bool { return r.spokenBy("quietvoice", "Still there for the test?") })
	eventually(t, "the goodbye", func() bool { return r.spokenBy("quietvoice", "Goodbye for now.") })
	eventually(t, "the hang-up", c.ended)
	if r.s.metrics.UserIdleHangupsTotal.Load() != 1 {
		t.Fatalf("hang-ups counted: %d", r.s.metrics.UserIdleHangupsTotal.Load())
	}
	if len(r.probe.sayings("plainvoice")) != 0 || c2.ended() {
		t.Fatalf("the flow without idle settings followed the server's one-minute wait, not the other flow's: %v", r.probe.sayings("plainvoice"))
	}
	if n := r.probe.spoke("quietvoice"); n != 2 {
		t.Fatalf("the model must not word the exact prompt: %d units", n)
	}
}

func TestCallBehaviorAFlowCanSwitchQuietCallerHandlingOff(t *testing.T) {
	r := newFlowRigE2E(t, newProbe(), serverIdle(time.Second, "server prompt"))
	off := behaviorFlow(t, r, "Off", "offvoice", `,"idle":{"user_timeout_ms":0}`)
	c := r.dial(off)
	time.Sleep(2500 * time.Millisecond)
	if n := len(r.probe.sayings("offvoice")); n != 0 || c.ended() {
		t.Fatalf("handling is off for this flow but %d prompt(s) were spoken, ended=%v", n, c.ended())
	}
}

func TestCallBehaviorAFlowWithoutIdleSettingsUsesTheServers(t *testing.T) {
	r := newFlowRigE2E(t, newProbe(), serverIdle(time.Second, "server prompt"))
	plain := behaviorFlow(t, r, "Plain", "plainvoice", ``)
	r.dial(plain)
	eventually(t, "the server's prompt", func() bool { return r.spokenBy("plainvoice", "server prompt") })
}

func TestCallBehaviorADraftCallUsesTheDraftsQuietCallerSettings(t *testing.T) {
	r := newFlowRigE2E(t, newProbe(), func(c *Config) { c.Idle.UserTimeout = 0 })
	id := behaviorFlow(t, r, "Published", "pubvoice", ``)
	draft := `{"stt":{"provider":"echo","model":"s"},"llm":{"provider":"echo","model":"m","system_prompt":"D"},"tts":{"provider":"echo","voice":"draftvoice"},` +
		`"idle":{"user_timeout_ms":1000,"max_prompts":1,"prompt":"Draft prompt.","prompt_mode":"say"}}`
	code, token, body := issueDraftToken(r, id, draft)
	if code != 201 {
		t.Fatalf("%d %s", code, body)
	}
	r.dialToken(token)
	eventually(t, "the draft's prompt", func() bool { return r.spokenBy("draftvoice", "Draft prompt.") })
	r.dial(id)
	time.Sleep(1500 * time.Millisecond)
	if len(r.probe.sayings("pubvoice")) != 0 {
		t.Fatal("the published flow has no quiet-caller handling")
	}
}

func TestCallBehaviorAFlowsBackgroundSoundIsHeardAtItsLevelAndAnotherFlowsIsNot(t *testing.T) {
	loud, soft := constWAV(t, "loud", 20000), constWAV(t, "soft", 20000)
	r := newFlowRigE2E(t, newProbe(), func(c *Config) {
		c.Idle.UserTimeout = 0
		c.Audio.Backgrounds = map[string]CueConfig{"loud": {File: loud, GainDB: ptrF(0)}, "soft": {File: soft, GainDB: ptrF(-30)}}
	})
	withLoud := r.dial(behaviorFlow(t, r, "A", "a", `,"audio":{"background":{"name":"loud"}}`))
	withSoftened := r.dial(behaviorFlow(t, r, "B", "b", `,"audio":{"background":{"name":"loud","gain_db":-20}}`))
	none := r.dial(behaviorFlow(t, r, "C", "c", ``))
	time.Sleep(1500 * time.Millisecond)
	if p := withLoud.maxPeak(); p < 15000 {
		t.Fatalf("the background should be heard at its own level in silence, peak %d", p)
	}
	if p := withSoftened.maxPeak(); p < 1500 || p > 3000 {
		t.Fatalf("a flow's gain_db of -20 should lower it to about 2000, peak %d", p)
	}
	if p := none.maxPeak(); p != 0 {
		t.Fatalf("a flow with no background must carry none, peak %d", p)
	}
}

func TestCallBehaviorTheServerDefaultBackgroundPlaysUnlessAFlowChoosesAnother(t *testing.T) {
	loud, soft := constWAV(t, "loud", 20000), constWAV(t, "soft", 4000)
	r := newFlowRigE2E(t, newProbe(), func(c *Config) {
		c.Idle.UserTimeout = 0
		c.Audio.Backgrounds = map[string]CueConfig{"loud": {File: loud, GainDB: ptrF(0)}, "soft": {File: soft, GainDB: ptrF(0)}}
		c.Audio.BackgroundDefault = "soft"
	})
	def := r.dial(behaviorFlow(t, r, "D", "d", ``))
	own := r.dial(behaviorFlow(t, r, "E", "e", `,"audio":{"background":{"name":"loud"}}`))
	time.Sleep(1200 * time.Millisecond)
	if p := def.maxPeak(); p < 3000 || p > 5000 {
		t.Fatalf("default sound should play at its own level, peak %d", p)
	}
	if p := own.maxPeak(); p < 15000 {
		t.Fatalf("the flow's own sound should replace the default, peak %d", p)
	}
}

func TestCallBehaviorThePublishAPIRefusesASoundTheServerDoesNotOfferOrAllow(t *testing.T) {
	sound := constWAV(t, "office", 12000)
	post := func(r *flowRigE2E, name, extra string) (int, string) {
		body := `{"name":"` + name + `","config":{"stt":{"provider":"echo","model":"s"},"llm":{"provider":"echo","model":"m"},"tts":{"provider":"echo","voice":"v"},` + extra + `}}`
		w := apiRequest(r.s, http.MethodPost, "/api/v1/flows", e2eMgmtKey, body)
		return w.Code, w.Body.String()
	}
	allowed := newFlowRigE2E(t, newProbe(), func(c *Config) {
		c.Audio.Backgrounds = map[string]CueConfig{"office": {File: sound}, "cafe": {File: sound}}
		c.Audio.AllowFlowBackground = true
	})
	code, body := post(allowed, "Unknown", `"audio":{"background":{"name":"nope"}}`)
	if code != http.StatusBadRequest || !strings.Contains(body, "invalid_request") || !strings.Contains(body, "available: cafe, office") {
		t.Fatalf("an unknown sound: %d %s", code, body)
	}
	if code, body := post(allowed, "Known", `"audio":{"background":{"name":"office"},"output_gain_db":-3}`); code != http.StatusCreated {
		t.Fatalf("a known sound: %d %s", code, body)
	}
	if code, body := post(allowed, "NoneAtAll", `"audio":{"background":{"name":""}}`); code != http.StatusCreated {
		t.Fatalf("an explicit none: %d %s", code, body)
	}
	denied := newFlowRigE2E(t, newProbe(), func(c *Config) {
		c.Audio.Backgrounds = map[string]CueConfig{"office": {File: sound}}
		c.Audio.AllowFlowBackground = false
	})
	code, body = post(denied, "Denied", `"audio":{"background":{"name":"office"}}`)
	if code != http.StatusBadRequest || !strings.Contains(body, "audio.allow_flow_background") {
		t.Fatalf("the ceiling off: %d %s", code, body)
	}
	if code, body := post(denied, "OtherAudio", `"audio":{"denoise":{"strength":0.8},"output_gain_db":-6}`); code != http.StatusCreated {
		t.Fatalf("noise reduction and volume need no ceiling: %d %s", code, body)
	}
}
