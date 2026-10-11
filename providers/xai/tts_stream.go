//go:build xaispeech

package xai

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/coder/websocket"

	"govoice/core/netpool"
)

// wsMsg is one JSON message on the TTS WebSocket, in both directions.
type wsMsg struct {
	Type    string `json:"type"`
	Delta   string `json:"delta,omitempty"`   // text.delta: text; audio.delta: base64 PCM16LE
	Message string `json:"message,omitempty"` // error
	Error   string `json:"error,omitempty"`   // error (alternate field)
}

// streamURL is the WebSocket endpoint for cfg: the configured endpoint with the scheme swapped
// (https -> wss, http -> ws) and the audio parameters in the query.
func streamURL(cfg TTSConfig) (string, error) {
	u, err := url.Parse(cfg.Endpoint)
	if err != nil {
		return "", fmt.Errorf("xai tts endpoint: %w", err)
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	case "wss", "ws":
	default:
		return "", fmt.Errorf("xai tts endpoint %q needs an http(s) or ws(s) scheme", cfg.Endpoint)
	}
	q := url.Values{}
	q.Set("voice", cfg.Voice)
	q.Set("language", cfg.Language)
	q.Set("codec", "pcm")
	q.Set("sample_rate", strconv.Itoa(cfg.SampleRate))
	q.Set("optimize_streaming_latency", "1")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// wsError turns a server error message into a retryable ProviderError: the unary retry returns the
// authoritative classification (a bad voice is a 400 there), so the stream never hides it.
func wsError(m wsMsg) *ProviderError {
	msg := m.Message
	if msg == "" {
		msg = m.Error
	}
	return &ProviderError{Capability: "tts", Code: "STREAM_ERROR", Message: SanitizeString(msg), Class: ClassificationRetryable}
}

// dialTTS opens the TTS WebSocket through the shared HTTP/1.1 pool. A rejected upgrade is mapped
// like an HTTP failure (401 is a client error, 5xx is retryable); a transport error is retryable.
func dialTTS(ctx context.Context, pool *netpool.Pool, wsURL, apiKey string) (*websocket.Conn, error) {
	opts := &websocket.DialOptions{HTTPClient: pool.Client(0), HTTPHeader: http.Header{"Authorization": {"Bearer " + apiKey}}}
	c, resp, err := websocket.Dial(ctx, wsURL, opts)
	if err != nil {
		var body string
		if resp != nil && resp.Body != nil {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			body = string(b)
		}
		return nil, MapHTTPError("tts", resp, body, err)
	}
	c.SetReadLimit(4 << 20)
	return c, nil
}
