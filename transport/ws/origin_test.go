package ws

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCheckOriginSameHost(t *testing.T) {
	cfg := &SecurityConfig{AllowedOrigins: []string{"https://app.example"}, AllowSameHost: true}
	for _, tc := range []struct {
		name, host, origin string
		want               bool
	}{
		{"same host https", "voice.run.app", "https://voice.run.app", true},
		{"same host http (local rehearsal)", "localhost:8080", "http://localhost:8080", true},
		{"host case differs", "Voice.Run.App", "https://voice.run.app", true},
		{"listed origin still works", "voice.run.app", "https://app.example", true},
		{"other site", "voice.run.app", "https://evil.example", false},
		{"other port", "voice.run.app", "https://voice.run.app:8443", false},
		{"suffix trick", "voice.run.app", "https://voice.run.app.evil.example", false},
		{"not a web origin", "voice.run.app", "chrome-extension://voice.run.app", false},
		{"null origin", "voice.run.app", "null", false},
		{"unparseable", "voice.run.app", "https://%zz", false},
	} {
		r := httptest.NewRequest(http.MethodGet, "/ws", nil)
		r.Host = tc.host
		r.Header.Set("Origin", tc.origin)
		if got := cfg.CheckOrigin(r) == nil; got != tc.want {
			t.Errorf("%s: allowed=%v, want %v", tc.name, got, tc.want)
		}
	}
	// Off by default: the same host alone is not enough.
	off := &SecurityConfig{AllowedOrigins: []string{"https://app.example"}}
	r := httptest.NewRequest(http.MethodGet, "/ws", nil)
	r.Host = "voice.run.app"
	r.Header.Set("Origin", "https://voice.run.app")
	if off.CheckOrigin(r) == nil {
		t.Fatal("same-host must be opt-in")
	}
}
