package nvidia

import (
	"context"
	"io"
	"time"

	"google.golang.org/grpc"

	"govoice/core/netpool"
	"govoice/core/resilience"
	"govoice/core/transport/resample"
	"govoice/core/ttsunit"
	"govoice/providers/nvidia/rivapb"
)

// TTSConfig configures the Riva synthesizer
// (nvidia.riva.tts.RivaSpeechSynthesis/SynthesizeOnline).
type TTSConfig struct {
	Endpoint     string // host:port; default DefaultEndpoint (NVCF)
	Insecure     bool   // plaintext gRPC for self-hosted Riva; the API key is then never sent
	APIKey       string
	FunctionID   string // NVCF function-id of the hosted Riva/Magpie TTS function
	AllowPrivate bool   // permit loopback/private endpoints (self-hosted Riva)
	Language     string // default "en-US"
	Voice        string // Riva voice name; default Magpie "Magpie-Multilingual.EN-US.Aria"
	Model        string // label for usage metrics only (the voice picks the model)
	// SampleRate is the rate of the frames this processor emits (the pipeline's).
	SampleRate int // default 24000
	// RivaRate is the rate requested from Riva; zero means SampleRate. Riva
	// resamples from the model's native rate (22050 Hz for Magpie) itself, as
	// Pipecat relies on. Set it only for a server that refuses SampleRate: the
	// local resampler is linear with no anti-alias filter, so a RivaRate above
	// SampleRate aliases audibly (about -2 to -6 dB at 22050->16000/8000).
	RivaRate int

	// Timeout bounds one synthesis request (stream open through the last audio).
	Timeout        time.Duration
	RetryPolicy    resilience.Policy
	CircuitBreaker *resilience.Breaker
	// Conn is an existing channel (tests); the processor does not close it.
	Conn *grpc.ClientConn
}

func DefaultTTSConfig() TTSConfig {
	return TTSConfig{
		Endpoint: DefaultEndpoint, Language: "en-US", Voice: "Magpie-Multilingual.EN-US.Aria", Model: "riva-tts",
		SampleRate: 24000, Timeout: 15 * time.Second, RetryPolicy: resilience.DefaultPolicy(),
	}
}

// TTSProcessor turns SynthesisUnitFrames into 20 ms OutputAudioFrames. One
// server-streaming RPC per unit on a per-processor channel; the shared unit
// machinery lives in core/ttsunit. Riva returns no word timestamps, so
// WordTimings stay empty (the heard-text tracker estimates proportionally).
type TTSProcessor struct {
	*ttsunit.Runner
	cfg    TTSConfig
	conn   *Conn
	client rivapb.RivaSpeechSynthesisClient
}

func NewTTSProcessor(cfg TTSConfig) (*TTSProcessor, error) {
	def := DefaultTTSConfig()
	if cfg.Endpoint == "" {
		cfg.Endpoint = def.Endpoint
	}
	if cfg.Language == "" {
		cfg.Language = def.Language
	}
	if cfg.Voice == "" {
		cfg.Voice = def.Voice
	}
	if cfg.Model == "" {
		cfg.Model = def.Model
	}
	if cfg.SampleRate <= 0 {
		cfg.SampleRate = def.SampleRate
	}
	if cfg.RivaRate <= 0 {
		cfg.RivaRate = cfg.SampleRate
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = def.Timeout
	}
	if cfg.RetryPolicy.MaxAttempts <= 0 {
		cfg.RetryPolicy.MaxAttempts = def.RetryPolicy.MaxAttempts
	}
	cfg.RetryPolicy.AttemptTimeout = cfg.Timeout // Timeout is the one knob
	if !cfg.Insecure && cfg.Endpoint == DefaultEndpoint {
		switch {
		case cfg.FunctionID == "":
			return nil, ErrMissingFunctionID
		case cfg.APIKey == "":
			return nil, ErrMissingAPIKey
		}
	}
	if _, err := resample.NewResampler(cfg.RivaRate, cfg.SampleRate); err != nil {
		return nil, err
	}
	conn, err := Dial(ConnConfig{Endpoint: cfg.Endpoint, Insecure: cfg.Insecure, APIKey: cfg.APIKey,
		FunctionID: cfg.FunctionID, AllowPrivate: cfg.AllowPrivate, Conn: cfg.Conn})
	if err != nil {
		return nil, err
	}
	p := &TTSProcessor{cfg: cfg, conn: conn, client: rivapb.NewRivaSpeechSynthesisClient(conn.Channel())}
	p.Runner = ttsunit.New(ttsunit.Config{
		Provider: "nvidia", Model: cfg.Model, SampleRate: cfg.SampleRate,
		Breaker: cfg.CircuitBreaker, Policy: cfg.RetryPolicy, Classify: classify,
		Attempt: p.attempt,
	})
	return p, nil
}

// Warm brings the channel to READY (no RPC: no auth, no billing). Skipped
// while the breaker is open; never reports to it.
func (p *TTSProcessor) Warm(ctx context.Context) error {
	if br := p.cfg.CircuitBreaker; br != nil && br.WouldRefuse() {
		skipWarm("tts")
		return nil
	}
	return p.conn.Warm(ctx, "tts")
}

// Close cancels in-flight synthesis, then releases the channel; idempotent.
func (p *TTSProcessor) Close() error {
	_ = p.Runner.Close()
	return p.conn.Close()
}

func (p *TTSProcessor) attempt(ctx context.Context, u *ttsunit.Unit, _ int) error {
	cfg := p.cfg
	var rs *resample.Resampler // per attempt: its phase must start clean
	if cfg.RivaRate != cfg.SampleRate {
		rs, _ = resample.NewResampler(cfg.RivaRate, cfg.SampleRate) // rates validated in New
	}
	sctx, cancel := context.WithCancel(p.conn.Context(ctx))
	defer cancel() // tears the RPC down on every exit, including barge-in
	netpool.CountRequest("nvidia", "tts")
	st, err := p.client.SynthesizeOnline(sctx)
	if err != nil {
		return MapError("tts", err)
	}
	req := &rivapb.SynthesizeSpeechRequest{
		Text: u.Frame.Text, LanguageCode: cfg.Language, Encoding: rivapb.AudioEncoding_LINEAR_PCM,
		SampleRateHz: int32(cfg.RivaRate), VoiceName: cfg.Voice,
	}
	// One request then half-close: also what a pre-bidi (server-streaming)
	// SynthesizeOnline server expects on the wire.
	if err := st.Send(req); err != nil && err != io.EOF { // EOF: the real status comes from Recv
		return MapError("tts", err)
	}
	_ = st.CloseSend()

	var carry []byte // a Riva message may split a 16-bit sample
	got := false
	for {
		resp, err := st.Recv()
		if err == io.EOF {
			if !got {
				return &ProviderError{Capability: "tts", Code: "BAD_RESPONSE", Message: "stream ended without audio", Retryable: true}
			}
			return u.Finish()
		}
		if err != nil {
			return MapError("tts", err)
		}
		pcm := resp.GetAudio()
		if len(pcm) == 0 {
			continue
		}
		got = true
		if len(carry) > 0 {
			pcm = append(carry, pcm...)
			carry = nil
		}
		if len(pcm)%2 != 0 {
			carry = append([]byte(nil), pcm[len(pcm)-1])
			pcm = pcm[:len(pcm)-1]
		}
		if rs != nil {
			if pcm, err = rs.ResamplePCM16(pcm); err != nil {
				return MapError("tts", err)
			}
		}
		if err := u.Write(pcm); err != nil {
			return err
		}
	}
}
