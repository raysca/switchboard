package server

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func loadYAML(t *testing.T, body string) (*Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return LoadConfig(p)
}

// Every default documented in docs/ (operations_runbook, quickstart, config
// comments) must come out of LoadConfig for a minimal YAML.
func TestLoadConfig_DocumentedDefaults(t *testing.T) {
	c, err := loadYAML(t, "profile: development\n")
	if err != nil {
		t.Fatal(err)
	}
	for name, ok := range map[string]bool{
		"public addr":           c.PublicListenAddr == "127.0.0.1:8080",
		"admin addr":            c.AdminListenAddr == "127.0.0.1:9090",
		"storage sqlite":        c.Storage.Dialect == "sqlite",
		"token lifetime 5m":     c.Token.MaxLifetime == 5*time.Minute,
		"token skew 30s":        c.Token.ClockSkew == 30*time.Second,
		"token key id":          c.Token.KeyID == "v1",
		"max_sessions 100":      c.Limits.MaxSessions == 100,
		"per principal 5":       c.Limits.MaxSessionsPerPrincipal == 5,
		"handshakes 50":         c.Limits.MaxPendingHandshakes == 50,
		"idle_timeout 60s":      c.Limits.IdleTimeout == 60*time.Second,
		"drain 30s":             c.Limits.DrainTimeout == 30*time.Second,
		"max call 30m":          c.Limits.MaxCallDuration == 30*time.Minute,
		"vad workers 4":         c.VAD.Workers == 4 && c.VAD.Batch == 8 && c.VAD.QueueCap == 8192,
		"stt replay 10s":        c.STT.ReplayBuffer == 10*time.Second,
		"idle max_prompts 2":    c.Idle.MaxPrompts == 2,
		"idle bot_max_stuck 3":  c.Idle.BotMaxStuck == 3,
		"dtmf timeout 2s":       c.DTMF.AggregateTimeout == 2*time.Second,
		"denoise 0.5":           c.Audio.DenoiseStrength == 0.5,
		"background unset":      c.Audio.Background.GainDB == nil && c.Audio.Background.Loop == nil,
		"analyzer defaults":     c.Turn.Analyzer.Workers == 1 && c.Turn.Analyzer.StopSecs == 3*time.Second && c.Turn.Analyzer.Budget == 150*time.Millisecond,
		"analyzer mode derived": c.TurnAnalyzerMode() == "inprocess",
		"recording 7d/1h/256M":  c.Recording.RetentionDays == 7 && c.Recording.MaxDuration == time.Hour && c.Recording.MaxBytes == 256<<20,
		"warmup shared":         c.Warmup.shared() && !c.Warmup.Predial && !c.Warmup.OnStart && c.Warmup.WarmTimeout() == 3*time.Second && c.Warmup.SpareTTL == 10*time.Second,
		"otel defaults":         c.OTel.QueueSize == 2048 && c.OTel.ShutdownTimeout == 5*time.Second && c.OTel.SamplingRatio == nil,
		"timeline 500/10m":      c.Observability.TimelineMaxEvents == 500 && c.Observability.TimelineTTL == 10*time.Minute,
		"retention off":         !c.Retention.Enabled && c.Retention.ContentRetentionDays == 7 && c.Retention.MetadataRetentionDays == 30,
		"turn manager on":       c.Turn.Manager && c.VAD.StopMS == 200,
	} {
		if !ok {
			t.Errorf("default not applied: %s", name)
		}
	}
}

// The confirmed failure modes: a minimal feature block must load on defaults.
func TestLoadConfig_MinimalFeatureBlocks(t *testing.T) {
	for name, y := range map[string]string{
		"idle user_timeout only": "idle: {user_timeout: 15s}",
		"recording dir only":     "recording: {enabled: true, dir: rec}",
		"smart turn":             "turn: {manager: true, stop: [turn-analyzer, speech-timeout]}",
	} {
		_, err := loadYAML(t, "profile: development\n"+y+"\n")
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	c, err := loadYAML(t, "profile: development\nlimits: {max_sessions: 7}\nidle: {user_timeout: 15s}\n")
	if err != nil || c.Limits.MaxSessions != 7 || c.Limits.MaxSessionsPerPrincipal != 5 || c.Idle.MaxPrompts != 2 {
		t.Fatalf("partial override must merge over defaults: %+v %v", c.Limits, err)
	}
}

// Production must not silently inherit the dev "*" origin or a sqlite store.
func TestLoadConfig_ProductionKeepsRequiredFieldsRequired(t *testing.T) {
	t.Setenv("TOKEN_SECRET", "production-test-secret-not-the-dev-key-123456")
	base := "profile: production\ntls: {trusted_proxy_mode: true, trusted_proxies: [\"127.0.0.1/32\"]}\n"
	if _, err := loadYAML(t, base); err == nil || !strings.Contains(err.Error(), "storage.dialect") {
		t.Fatalf("want storage.dialect required, got %v", err)
	}
	c, err := loadYAML(t, base+"storage: {dialect: postgres}\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.OriginAllowlist) != 0 || c.VAD.Mode != "process" || !c.Observability.JSONFormat {
		t.Fatalf("production defaults wrong: %+v %s", c.OriginAllowlist, c.VAD.Mode)
	}
}

// -profile production over a development YAML must get production defaults
// and production validation, not the dev "*"/sqlite/inprocess defaults.
func TestLoadConfigProfile_OverrideUsesProductionDefaults(t *testing.T) {
	t.Setenv("TOKEN_SECRET", "production-test-secret-not-the-dev-key-123456")
	p := filepath.Join(t.TempDir(), "c.yaml")
	body := "profile: development\ntls: {trusted_proxy_mode: true, trusted_proxies: [\"127.0.0.1/32\"]}\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfigProfile(p, ProfileProduction); err == nil || !strings.Contains(err.Error(), "storage.dialect") {
		t.Fatalf("want storage.dialect required, got %v", err)
	}
	if err := os.WriteFile(p, []byte(body+"storage: {dialect: postgres}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfigProfile(p, ProfileProduction)
	if err != nil || c.Profile != ProfileProduction || len(c.OriginAllowlist) != 0 || c.VAD.Mode != "process" {
		t.Fatalf("override not applied: %v %+v", err, c)
	}
	if _, err := LoadConfigProfile("../config/development.yaml", ProfileProduction); err == nil {
		t.Fatal("shipped development.yaml must not validate as production")
	}
}

func TestLoadConfig_TurnStopValidatedAtLoad(t *testing.T) {
	for stop, want := range map[string]string{
		"[eager]":                         "must include speech-timeout",
		"[external, speech-timeout]":      "not a server setting",
		"[bogus, speech-timeout]":         "unknown strategy",
		"[eager, speech-timeout]":         "",
		"[turn-analyzer, speech-timeout]": "",
	} {
		_, err := loadYAML(t, "profile: development\nturn: {manager: true, stop: "+stop+"}\n")
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("stop %s: err=%v want %q", stop, err, want)
		}
	}
}

// Each commented example block at the end of config/development.yaml, once
// uncommented, must load and validate.
func TestDevelopmentYAMLExamplesLoad(t *testing.T) {
	raw, err := os.ReadFile("../config/development.yaml")
	if err != nil {
		t.Fatal(err)
	}
	start := regexp.MustCompile(`^# [a-z_]+:`)
	var blocks []string
	for _, l := range strings.Split(string(raw), "\n") {
		switch {
		case start.MatchString(l):
			blocks = append(blocks, l[2:])
		case len(blocks) > 0 && strings.HasPrefix(l, "#  "):
			blocks[len(blocks)-1] += "\n" + l[2:]
		}
	}
	if len(blocks) < 10 {
		t.Fatalf("found only %d example blocks", len(blocks))
	}
	for _, b := range blocks {
		if _, err := loadYAML(t, "profile: development\n"+b+"\n"); err != nil {
			t.Errorf("example does not load:\n%s\n%v", b, err)
		}
	}
}

func TestDefaultsAreTurnManagerOnWith200msVADStop(t *testing.T) {
	for _, p := range []Profile{ProfileDevelopment, ProfileProduction} {
		c := DefaultConfig(p)
		if !c.Turn.Manager || c.VAD.StopMS != 200 {
			t.Fatalf("%s: manager=%t stop_ms=%d, want true and 200", p, c.Turn.Manager, c.VAD.StopMS)
		}
	}
}

// An existing YAML that turns the manager off still loads with the new defaults.
func TestManagerOffStillLoads(t *testing.T) {
	c := DefaultConfig(ProfileDevelopment)
	c.Turn.Manager = false
	if err := validateTurn(c); err != nil {
		t.Fatalf("turn.manager: false with defaults rejected: %v", err)
	}
	if !lowVADStopWithoutManager(c) {
		t.Fatal("200ms VAD stop with the manager off must warn")
	}
	c.VAD.StopMS = 800
	if lowVADStopWithoutManager(c) {
		t.Fatal("800ms VAD stop with the manager off must not warn")
	}
}

func TestMCPAllowLocalHTTPIsForDevelopmentOnly(t *testing.T) {
	t.Setenv("TOKEN_SECRET", "production-test-secret-not-the-dev-key-123456")
	base := "profile: production\ntls: {trusted_proxy_mode: true, trusted_proxies: [\"127.0.0.1/32\"]}\nstorage: {dialect: postgres}\n"
	if _, err := loadYAML(t, base+"mcp: {allow_local_http: true}\n"); err == nil || !strings.Contains(err.Error(), "mcp.allow_local_http is for development and tests") {
		t.Fatalf("production must refuse mcp.allow_local_http, got %v", err)
	}
	c, err := loadYAML(t, "profile: development\nmcp: {allow_local_http: true}\n")
	if err != nil || !c.MCP.AllowLocalHTTP {
		t.Fatalf("development: err=%v cfg=%+v", err, c.MCP)
	}
	if c.MCP.Timeout != 15*time.Second {
		t.Fatalf("mcp.timeout default = %v, want 15s", c.MCP.Timeout)
	}
}
