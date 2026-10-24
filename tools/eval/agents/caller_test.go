package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCallerLLMGate(t *testing.T) {
	for _, tc := range []struct {
		llm, url string
		live     bool
	}{
		{"offline", "", false}, {"offline", "http://127.0.0.1:11434/v1/responses", false},
		{"offline", "http://localhost:11434/v1/responses", false}, {"offline", "http://[::1]:11434/v1/responses", false},
		{"xai", "", true},
		{"offline", "https://api.x.ai/v1/responses", true}, {"offline", "https://api.openai.com/v1/responses", true},
		{"offline", "http://127.0.0.1.evil.example/v1/responses", true}, {"offline", "http://localhost@evil.example/v1", true},
		{"offline", "http://evil.example/127.0.0.1", true}, {"offline", "not a url", true}, {"offline", "http://", true},
	} {
		if got := callerNeedsLive(tc.llm, tc.url); got != tc.live {
			t.Errorf("callerNeedsLive(%q, %q) = %v, want %v", tc.llm, tc.url, got, tc.live)
		}
	}
}

func TestNewAskSendsPromptAndKeepsKeyOut(t *testing.T) {
	var auth, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		b := make([]byte, 4096)
		n, _ := r.Body.Read(b)
		body = string(b[:n])
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"output_text":"I need a cleaning"}`)
	}))
	defer srv.Close()
	ask, closeAsk, err := newAsk(config{CallerLLM: callerOffline, CallerURL: srv.URL + "/v1/responses", CallerModel: "m1"})
	if err != nil {
		t.Fatal(err)
	}
	defer closeAsk()
	got, err := ask(context.Background(), "SYS", "USR")
	if err != nil || got != "I need a cleaning" {
		t.Fatalf("ask = %q, %v", got, err)
	}
	if auth != "Bearer offline" || !strings.Contains(body, `"m1"`) || !strings.Contains(body, "SYS") || !strings.Contains(body, "USR") {
		t.Errorf("request auth=%q body=%s", auth, body)
	}

	t.Setenv("XAI_API_KEY", "")
	if _, _, err := newAsk(config{CallerLLM: callerXAI}); err == nil || !strings.Contains(err.Error(), "XAI_API_KEY") {
		t.Errorf("xai without a key: %v", err)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "denied for "+r.Header.Get("Authorization"), http.StatusUnauthorized)
	}))
	defer bad.Close()
	ask2, close2, err := newAsk(config{CallerLLM: callerOffline, CallerURL: bad.URL + "/v1/responses"})
	if err != nil {
		t.Fatal(err)
	}
	defer close2()
	if _, err := ask2(context.Background(), "s", "u"); err == nil {
		t.Error("401 accepted")
	}
}

// The persona model client never follows a redirect: the second server must see nothing.
func TestNewAskDoesNotFollowRedirects(t *testing.T) {
	hit := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		fmt.Fprint(w, `{"output_text":"leaked"}`)
	}))
	defer target.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redir.Close()
	ask, closeAsk, err := newAsk(config{CallerLLM: callerOffline, CallerURL: redir.URL + "/v1/responses"})
	if err != nil {
		t.Fatal(err)
	}
	defer closeAsk()
	if got, err := ask(context.Background(), "s", "u"); err == nil || hit {
		t.Errorf("redirect followed or accepted: reply %q, err %v, target hit %v", got, err, hit)
	}
}
