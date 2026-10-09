//go:build xaispeech

package xai

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// M3-03: connection refused and a truncated body are outages (retryable, count
// on the breaker, eligible for failover), not client errors.
func TestTTSNetworkFailuresAreRetryable(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()

	truncated := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100000")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("abc"))
		if c, _, err := w.(http.Hijacker).Hijack(); err == nil {
			_ = c.(net.Conn).Close()
		}
	}))
	defer truncated.Close()

	for name, url := range map[string]string{"refused": closedURL, "truncated body": truncated.URL} {
		cfg := DefaultTTSConfig()
		cfg.APIKey = "k"
		cfg.Endpoint = url
		_, err := NewRealXAITTSClient().Synthesize(context.Background(), "hi", cfg)
		if err == nil {
			t.Fatalf("%s: no error", name)
		}
		v := classify(err)
		if !v.Retryable || v.ClientError {
			t.Fatalf("%s: verdict %+v (err %v); want retryable, not a client error", name, v, err)
		}
		if pe := MapError("tts", err); pe.ClientError() {
			t.Fatalf("%s: ClientError() is true", name)
		}
	}
}
