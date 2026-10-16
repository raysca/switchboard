package elevenlabs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"govoice/core"
	"govoice/core/netpool"
	"govoice/core/resilience"
	"govoice/core/sttunit"
)

// sttRates are the documented pcm_<rate> input formats of the realtime STT.
var sttRates = map[int]bool{8000: true, 16000: true, 22050: true, 24000: true, 44100: true, 48000: true}

// STTConfig configures the ElevenLabs realtime (Scribe) STT processor.
type STTConfig struct {
	Endpoint string // full URL, default "wss://api.elevenlabs.io/v1/speech-to-text/realtime"
	Model    string // default "scribe_v2_realtime"
	// Language is an ISO 639 code ("en"); "" lets the model detect it. A regional
	// tag ("en-US") is cut to its primary subtag.
	Language   string
	SampleRate int // default 16000; input is 16-bit mono PCM (pcm_<rate>)
	// IncludeTimestamps asks for word timings (not used by the pipeline yet).
	IncludeTimestamps bool
	APIKey            string
	RetryPolicy       resilience.Policy // applied to opening the stream
	CircuitBreaker    *resilience.Breaker
	// DrainTimeout bounds the wait for the committed transcript after the turn
	// ends before the socket is torn down.
	DrainTimeout time.Duration
	Now          func() time.Time
	// Pool dials the websocket; default is the process-wide "elevenlabs"/"stt" pool.
	Pool *netpool.Pool
}

func DefaultSTTConfig() STTConfig {
	return STTConfig{
		Endpoint: "wss://api.elevenlabs.io/v1/speech-to-text/realtime", Model: "scribe_v2_realtime",
		SampleRate: 16000, DrainTimeout: 2 * time.Second,
		RetryPolicy: resilience.Policy{MaxAttempts: 2, AttemptTimeout: 5 * time.Second},
	}
}

// sttMessage is the subset of ElevenLabs' realtime STT server messages we read.
type sttMessage struct {
	Type  string `json:"message_type"`
	Text  string `json:"text"`
	Error string `json:"error"`
}

// STTProcessor implements core.Processor for ElevenLabs realtime speech-to-text:
// one websocket per utterance, dialed off the run loop (the stream lifecycle
// lives in core/sttunit). Turn detection is ours: the stream uses the manual
// commit strategy and is committed when the turn ends.
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
	if cfg.Model == "" {
		cfg.Model = def.Model
	}
	if cfg.SampleRate <= 0 {
		cfg.SampleRate = def.SampleRate
	}
	if !sttRates[cfg.SampleRate] {
		return nil, fmt.Errorf("%w: unsupported STT sample rate %d", ErrInvalidConfig, cfg.SampleRate)
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
		cfg.Pool = netpool.Get(netpool.Options{Provider: "elevenlabs", Stage: "stt", HTTP1Only: true})
	}
	p := &STTProcessor{cfg: cfg}
	p.Runner = sttunit.New(sttunit.Config{
		Provider: "elevenlabs", Dial: p.dial, Breaker: cfg.CircuitBreaker, Classify: classify,
		DrainTimeout: cfg.DrainTimeout, Now: cfg.Now,
	})
	return p, nil
}

// Warm pre-dials TCP+TLS to the endpoint (no websocket session is opened).
// Skipped while the breaker is open; never reports to it.
func (p *STTProcessor) Warm(ctx context.Context) error {
	if br := p.cfg.CircuitBreaker; br != nil && br.WouldRefuse() {
		p.cfg.Pool.NoteSkipped()
		return nil
	}
	return p.cfg.Pool.Warm(ctx, p.cfg.Endpoint)
}

type audioChunk struct {
	MessageType string `json:"message_type"`
	Audio       string `json:"audio_base_64"`
	Commit      bool   `json:"commit"`
	SampleRate  int    `json:"sample_rate"`
}

func (p *STTProcessor) chunk(pcm []byte, commit bool) []byte {
	b, _ := json.Marshal(audioChunk{"input_audio_chunk", base64.StdEncoding.EncodeToString(pcm), commit, p.cfg.SampleRate})
	return b
}

// mapErr classifies err and scrubs the API key from its text.
func (p *STTProcessor) mapErr(err error, resp *http.Response) *ProviderError {
	pe := MapError("stt", err, resp)
	pe.Message = strings.ReplaceAll(pe.Message, p.cfg.APIKey, "[REDACTED]")
	return pe
}

// wsURL carries parameters only; the key travels in the xi-api-key header.
func (p *STTProcessor) wsURL() (string, error) {
	u, err := url.Parse(p.cfg.Endpoint)
	if err != nil {
		return "", fmt.Errorf("parse elevenlabs endpoint: %w", err)
	}
	q := u.Query()
	q.Set("model_id", p.cfg.Model)
	q.Set("audio_format", "pcm_"+strconv.Itoa(p.cfg.SampleRate))
	q.Set("commit_strategy", "manual") // we own turns
	if l := p.cfg.Language; l != "" {
		if i := strings.IndexAny(l, "-_"); i > 0 {
			l = l[:i]
		}
		q.Set("language_code", strings.ToLower(l))
	}
	if p.cfg.IncludeTimestamps {
		q.Set("include_timestamps", "true")
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// dial opens one stream on ctx. The breaker and retries wrap only the dial (as
// Deepgram): nothing is committed until audio flows, so a failed open is safe
// to retry. Execute releases a half-open probe on client errors/cancel without
// closing the breaker. The dial ctx governs the handshake only.
func (p *STTProcessor) dial(ctx context.Context) (sttunit.Conn, error) {
	target, err := p.wsURL()
	if err != nil {
		return nil, err
	}
	var conn *websocket.Conn
	opts := &websocket.DialOptions{HTTPClient: p.cfg.Pool.Client(0), HTTPHeader: http.Header{"xi-api-key": {p.cfg.APIKey}}}
	err = resilience.Execute(ctx, p.cfg.CircuitBreaker, p.cfg.RetryPolicy, classify, nil, func(actx context.Context, _ int) error {
		c, resp, derr := websocket.Dial(actx, target, opts)
		if derr != nil {
			return p.mapErr(derr, resp)
		}
		c.SetReadLimit(1 << 20) // committed_transcript_with_timestamps (per-word objects) can pass the 32 KiB default
		conn = c
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &sttConn{p: p, conn: conn}, nil
}

// quietClose reports a close that is our own silence or a clean end, not an outage.
func quietClose(err error) bool {
	if s := websocket.CloseStatus(err); s == websocket.StatusNormalClosure || s == websocket.StatusGoingAway {
		return true
	}
	// ponytail: the idle-close wording is undocumented; keyword guess.
	var ce websocket.CloseError
	if errors.As(err, &ce) {
		r := strings.ToLower(ce.Reason)
		return strings.Contains(r, "idle") || strings.Contains(r, "inactiv")
	}
	return false
}

// errNothingToCommit: a stream that never got audio is closed, not committed.
var errNothingToCommit = errors.New("elevenlabs: no audio to commit")

// sttConn is one ElevenLabs stream and its turn transcript.
type sttConn struct {
	p    *STTProcessor
	conn *websocket.Conn

	sent       atomic.Bool // audio went out: there is something to commit
	commitSent atomic.Bool // our commit is in flight: the next committed text ends the utterance

	// Recv's task only:
	committed string // committed segments of this turn, space-joined
	lastKind  string // last committed message kind with no partial since (dedup of the two committed forms)
	lastText  string
	done      bool // the committed text answering our commit arrived
}

func (c *sttConn) Send(ctx context.Context, pcm []byte) error {
	if err := c.conn.Write(ctx, websocket.MessageText, c.p.chunk(pcm, false)); err != nil {
		return c.p.mapErr(err, nil)
	}
	c.sent.Store(true)
	return nil
}

// Finish sends the manual commit (an empty audio chunk with commit=true, as
// the official SDKs send it). A stream that never got audio has nothing to commit.
func (c *sttConn) Finish(ctx context.Context) error {
	if !c.sent.Load() {
		return errNothingToCommit
	}
	c.commitSent.Store(true)
	return c.conn.Write(ctx, websocket.MessageText, c.p.chunk(nil, true))
}

func (c *sttConn) Close() { _ = c.conn.CloseNow() }

func (c *sttConn) Recv(ctx context.Context) (sttunit.Result, error) {
	if c.done {
		return sttunit.Result{}, sttunit.ErrStreamEnded
	}
	for {
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			if quietClose(err) {
				return sttunit.Result{}, sttunit.ErrStreamEnded
			}
			return sttunit.Result{}, c.p.mapErr(err, nil)
		}
		var m sttMessage
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		switch {
		case m.Type == "partial_transcript" || m.Type == "committed_transcript" || m.Type == "committed_transcript_with_timestamps":
			res, eou := c.transcript(&m)
			c.done = eou // committed after our commit: the turn is done
			return res, nil
		case m.Type == "session_time_limit_exceeded" || m.Type == "insufficient_audio_activity":
			// A very long session, or our own silence: the server closes next.
			return sttunit.Result{}, sttunit.ErrStreamEnded
		case sttErrorKinds[m.Type]:
			return sttunit.Result{}, sttMessageError(m.Type, strings.ReplaceAll(m.Error, c.p.cfg.APIKey, "[REDACTED]"))
		} // session_started, warning, entities, edited_transcript...: nothing to do
	}
}

// transcript turns one transcript message into the cumulative turn text:
// committed segments so far plus this message's text. It reports whether the
// stream is finished (the committed text answering our commit).
func (c *sttConn) transcript(m *sttMessage) (sttunit.Result, bool) {
	seg := strings.TrimSpace(m.Text)
	final := m.Type != "partial_transcript"
	if final && c.lastKind != "" && c.lastKind != m.Type && c.lastText == seg {
		return sttunit.Result{}, false // the same commit sent as plain and as with-timestamps
	}
	text := join(c.committed, seg)
	if final {
		c.committed, c.lastKind, c.lastText = text, m.Type, seg
	} else {
		c.lastKind = ""
	}
	eou := final && c.commitSent.Load()
	if seg == "" && !final {
		return sttunit.Result{}, eou
	}
	return sttunit.Result{Text: text, Final: final, EndOfUtterance: eou}, eou
}

func join(a, b string) string {
	if a == "" || b == "" {
		return a + b
	}
	return a + " " + b
}
