package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"govoice/core/resilience"
)

func TestSummarizeText(t *testing.T) {
	var body map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":" the summary "}}]}`)
	}))
	defer srv.Close()
	p, err := NewProcessor(Config{BaseURL: srv.URL, Model: "m", APIKey: "k-1", AllowPrivate: true, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.SummarizeText(context.Background(), "sys", "user text")
	if err != nil || got != "the summary" {
		t.Fatalf("got %q err %v", got, err)
	}
	msgs := body["messages"].([]any)
	if auth != "Bearer k-1" || body["stream"] != false || body["tools"] != nil || len(msgs) != 2 || msgs[1].(map[string]any)["content"] != "user text" {
		t.Fatalf("auth=%q body=%v", auth, body)
	}
}

func TestSummarizeTextFailuresNeverTouchBreakerAndRedact(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("slow") != "" {
			time.Sleep(time.Second)
		}
		http.Error(w, "bad Bearer sk-abcdefghijklmnopqrstuvwxyz0123", http.StatusInternalServerError)
	}))
	defer srv.Close()
	br := resilience.NewBreaker(resilience.Config{FailureThreshold: 1, Cooldown: time.Minute})
	p, err := NewProcessor(Config{BaseURL: srv.URL, AllowPrivate: true, HTTPClient: srv.Client(), CircuitBreaker: br, Timeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		_, err = p.SummarizeText(context.Background(), "s", "u")
		if err == nil || strings.Contains(err.Error(), "sk-abcdef") {
			t.Fatalf("err = %v", err)
		}
	}
	if br.WouldRefuse() {
		t.Fatal("summary failures opened the LLM breaker")
	}
	p.endpoint += "?slow=1"
	start := time.Now()
	if _, err = p.SummarizeText(context.Background(), "s", "u"); err == nil || time.Since(start) > 700*time.Millisecond {
		t.Fatalf("timeout not enforced: err=%v after %v", err, time.Since(start))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = p.SummarizeText(ctx, "s", "u"); err == nil {
		t.Fatal("cancelled context not honoured")
	}
}

func TestSummarizeTextSkipsWhenBreakerOpen(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	defer srv.Close()
	br := resilience.NewBreaker(resilience.Config{FailureThreshold: 1, Cooldown: time.Minute})
	br.RecordFailure()
	p, err := NewProcessor(Config{BaseURL: srv.URL, AllowPrivate: true, HTTPClient: srv.Client(), CircuitBreaker: br})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.SummarizeText(context.Background(), "s", "u"); !errors.Is(err, resilience.ErrCircuitOpen) || hit {
		t.Fatalf("err=%v hit=%v", err, hit)
	}
}
