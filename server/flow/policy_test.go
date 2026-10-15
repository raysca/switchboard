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
}

func (p *fakePolicy) Backgrounds() []string         { return p.sounds }
func (p *fakePolicy) AllowFlowBackground() bool     { return p.allow }
func (p *fakePolicy) MinUserTimeout() time.Duration { return p.minTimer }

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
