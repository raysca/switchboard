package main

import (
	"encoding/base64"
	"testing"
)

func TestJTI(t *testing.T) {
	seg := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	tok := seg(`{"alg":"HS256"}`) + "." + seg(`{"jti":"01JSESSION","sub":"agent-eval"}`) + ".sig"
	if got, err := jti(tok); err != nil || got != "01JSESSION" {
		t.Fatalf("jti = %q, %v", got, err)
	}
	for _, bad := range []string{"", "a.b", "a.!!.c", seg("x") + "." + seg(`{}`) + ".s", seg("x") + "." + seg(`[1]`) + ".s"} {
		if _, err := jti(bad); err == nil {
			t.Errorf("jti(%q) accepted", bad)
		}
	}
}
