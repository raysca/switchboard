package envload

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadKeepsEnvironment(t *testing.T) {
	f := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(f, []byte("ENVLOAD_SET=file\nENVLOAD_UNSET=file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ENV_FILE", f)
	t.Setenv("ENVLOAD_SET", "env")
	t.Setenv("ENVLOAD_UNSET", "")
	os.Unsetenv("ENVLOAD_UNSET")
	if err := Load(); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("ENVLOAD_SET"); got != "env" {
		t.Errorf("ENVLOAD_SET = %q, want env", got)
	}
	if got := os.Getenv("ENVLOAD_UNSET"); got != "file" {
		t.Errorf("ENVLOAD_UNSET = %q, want file", got)
	}
}
