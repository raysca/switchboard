package server

import (
	"os"
	"path/filepath"
	"testing"
)

// The development profile's local agent store is named after the product: <dir>/switchboard.db.
func TestDevelopmentStoreIsNamedSwitchboard(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GOVOICE_DEV_DIR", dir)
	t.Setenv("GOVOICE_DATABASE_DSN", "")
	t.Setenv("GOVOICE_MANAGEMENT_API_KEY", "management-test-key-32-bytes-long-and-separate")
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Token.Secret = "voice-test-secret-32-bytes-long-and-separate"
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.EnableManagement(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.closeManagement)
	if _, err := os.Stat(filepath.Join(dir, "switchboard.db")); err != nil {
		t.Fatalf("no switchboard.db in the dev directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "govoice.db")); err == nil {
		t.Fatal("the old govoice.db name is still being created")
	}
}
