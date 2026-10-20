package flow

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakePolicy is an available-everything catalogue that also knows the server's sound and silence rules.
type fakePolicy struct {
	fakeAvail
	sounds   []string
	allow    bool
	minTimer time.Duration
	cues     []string
	manager  bool
	maxMute  time.Duration
	smart    bool
}

func (p *fakePolicy) Backgrounds() []string         { return p.sounds }
func (p *fakePolicy) AllowFlowBackground() bool     { return p.allow }
func (p *fakePolicy) MinUserTimeout() time.Duration { return p.minTimer }
func (p *fakePolicy) Cues() []string                { return p.cues }
func (p *fakePolicy) TurnManager() bool             { return p.manager }
func (p *fakePolicy) MaxMute() time.Duration        { return p.maxMute }
func (p *fakePolicy) SmartTurn() bool               { return p.smart }

func withExtra(extra string) json.RawMessage {
	return json.RawMessage(`{"stt":{"provider":"google","model":"s"},"llm":{"provider":"google","model":"l"},"tts":{"provider":"google","voice":"t"},` + extra + `}`)
}

func TestPublishRefusesFlowSettingsTheServerDoesNotAllow(t *testing.T) {
	p := &fakePolicy{sounds: []string{"cafe", "office"}, allow: true, minTimer: 2 * time.Second}
	svc := testServiceWith(t, p)
	cases := []struct{ name, extra, want string }{
		{"unknown sound", `"audio":{"background":{"name":"nope"}}`, `"nope"`},
		{"unknown sound lists the available ones", `"audio":{"background":{"name":"nope"}}`, "available: cafe, office"},
		{"timeout under the dtmf floor", `"idle":{"user_timeout_ms":2000}`, "idle.user_timeout_ms must exceed 2s"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := svc.Create(context.Background(), CreateRequest{Name: c.name, Config: withExtra(c.extra)})
			if !errors.Is(err, ErrInvalidConfig) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an invalid-config error with %q, got %v", c.want, err)
			}
		})
	}
	ok := []string{`"audio":{"background":{"name":"office"}}`, `"idle":{"user_timeout_ms":2001}`, `"idle":{"user_timeout_ms":0}`, `"audio":{"output_gain_db":-3}`}
	for i, extra := range ok {
		if _, err := svc.Create(context.Background(), CreateRequest{Name: "ok" + string(rune('a'+i)), Config: withExtra(extra)}); err != nil {
			t.Fatalf("%s: %v", extra, err)
		}
	}
}

func TestACeilingOffRefusesAnyFlowBackgroundButNotOtherAudioSettings(t *testing.T) {
	p := &fakePolicy{sounds: []string{"office"}, allow: false}
	svc := testServiceWith(t, p)
	_, err := svc.Create(context.Background(), CreateRequest{Name: "bg", Config: withExtra(`"audio":{"background":{"name":"office"}}`)})
	if !errors.Is(err, ErrInvalidConfig) || !strings.Contains(err.Error(), "audio.allow_flow_background") {
		t.Fatalf("got %v", err)
	}
	if _, err := svc.Create(context.Background(), CreateRequest{Name: "gain", Config: withExtra(`"audio":{"output_gain_db":-3,"denoise":{}}`)}); err != nil {
		t.Fatalf("gain and denoise do not need the ceiling: %v", err)
	}
}

func TestADraftIsCheckedLikeAPublishAndWithoutAPolicyNothingExtraIsChecked(t *testing.T) {
	p := &fakePolicy{sounds: []string{"office"}, allow: true}
	svc := testServiceWith(t, p)
	f, err := svc.Create(context.Background(), CreateRequest{Name: "base", Config: validConfig()})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.ResolveDraft(context.Background(), f.ID, withExtra(`"audio":{"background":{"name":"nope"}}`), nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("draft with an unknown sound: %v", err)
	}
	plain := testServiceWith(t, ready()) // an Availability with no policy: the rules do not apply
	if _, err := plain.Create(context.Background(), CreateRequest{Name: "any", Config: withExtra(`"audio":{"background":{"name":"whatever"}}`)}); err != nil {
		t.Fatalf("no policy: %v", err)
	}
}

func TestASoundTheServerNoLongerOffersDoesNotTakeTheFlowOffline(t *testing.T) {
	p := &fakePolicy{sounds: []string{"office"}, allow: true}
	svc := testServiceWith(t, p)
	f, err := svc.Create(context.Background(), CreateRequest{Name: "bg", Config: withExtra(`"audio":{"background":{"name":"office"}}`)})
	if err != nil || f.Readiness.State != ReadinessReady {
		t.Fatalf("created: %+v %v", f.Readiness, err)
	}
	for name, mutate := range map[string]func(){"sound removed": func() { p.sounds = nil }, "ceiling off": func() { p.sounds, p.allow = []string{"office"}, false }} {
		mutate()
		got, err := svc.Get(context.Background(), f.ID)
		if err != nil || got.Readiness.State != ReadinessReady {
			t.Fatalf("%s: the call plays without the sound, so the flow stays ready: %+v %v", name, got.Readiness, err)
		}
	}
}

func TestAnExplicitNoBackgroundNeedsNeitherACatalogueEntryNorTheCeiling(t *testing.T) {
	svc := testServiceWith(t, &fakePolicy{allow: false})
	if _, err := svc.Create(context.Background(), CreateRequest{Name: "none", Config: withExtra(`"audio":{"background":{"name":""}}`)}); err != nil {
		t.Fatal(err)
	}
}

func TestAWaitUnderTheDTMFFloorTakesTheFlowOfflineWhenTheFloorRisesLater(t *testing.T) {
	p := &fakePolicy{allow: true}
	svc := testServiceWith(t, p)
	f, err := svc.Create(context.Background(), CreateRequest{Name: "wait", Config: withExtra(`"idle":{"user_timeout_ms":3000}`)})
	if err != nil {
		t.Fatal(err)
	}
	p.minTimer = 5 * time.Second
	got, err := svc.Get(context.Background(), f.ID)
	if err != nil || got.Readiness.State != ReadinessNotReady || !contains(got.Readiness.Reasons, reasonIdleTooShort) {
		t.Fatalf("%+v %v", got.Readiness, err)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestPublishRefusesARouteSoundTheServerDoesNotHave(t *testing.T) {
	routed := func(cue string) json.RawMessage {
		return json.RawMessage(strings.Replace(twoStepCfg(""), `"routes":`, `"route_cue":`+cue+`,"routes":`, 1))
	}
	for _, c := range []struct {
		name string
		cues []string
		cue  string
		want string // "" = accepted
	}{
		{"unknown lists the available ones", []string{"chime"}, `{"name":"chimes"}`, `route_cue.name "chimes" is not a sound on this server (available: chime)`},
		{"no cues at all", nil, `{"name":"chime","after_ms":50}`, `route_cue.name "chime" is not a sound on this server (this server has no cues)`},
		{"known", []string{"chime"}, `{"name":"chime"}`, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			svc := testServiceWith(t, &fakePolicy{cues: c.cues})
			a, err := svc.Create(context.Background(), CreateRequest{Name: c.name, Config: routed(c.cue)})
			if c.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if !errors.Is(err, ErrInvalidConfig) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v (%+v)", c.want, err, a)
			}
		})
	}
	// Readiness stays green for a stored flow whose sound the server dropped: the call runs without it (K6).
	g, err := Decode(routed(`{"name":"gone"}`))
	if err != nil {
		t.Fatal(err)
	}
	if reasons, _ := g.Problems(&fakePolicy{cues: []string{"chime"}}); len(reasons) != 0 {
		t.Fatalf("a dropped sound must not take the flow offline: %v", reasons)
	}
}

func TestPublishRefusesTurnSettingsTheServerCannotHonour(t *testing.T) {
	cases := []struct {
		name  string
		p     *fakePolicy
		extra string
		want  string // "" = accepted
	}{
		{"manager off", &fakePolicy{maxMute: 30 * time.Second}, `"turn":{"min_words":2}`, "turn settings need turn.manager: true on this server"},
		{"manager off, mute only", &fakePolicy{maxMute: 30 * time.Second}, `"turn":{"mute":["always"]}`, "turn.manager"},
		{"manager on", &fakePolicy{manager: true, maxMute: 30 * time.Second}, `"turn":{"min_words":2,"mute":["always"]}`, ""},
		{"max_mute above the server's", &fakePolicy{manager: true, maxMute: 30 * time.Second}, `"turn":{"max_mute_ms":40000}`, "turn.max_mute_ms 40000 is above this server's limit of 30000"},
		{"max_mute one above", &fakePolicy{manager: true, maxMute: 30 * time.Second}, `"turn":{"max_mute_ms":30001}`, "turn.max_mute_ms"},
		{"max_mute equal", &fakePolicy{manager: true, maxMute: 30 * time.Second}, `"turn":{"max_mute_ms":30000}`, ""},
		{"max_mute below", &fakePolicy{manager: true, maxMute: 30 * time.Second}, `"turn":{"max_mute_ms":20000}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc := testServiceWith(t, c.p)
			_, err := svc.Create(context.Background(), CreateRequest{Name: c.name, Config: withExtra(c.extra)})
			if c.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if !errors.Is(err, ErrInvalidConfig) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
	// Readiness stays green for a stored flow when the server changes later (K6): the call is still built.
	p := &fakePolicy{manager: true, maxMute: 30 * time.Second}
	svc := testServiceWith(t, p)
	f, err := svc.Create(context.Background(), CreateRequest{Name: "later", Config: withExtra(`"turn":{"min_words":2,"max_mute_ms":25000}`)})
	if err != nil {
		t.Fatal(err)
	}
	p.manager, p.maxMute = false, 10*time.Second
	got, err := svc.Get(context.Background(), f.ID)
	if err != nil || got.Readiness.State != ReadinessReady {
		t.Fatalf("turn problems are publish-only: %+v %v", got.Readiness, err)
	}
}

func TestSmartEndOfTurnNeedsTheModel(t *testing.T) {
	const want = "Smart end-of-turn needs the Smart Turn model on this server (turn.analyzer.for_flows)"
	smart := withExtra(`"turn":{"end_of_turn":"smart","smart_hold_ms":1500}`)
	svc := testServiceWith(t, &fakePolicy{manager: true, maxMute: 30 * time.Second})
	if _, err := svc.Create(context.Background(), CreateRequest{Name: "no model", Config: smart}); !errors.Is(err, ErrInvalidConfig) || !strings.Contains(err.Error(), want) {
		t.Fatalf("want a refusal naming for_flows, got %v", err)
	}
	for _, extra := range []string{`"turn":{"end_of_turn":"eager"}`, `"turn":{"end_of_turn":"default","speech_timeout_ms":900}`} {
		if _, err := svc.Create(context.Background(), CreateRequest{Name: extra, Config: withExtra(extra)}); err != nil {
			t.Fatalf("%s needs no model: %v", extra, err)
		}
	}
	p := &fakePolicy{manager: true, maxMute: 30 * time.Second, smart: true}
	svc = testServiceWith(t, p)
	f, err := svc.Create(context.Background(), CreateRequest{Name: "model", Config: smart})
	if err != nil || f.Readiness.State != ReadinessReady {
		t.Fatalf("with the model: %+v %v", f.Readiness, err)
	}
	// A stored smart flow on a server that lost the model is not ready: its calls could not be built.
	p.smart = false
	got, err := svc.Get(context.Background(), f.ID)
	if err != nil || got.Readiness.State != ReadinessNotReady || !contains(got.Readiness.Reasons, reasonSmartTurnUnavailable) {
		t.Fatalf("want not_ready/%s, got %+v %v", reasonSmartTurnUnavailable, got.Readiness, err)
	}
	// turn.manager off is a publish-only refusal: readiness stays green (push-to-talk calls still work).
	p.smart, p.manager = true, false
	got, err = svc.Get(context.Background(), f.ID)
	if err != nil || got.Readiness.State != ReadinessReady {
		t.Fatalf("manager off must not change readiness: %+v %v", got.Readiness, err)
	}
}
