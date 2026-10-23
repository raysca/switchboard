package server

import (
	"encoding/json"
	"testing"

	"govoice/server/agent"
)

func TestRecordingConfigDefaultsOffAndValidates(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	if cfg.Recording.Enabled {
		t.Fatal("recording must default off")
	}
	cfg.Recording.Enabled = true
	if err := ValidateConfig(cfg); err == nil {
		t.Fatal("enabled without dir must fail")
	}
	cfg.Recording.Dir = t.TempDir()
	cfg.Recording.RetentionDays = 0
	if err := ValidateConfig(cfg); err == nil {
		t.Fatal("enabled without retention must fail")
	}
	cfg.Recording.RetentionDays = cfg.Retention.ContentRetentionDays + 1
	cfg.Retention.Enabled = true
	if err := ValidateConfig(cfg); err == nil {
		t.Fatal("recordings must not outlive content retention")
	}
	cfg.Recording.RetentionDays = cfg.Retention.ContentRetentionDays
	if err := ValidateConfig(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestAgentRecordingNeedsOptInAndDisclosure(t *testing.T) {
	dec := func(s string) agent.Config {
		c, err := agent.DecodeConfig(json.RawMessage(s))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	if dec(`{}`).Recording.Active() {
		t.Fatal("recording must default off")
	}
	c := dec(`{"stt":{"provider":"p"},"llm":{"provider":"p","model":"m"},"tts":{"provider":"p"},"recording":{"enabled":true}}`)
	if c.Recording.Active() {
		t.Fatal("no disclosure, must not record")
	}
	found := false
	for _, m := range c.Missing() {
		found = found || m == "recording_disclosure_unconfirmed"
	}
	if !found {
		t.Fatalf("missing=%v", c.Missing())
	}
	c = dec(`{"recording":{"enabled":true,"disclosure_confirmed":true}}`)
	if !c.Recording.Active() {
		t.Fatal("opted in with disclosure must record")
	}
}

func TestAttachRecordingIsNoopWhenDisabled(t *testing.T) {
	s := &Server{cfg: DefaultConfig(ProfileDevelopment)}
	if stop := s.attachRecording(CallSpec{Recording: true}, "a", 1, nil, nil); stop != nil {
		t.Fatal("server-level switch off must not record")
	}
}
