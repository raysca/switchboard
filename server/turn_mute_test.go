package server

import (
	"strings"
	"testing"
)

func TestTurnMuteConfigValidation(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Storage.Dialect = "sqlite"
	cfg.Turn.Manager = false // on by default since the turn-latency change
	cfg.Turn.Mute = []string{"function_call"}
	if err := ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), "turn.manager") {
		t.Fatalf("turn.mute without turn.manager: %v", err)
	}
	cfg.Turn.Manager = true
	if err := ValidateConfig(cfg); err != nil && strings.Contains(err.Error(), "turn") {
		t.Fatal(err)
	}
	cfg.Turn.Mute = []string{"nope"}
	if err := ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), "turn.mute") {
		t.Fatalf("unknown strategy accepted: %v", err)
	}
	cfg.Turn.Mute, cfg.Turn.MaxMute = nil, -1
	if err := ValidateConfig(cfg); err == nil {
		t.Fatal("negative max_mute accepted")
	}
}
