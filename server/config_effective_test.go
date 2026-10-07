package server

import (
	"encoding/json"
	"os"
	"testing"
)

// The shipped YAMLs must keep their effective values. Regenerate the goldens
// with UPDATE_GOLDEN=1 only for an intentional change.
func TestConfig_ShippedYAMLEffectiveGolden(t *testing.T) {
	t.Setenv("TOKEN_SECRET", "production-test-secret-not-the-dev-key-123456")
	for _, name := range []string{"development", "production"} {
		cfg, err := LoadConfig("../config/" + name + ".yaml")
		if err != nil {
			t.Fatal(err)
		}
		got, err := json.MarshalIndent(cfg, "", " ")
		if err != nil {
			t.Fatal(err)
		}
		golden := "testdata/effective_" + name + ".json"
		if os.Getenv("UPDATE_GOLDEN") != "" {
			if err := os.WriteFile(golden, append(got, '\n'), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatal(err)
		}
		if string(want) != string(got)+"\n" {
			t.Errorf("%s effective config changed:\n%s", name, got)
		}
	}
}
