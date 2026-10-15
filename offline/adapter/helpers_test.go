package adapter

import "testing"

func mustConfig(t *testing.T) Config {
	t.Helper()
	c, err := LoadConfig(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	return c
}
