//go:build xaispeech

package xai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"govoice/core/netpool"
)

func TestStreamURL(t *testing.T) {
	cfg := normalizeTTSConfig(TTSConfig{Endpoint: "https://api.x.ai/v1/tts", Voice: "ara", Language: "fr", SampleRate: 16000})
	got, err := streamURL(cfg)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "wss" || u.Host != "api.x.ai" || u.Path != "/v1/tts" {
		t.Errorf("url = %s, want wss://api.x.ai/v1/tts", got)
	}
	q := u.Query()
	for k, want := range map[string]string{"voice": "ara", "language": "fr", "codec": "pcm", "sample_rate": "16000", "optimize_streaming_latency": "1"} {
		if q.Get(k) != want {
			t.Errorf("query %s = %q, want %q", k, q.Get(k), want)
		}
	}
	if got, _ := streamURL(normalizeTTSConfig(TTSConfig{Endpoint: "http://127.0.0.1:9/v1/tts"})); got == "" || got[:5] != "ws://" {
		t.Errorf("http endpoint became %q, want ws://", got)
	}
	if _, err := streamURL(TTSConfig{Endpoint: "api.x.ai/v1/tts"}); err == nil {
		t.Error("an endpoint without a scheme was accepted")
	}
	if cfg.Language != "fr" || normalizeTTSConfig(TTSConfig{}).Language != "en" {
		t.Error("language must default to en: the WebSocket requires it")
	}
}

func TestWSErrorIsRetryableAndSanitized(t *testing.T) {
	pe := wsError(wsMsg{Type: "error", Message: "voice not available, key xai-abcdefghijklmnopqrstuv"})
	if !classify(pe).Retryable {
		t.Errorf("class = %v, want retryable: the unary retry returns the authoritative error", pe.Class)
	}
	if !strings.Contains(pe.Message, "xai-[REDACTED]") || strings.Contains(pe.Message, "abcdefghijkl") {
		t.Errorf("message = %q, want the key redacted", pe.Message)
	}
	if got := wsError(wsMsg{Type: "error", Error: "from error field"}); !strings.Contains(got.Message, "from error field") {
		t.Errorf("message = %q, want the error field when message is empty", got.Message)
	}
}

func TestDialTTS_MapsUpgradeFailures(t *testing.T) {
	for status, wantClass := range map[int]ErrorClassification{
		http.StatusUnauthorized:       ClassificationNonRetryable,
		http.StatusServiceUnavailable: ClassificationRetryable,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, err := dialTTS(ctx, netpool.Get(netpool.Options{Provider: "xai", Stage: "tts", HTTP1Only: true}), "ws"+srv.URL[len("http"):]+"/v1/tts", "k")
		cancel()
		srv.Close()
		var pe *ProviderError
		if !errors.As(err, &pe) || pe.Class != wantClass || pe.StatusCode != status {
			t.Errorf("status %d: err = %v, want class %v with that status", status, err, wantClass)
		}
	}
}
