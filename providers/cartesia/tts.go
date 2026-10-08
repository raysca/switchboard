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

// TTSConfig configures the Cartesia processor.
type TTSConfig struct {
	Endpoint   string // default "wss://api.cartesia.ai/tts/websocket"
	Version    string // Cartesia-Version header, default "2025-04-16"
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
		Endpoint: "wss://api.cartesia.ai/tts/websocket", Version: "2025-04-16", Model: "sonic-3",
		Voice: defaultVoice, Language: "en", SampleRate: 24000, Timeout: 15 * time.Second,
		RetryPolicy: resilience.DefaultPolicy(),
	}
}

// TTSProcessor turns SynthesisUnitFrames into 20 ms OutputAudioFrames, with
// word timings on the frames. One websocket per unit; units play in order
// (the shared machinery lives in core/ttsunit).
type TTSProcessor struct {
	*ttsunit.Runner
	cfg TTSConfig
}

func NewTTSProcessor(cfg TTSConfig) (*TTSProcessor, error) {
	if cfg.APIKey == "" {
		return nil, ErrMissingAPIKey
	}
	def := DefaultTTSConfig()
	if cfg.Endpoint == "" {
		cfg.Endpoint = def.Endpoint
	}
	if cfg.Version == "" {
		cfg.Version = def.Version
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
	p.Runner = ttsunit.New(ttsunit.Config{
		Provider: "cartesia", Model: cfg.Model, SampleRate: cfg.SampleRate,
		Breaker: cfg.CircuitBreaker, Policy: cfg.RetryPolicy, Classify: classify,
		Attempt: func(ctx context.Context, u *ttsunit.Unit, attempt int) error {
			return (&unitRun{p: p, Unit: u}).attempt(ctx, attempt)
		},
	})
	return p, nil
}

// Warm pre-dials TCP+TLS only. Each synthesis unit owns its websocket
// (a context/session per unit), so a speculative websocket would not be
// reusable; the transport spare is anonymous and unbilled. Skipped while the
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
}

type outputFormat struct {
	Container  string `json:"container"`
	Encoding   string `json:"encoding"`
	SampleRate int    `json:"sample_rate"`
}

type message struct {
	Type           string `json:"type"`
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

// unitRun is one attempt's provider-side state.
type unitRun struct {
	p *TTSProcessor
	*ttsunit.Unit
}

func (u *unitRun) dial(ctx context.Context) (*websocket.Conn, error) {
	cfg := u.p.cfg
	opts := &websocket.DialOptions{HTTPClient: u.p.cfg.Pool.Client(0), HTTPHeader: http.Header{"X-API-Key": {cfg.APIKey}, "Cartesia-Version": {cfg.Version}}}
	c, resp, err := websocket.Dial(ctx, cfg.Endpoint, opts)
	if err != nil {
		return nil, MapError("tts", err, resp)
	}
	c.SetReadLimit(1 << 20)
	return c, nil
}

func (u *unitRun) attempt(ctx context.Context, attempt int) error {
	cfg := u.p.cfg
	conn, err := u.dial(ctx)
	if err != nil {
		return err
	}
	defer conn.CloseNow()

	req := request{
		ModelID: cfg.Model, Transcript: u.Frame.Text, Voice: map[string]string{"mode": "id", "id": cfg.Voice},
		Language: cfg.Language, ContextID: "u" + strconv.FormatUint(u.Frame.UnitID, 10) + "-" + strconv.Itoa(attempt),
		Output:     outputFormat{Container: "raw", Encoding: "pcm_s16le", SampleRate: cfg.SampleRate},
		Timestamps: true,
	}
	body, _ := json.Marshal(req)
	if err := conn.Write(ctx, websocket.MessageText, body); err != nil {
		return MapError("tts", err, nil)
	}
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return MapError("tts", err, nil)
		}
		var m message
		if json.Unmarshal(data, &m) != nil {
			continue
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
		case "done":
			return u.Finish()
		case "error":
			if m.Error == "" {
				m.Error = m.Message
			}
			return messageError("tts", m.StatusCode, m.Error)
		}
	}
}
