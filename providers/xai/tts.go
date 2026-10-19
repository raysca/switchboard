//go:build xaispeech

package xai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"govoice/core/netpool"
	"govoice/core/resilience"
	"govoice/core/ttsunit"
)

// TTSConfig holds configuration for the xAI Text-to-Speech processor.
type TTSConfig struct {
	Endpoint       string        // Default: "https://api.x.ai/v1/tts"
	Voice          string        // Default: "eve" (e.g. "ara", "eve", "leo", "rex", "sal")
	Language       string        // Default: "en"
	SampleRate     int           // Default: 24000
	APIKey         string        // Bearer token
	Timeout        time.Duration // Default: 10s
	CircuitBreaker *resilience.Breaker
	RetryPolicy    resilience.Policy
	Client         TTSClient
}

// DefaultTTSConfig returns recommended default settings for xAI TTS.
func DefaultTTSConfig() TTSConfig {
	return TTSConfig{
		Endpoint:    "https://api.x.ai/v1/tts",
		Voice:       "eve",
		Language:    "en",
		SampleRate:  24000,
		Timeout:     10 * time.Second,
		RetryPolicy: resilience.DefaultPolicy(),
	}
}

// TTSClient abstracts HTTP synthesis execution for xAI.
type TTSClient interface {
	Synthesize(ctx context.Context, text string, cfg TTSConfig) ([]byte, error)
}

// RealXAITTSClient implements TTSClient against https://api.x.ai/v1/tts.
type RealXAITTSClient struct {
	httpClient *http.Client
}

// NewRealXAITTSClient creates an HTTP client for xAI TTS.
func NewRealXAITTSClient() *RealXAITTSClient {
	return &RealXAITTSClient{httpClient: ttsPool().Client(15 * time.Second)}
}

func ttsPool() *netpool.Pool { return netpool.Get(netpool.Options{Provider: "xai", Stage: "tts"}) }

// Warm pre-dials TCP+TLS to the endpoint when the real HTTP client is used.
func (p *TTSProcessor) Warm(ctx context.Context) error {
	if _, ok := p.client.(*RealXAITTSClient); !ok {
		return nil
	}
	if br := p.cfg.CircuitBreaker; br != nil && br.WouldRefuse() {
		ttsPool().NoteSkipped()
		return nil
	}
	return ttsPool().Warm(ctx, p.cfg.Endpoint)
}

type ttsOutputFormat struct {
	Codec      string `json:"codec"`
	SampleRate int    `json:"sample_rate"`
}

type ttsRequestBody struct {
	Text         string          `json:"text"`
	VoiceID      string          `json:"voice_id"`
	Language     string          `json:"language"`
	OutputFormat ttsOutputFormat `json:"output_format"`
}

func (c *RealXAITTSClient) Synthesize(ctx context.Context, text string, cfg TTSConfig) ([]byte, error) {
	sr := cfg.SampleRate
	if sr <= 0 {
		sr = 24000
	}
	voice := cfg.Voice
	if voice == "" {
		voice = "eve"
	}
	lang := cfg.Language
	if lang == "" {
		lang = "en"
	}

	reqPayload := ttsRequestBody{
		Text:     text,
		VoiceID:  voice,
		Language: lang,
		OutputFormat: ttsOutputFormat{
			Codec:      "pcm",
			SampleRate: sr,
		},
	}

	bodyBytes, err := json.Marshal(reqPayload)
	if err != nil {
		return nil, fmt.Errorf("marshal tts request: %w", err)
	}

	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = "https://api.x.ai/v1/tts"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create tts request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")

	// A transport failure (refused, reset, DNS, TLS, truncated body) is a
	// retryable outage, not a client error: nil response classes it retryable.
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, MapHTTPError("tts", nil, "", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, MapHTTPError("tts", resp, string(respBody), nil)
	}

	pcmBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, MapHTTPError("tts", nil, "", err)
	}
	return pcmBytes, nil
}

// TTSProcessor converts SynthesisUnitFrame into raw OutputAudioFrame chunks using xAI, one request
// per unit; scheduling, ordering, retries and framing live in core/ttsunit.
type TTSProcessor struct {
	*ttsunit.Runner
	cfg    TTSConfig
	client TTSClient
}

// normalizeTTSConfig fills the defaults the unary and streaming processors share.
func normalizeTTSConfig(cfg TTSConfig) TTSConfig {
	if cfg.Endpoint == "" {
		cfg.Endpoint = "https://api.x.ai/v1/tts"
	}
	if cfg.Voice == "" {
		cfg.Voice = "eve"
	}
	if cfg.Language == "" {
		cfg.Language = "en" // the WebSocket requires a language
	}
	if cfg.SampleRate <= 0 {
		cfg.SampleRate = 24000
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.RetryPolicy.MaxAttempts <= 0 {
		cfg.RetryPolicy.MaxAttempts = resilience.DefaultPolicy().MaxAttempts
	}
	cfg.RetryPolicy.AttemptTimeout = cfg.Timeout // Timeout is the one knob: it bounds each synthesis request
	return cfg
}

// NewTTSProcessor creates a new xAI TTSProcessor. Close never closes the client: the real one
// shares the xAI netpool.
func NewTTSProcessor(cfg TTSConfig) (*TTSProcessor, error) {
	cfg = normalizeTTSConfig(cfg)
	client := cfg.Client
	if client == nil {
		client = NewRealXAITTSClient()
	}
	p := &TTSProcessor{cfg: cfg, client: client}
	p.Runner = ttsunit.New(ttsunit.Config{
		Provider: "xai", Model: cfg.Voice, SampleRate: cfg.SampleRate,
		Breaker: cfg.CircuitBreaker, Policy: cfg.RetryPolicy, Classify: classify, Attempt: p.attempt,
	})
	return p, nil
}

func (p *TTSProcessor) attempt(ctx context.Context, u *ttsunit.Unit, _ int) error {
	pcm, err := p.client.Synthesize(ctx, u.Frame.Text, p.cfg)
	if err != nil {
		return err
	}
	if err := u.Write(pcm); err != nil {
		return err
	}
	return u.Finish()
}
