package xai

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

func TestSummarizeTextResponsesAndChat(t *testing.T) {
	for name, tc := range map[string]struct{ path, reply, key string }{
		"responses": {"/v1/responses", `{"output":[{"type":"message","content":[{"type":"output_text","text":" the summary "}]}]}`, "input"},
		"chat":      {"/v1/chat/completions", `{"choices":[{"message":{"content":"the summary"}}]}`, "messages"},
	} {
		t.Run(name, func(t *testing.T) {
			var body map[string]any
			var auth string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				auth = r.Header.Get("Authorization")
				b, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(b, &body)
				_, _ = io.WriteString(w, tc.reply)
			}))
			defer srv.Close()
			p, err := NewProcessor(Config{Endpoint: srv.URL + tc.path, Model: "m", APIKey: "k-1", HTTPClient: srv.Client()})
			if err != nil {
				t.Fatal(err)
			}
			got, err := p.SummarizeText(context.Background(), "sys", "user text")
			if err != nil || got != "the summary" {
				t.Fatalf("got %q err %v", got, err)
			}
			if auth != "Bearer k-1" || body["store"] != false || body["stream"] == true || body["tools"] != nil || len(body[tc.key].([]any)) != 2 {
				t.Fatalf("auth=%q body=%v", auth, body)
			}
		})
	}
}

func TestSummarizeTextFailuresNeverTouchBreakerAndRedact(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("slow") != "" {
			time.Sleep(time.Second)
		}
		http.Error(w, "bad Bearer xai-abcdefghijklmnopqrstuvwxyz0123", http.StatusInternalServerError)
	}))
	defer srv.Close()
	br := resilience.NewBreaker(resilience.Config{FailureThreshold: 1, Cooldown: time.Minute})
	p, err := NewProcessor(Config{Endpoint: srv.URL + "/v1/responses", APIKey: "k", HTTPClient: srv.Client(), CircuitBreaker: br, Timeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err = p.SummarizeText(context.Background(), "s", "u"); err == nil || strings.Contains(err.Error(), "xai-abcdef") {
			t.Fatalf("err = %v", err)
		}
	}
	if br.WouldRefuse() {
		t.Fatal("summary failures opened the LLM breaker")
	}
	p.cfg.Endpoint += "?slow=1"
	start := time.Now()
	if _, err = p.SummarizeText(context.Background(), "s", "u"); err == nil || time.Since(start) > 700*time.Millisecond {
		t.Fatalf("timeout not enforced: err=%v after %v", err, time.Since(start))
	}
}

func TestSummarizeTextSkipsWhenBreakerOpen(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	defer srv.Close()
	br := resilience.NewBreaker(resilience.Config{FailureThreshold: 1, Cooldown: time.Minute})
	br.RecordFailure()
	p, err := NewProcessor(Config{Endpoint: srv.URL + "/v1/responses", HTTPClient: srv.Client(), CircuitBreaker: br})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.SummarizeText(context.Background(), "s", "u"); !errors.Is(err, resilience.ErrCircuitOpen) || hit {
		t.Fatalf("err=%v hit=%v", err, hit)
	}
}

func TestSummarizeTextResponsesShapes(t *testing.T) {
	for name, tc := range map[string]struct{ reply, want string }{
		"output_text and output":      {`{"output_text":"the summary","output":[{"type":"reasoning","content":[{"type":"reasoning_text","text":"think"}]},{"type":"message","content":[{"type":"output_text","text":"the summary"}]}]}`, "the summary"},
		"output only, skip reasoning": {`{"output":[{"type":"reasoning","content":[{"type":"reasoning_text","text":"think "}]},{"type":"message","content":[{"type":"output_text","text":"the summary"}]}]}`, "the summary"},
		"empty":                       {`{"output":[]}`, ""},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, tc.reply) }))
			defer srv.Close()
			p, err := NewProcessor(Config{Endpoint: srv.URL + "/v1/responses", HTTPClient: srv.Client()})
			if err != nil {
				t.Fatal(err)
			}
			got, err := p.SummarizeText(context.Background(), "s", "u")
			if got != tc.want || (tc.want == "") != (err != nil) {
				t.Fatalf("got %q err %v", got, err)
			}
		})
	}
}
