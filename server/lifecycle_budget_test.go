package server

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestShutdownBudgetAddsFiveSeconds(t *testing.T) {
	if got := ShutdownBudget(45 * time.Second); got != 50*time.Second {
		t.Fatalf("budget = %s, want 50s", got)
	}
}

func TestComposeGraceExceedsShutdownBudget(t *testing.T) {
	t.Setenv("TOKEN_SECRET", "contract-test-secret-not-a-real-token-value")
	cfg, err := LoadConfig("../config/production.yaml")
	if err != nil {
		t.Fatal(err)
	}
	budget := ShutdownBudget(cfg.Limits.DrainTimeout)
	compose, err := os.ReadFile("../deploy/docker-compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(compose), "stop_grace_period: 60s") {
		t.Fatalf("compose grace must be 60s, which is longer than budget %s", budget)
	}
	unit, err := os.ReadFile("../deploy/govoice.service")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(unit), "TimeoutStopSec=60s") {
		t.Fatal("reference unit TimeoutStopSec must be 60s")
	}
}
