package google

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"strings"
	"time"
	"unicode"

	"govoice/core/netpool"
	"govoice/core/resilience"
	"govoice/core/ttsunit"

	texttospeech "cloud.google.com/go/texttospeech/apiv1"
	texttospeechpb "cloud.google.com/go/texttospeech/apiv1/texttospeechpb"
)

// TTSConfig configures Google Cloud Text-to-Speech V1.
type TTSConfig struct {
	Voice                 string // Default: "en-US-Chirp3-HD-Aoede" (streams); other voices use the unary request
	LanguageCode          string // Used when the voice name has no xx-YY prefix (default "en-US")
	SampleRate            int    // Pinned: 24000 Hz
	MaxSynthesisUnitChars int    // Pinned: 250
	CredentialsFile       string
	CredentialsJSON       []byte // In-memory service account JSON; preferred over CredentialsFile/APIKey
	APIKey                string
	Client                TTSClient
	RetryPolicy           resilience.Policy
	CircuitBreaker        *resilience.Breaker
	Provider              string // label on frames, usage and traces; empty means "google"
}

// DefaultVoice is the voice agents get when none is set.
const DefaultVoice = "en-US-Chirp3-HD-Aoede"

// DefaultTTSConfig returns configuration matching v1-lock.json.
func DefaultTTSConfig() TTSConfig {
	return TTSConfig{
		Voice:                 DefaultVoice,
		LanguageCode:          "en-US",
		SampleRate:            24000,
		MaxSynthesisUnitChars: 250,
		RetryPolicy:           ttsRetryPolicy(),
		CircuitBreaker:        resilience.NewBreaker(resilience.DefaultConfig()),
	}
}

// TTSClient abstracts synchronous speech synthesis.
type TTSClient interface {
	SynthesizeSpeech(ctx context.Context, text string, voice string, sampleRate int) ([]byte, error)
}

// RealGoogleTTSClient wraps cloud.google.com/go/texttospeech/apiv1.
type RealGoogleTTSClient struct {
	client       *texttospeech.Client
	languageCode string
}

func NewRealGoogleTTSClient(ctx context.Context, cfg TTSConfig) (*RealGoogleTTSClient, error) {
	c, err := texttospeech.NewClient(ctx, append(clientAuthOptions(cfg.CredentialsJSON, cfg.CredentialsFile, cfg.APIKey), grpcKeepalive())...)
	if err != nil {
		return nil, MapError("tts", err)
	}
	return &RealGoogleTTSClient{client: c, languageCode: cfg.LanguageCode}, nil
}

// voiceLanguage returns the language a Google voice name starts with
// ("en-GB-Neural2-A" -> "en-GB"), else fallback, else "en-US".
func voiceLanguage(voice, fallback string) string {
	if parts := strings.SplitN(voice, "-", 3); len(parts) == 3 {
		return parts[0] + "-" + parts[1]
	}
	if fallback != "" {
		return fallback
	}
	return "en-US"
}

func (c *RealGoogleTTSClient) SynthesizeSpeech(ctx context.Context, text string, voice string, sampleRate int) ([]byte, error) {
	req := &texttospeechpb.SynthesizeSpeechRequest{
		Input: &texttospeechpb.SynthesisInput{
			InputSource: &texttospeechpb.SynthesisInput_Text{Text: text},
		},
		Voice: &texttospeechpb.VoiceSelectionParams{
			LanguageCode: voiceLanguage(voice, c.languageCode),
			Name:         voice,
		},
		AudioConfig: &texttospeechpb.AudioConfig{
			// Journey voices reject AudioEncoding_PCM. LINEAR16 is headered WAV;
			// strip the container so the pipeline still sees raw PCM16LE.
			AudioEncoding:   texttospeechpb.AudioEncoding_LINEAR16,
			SampleRateHertz: int32(sampleRate),
		},
	}

	netpool.CountRequest("google", "tts")
	resp, err := c.client.SynthesizeSpeech(ctx, req)
	if err != nil {
		return nil, MapError("tts", err)
	}
	pcm, err := stripWAV(resp.AudioContent)
	if err != nil {
		return nil, err
	}
	return pcm, nil
}

func stripWAV(b []byte) ([]byte, error) {
	if len(b) < 12 || !bytes.Equal(b[:4], []byte("RIFF")) {
		return b, nil
	}
	idx := bytes.Index(b, []byte("data"))
	if idx < 0 || idx+8 > len(b) {
		return nil, fmt.Errorf("google tts LINEAR16 response is missing a WAV data chunk")
	}
	n := binary.LittleEndian.Uint32(b[idx+4 : idx+8])
	start := idx + 8
	if start+int(n) > len(b) {
		return nil, fmt.Errorf("google tts LINEAR16 WAV data is truncated")
	}
	return append([]byte(nil), b[start:start+int(n)]...), nil
}

func (c *RealGoogleTTSClient) Close() error {
	if c.client != nil {
		return c.client.Close()
	}
	return nil
}

// TTSProcessor converts SynthesisUnitFrame into raw OutputAudioFrame chunks with the unary request;
// scheduling, ordering, retries and framing live in core/ttsunit.
type TTSProcessor struct {
	*ttsunit.Runner
	cfg TTSConfig
}

// normalizeTTSConfig fills the defaults the unary and streaming processors share.
func normalizeTTSConfig(cfg TTSConfig) TTSConfig {
	if cfg.Provider == "" {
		cfg.Provider = "google"
	}
	if cfg.Voice == "" {
		cfg.Voice = DefaultVoice
	}
	if cfg.SampleRate <= 0 {
		cfg.SampleRate = 24000
	}
	if cfg.MaxSynthesisUnitChars <= 0 {
		cfg.MaxSynthesisUnitChars = 250
	}
	if cfg.CircuitBreaker == nil {
		cfg.CircuitBreaker = resilience.NewBreaker(resilience.DefaultConfig())
	}
	return cfg
}

// ttsLookAhead is how many units past the one being emitted may already be
// synthesizing (bounds provider concurrency and buffered audio per call).
const ttsLookAhead = 2

// NewTTSProcessor creates a new TTSProcessor.
func NewTTSProcessor(cfg TTSConfig) (*TTSProcessor, error) {
	cfg = normalizeTTSConfig(cfg)
	p := &TTSProcessor{cfg: cfg}
	p.Runner = ttsunit.New(ttsunit.Config{
		Provider: cfg.Provider, Model: cfg.Voice, SampleRate: cfg.SampleRate,
		Breaker: cfg.CircuitBreaker, Policy: cfg.RetryPolicy, Classify: classify,
		Attempt: p.attempt, LookAhead: ttsLookAhead,
	})
	return p, nil
}

// attempt synthesizes the unit's pieces with the unary request and plays them as one unit.
func (p *TTSProcessor) attempt(ctx context.Context, u *ttsunit.Unit, _ int) error {
	if p.cfg.Client == nil {
		return nil // ponytail: construction-without-client guard (flush-ack test); emits nothing, as before
	}
	for _, piece := range splitUnit(u.Frame.Text, p.cfg.MaxSynthesisUnitChars) {
		pcm, err := p.cfg.Client.SynthesizeSpeech(ctx, piece, p.cfg.Voice, p.cfg.SampleRate)
		if err != nil {
			return MapError("tts", err) // R5: error frames keep carrying *ProviderError
		}
		if err := u.Write(pcm); err != nil {
			return err
		}
	}
	return u.Finish()
}

// Close cancels in-flight synthesis and closes the client.
func (p *TTSProcessor) Close() error {
	err := p.Runner.Close()
	if c, ok := p.cfg.Client.(interface{ Close() error }); ok {
		_ = c.Close()
	}
	return err
}

// splitUnit cuts text into pieces of at most max runes, at the last space
// when there is one, so a long unit is synthesized piece by piece.
func splitUnit(text string, max int) []string {
	r := []rune(text)
	var pieces []string
	for len(r) > max {
		cut := max
		for i := max - 1; i > 0; i-- {
			if unicode.IsSpace(r[i]) {
				cut = i
				break
			}
		}
		pieces = appendPiece(pieces, r[:cut])
		r = r[cut:]
	}
	return appendPiece(pieces, r)
}

// appendPiece adds the trimmed piece unless it is only whitespace (TTS rejects
// empty input).
func appendPiece(pieces []string, r []rune) []string {
	if s := strings.TrimSpace(string(r)); s != "" {
		return append(pieces, s)
	}
	return pieces
}

// ttsRetryPolicy bounds one synthesis request (at most MaxSynthesisUnitChars)
// tighter than an LLM stream.
func ttsRetryPolicy() resilience.Policy {
	p := resilience.DefaultPolicy()
	p.AttemptTimeout = 10 * time.Second
	return p
}
