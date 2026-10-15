package adapter

import (
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestLoadConfigDefaults(t *testing.T) {
	c, err := LoadConfig(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8099" || c.SpeachesURL != "http://127.0.0.1:8000" || c.MaxConcurrency != 2 ||
		c.InterimEvery != 700*time.Millisecond || c.EndpointSilence != 600*time.Millisecond ||
		c.MaxUtterance != 25*time.Second || c.EnergyThreshold != 0.01 {
		t.Fatalf("defaults: %+v", c)
	}
	if c.Voice("") != "af_heart" || c.Voice("eve") != "af_heart" || c.Voice("af_bella") != "af_bella" {
		t.Fatalf("voice resolution: %q %q %q", c.Voice(""), c.Voice("eve"), c.Voice("af_bella"))
	}
}

func TestLoadConfigOverridesAndAliases(t *testing.T) {
	c, err := LoadConfig(env(map[string]string{
		"OFFLINE_LISTEN": ":9000", "OFFLINE_TOKEN": "s3", "OFFLINE_MAX_CONCURRENCY": "4",
		"OFFLINE_INTERIM_EVERY": "1s", "OFFLINE_VOICE_ALIASES": "eve=af_bella, ara=am_adam",
		"OFFLINE_ENERGY_THRESHOLD": "0.02",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":9000" || c.Token != "s3" || c.MaxConcurrency != 4 || c.InterimEvery != time.Second ||
		c.EnergyThreshold != 0.02 || c.Voice("eve") != "af_bella" || c.Voice("ara") != "am_adam" {
		t.Fatalf("overrides: %+v", c)
	}
}

func TestLoadConfigRejectsBadValues(t *testing.T) {
	for k, v := range map[string]string{
		"OFFLINE_INTERIM_EVERY": "soon", "OFFLINE_MAX_CONCURRENCY": "0", "OFFLINE_ENERGY_THRESHOLD": "-1",
		"OFFLINE_VOICE_ALIASES": "noequals",
	} {
		if _, err := LoadConfig(env(map[string]string{k: v})); err == nil {
			t.Errorf("%s=%q accepted", k, v)
		}
	}
}
