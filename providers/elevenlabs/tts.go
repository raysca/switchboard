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
	"time"

	"github.com/coder/websocket"
	"govoice/core"
	"govoice/core/netpool"
	"govoice/core/resilience"
	"govoice/core/ttsunit"
)

const (
	// defaultVoice is ElevenLabs' public "Rachel" voice; agents normally set one.
	defaultVoice = "21m00Tcm4TlvDq8ikWAM"
)

// ErrInvalidConfig marks a configuration the API can never accept.
var ErrInvalidConfig = errors.New("elevenlabs: invalid config")

// pcmRates are the documented pcm_<rate> output formats (44100+ need a Pro plan).
var pcmRates = map[int]bool{8000: true, 16000: true, 22050: true, 24000: true, 32000: true, 44100: true, 48000: true}

// TTSConfig configures the ElevenLabs processor.
type TTSConfig struct {
	Endpoint   string // base URL, default "wss://api.elevenlabs.io" (regional hosts also work)
	Model      string // default "eleven_flash_v2_5"
	Voice      string // voice id
	SampleRate int    // default 24000; output is mono pcm_s16le (pcm_<rate>)
	// Optional voice settings, sent in the opening message when non-nil.
	Stability, SimilarityBoost, Style, Speed *float64
	APIKey                                   string
	// Timeout bounds one synthesis request (dial through "isFinal").
	Timeout        time.Duration
	RetryPolicy    resilience.Policy
	CircuitBreaker *resilience.Breaker
	// Pool dials the websocket; default is the process-wide pool (core/netpool).
	Pool *netpool.Pool
}

func DefaultTTSConfig() TTSConfig {
	return TTSConfig{
		Endpoint: "wss://api.elevenlabs.io", Model: "eleven_flash_v2_5",
		Voice: defaultVoice, SampleRate: 24000, Timeout: 15 * time.Second,
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
	if cfg.Model == "" {
		cfg.Model = def.Model
	}
	if cfg.Voice == "" {
		cfg.Voice = def.Voice
	}
	if cfg.SampleRate == 0 {
		cfg.SampleRate = def.SampleRate
	}
	if !pcmRates[cfg.SampleRate] {
		return nil, fmt.Errorf("%w: sample rate %d is not an ElevenLabs pcm output (8000, 16000, 22050, 24000, 32000, 44100, 48000)", ErrInvalidConfig, cfg.SampleRate)
	}
	if _, err := url.Parse(cfg.Endpoint); err != nil {
		return nil, fmt.Errorf("%w: endpoint: %v", ErrInvalidConfig, err)
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = def.Timeout
	}
	if cfg.RetryPolicy.MaxAttempts <= 0 {
		cfg.RetryPolicy.MaxAttempts = def.RetryPolicy.MaxAttempts
	}
	cfg.RetryPolicy.AttemptTimeout = cfg.Timeout // Timeout is the one knob
	if cfg.Pool == nil {
		cfg.Pool = netpool.Get(netpool.Options{Provider: "elevenlabs", Stage: "tts", HTTP1Only: true})
	}
	p := &TTSProcessor{cfg: cfg}
	p.Runner = ttsunit.New(ttsunit.Config{
		Provider: "elevenlabs", Model: cfg.Model, SampleRate: cfg.SampleRate,
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

// clientMsg covers the opening (BOS), text, and closing (EOS) messages.
type clientMsg struct {
	Text                 string         `json:"text"`
	VoiceSettings        map[string]any `json:"voice_settings,omitempty"`
	TryTriggerGeneration bool           `json:"try_trigger_generation,omitempty"`
}

type alignment struct {
	Chars      []string  `json:"chars"`
	StartTimes []float64 `json:"charStartTimesMs"`
	Durations  []float64 `json:"charDurationsMs"`
}

type message struct {
	Audio               string     `json:"audio"`
	IsFinal             bool       `json:"isFinal"`
	Alignment           *alignment `json:"alignment"`
	NormalizedAlignment *alignment `json:"normalizedAlignment"`
	Error               string     `json:"error"`
	Message             string     `json:"message"`
	Code                int        `json:"code"`
}

func ms(v float64) time.Duration { return time.Duration(v * float64(time.Millisecond)) }

func (c TTSConfig) voiceSettings() map[string]any {
	m := map[string]any{}
	for k, v := range map[string]*float64{"stability": c.Stability, "similarity_boost": c.SimilarityBoost, "style": c.Style, "speed": c.Speed} {
		if v != nil {
			m[k] = *v
		}
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

func (c TTSConfig) url() string {
	u, _ := url.Parse(c.Endpoint)
	u.Path = strings.TrimRight(u.Path, "/") + "/v1/text-to-speech/" + url.PathEscape(c.Voice) + "/stream-input"
	u.RawQuery = url.Values{"model_id": {c.Model}, "output_format": {"pcm_" + strconv.Itoa(c.SampleRate)}, "sync_alignment": {"true"}}.Encode()
	return u.String()
}

// ---- synthesis ----

// unitRun is one attempt's provider-side state.
type unitRun struct {
	p *TTSProcessor
	*ttsunit.Unit
	recv   int // audio bytes received (alignment is chunk-relative; this is the offset)
	word   strings.Builder
	wStart time.Duration
	wEnd   time.Duration
}

func (u *unitRun) dial(ctx context.Context) (*websocket.Conn, error) {
	cfg := u.p.cfg
	opts := &websocket.DialOptions{HTTPClient: u.p.cfg.Pool.Client(0), HTTPHeader: http.Header{"xi-api-key": {cfg.APIKey}}}
	c, resp, err := websocket.Dial(ctx, cfg.url(), opts)
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

	// BOS, the text (trailing space per the docs), then EOS to flush and finish.
	for _, m := range []clientMsg{
		{Text: " ", VoiceSettings: cfg.voiceSettings()},
		{Text: u.Frame.Text + " ", TryTriggerGeneration: true},
		{Text: ""},
	} {
		body, _ := json.Marshal(m)
		if err := conn.Write(ctx, websocket.MessageText, body); err != nil {
			return MapError("tts", err, nil)
		}
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
		if m.Error != "" || (m.Message != "" && m.Audio == "" && !m.IsFinal) {
			return messageError(m.Error, m.Message, m.Code)
		}
		// Prefer the original-text alignment: its words match the LLM text in the
		// assistant context ("$5", not "five dollars"), which is what timings reconcile against.
		if a := m.Alignment; a != nil {
			u.align(a)
		} else if m.NormalizedAlignment != nil {
			u.align(m.NormalizedAlignment)
		}
		if m.Audio != "" {
			pcm, err := base64.StdEncoding.DecodeString(m.Audio)
			if err != nil {
				return &ProviderError{Capability: "tts", Code: "BAD_RESPONSE", Message: "undecodable audio chunk", Retryable: true, Cause: err}
			}
			u.recv += len(pcm)
			if err := u.Write(pcm); err != nil {
				return err
			}
		}
		if m.IsFinal {
			u.flushWord()
			return u.Finish()
		}
	}
}

// align groups characters into words. Times are relative to the chunk they
// arrived with, so they are offset by the audio received before this message;
// a word may span messages.
func (u *unitRun) align(a *alignment) {
	off := time.Duration(u.recv/2) * time.Second / time.Duration(u.p.cfg.SampleRate)
	for i, c := range a.Chars {
		if i >= len(a.StartTimes) || i >= len(a.Durations) {
			break
		}
		if strings.TrimSpace(c) == "" {
			u.flushWord()
			continue
		}
		if u.word.Len() == 0 {
			u.wStart = off + ms(a.StartTimes[i])
		}
		u.word.WriteString(c)
		u.wEnd = off + ms(a.StartTimes[i]+a.Durations[i])
	}
}

func (u *unitRun) flushWord() {
	if u.word.Len() > 0 {
		u.Timings = append(u.Timings, core.WordTiming{Word: u.word.String(), Start: u.wStart, End: u.wEnd})
		u.word.Reset()
	}
}
