package cartesia

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"govoice/core"
	"govoice/core/netpool"
	"govoice/core/resilience"
	"govoice/core/sttunit"
)

// Protocol (docs.cartesia.ai STT websocket reference): binary PCM frames up;
// text "finalize" (flush buffered audio -> server "flush_done") and "close"
// (flush and end -> server "done"). Server JSON: transcript {text, is_final,
// words?}, flush_done, done, error {status_code, title, message}. A transcript's
// text is a delta: finals are concatenated verbatim (no added whitespace), an
// interim replaces the previous interim.

// STTConfig configures the Cartesia (Ink) streaming transcription processor.
type STTConfig struct {
	Endpoint   string // default "wss://api.cartesia.ai/stt/websocket"
	Version    string // Cartesia-Version (header and cartesia_version query), default "2026-08-14"
	Model      string // default "ink-whisper"; docs also list "ink-2", "ink-preview"
	Language   string // default "en"; sent for ink-whisper only (per the docs)
	SampleRate int    // default 16000; input is 16-bit mono PCM (pcm_s16le)
	// MinVolume (0-1) and MaxSilenceSecs tune Cartesia's own silence
	// auto-finalization (ink-whisper only); zero leaves the server default.
	MinVolume      float64
	MaxSilenceSecs float64
	APIKey         string
	// RetryPolicy bounds opening the websocket (default 2 tries x 5 s).
	RetryPolicy    resilience.Policy
	CircuitBreaker *resilience.Breaker
	// DrainTimeout bounds the wait for the final results after the turn ends
	// before the socket is torn down.
	DrainTimeout time.Duration
	Now          func() time.Time
	// Pool dials the websocket; default is the process-wide "cartesia"/"stt" pool.
	Pool *netpool.Pool
}

func DefaultSTTConfig() STTConfig {
	return STTConfig{
		Endpoint: "wss://api.cartesia.ai/stt/websocket", Version: "2026-08-14", Model: "ink-whisper",
		Language: "en", SampleRate: 16000, DrainTimeout: 2 * time.Second,
		RetryPolicy: resilience.Policy{MaxAttempts: 2, AttemptTimeout: 5 * time.Second, InitialBackoff: 100 * time.Millisecond},
	}
}

// sttMessage is the subset of Cartesia's STT server messages we read.
type sttMessage struct {
	Type       string `json:"type"`
	Text       string `json:"text"`
	IsFinal    bool   `json:"is_final"`
	StatusCode int    `json:"status_code"`
	Title      string `json:"title"`
	Message    string `json:"message"`
}

// STTProcessor implements core.Processor for Cartesia speech-to-text: one
// websocket per utterance, dialed off the run loop (the stream lifecycle lives
// in core/sttunit).
type STTProcessor struct {
	*sttunit.Runner
	cfg STTConfig
}

var _ core.STTCounters = (*STTProcessor)(nil)

func NewSTTProcessor(cfg STTConfig) (*STTProcessor, error) {
	if cfg.APIKey == "" {
		return nil, ErrMissingAPIKey
	}
	def := DefaultSTTConfig()
	if cfg.Endpoint == "" {
		cfg.Endpoint = def.Endpoint
	}
	if cfg.Version == "" {
		cfg.Version = def.Version
	}
	if cfg.Model == "" {
		cfg.Model = def.Model
	}
	if cfg.SampleRate <= 0 {
		cfg.SampleRate = def.SampleRate
	}
	if cfg.DrainTimeout <= 0 {
		cfg.DrainTimeout = def.DrainTimeout
	}
	if cfg.RetryPolicy.MaxAttempts <= 0 {
		cfg.RetryPolicy = def.RetryPolicy
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Pool == nil {
		cfg.Pool = netpool.Get(netpool.Options{Provider: "cartesia", Stage: "stt", HTTP1Only: true})
	}
	p := &STTProcessor{cfg: cfg}
	p.Runner = sttunit.New(sttunit.Config{
		Provider: "cartesia", Dial: p.dial, Breaker: cfg.CircuitBreaker, Classify: classify,
		DrainTimeout: cfg.DrainTimeout, Now: cfg.Now,
	})
	return p, nil
}

// Warm pre-dials TCP+TLS only: an unused websocket would just idle out.
// Skipped while the breaker is open; never reports to it.
func (p *STTProcessor) Warm(ctx context.Context) error {
	if br := p.cfg.CircuitBreaker; br != nil && br.WouldRefuse() {
		p.cfg.Pool.NoteSkipped()
		return nil
	}
	return p.cfg.Pool.Warm(ctx, p.cfg.Endpoint)
}

// wsURL carries no credential; the key travels in a header only.
func (p *STTProcessor) wsURL() (string, error) {
	u, err := url.Parse(p.cfg.Endpoint)
	if err != nil {
		return "", errors.New("cartesia: invalid stt endpoint")
	}
	q := u.Query()
	q.Set("model", p.cfg.Model)
	q.Set("encoding", "pcm_s16le")
	q.Set("sample_rate", strconv.Itoa(p.cfg.SampleRate))
	q.Set("cartesia_version", p.cfg.Version)
	// language, min_volume and max_silence_duration_secs are documented as
	// ink-whisper only; other models (ink-2, ink-preview) do not get them.
	whisper := strings.HasPrefix(p.cfg.Model, "ink-whisper")
	if whisper && p.cfg.Language != "" {
		q.Set("language", p.cfg.Language)
	}
	if whisper && p.cfg.MinVolume > 0 {
		q.Set("min_volume", strconv.FormatFloat(p.cfg.MinVolume, 'f', -1, 64))
	}
	if whisper && p.cfg.MaxSilenceSecs > 0 {
		q.Set("max_silence_duration_secs", strconv.FormatFloat(p.cfg.MaxSilenceSecs, 'f', -1, 64))
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// dial opens one stream on ctx. Retries wrap only the dial; the breaker gets
// one report per open.
func (p *STTProcessor) dial(ctx context.Context) (sttunit.Conn, error) {
	target, err := p.wsURL()
	if err != nil {
		return nil, err
	}
	br := p.cfg.CircuitBreaker
	if br != nil && br.WouldRefuse() {
		return nil, ErrCircuitOpen
	}
	opts := &websocket.DialOptions{HTTPClient: p.cfg.Pool.Client(0),
		HTTPHeader: http.Header{"X-API-Key": {p.cfg.APIKey}, "Cartesia-Version": {p.cfg.Version}}}
	conn, err := resilience.OpenBounded(ctx, p.cfg.RetryPolicy, classify, func(actx context.Context) (*websocket.Conn, error) {
		c, resp, derr := websocket.Dial(actx, target, opts)
		if derr != nil {
			return nil, MapError("stt", derr, resp)
		}
		c.SetReadLimit(1 << 20)
		return c, nil
	})
	if err != nil {
		if errors.Is(err, resilience.ErrTimeout) { // a hang is an outage
			err = &ProviderError{Capability: "stt", Code: "TIMEOUT", Message: SanitizeString(err.Error()), Retryable: true, Cause: err}
		}
		if br != nil {
			br.Report(err, classify)
		}
		return nil, err
	}
	if br != nil {
		br.RecordSuccess()
	}
	return &sttConn{conn: conn}, nil
}

// idleClose reports whether err is the server dropping a connection that got
// no audio (our silence, not an outage). The exact idle close code/reason is
// undocumented, so match the reason text.
func idleClose(err error) bool {
	var ce websocket.CloseError
	if !errors.As(err, &ce) {
		return false
	}
	r := strings.ToLower(ce.Reason)
	return strings.Contains(r, "idle") || strings.Contains(r, "timeout") || strings.Contains(r, "timed out")
}

// sttConn is one Cartesia stream and its turn transcript (Recv's task only).
type sttConn struct {
	conn               *websocket.Conn
	committed, interim string // final deltas concatenated verbatim; the current non-final delta
}

func (c *sttConn) Send(ctx context.Context, pcm []byte) error {
	if err := c.conn.Write(ctx, websocket.MessageBinary, pcm); err != nil {
		return MapError("stt", err, nil)
	}
	return nil
}

// Finish asks Cartesia to flush (flush_done) and close (done).
func (c *sttConn) Finish(ctx context.Context) error {
	for _, msg := range []string{"finalize", "close"} {
		if err := c.conn.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
			return err
		}
	}
	return nil
}

func (c *sttConn) Close() { _ = c.conn.CloseNow() }

func (c *sttConn) Recv(ctx context.Context) (sttunit.Result, error) {
	for {
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			if s := websocket.CloseStatus(err); s == websocket.StatusNormalClosure || s == websocket.StatusGoingAway || idleClose(err) {
				return sttunit.Result{}, sttunit.ErrStreamEnded
			}
			return sttunit.Result{}, MapError("stt", err, nil)
		}
		var m sttMessage
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		switch m.Type {
		case "transcript":
			return c.transcript(&m), nil
		case "flush_done":
			return c.flushed(), nil
		case "done":
			return sttunit.Result{}, sttunit.ErrStreamEnded
		case "error":
			msg := m.Message
			if msg == "" {
				msg = m.Title
			}
			return sttunit.Result{}, messageError("stt", m.StatusCode, msg)
		}
	}
}

// transcript turns one transcript delta into the cumulative turn text: the
// finalized deltas so far plus the current interim.
func (c *sttConn) transcript(m *sttMessage) sttunit.Result {
	if m.IsFinal {
		c.committed += m.Text
		c.interim = ""
	} else {
		c.interim = m.Text
	}
	return sttunit.Result{Text: strings.TrimSpace(c.committed + c.interim), Final: m.IsFinal}
}

// flushed handles flush_done: everything buffered at finalize is now final, so
// the cumulative text ends the utterance.
func (c *sttConn) flushed() sttunit.Result {
	c.committed += c.interim
	c.interim = ""
	return sttunit.Result{Text: strings.TrimSpace(c.committed), Final: true, EndOfUtterance: true}
}
