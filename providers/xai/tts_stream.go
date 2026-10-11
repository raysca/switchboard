//go:build xaispeech

package xai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/coder/websocket"

	"govoice/core/netpool"
	"govoice/core/ttsunit"
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

// wsSession is one reply's open TTS connection.
type wsSession struct{ conn *websocket.Conn }

func (s *wsSession) abort() { s.conn.CloseNow() }

// StreamingTTSProcessor synthesizes over a WebSocket: one connection per reply, reused across its
// units, with audio forwarded as it arrives. Attempt 0 streams; a retry uses the unary request, so
// a broken stream never costs the call its speech. Shared unit machinery lives in core/ttsunit.
type StreamingTTSProcessor struct {
	*ttsunit.Runner
	cfg   TTSConfig
	wsURL string
	pool  *netpool.Pool
	slot  ttsunit.ReplySlot[*wsSession]
}

func NewStreamingTTSProcessor(cfg TTSConfig) (*StreamingTTSProcessor, error) {
	if cfg.APIKey == "" {
		return nil, ErrMissingAPIKey
	}
	cfg = normalizeTTSConfig(cfg)
	if cfg.Client == nil {
		cfg.Client = NewRealXAITTSClient()
	}
	wsURL, err := streamURL(cfg)
	if err != nil {
		return nil, err
	}
	p := &StreamingTTSProcessor{cfg: cfg, wsURL: wsURL, pool: netpool.Get(netpool.Options{Provider: "xai", Stage: "tts", HTTP1Only: true})}
	p.slot.Close = (*wsSession).abort
	p.Runner = ttsunit.New(ttsunit.Config{
		Provider: "xai", Model: cfg.Voice, SampleRate: cfg.SampleRate,
		Breaker: cfg.CircuitBreaker, Policy: cfg.RetryPolicy, Classify: classify, EndReply: p.slot.Drop,
		Attempt: func(ctx context.Context, u *ttsunit.Unit, attempt int) error {
			return (&unitRun{p: p, Unit: u}).attempt(ctx, attempt)
		},
	})
	return p, nil
}

// Warm pre-dials TCP+TLS only; the reply's WebSocket is dialed with its first unit.
func (p *StreamingTTSProcessor) Warm(ctx context.Context) error {
	if br := p.cfg.CircuitBreaker; br != nil && br.WouldRefuse() {
		p.pool.NoteSkipped()
		return nil
	}
	return p.pool.Warm(ctx, p.wsURL)
}

type unitRun struct {
	p *StreamingTTSProcessor
	*ttsunit.Unit
}

// attempt synthesizes one unit. The next unit's text is sent only after this one's audio.done.
func (u *unitRun) attempt(ctx context.Context, attempt int) (err error) {
	cfg, slot := u.p.cfg, &u.p.slot
	if attempt > 0 { // the stream failed before audio: the unary request is authoritative
		slot.Drop()
		pcm, err := cfg.Client.Synthesize(ctx, u.Frame.Text, cfg)
		if err != nil {
			return err
		}
		if err := u.Write(pcm); err != nil {
			return err
		}
		return u.Finish()
	}
	s, ok := slot.Get(u.Frame)
	if !ok {
		conn, err := dialTTS(ctx, u.p.pool, u.p.wsURL, cfg.APIKey)
		if err != nil {
			return err
		}
		s = &wsSession{conn: conn}
		slot.Put(u.Frame, s)
	}
	defer func() {
		if err != nil {
			slot.Drop()
		}
	}()
	// Reads must outlive ctx only until abort closes the socket; the AfterFunc does that on cancel.
	defer context.AfterFunc(ctx, slot.Drop)()
	fail := func(err error) error { // report the cause (timeout/cancel), not the abort's hang-up
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return MapHTTPError("tts", nil, "", err)
	}
	for _, m := range []wsMsg{{Type: "text.delta", Delta: u.Frame.Text}, {Type: "text.done"}} {
		b, _ := json.Marshal(m)
		if err := s.conn.Write(ctx, websocket.MessageText, b); err != nil {
			return fail(err)
		}
	}
	for {
		_, data, err := s.conn.Read(ctx)
		if err != nil {
			return fail(err)
		}
		var m wsMsg
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		switch m.Type {
		case "audio.delta":
			pcm, err := base64.StdEncoding.DecodeString(m.Delta)
			if err != nil {
				return &ProviderError{Capability: "tts", Code: "BAD_RESPONSE", Message: "undecodable audio chunk", Class: ClassificationRetryable, Cause: err}
			}
			if err := u.Write(pcm); err != nil {
				return err
			}
		case "audio.done":
			return u.Finish() // the connection stays open for the reply's next unit
		case "error":
			return wsError(m)
		}
	}
}
