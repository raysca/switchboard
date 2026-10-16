package nvidia

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"

	"govoice/core"
	"govoice/core/netpool"
	"govoice/core/resilience"
	"govoice/core/sttunit"
	"govoice/providers/nvidia/rivapb"
)

// STTConfig configures the Riva streaming recognizer
// (nvidia.riva.asr.RivaSpeechRecognition/StreamingRecognize).
type STTConfig struct {
	Endpoint     string // host:port; default DefaultEndpoint (NVCF)
	Insecure     bool   // plaintext gRPC for self-hosted Riva; the API key is then never sent
	APIKey       string
	FunctionID   string // NVCF function-id of the hosted Riva/Parakeet function
	AllowPrivate bool   // permit loopback/private endpoints (self-hosted Riva)
	Language     string // default "en-US"
	Model        string // optional Riva model name; empty = server default
	SampleRate   int    // default 16000; input is 16-bit mono PCM

	RetryPolicy    resilience.Policy // bounds opening a stream: MaxAttempts tries of AttemptTimeout each
	CircuitBreaker *resilience.Breaker
	// DrainTimeout bounds the wait for the final results after the turn ends
	// before the RPC is cancelled.
	DrainTimeout time.Duration
	// SendTimeout bounds one audio Send (HTTP/2 flow control can block it).
	// Live sends run on the session run loop: keep it under the 2 s frame watchdog.
	SendTimeout time.Duration
	Now         func() time.Time
	// Conn is an existing channel (tests); the processor does not close it.
	Conn *grpc.ClientConn
}

// DefaultSTTConfig returns hosted-endpoint defaults. A stream open gets two
// tries of 5 s: it must not hang on a dead channel while a caller waits. A
// send gets 1 s: it runs on the session run loop, whose watchdog is 2 s.
func DefaultSTTConfig() STTConfig {
	return STTConfig{
		Endpoint: DefaultEndpoint, Language: "en-US", SampleRate: 16000,
		DrainTimeout: 2 * time.Second, SendTimeout: time.Second,
		RetryPolicy: resilience.Policy{MaxAttempts: 2, AttemptTimeout: 5 * time.Second, InitialBackoff: 100 * time.Millisecond},
	}
}

// STTProcessor implements core.Processor for Riva streaming ASR: one RPC per
// utterance on a per-processor channel, opened off the run loop (the stream
// lifecycle lives in core/sttunit).
type STTProcessor struct {
	*sttunit.Runner
	cfg    STTConfig
	conn   *Conn
	client rivapb.RivaSpeechRecognitionClient
}

var _ core.STTCounters = (*STTProcessor)(nil)

func NewSTTProcessor(cfg STTConfig) (*STTProcessor, error) {
	def := DefaultSTTConfig()
	if cfg.Endpoint == "" {
		cfg.Endpoint = def.Endpoint
	}
	if cfg.Language == "" {
		cfg.Language = def.Language
	}
	if cfg.SampleRate <= 0 {
		cfg.SampleRate = def.SampleRate
	}
	if cfg.DrainTimeout <= 0 {
		cfg.DrainTimeout = def.DrainTimeout
	}
	if cfg.SendTimeout <= 0 {
		cfg.SendTimeout = def.SendTimeout
	}
	if cfg.RetryPolicy.MaxAttempts <= 0 {
		cfg.RetryPolicy = def.RetryPolicy
	}
	if cfg.CircuitBreaker == nil {
		cfg.CircuitBreaker = resilience.NewBreaker(resilience.DefaultConfig())
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if !cfg.Insecure && cfg.Endpoint == DefaultEndpoint {
		switch {
		case cfg.FunctionID == "":
			return nil, ErrMissingFunctionID
		case cfg.APIKey == "":
			return nil, ErrMissingAPIKey
		}
	}
	conn, err := Dial(ConnConfig{Endpoint: cfg.Endpoint, Insecure: cfg.Insecure, APIKey: cfg.APIKey,
		FunctionID: cfg.FunctionID, AllowPrivate: cfg.AllowPrivate, Conn: cfg.Conn})
	if err != nil {
		return nil, err
	}
	p := &STTProcessor{cfg: cfg, conn: conn, client: rivapb.NewRivaSpeechRecognitionClient(conn.Channel())}
	p.Runner = sttunit.New(sttunit.Config{
		Provider: "nvidia", Dial: p.dial, Breaker: cfg.CircuitBreaker, Classify: classify,
		DrainTimeout: cfg.DrainTimeout, SendTimeout: cfg.SendTimeout, Now: cfg.Now,
	})
	return p, nil
}

// Warm brings the channel to READY before the first utterance (no RPC, so no
// auth and no billing). Skipped while the breaker is open; never reports to it.
func (p *STTProcessor) Warm(ctx context.Context) error {
	if br := p.cfg.CircuitBreaker; br.WouldRefuse() {
		skipWarm("stt")
		return nil
	}
	return p.conn.Warm(ctx, "stt")
}

// Close tears down every stream and the channel; it is idempotent.
func (p *STTProcessor) Close() error {
	_ = p.Runner.Close()
	return p.conn.Close()
}

func (p *STTProcessor) initRequest() *rivapb.StreamingRecognizeRequest {
	return &rivapb.StreamingRecognizeRequest{StreamingRequest: &rivapb.StreamingRecognizeRequest_StreamingConfig{
		StreamingConfig: &rivapb.StreamingRecognitionConfig{
			Config: &rivapb.RecognitionConfig{
				Encoding:                   rivapb.AudioEncoding_LINEAR_PCM,
				SampleRateHertz:            int32(p.cfg.SampleRate),
				LanguageCode:               p.cfg.Language,
				MaxAlternatives:            1,
				EnableAutomaticPunctuation: true,
				AudioChannelCount:          1,
				Model:                      p.cfg.Model,
			},
			InterimResults: true,
		}}}
}

// openOnce starts the RPC and sends the config message. A failed first Send
// is io.EOF; the real status is on Recv.
func (p *STTProcessor) openOnce(actx context.Context) (*sttConn, error) {
	netpool.CountRequest("nvidia", "stt")
	rpc, err := p.client.StreamingRecognize(p.conn.Context(actx))
	if err != nil {
		return nil, MapError("stt", err)
	}
	if err := rpc.Send(p.initRequest()); err != nil {
		if errors.Is(err, io.EOF) {
			if _, rerr := rpc.Recv(); rerr != nil {
				err = rerr
			}
		}
		return nil, MapError("stt", err)
	}
	return &sttConn{rpc: rpc}, nil
}

// dial opens one RPC on ctx. OpenBounded retries only the open and the breaker
// gets one report per open. Success is recorded when the server first answers
// (sttConn.Recv): a connected channel still rejects with Unavailable per RPC,
// and counting the open as success would reset the breaker on every turn.
func (p *STTProcessor) dial(ctx context.Context) (sttunit.Conn, error) {
	br := p.cfg.CircuitBreaker
	if br.WouldRefuse() {
		return nil, ErrCircuitOpen
	}
	rctx, cancel := context.WithCancel(ctx) // the RPC lives on rctx; Close cancels it
	c, err := resilience.OpenBounded(rctx, p.cfg.RetryPolicy, classify, p.openOnce)
	if err != nil {
		cancel()
		perr := MapError("stt", err)
		br.Report(perr, classify)
		return nil, perr
	}
	c.cancel, c.br = cancel, br
	return c, nil
}

// sttConn is one Riva RPC and its turn transcript.
type sttConn struct {
	rpc     rivapb.RivaSpeechRecognition_StreamingRecognizeClient
	cancel  context.CancelFunc
	br      *resilience.Breaker
	okOnce  sync.Once
	closing atomic.Bool // CloseSend sent: a final now ends the utterance

	committed string // finalized segments of this turn, space-joined (Recv's task only)
}

// Send writes one chunk. A Send still blocked when ctx ends (the runner's
// SendTimeout) cancels the RPC and fails as a timeout: a hung connection is
// an outage.
func (c *sttConn) Send(ctx context.Context, pcm []byte) error {
	var stuck atomic.Bool
	stop := context.AfterFunc(ctx, func() { stuck.Store(true); c.cancel() })
	err := c.rpc.Send(&rivapb.StreamingRecognizeRequest{StreamingRequest: &rivapb.StreamingRecognizeRequest_AudioContent{AudioContent: pcm}})
	stop()
	switch {
	case err != nil && stuck.Load():
		return MapError("stt", fmt.Errorf("%w: audio send blocked past its deadline: %w", resilience.ErrTimeout, err))
	case err == nil, errors.Is(err, io.EOF):
		return nil // io.EOF: the RPC ended; Recv reads the real status and reports it
	}
	return MapError("stt", err)
}

// Finish half-closes the RPC so Riva flushes its final results and ends it.
func (c *sttConn) Finish(context.Context) error {
	c.closing.Store(true)
	return c.rpc.CloseSend()
}

func (c *sttConn) Close() { c.cancel() }

func (c *sttConn) Recv(context.Context) (sttunit.Result, error) {
	for {
		resp, err := c.rpc.Recv()
		if err != nil {
			if streamEnded(err) { // server finished (after our CloseSend, or its own end)
				c.okOnce.Do(c.br.RecordSuccess)
				return sttunit.Result{}, sttunit.ErrStreamEnded
			}
			return sttunit.Result{}, MapError("stt", err)
		}
		c.okOnce.Do(c.br.RecordSuccess)
		if res, ok := c.results(resp); ok {
			return res, nil
		}
	}
}

// results turns one response into the cumulative turn text: the finalized
// segments so far plus this response's interims. Riva interims restate the
// whole current segment, so they replace the previous response's; one response
// may split it into several results (stable head, unstable tail), which are
// joined in order as riva/client/asr.py does. Stability is the tail's lowest.
func (c *sttConn) results(resp *rivapb.StreamingRecognizeResponse) (sttunit.Result, bool) {
	var interim string
	var final, any, emptyFinal bool
	var stability float32
	for _, r := range resp.GetResults() {
		alts := r.GetAlternatives()
		if len(alts) == 0 {
			continue
		}
		seg := strings.TrimSpace(alts[0].GetTranscript())
		if seg == "" {
			emptyFinal = emptyFinal || r.GetIsFinal()
			continue
		}
		any, final = true, r.GetIsFinal()
		if final {
			c.committed, interim, stability = join(c.committed, seg), "", 0
		} else {
			if s := r.GetStability(); interim == "" || s < stability {
				stability = s
			}
			interim = join(interim, seg)
		}
	}
	if !any {
		if emptyFinal { // retracted noise: an empty Final settles the turn (keeps committed text)
			return sttunit.Result{Text: c.committed, Final: true, EndOfUtterance: c.closing.Load()}, true
		}
		return sttunit.Result{}, false
	}
	return sttunit.Result{
		Text: join(c.committed, interim), Final: final, EndOfUtterance: final && c.closing.Load(),
		Stability: float64(stability), HasStability: !final && stability > 0,
	}, true
}

func join(a, b string) string {
	if a == "" || b == "" {
		return a + b
	}
	return a + " " + b
}
