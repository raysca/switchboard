package cartesia

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"
	"govoice/core"
	"govoice/core/netpool"
	"govoice/core/resilience"
	"govoice/core/ttsunit"
)

const (
	// defaultVoice is Cartesia's public "Sarah" voice; agents normally set one.
	defaultVoice = "694f9389-aac1-45b6-b726-9d9369183238"
)

// ttsVersion is the Cartesia-Version header sent on the TTS websocket.
const ttsVersion = "2025-04-16"

// TTSConfig configures the Cartesia processor.
type TTSConfig struct {
	Endpoint   string // default "wss://api.cartesia.ai/tts/websocket"
	Model      string // default "sonic-3"
	Voice      string // voice id
	Language   string // default "en"
	SampleRate int    // default 24000; output is mono pcm_s16le
	APIKey     string
	// Timeout bounds one synthesis request (dial through "done").
	Timeout        time.Duration
	RetryPolicy    resilience.Policy
	CircuitBreaker *resilience.Breaker
	// Pool dials the websocket; default is the process-wide pool (core/netpool).
	Pool *netpool.Pool
}

func DefaultTTSConfig() TTSConfig {
	return TTSConfig{
		Endpoint: "wss://api.cartesia.ai/tts/websocket", Model: "sonic-3",
		Voice: defaultVoice, Language: "en", SampleRate: 24000, Timeout: 15 * time.Second,
		RetryPolicy: resilience.DefaultPolicy(),
	}
}

// TTSProcessor turns SynthesisUnitFrames into 20 ms OutputAudioFrames, with
// word timings on the frames. One websocket and context per reply (units continue the context); units play in order
// (the shared machinery lives in core/ttsunit).
type TTSProcessor struct {
	*ttsunit.Runner
	cfg  TTSConfig
	slot ttsunit.ReplySlot[*session]
}

func NewTTSProcessor(cfg TTSConfig) (*TTSProcessor, error) {
	if cfg.APIKey == "" {
		return nil, ErrMissingAPIKey
	}
	def := DefaultTTSConfig()
	if cfg.Endpoint == "" {
		cfg.Endpoint = def.Endpoint
	}
	if cfg.Model == "" {
		cfg.Model = def.Model
	}
	if cfg.Voice == "" {
		cfg.Voice = def.Voice
	}
	if cfg.Language == "" {
		cfg.Language = def.Language
	}
	if cfg.SampleRate <= 0 {
		cfg.SampleRate = def.SampleRate
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = def.Timeout
	}
	if cfg.RetryPolicy.MaxAttempts <= 0 {
		cfg.RetryPolicy.MaxAttempts = def.RetryPolicy.MaxAttempts
	}
	cfg.RetryPolicy.AttemptTimeout = cfg.Timeout // Timeout is the one knob
	if cfg.Pool == nil {
		cfg.Pool = netpool.Get(netpool.Options{Provider: "cartesia", Stage: "tts", HTTP1Only: true})
	}
	p := &TTSProcessor{cfg: cfg}
	p.slot.Close = (*session).abort
	p.Runner = ttsunit.New(ttsunit.Config{
		Provider: "cartesia", Model: cfg.Model, SampleRate: cfg.SampleRate,
		Breaker: cfg.CircuitBreaker, Policy: cfg.RetryPolicy, Classify: classify, EndReply: p.slot.Drop,
		Attempt: func(ctx context.Context, u *ttsunit.Unit, attempt int) error {
			return (&unitRun{p: p, Unit: u}).attempt(ctx, attempt)
		},
	})
	return p, nil
}

// Warm pre-dials TCP+TLS only. The reply's websocket is dialed
// with its first unit, so a speculative websocket would not be reusable; the transport spare is anonymous and unbilled. Skipped while the
// breaker is open; never reports to it.
func (p *TTSProcessor) Warm(ctx context.Context) error {
	if br := p.cfg.CircuitBreaker; br != nil && br.WouldRefuse() {
		p.cfg.Pool.NoteSkipped()
		return nil
	}
	return p.cfg.Pool.Warm(ctx, p.cfg.Endpoint)
}

// ---- wire format ----

type request struct {
	ModelID    string            `json:"model_id"`
	Transcript string            `json:"transcript"`
	Voice      map[string]string `json:"voice"`
	Language   string            `json:"language,omitempty"`
	ContextID  string            `json:"context_id"`
	Output     outputFormat      `json:"output_format"`
	Timestamps bool              `json:"add_timestamps"`
	Continue   bool              `json:"continue"`
	Flush      bool              `json:"flush,omitempty"`
	// MaxBufferDelayMs 0: units are whole sentences, so generate immediately.
	MaxBufferDelayMs int `json:"max_buffer_delay_ms"`
}

type outputFormat struct {
	Container  string `json:"container"`
	Encoding   string `json:"encoding"`
	SampleRate int    `json:"sample_rate"`
}

type message struct {
	Type           string `json:"type"`
	ContextID      string `json:"context_id"`
	Data           string `json:"data"`
	Error          string `json:"error"`   // Cartesia-Version before 2026-03-01
	Message        string `json:"message"` // 2026-03-01 and later
	StatusCode     int    `json:"status_code"`
	WordTimestamps *struct {
		Words []string  `json:"words"`
		Start []float64 `json:"start"`
		End   []float64 `json:"end"`
	} `json:"word_timestamps"`
}

func seconds(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// ---- synthesis ----

// session is the websocket and context shared by the units of one reply.
type session struct {
	conn  *websocket.Conn
	ctxID string
	idle  time.Time // last flush_done; read only by the next unit (units run in order)
}

// contextTTL: Cartesia expires a context 1 s after its last audio output, and
// reusing an expired context is unsupported. Past this idle gap the next unit
// opens a fresh context on the same socket (losing only prosody continuity).
// ponytail: fixed margin for latency, tune if expiry errors show up live.
const contextTTL = 700 * time.Millisecond

// abort cancels the context and hangs up (barge-in, error, stale reply).
func (s *session) abort() {
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	b, _ := json.Marshal(map[string]any{"context_id": s.ctxID, "cancel": true})
	_ = s.conn.Write(ctx, websocket.MessageText, b)
	s.conn.CloseNow()
}

// unitRun is one attempt's provider-side state.
type unitRun struct {
	p *TTSProcessor
	*ttsunit.Unit
}

func (u *unitRun) dial(ctx context.Context) (*websocket.Conn, error) {
	cfg := u.p.cfg
	opts := &websocket.DialOptions{HTTPClient: u.p.cfg.Pool.Client(0), HTTPHeader: http.Header{"X-API-Key": {cfg.APIKey}, "Cartesia-Version": {ttsVersion}}}
	c, resp, err := websocket.Dial(ctx, cfg.Endpoint, opts)
	if err != nil {
		return nil, MapError("tts", err, resp)
	}
	c.SetReadLimit(1 << 20)
	return c, nil
}

// attempt synthesizes one unit on the reply's context: every unit but the last
// is sent with continue:true and a flush, whose flush_done ends the unit; the
// last is sent with continue:false and ends on done. A retry starts a fresh
// socket and context.
func (u *unitRun) attempt(ctx context.Context, attempt int) (err error) {
	cfg := u.p.cfg
	slot := &u.p.slot
	s, ok := slot.Get(u.Frame)
	if ok && attempt > 0 {
		slot.Drop()
		ok = false
	}
	ctxID := "u" + strconv.FormatUint(u.Frame.UnitID, 10) + "-" + strconv.Itoa(attempt)
	if ok && time.Since(s.idle) > contextTTL { // context expired: new one, same socket
		slot.Release()
		s = &session{conn: s.conn, ctxID: ctxID}
		slot.Put(u.Frame, s)
	}
	if !ok {
		conn, err := u.dial(ctx)
		if err != nil {
			return err
		}
		s = &session{conn: conn, ctxID: ctxID}
		slot.Put(u.Frame, s)
	}
	defer func() {
		if err != nil {
			slot.Drop()
		}
	}()
	conn := s.conn
	// Reads must outlive ctx so that abort can still send the cancel message:
	// a canceled read ctx would close the socket first.
	rctx := context.WithoutCancel(ctx)
	defer context.AfterFunc(ctx, slot.Drop)()
	fail := func(err error) error { // report the cause (timeout/cancel), not the abort's hang-up
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return MapError("tts", err, nil)
	}

	req := request{
		ModelID: cfg.Model, Transcript: u.Frame.Text, Voice: map[string]string{"mode": "id", "id": cfg.Voice},
		Language: cfg.Language, ContextID: s.ctxID, Continue: !u.Frame.IsFinal,
		Output:     outputFormat{Container: "raw", Encoding: "pcm_s16le", SampleRate: cfg.SampleRate},
		Timestamps: true,
	}
	msgs := []request{req}
	if !u.Frame.IsFinal { // the flush marker repeats the fixed fields with an empty transcript
		fl := req
		fl.Transcript, fl.Flush = "", true
		msgs = append(msgs, fl)
	}
	for _, m := range msgs {
		body, _ := json.Marshal(m)
		if err := conn.Write(rctx, websocket.MessageText, body); err != nil {
			return fail(err)
		}
	}
	for {
		_, data, err := conn.Read(rctx)
		if err != nil {
			return fail(err)
		}
		var m message
		if json.Unmarshal(data, &m) != nil || (m.ContextID != "" && m.ContextID != s.ctxID) {
			continue // e.g. a late done for an expired context
		}
		switch m.Type {
		case "chunk":
			pcm, err := base64.StdEncoding.DecodeString(m.Data)
			if err != nil {
				return &ProviderError{Capability: "tts", Code: "BAD_RESPONSE", Message: "undecodable audio chunk", Retryable: true, Cause: err}
			}
			if err := u.Write(pcm); err != nil {
				return err
			}
		case "timestamps":
			if w := m.WordTimestamps; w != nil {
				for i, word := range w.Words {
					if i < len(w.Start) && i < len(w.End) {
						u.Timings = append(u.Timings, core.WordTiming{Word: word, Start: seconds(w.Start[i]), End: seconds(w.End[i])})
					}
				}
			}
		case "flush_done":
			s.idle = time.Now()
			return u.Finish() // the reply's context stays open for the next unit
		case "done": // the context ended (last unit, or the server closed it early)
			slot.Release()
			conn.CloseNow()
			return u.Finish()
		case "error":
			if m.Error == "" {
				m.Error = m.Message
			}
			return messageError("tts", m.StatusCode, m.Error)
		}
	}
}
