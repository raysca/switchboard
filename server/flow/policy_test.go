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

func TestReadinessReportsASoundTheServerNoLongerOffers(t *testing.T) {
	p := &fakePolicy{sounds: []string{"office"}, allow: true}
	svc := testServiceWith(t, p)
	f, err := svc.Create(context.Background(), CreateRequest{Name: "bg", Config: withExtra(`"audio":{"background":{"name":"office"}}`)})
	if err != nil || f.Readiness.State != ReadinessReady {
		t.Fatalf("created: %+v %v", f.Readiness, err)
	}
	p.sounds = nil
	got, err := svc.Get(context.Background(), f.ID)
	if err != nil || got.Readiness.State != ReadinessNotReady || !contains(got.Readiness.Reasons, "missing_audio_background") {
		t.Fatalf("sound removed: %+v %v", got.Readiness, err)
	}
	p.sounds, p.allow = []string{"office"}, false
	got, err = svc.Get(context.Background(), f.ID)
	if err != nil || !contains(got.Readiness.Reasons, "audio_background_not_allowed") {
		t.Fatalf("ceiling off: %+v %v", got.Readiness, err)
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
