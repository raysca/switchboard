package deepgram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// STTConfig configures the Deepgram live-transcription processor.
type STTConfig struct {
	Endpoint       string // default "wss://api.deepgram.com/v1/listen"
	Model          string // default "nova-3"
	Language       string // default "en"
	SampleRate     int    // default 16000; input is 16-bit mono PCM (linear16)
	InterimResults bool   // default true
	APIKey         string
	RetryPolicy    resilience.Policy // applied to opening the stream
	CircuitBreaker *resilience.Breaker
	// DrainTimeout bounds the wait for Deepgram's final results after the
	// turn ends before the socket is torn down.
	DrainTimeout time.Duration
	Now          func() time.Time
	// Pool is the connection pool used to dial the websocket; default is the
	// process-wide "deepgram"/"stt" pool (see core/netpool).
	Pool *netpool.Pool
}

func DefaultSTTConfig() STTConfig {
	return STTConfig{
		Endpoint: "wss://api.deepgram.com/v1/listen", Model: "nova-3", Language: "en",
		SampleRate: 16000, InterimResults: true, DrainTimeout: 2 * time.Second,
		RetryPolicy: resilience.Policy{MaxAttempts: 3, AttemptTimeout: 5 * time.Second},
	}
}

// message is the subset of Deepgram's server messages we read.
type message struct {
	Type         string `json:"type"`
	IsFinal      bool   `json:"is_final"`
	SpeechFinal  bool   `json:"speech_final"`
	FromFinalize bool   `json:"from_finalize"`
	Channel      struct {
		Alternatives []struct {
			Transcript string `json:"transcript"`
		} `json:"alternatives"`
	} `json:"channel"`
	ErrMsg string `json:"err_msg"`
}

// STTProcessor implements core.Processor for Deepgram speech-to-text: one
// websocket per utterance, dialed off the run loop (turn binding, pre-roll,
// drain and drop recovery live in core/sttunit).
type STTProcessor struct {
	*sttunit.Runner
	cfg STTConfig
}

// B7: the runner's error count reaches voice_provider_errors_total.
var _ core.STTCounters = (*STTProcessor)(nil)

func NewSTTProcessor(cfg STTConfig) (*STTProcessor, error) {
	if cfg.APIKey == "" {
		return nil, ErrMissingAPIKey
	}
	def := DefaultSTTConfig()
	if cfg.Endpoint == "" {
		cfg.Endpoint = def.Endpoint
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
		cfg.Pool = netpool.Get(netpool.Options{Provider: "deepgram", Stage: "stt", HTTP1Only: true})
	}
	p := &STTProcessor{cfg: cfg}
	p.Runner = sttunit.New(sttunit.Config{
		Provider: "deepgram", Dial: p.dial, Breaker: cfg.CircuitBreaker, Classify: classify,
		DrainTimeout: cfg.DrainTimeout, Now: cfg.Now,
	})
	return p, nil
}

// Warm pre-dials TCP+TLS to the endpoint (no websocket session: Deepgram
// closes idle sockets after ~10 s and an unused one would also be closed on
// KeepAlive timeout, so only the transport is warmed). Skipped while the
// breaker is open; never reports to it.
func (p *STTProcessor) Warm(ctx context.Context) error {
	if br := p.cfg.CircuitBreaker; br != nil && br.WouldRefuse() {
		p.cfg.Pool.NoteSkipped()
		return nil
	}
	return p.cfg.Pool.Warm(ctx, p.cfg.Endpoint)
}

func (p *STTProcessor) wsURL() (string, error) {
	u, err := url.Parse(p.cfg.Endpoint)
	if err != nil {
		return "", fmt.Errorf("parse deepgram endpoint: %w", err)
	}
	q := u.Query()
	q.Set("model", p.cfg.Model)
	if p.cfg.Language != "" {
		q.Set("language", p.cfg.Language)
	}
	q.Set("encoding", "linear16")
	q.Set("sample_rate", strconv.Itoa(p.cfg.SampleRate))
	q.Set("channels", "1")
	q.Set("interim_results", strconv.FormatBool(p.cfg.InterimResults))
	q.Set("endpointing", "false") // we own turns
	q.Set("smart_format", "true")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// dial opens one stream on ctx. The breaker and retries wrap only the dial:
// nothing is committed until audio flows, so a failed open is always safe to retry.
func (p *STTProcessor) dial(ctx context.Context) (sttunit.Conn, error) {
	target, err := p.wsURL()
	if err != nil {
		return nil, err
	}
	var conn *websocket.Conn
	opts := &websocket.DialOptions{HTTPClient: p.cfg.Pool.Client(0), HTTPHeader: http.Header{"Authorization": {"Token " + p.cfg.APIKey}}}
	err = resilience.Execute(ctx, p.cfg.CircuitBreaker, p.cfg.RetryPolicy, classify, nil, func(actx context.Context, _ int) error {
		c, resp, derr := websocket.Dial(actx, target, opts)
		if derr != nil {
			return MapError("stt", derr, resp)
		}
		conn = c
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &dgConn{conn: conn}, nil
}

// errInBand marks Deepgram's in-band "Error" message: a failure of one
// stream, which says nothing about the provider's health. The breaker is
// shared by every call, so classify keeps it out (as before this runner).
var errInBand = errors.New("deepgram: in-band stream error")

// dgConn is one Deepgram stream and its turn transcript.
type dgConn struct {
	conn      *websocket.Conn
	committed string // finalized segments of this turn, space-joined (Recv's task only)
}

func (c *dgConn) Send(ctx context.Context, pcm []byte) error {
	if err := c.conn.Write(ctx, websocket.MessageBinary, pcm); err != nil {
		return MapError("stt", err, nil)
	}
	return nil
}

// Finish asks Deepgram to flush its final results and close.
func (c *dgConn) Finish(ctx context.Context) error {
	for _, msg := range []string{`{"type":"Finalize"}`, `{"type":"CloseStream"}`} {
		if err := c.conn.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
			return err
		}
	}
	return nil
}

func (c *dgConn) Close() { _ = c.conn.CloseNow() }

func (c *dgConn) Recv(ctx context.Context) (sttunit.Result, error) {
	for {
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			return sttunit.Result{}, readError(err)
		}
		var m message
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		switch m.Type {
		case "Results", "":
			return c.results(&m), nil
		case "Error":
			// errInBand keeps it out of the shared breaker (classify), as before.
			return sttunit.Result{}, MapError("stt", fmt.Errorf("%w: %s", errInBand, m.ErrMsg), nil)
		}
	}
}

// readError maps a failed read: a clean close, or Deepgram's idle timeout
// (NET-0001, no audio for ~10 s: our silence, not an outage), ends the
// stream; anything else is a provider failure.
func readError(err error) error {
	if s := websocket.CloseStatus(err); s == websocket.StatusNormalClosure || s == websocket.StatusGoingAway {
		return sttunit.ErrStreamEnded
	}
	var ce websocket.CloseError
	if errors.As(err, &ce) && strings.Contains(strings.ReplaceAll(strings.ToUpper(ce.Reason), "-", ""), "NET0001") {
		return sttunit.ErrStreamEnded
	}
	return MapError("stt", err, nil)
}

// results turns one Results message into the cumulative turn text: the
// finalized segments so far plus this message's text.
func (c *dgConn) results(m *message) sttunit.Result {
	seg := ""
	if len(m.Channel.Alternatives) > 0 {
		seg = strings.TrimSpace(m.Channel.Alternatives[0].Transcript)
	}
	text := join(c.committed, seg)
	if m.IsFinal {
		c.committed = text
	}
	eou := m.SpeechFinal || m.FromFinalize
	if seg == "" && !(m.IsFinal && eou) {
		return sttunit.Result{}
	}
	return sttunit.Result{Text: text, Final: m.IsFinal, EndOfUtterance: m.IsFinal && eou}
}

func join(a, b string) string {
	if a == "" || b == "" {
		return a + b
	}
	return a + " " + b
}
