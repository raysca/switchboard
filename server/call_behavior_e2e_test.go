package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"govoice/server/agent"
	"govoice/server/storage"
)

// Per-agent quiet-caller and audio settings, end to end on the echo provider: a real server, real
// WebSocket calls, agents that set (or do not set) idle and audio. The echo TTS speaks silence, so what
// was said is read from the voice's synthesis units and audio energy is the background's.

func behaviorAgent(t *testing.T, r *agentRigE2E, name, voice, extra string) string {
	t.Helper()
	cfg := `{"stt":{"provider":"echo","model":"s"},"llm":{"provider":"echo","model":"m","system_prompt":"` + name + `"},"tts":{"provider":"echo","voice":"` + voice + `"}` + extra + `}`
	f, err := r.s.agents.Create(t.Context(), agent.CreateRequest{Name: name, Config: json.RawMessage(cfg)})
	if err != nil || f.Readiness.State != agent.ReadinessReady {
		t.Fatalf("%s: %+v err %v", name, f.Readiness, err)
	}
	return f.ID
}

func (r *agentRigE2E) spokenBy(voice, want string) bool {
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

func TestCallBehaviorAnAgentsQuietCallerSettingsPromptThenEndTheCall(t *testing.T) {
	r := newAgentRigE2E(t, newProbe(), serverIdle(time.Minute, "server prompt"))
	quiet := behaviorAgent(t, r, "Quiet", "quietvoice", `,"idle":{"user_timeout_ms":1000,"max_prompts":1,"prompt":"Still there for the test?","prompt_mode":"say","goodbye":"Goodbye for now."}`)
	plain := behaviorAgent(t, r, "Plain", "plainvoice", ``)
	c := r.dial(quiet)
	c2 := r.dial(plain)
	eventually(t, "the exact prompt", func() bool { return r.spokenBy("quietvoice", "Still there for the test?") })
	eventually(t, "the goodbye", func() bool { return r.spokenBy("quietvoice", "Goodbye for now.") })
	eventually(t, "the hang-up", c.ended)
	// The counter is added in the call's finish path, after the client already sees the disconnect.
	eventually(t, "the hang-up is counted", func() bool { return r.s.metrics.UserIdleHangupsTotal.Load() == 1 })
	if r.s.metrics.UserIdleHangupsTotal.Load() != 1 {
		t.Fatalf("hang-ups counted: %d", r.s.metrics.UserIdleHangupsTotal.Load())
	}
	if len(r.probe.sayings("plainvoice")) != 0 || c2.ended() {
		t.Fatalf("the agent without idle settings followed the server's one-minute wait, not the other agent's: %v", r.probe.sayings("plainvoice"))
	}
	if n := r.probe.spoke("quietvoice"); n != 2 {
		t.Fatalf("the model must not word the exact prompt: %d units", n)
	}
}

func TestCallBehaviorAnAgentCanSwitchQuietCallerHandlingOff(t *testing.T) {
	r := newAgentRigE2E(t, newProbe(), serverIdle(time.Second, "server prompt"))
	off := behaviorAgent(t, r, "Off", "offvoice", `,"idle":{"user_timeout_ms":0}`)
	c := r.dial(off)
	time.Sleep(2500 * time.Millisecond)
	if n := len(r.probe.sayings("offvoice")); n != 0 || c.ended() {
		t.Fatalf("handling is off for this agent but %d prompt(s) were spoken, ended=%v", n, c.ended())
	}
}

func TestCallBehaviorAnAgentWithoutIdleSettingsUsesTheServers(t *testing.T) {
	r := newAgentRigE2E(t, newProbe(), serverIdle(time.Second, "server prompt"))
	plain := behaviorAgent(t, r, "Plain", "plainvoice", ``)
	r.dial(plain)
	eventually(t, "the server's prompt", func() bool { return r.spokenBy("plainvoice", "server prompt") })
}

func TestCallBehaviorADraftCallUsesTheDraftsQuietCallerSettings(t *testing.T) {
	r := newAgentRigE2E(t, newProbe(), func(c *Config) { c.Idle.UserTimeout = 0 })
	id := behaviorAgent(t, r, "Published", "pubvoice", ``)
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
		t.Fatal("the published agent has no quiet-caller handling")
	}
}

func TestCallBehaviorAnAgentsBackgroundSoundIsHeardAtItsLevelAndAnotherAgentsIsNot(t *testing.T) {
	loud, soft := constWAV(t, "loud", 20000), constWAV(t, "soft", 20000)
	r := newAgentRigE2E(t, newProbe(), func(c *Config) {
		c.Idle.UserTimeout = 0
		c.Audio.Backgrounds = map[string]CueConfig{"loud": {File: loud, GainDB: ptrF(0)}, "soft": {File: soft, GainDB: ptrF(-30)}}
	})
	withLoud := r.dial(behaviorAgent(t, r, "A", "a", `,"audio":{"background":{"name":"loud"}}`))
	withSoftened := r.dial(behaviorAgent(t, r, "B", "b", `,"audio":{"background":{"name":"loud","gain_db":-20}}`))
	none := r.dial(behaviorAgent(t, r, "C", "c", ``))
	time.Sleep(1500 * time.Millisecond)
	if p := withLoud.maxPeak(); p < 15000 {
		t.Fatalf("the background should be heard at its own level in silence, peak %d", p)
	}
	if p := withSoftened.maxPeak(); p < 1500 || p > 3000 {
		t.Fatalf("an agent's gain_db of -20 should lower it to about 2000, peak %d", p)
	}
	if p := none.maxPeak(); p != 0 {
		t.Fatalf("an agent with no background must carry none, peak %d", p)
	}
}

func TestCallBehaviorTheServerDefaultBackgroundPlaysUnlessAnAgentChoosesAnother(t *testing.T) {
	loud, soft := constWAV(t, "loud", 20000), constWAV(t, "soft", 4000)
	r := newAgentRigE2E(t, newProbe(), func(c *Config) {
		c.Idle.UserTimeout = 0
		c.Audio.Backgrounds = map[string]CueConfig{"loud": {File: loud, GainDB: ptrF(0)}, "soft": {File: soft, GainDB: ptrF(0)}}
		c.Audio.BackgroundDefault = "soft"
	})
	def := r.dial(behaviorAgent(t, r, "D", "d", ``))
	own := r.dial(behaviorAgent(t, r, "E", "e", `,"audio":{"background":{"name":"loud"}}`))
	time.Sleep(1200 * time.Millisecond)
	if p := def.maxPeak(); p < 3000 || p > 5000 {
		t.Fatalf("default sound should play at its own level, peak %d", p)
	}
	if p := own.maxPeak(); p < 15000 {
		t.Fatalf("the agent's own sound should replace the default, peak %d", p)
	}
}

func TestCallBehaviorThePublishAPIRefusesASoundTheServerDoesNotOfferOrAllow(t *testing.T) {
	sound := constWAV(t, "office", 12000)
	post := func(r *agentRigE2E, name, extra string) (int, string) {
		body := `{"name":"` + name + `","config":{"stt":{"provider":"echo","model":"s"},"llm":{"provider":"echo","model":"m"},"tts":{"provider":"echo","voice":"v"},` + extra + `}}`
		w := apiRequest(r.s, http.MethodPost, "/api/v1/agents", e2eMgmtKey, body)
		return w.Code, w.Body.String()
	}
	allowed := newAgentRigE2E(t, newProbe(), func(c *Config) {
		c.Audio.Backgrounds = map[string]CueConfig{"office": {File: sound}, "cafe": {File: sound}}
		c.Audio.AllowAgentBackground = true
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
	denied := newAgentRigE2E(t, newProbe(), func(c *Config) {
		c.Audio.Backgrounds = map[string]CueConfig{"office": {File: sound}}
		c.Audio.AllowAgentBackground = false
	})
	code, body = post(denied, "Denied", `"audio":{"background":{"name":"office"}}`)
	if code != http.StatusBadRequest || !strings.Contains(body, "audio.allow_agent_background") {
		t.Fatalf("the ceiling off: %d %s", code, body)
	}
	if code, body := post(denied, "OtherAudio", `"audio":{"denoise":{"strength":0.8},"output_gain_db":-6}`); code != http.StatusCreated {
		t.Fatalf("noise reduction and volume need no ceiling: %d %s", code, body)
	}
}

// The handoff sound is one of the server's cues: publish says which, and the editor reads the list from /server-config.
func TestCallBehaviorThePublishAPIRefusesAnUnknownHandoffCue(t *testing.T) {
	cue := constWAV(t, "chime", 12000)
	r := newAgentRigE2E(t, newProbe(), func(c *Config) {
		c.Audio.Cues = map[string]CueConfig{"ding": {File: cue}, "chime": {File: cue}}
	})
	id := r.createHandoffAgent("salesvoice", `{"name":"chime","after_ms":50}`)
	w := apiRequest(r.s, http.MethodGet, "/api/v1/server-config", e2eMgmtKey, "")
	var cfg struct {
		Data struct {
			Audio struct {
				Cues []string `json:"cues"`
			} `json:"audio"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &cfg); err != nil || !slices.Equal(cfg.Data.Audio.Cues, []string{"chime", "ding"}) {
		t.Fatalf("server-config cues = %v (err %v): %s", cfg.Data.Audio.Cues, err, w.Body.String())
	}
	put := func(cue string) (int, string) {
		body := `{"config":` + string(agentConfigJSON("SALES", "salesvoice", cue)) + `}`
		w := apiRequest(r.s, http.MethodPut, "/api/v1/agents/"+id, e2eMgmtKey, body)
		return w.Code, w.Body.String()
	}
	code, body := put(`{"name":"chimes"}`)
	if code != http.StatusBadRequest || !strings.Contains(body, "invalid_request") || !strings.Contains(body, `handoff_cue.name \"chimes\" is not a sound on this server (available: chime, ding)`) {
		t.Fatalf("an unknown handoff sound: %d %s", code, body)
	}
	if code, body := put(`{"name":"ding"}`); code != http.StatusOK {
		t.Fatalf("a known handoff sound: %d %s", code, body)
	}
}

// failover is a call-wide setting: on a later step publish says invalid_request and names the step.
func TestCallBehaviorThePublishAPIRefusesCallWideSettingsOnALaterStep(t *testing.T) {
	r := newAgentRigE2E(t, newProbe())
	id := r.createHandoffAgent("salesvoice")
	cfg := string(agentConfigJSON("SALES", "salesvoice"))
	i := strings.Index(cfg, `"id":"sales"`)
	cfg = cfg[:i] + strings.Replace(cfg[i:], `"config":{`, `"config":{"failover":{"on":["timeout"]},`, 1)
	w := apiRequest(r.s, http.MethodPut, "/api/v1/agents/"+id, e2eMgmtKey, `{"config":`+cfg+`}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid_request") || !strings.Contains(w.Body.String(), `step \"sales\"`) {
		t.Fatalf("failover on a later step: %d %s", w.Code, w.Body.String())
	}
}

// The prompt and the goodbye are lines the assistant spoke: they are in the call's turns (as the greeting
// is), they are no caller turn, and they leave no reply-time sample.
func TestCallBehaviorIdleLinesAreInTheCallsTurns(t *testing.T) {
	r := newAgentRigE2E(t, newProbe(), serverIdle(time.Minute, "server prompt"))
	c := r.dial(behaviorAgent(t, r, "Quiet", "quietvoice", `,"idle":{"user_timeout_ms":1000,"max_prompts":1,"prompt":"Still there for the test?","prompt_mode":"say","goodbye":"Goodbye for now."}`))
	r.waitEnded(c.sessionID, storage.EndSilentCaller)
	var turns []storage.TurnRecord
	eventually(t, "the prompt and the goodbye as stored turns", func() bool {
		var err error
		turns, err = r.s.store.Sessions().Turns(context.Background(), c.sessionID)
		return err == nil && len(turns) == 2
	})
	if turns[0].AssistantText != "Still there for the test?" || turns[1].AssistantText != "Goodbye for now." {
		t.Fatalf("stored turns: %+v", turns)
	}
	for _, turn := range turns {
		if turn.Transcript != "" || turn.Interrupted {
			t.Fatalf("an idle line is an assistant turn, not a caller turn: %+v", turn)
		}
		if strings.Contains(turn.LatenciesJSON, "llm_ttft_ms") || strings.Contains(turn.LatenciesJSON, "voice_to_voice_ms") {
			t.Fatalf("an idle line is no model reply and must not count as a latency sample: %s", turn.LatenciesJSON)
		}
	}
	w := apiRequest(r.s, http.MethodGet, "/api/v1/sessions/"+c.sessionID, e2eMgmtKey, "")
	var out struct {
		Data struct {
			TurnCount     int  `json:"turn_count"`
			MedianReplyMS *int `json:"median_reply_ms"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.Data.MedianReplyMS != nil || out.Data.TurnCount != 2 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

// An agent can ask for the Smart Turn model only on a server that loaded it: the echo rig has none, so publish
// says why (and names the key that loads it).
func TestCallBehaviorPublishRefusesSmartEndOfTurnWithoutTheModel(t *testing.T) {
	r := newAgentRigE2E(t, newProbe())
	body := func(turn string) string {
		return `{"name":"smart","config":{"stt":{"provider":"echo","model":"m"},"llm":{"provider":"echo","model":"m"},"tts":{"provider":"echo","voice":"v"},"turn":` + turn + `}}`
	}
	w := apiRequest(r.s, http.MethodPost, "/api/v1/agents", e2eMgmtKey, body(`{"end_of_turn":"smart"}`))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "turn.analyzer.for_agents") {
		t.Fatalf("smart without the model: %d %s", w.Code, w.Body.String())
	}
	if w := apiRequest(r.s, http.MethodPost, "/api/v1/agents", e2eMgmtKey, body(`{"end_of_turn":"eager","speech_timeout_ms":900}`)); w.Code != http.StatusCreated {
		t.Fatalf("eager needs no model: %d %s", w.Code, w.Body.String())
	}
}

// With the model loaded (for_agents) a smart agent is ready, and a call on it connects and replies. It does not
// prove the smart chain runs (a typed turn bypasses the detector): TestTurnForCall and the build test do.
func TestCallBehaviorASmartAgentConnectsAndRepliesWhenTheModelIsLoaded(t *testing.T) {
	r := newAgentRigE2E(t, newProbe(), func(c *Config) { c.Turn.Analyzer.ForAgents = true })
	r.s.SetTurnBackend(smartBackend{p: 0.9})
	f, err := r.s.agents.Create(t.Context(), agent.CreateRequest{Name: "smart", Config: json.RawMessage(
		`{"stt":{"provider":"echo","model":"m"},"llm":{"provider":"echo","model":"m"},"tts":{"provider":"echo","voice":"v"},"turn":{"end_of_turn":"smart","smart_hold_ms":1500}}`)})
	if err != nil || f.Readiness.State != agent.ReadinessReady {
		t.Fatalf("a smart agent on a server with the model: %+v err %v", f.Readiness, err)
	}
	c := r.dial(f.ID)
	c.say("hello")
	eventually(t, "a reply", func() bool { return c.replyCount() > 0 })
}

// An agent can shorten the call limit, never lengthen it: the publish API refuses a limit above the server's.
func TestCallBehaviorThePublishAPIRefusesALongerCallLimit(t *testing.T) {
	r := newAgentRigE2E(t, newProbe(), func(c *Config) { c.Limits.MaxCallDuration = 10 * time.Minute })
	post := func(name string, seconds int) (int, string) {
		body := fmt.Sprintf(`{"name":%q,"config":{"stt":{"provider":"echo","model":"s"},"llm":{"provider":"echo","model":"m"},"tts":{"provider":"echo","voice":"v"},"max_call_duration_seconds":%d}}`, name, seconds)
		w := apiRequest(r.s, http.MethodPost, "/api/v1/agents", e2eMgmtKey, body)
		return w.Code, w.Body.String()
	}
	code, body := post("TooLong", 601)
	if code != http.StatusBadRequest || !strings.Contains(body, "invalid_request") || !strings.Contains(body, "601") || !strings.Contains(body, "600") {
		t.Fatalf("a limit above the server's: %d %s", code, body)
	}
	if code, body := post("Shorter", 120); code != http.StatusCreated {
		t.Fatalf("a shorter limit: %d %s", code, body)
	}
	if code, body := post("Equal", 600); code != http.StatusCreated {
		t.Fatalf("the server's own limit: %d %s", code, body)
	}
}

// The server lowered its limit after the agent was published: the call still ends at the server's limit,
// the agent's longer one does not extend it.
func TestCallBehaviorAnAgentLimitLongerThanTheServersDoesNotExtendIt(t *testing.T) {
	r := newAgentRigE2E(t, newProbe(), serverIdle(0, ""))
	id := behaviorAgent(t, r, "Plain", "plainvoice", `,"max_call_duration_seconds":1500`)
	r.s.cfg.Limits.MaxCallDuration = time.Second
	c := r.dial(id)
	r.waitEnded(c.sessionID, storage.EndTimeLimit)
}
