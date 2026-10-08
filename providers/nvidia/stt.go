package nvidia

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"

	"govoice/core"
	"govoice/core/resilience"
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
	SendTimeout time.Duration
	Now         func() time.Time
	// Conn is an existing channel (tests); the processor does not close it.
	Conn *grpc.ClientConn
}

// DefaultSTTConfig returns hosted-endpoint defaults. A stream open gets two
// tries of 5 s: it must not hang on a dead channel while a caller waits.
func DefaultSTTConfig() STTConfig {
	return STTConfig{
		Endpoint: DefaultEndpoint, Language: "en-US", SampleRate: 16000,
		DrainTimeout: 2 * time.Second, SendTimeout: 2 * time.Second,
		RetryPolicy: resilience.Policy{MaxAttempts: 2, AttemptTimeout: 5 * time.Second, InitialBackoff: 100 * time.Millisecond},
	}
}

type stream struct {
	rpc    rivapb.RivaSpeechRecognition_StreamingRecognizeClient
	ctx    context.Context
	cancel context.CancelFunc

	sendMu     sync.Mutex // Send and CloseSend must not run concurrently
	sendClosed bool

	mu        sync.Mutex
	turn      string // "" until a turn adopts the stream
	gen       uint64
	committed string // finalized segments of this turn, space-joined
	draining  *time.Timer
	closing   bool

	okOnce, stopOnce, traceFinal, traceEnd sync.Once
	span                                   core.Span
	policy                                 core.TracePolicy
	traceSession                           core.SessionID
}

func (st *stream) owner() (string, uint64) {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.turn, st.gen
}

func (st *stream) event(name string) {
	if st.span != nil {
		turn, gen := st.owner()
		st.span.AddEvent(core.NewTraceEvent(name, time.Now(), st.traceSession, core.TurnID(turn), core.Generation(gen), st.policy, nil))
	}
}

func (st *stream) endTrace(err error) {
	st.traceEnd.Do(func() {
		if st.span == nil {
			return
		}
		if err != nil {
			st.event("stt.failure")
		} else {
			st.event("stt.complete")
		}
		st.span.End(err)
	})
}

var errSendClosed = errors.New("nvidia: stream send side closed")

// send writes one audio chunk; a Send that blocks past SendTimeout cancels the
// stream and fails as a timeout (a hung connection is an outage).
func (st *stream) send(req *rivapb.StreamingRecognizeRequest, timeout time.Duration) error {
	st.sendMu.Lock()
	defer st.sendMu.Unlock()
	if st.sendClosed {
		return errSendClosed
	}
	var stuck atomic.Bool
	t := time.AfterFunc(timeout, func() { stuck.Store(true); st.cancel() })
	err := st.rpc.Send(req)
	t.Stop()
	if err != nil && stuck.Load() {
		return fmt.Errorf("%w: audio send blocked for %v", resilience.ErrTimeout, timeout)
	}
	return err
}

func (st *stream) closeSend() error {
	st.sendMu.Lock()
	defer st.sendMu.Unlock()
	st.sendClosed = true
	return st.rpc.CloseSend()
}

// STTProcessor implements core.Processor for Riva streaming ASR. One RPC per
// turn on a per-processor channel, opened on the first speech/audio frame.
type STTProcessor struct {
	cfg     STTConfig
	conn    *Conn
	client  rivapb.RivaSpeechRecognitionClient
	session core.SessionController
	index   int

	mu     sync.Mutex
	active *stream
	all    map[*stream]struct{} // active plus draining, for teardown
	closed bool
	turn   string
	gen    uint64

	Errors atomic.Uint64
}

// STTCounts implements core.STTCounters (Riva has no rotation or dedup).
func (p *STTProcessor) STTCounts() (rot, rotFail, dedup, errs uint64) {
	return 0, 0, 0, p.Errors.Load()
}

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
	return &STTProcessor{cfg: cfg, conn: conn, client: rivapb.NewRivaSpeechRecognitionClient(conn.Channel()), all: map[*stream]struct{}{}}, nil
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

func (p *STTProcessor) Attach(s core.SessionController, index int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.session, p.index = s, index
	return nil
}

// Close tears down every stream and the channel; it is idempotent.
func (p *STTProcessor) Close() error {
	p.mu.Lock()
	p.closed = true
	streams := p.detachAllLocked()
	p.mu.Unlock()
	for _, st := range streams {
		p.stop(st, nil)
	}
	return p.conn.Close()
}

func (p *STTProcessor) detachAllLocked() []*stream {
	out := make([]*stream, 0, len(p.all))
	for st := range p.all {
		out = append(out, st)
	}
	p.all, p.active = map[*stream]struct{}{}, nil
	return out
}

func (p *STTProcessor) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	switch fr := f.(type) {
	case *core.UserStartedSpeakingFrame:
		p.beginTurn(fmt.Sprintf("turn-%d-%d", fr.Gen, fr.Seq), fr.Gen)
		if err := p.ensureStream(ctx); err != nil {
			return err
		}
		return e.Push(f)
	case *core.TurnEventFrame:
		switch fr.Kind {
		case core.TurnEventStarted:
			p.beginTurn(string(fr.Turn), uint64(fr.Gen))
		case core.TurnEventCompleted:
			p.finishActive(string(fr.Turn), uint64(fr.Gen))
		case core.TurnEventInterrupted:
			p.stopTurn(string(fr.Turn), uint64(fr.Gen))
		}
		return e.PushDirection(f, dir)
	case *core.InputAudioFrame:
		return p.handleAudio(ctx, fr, e)
	case *core.UserStoppedSpeakingFrame:
		p.finishActive("", 0)
		return e.PushDirection(f, dir)
	case *core.InterruptionFrame, *core.CancelFrame:
		p.mu.Lock()
		if _, ok := f.(*core.InterruptionFrame); ok && p.session != nil && p.turn != "" {
			// Barge-in advanced the generation: the rest of this utterance opens
			// a new stream that must carry it, or the session fence drops it.
			p.gen = max(p.gen, p.session.CurrentGeneration())
		}
		streams := p.detachAllLocked()
		p.mu.Unlock()
		for _, st := range streams {
			p.stop(st, nil)
		}
		return e.PushDirection(f, dir)
	default:
		return e.PushDirection(f, dir)
	}
}

// beginTurn makes (turn, gen) current. An unbound stream (opened between
// turns) is adopted: it already carries the speech onset VAD reported late.
// The same turn at a newer generation (barge-in) keeps its stream, rebound.
// A stream owned by another turn is finalized and drains on its own.
func (p *STTProcessor) beginTurn(turn string, gen uint64) {
	p.mu.Lock()
	p.turn, p.gen = turn, gen
	var old *stream
	if a := p.active; a != nil {
		a.mu.Lock()
		if a.turn == "" || (a.turn == turn && a.gen < gen) {
			a.turn, a.gen = turn, gen
		} else if a.turn != turn || a.gen != gen {
			old, p.active = a, nil
		}
		a.mu.Unlock()
	}
	p.mu.Unlock()
	if old != nil {
		p.finish(old)
	}
}

func (p *STTProcessor) handleAudio(ctx context.Context, f *core.InputAudioFrame, e core.Emitter) error {
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return nil
	}
	if err := p.ensureStream(ctx); err != nil {
		return err
	}
	p.mu.Lock()
	st := p.active
	p.mu.Unlock()
	if st == nil {
		return ErrProviderUnavailable
	}
	err := st.send(&rivapb.StreamingRecognizeRequest{StreamingRequest: &rivapb.StreamingRecognizeRequest_AudioContent{AudioContent: f.Samples}}, p.cfg.SendTimeout)
	switch {
	case err == nil, errors.Is(err, errSendClosed):
		return e.Push(f) // a send racing a finalize belongs to the ended turn
	case st.ctx.Err() != nil && !errors.Is(err, resilience.ErrTimeout): // torn down concurrently
		return e.Push(f)
	case errors.Is(err, io.EOF) && p.session != nil:
		return e.Push(f) // the RPC ended; receive() reads the real status and reports it
	}
	perr := MapError("stt", err)
	p.Errors.Add(1)
	p.release(st)
	p.stop(st, errors.New("provider_failure"))
	p.cfg.CircuitBreaker.Report(perr, classify)
	return perr
}

func (p *STTProcessor) ioParent(ctx context.Context) context.Context {
	if p.session != nil {
		return p.session.Context()
	}
	if ctx != nil {
		return ctx
	}
	return context.Background()
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
func (p *STTProcessor) openOnce(actx context.Context) (*stream, error) {
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
	return &stream{rpc: rpc}, nil
}

func (p *STTProcessor) ensureStream(ctx context.Context) error {
	p.mu.Lock()
	if p.active != nil || p.closed {
		p.mu.Unlock()
		return nil
	}
	turn, gen := p.turn, p.gen
	p.mu.Unlock()

	traceSession := core.SessionID("")
	if p.session != nil {
		traceSession = core.SessionID(strconv.FormatUint(p.session.ID(), 10))
	}
	span := core.StartContextSpan(ctx, "stt.stream", traceSession, core.TurnID(turn), core.Generation(gen), map[string]any{"provider": "nvidia"})
	policy := core.TraceContextFromContext(ctx).Policy
	fail := func(err error) error {
		if span != nil {
			span.AddEvent(core.NewTraceEvent("stt.failure", time.Now(), traceSession, core.TurnID(turn), core.Generation(gen), policy, nil))
			span.End(errors.New("provider_failure"))
		}
		return err
	}

	br := p.cfg.CircuitBreaker
	if br.WouldRefuse() {
		return fail(ErrCircuitOpen)
	}
	// The stream lives on streamCtx; OpenBounded retries only the open and the
	// breaker gets one report per open.
	streamCtx, cancel := context.WithCancel(p.ioParent(ctx))
	st, err := resilience.OpenBounded(streamCtx, p.cfg.RetryPolicy, classify, p.openOnce)
	if err != nil {
		cancel()
		p.Errors.Add(1)
		perr := MapError("stt", err)
		br.Report(perr, classify)
		return fail(perr)
	}
	// Success is recorded when the server first answers (see receive): a
	// connected channel still rejects with Unavailable per RPC, and counting
	// the open as success would reset the breaker on every turn.
	st.ctx, st.cancel = streamCtx, cancel
	st.turn, st.gen, st.span, st.policy, st.traceSession = turn, gen, span, policy, traceSession
	p.mu.Lock()
	if p.closed || p.active != nil {
		closed := p.closed
		p.mu.Unlock()
		cancel()
		if span != nil {
			span.End(errors.New("canceled"))
		}
		if closed {
			return core.ErrSessionClosed
		}
		return nil
	}
	p.active = st
	p.all[st] = struct{}{}
	p.mu.Unlock()
	st.event("stt.start")

	if p.session == nil {
		return nil
	}
	if err := p.session.Go(func(sctx context.Context) { p.receive(sctx, st) }); err != nil {
		p.release(st)
		p.stop(st, errors.New("canceled"))
		return err
	}
	return nil
}

// receive reads responses until the RPC ends, then cleans up.
func (p *STTProcessor) receive(sctx context.Context, st *stream) {
	var traceErr error
	defer func() {
		p.release(st)
		p.stop(st, traceErr)
		st.endTrace(traceErr)
	}()
	for {
		resp, err := st.rpc.Recv()
		if err != nil {
			if st.ctx.Err() != nil || sctx.Err() != nil {
				return
			}
			if streamEnded(err) { // server finished (after our CloseSend, or its own idle end)
				st.okOnce.Do(p.cfg.CircuitBreaker.RecordSuccess)
				return
			}
			traceErr = errors.New("provider_failure")
			p.Errors.Add(1)
			perr := MapError("stt", err)
			p.cfg.CircuitBreaker.Report(perr, classify)
			p.reportError(st, perr)
			return
		}
		st.okOnce.Do(p.cfg.CircuitBreaker.RecordSuccess)
		p.results(st, resp)
	}
}

func (p *STTProcessor) reportError(st *stream, err error) {
	if p.session != nil {
		turn, gen := st.owner()
		gen = p.frameGen(gen)
		_ = p.session.Reenter(&core.ErrorFrame{Err: err, Turn: core.TurnID(turn), Gen: core.Generation(gen)}, core.Downstream, p.index+1, gen)
	}
}

func (p *STTProcessor) frameGen(gen uint64) uint64 {
	if gen == 0 && p.session != nil {
		return p.session.CurrentGeneration()
	}
	return gen
}

// results turns one response into a cumulative turn candidate: the finalized
// segments so far plus this response's interims. Riva interims restate the
// whole current segment, so they replace the previous response's; one response
// may split it into several results (stable head, unstable tail), which are
// joined in order as riva/client/asr.py does. Stability is the tail's lowest.
func (p *STTProcessor) results(st *stream, resp *rivapb.StreamingRecognizeResponse) {
	st.mu.Lock()
	var interim string
	var final, any bool
	var stability float32
	for _, r := range resp.GetResults() {
		alts := r.GetAlternatives()
		if len(alts) == 0 {
			continue
		}
		seg := strings.TrimSpace(alts[0].GetTranscript())
		if seg == "" {
			continue
		}
		any, final = true, r.GetIsFinal()
		if final {
			st.committed, interim, stability = join(st.committed, seg), "", 0
		} else {
			if s := r.GetStability(); interim == "" || s < stability {
				stability = s
			}
			interim = join(interim, seg)
		}
	}
	text := join(st.committed, interim)
	turn, gen, closing := st.turn, st.gen, st.closing
	st.mu.Unlock()
	// Unbound (between turns): keep committed text for the adopting turn, but
	// a candidate needs a turn identity.
	if !any || turn == "" {
		return
	}
	gen = p.frameGen(gen)
	frame := &core.TranscriptCandidateFrame{
		Text: text, Turn: core.TurnID(turn), Gen: core.Generation(gen), Timestamp: p.cfg.Now(),
		Final: final, EndOfUtterance: final && closing, Provider: "nvidia",
		Stability: float64(stability), HasStability: !final && stability > 0,
	}
	if p.session != nil && p.session.Reenter(frame, core.Downstream, p.index+1, gen) == nil && final {
		st.traceFinal.Do(func() { st.event("stt.first_final") })
	}
}

func join(a, b string) string {
	if a == "" || b == "" {
		return a + b
	}
	return a + " " + b
}

// finishActive finalizes the active stream (turn "" = whichever is active,
// i.e. the user stopped speaking). The turn is released so audio after it opens
// an unbound stream for the next utterance.
func (p *STTProcessor) finishActive(turn string, gen uint64) {
	p.mu.Lock()
	st := p.active
	if st == nil {
		p.mu.Unlock()
		return
	}
	t, g := st.owner()
	if turn != "" && (t != turn || g != gen) {
		p.mu.Unlock()
		return
	}
	p.active = nil // late audio opens a new stream rather than a closing one
	if p.turn == t && p.gen == g {
		p.turn, p.gen = "", 0
	}
	p.mu.Unlock()
	p.finish(st)
}

// finish half-closes st so Riva flushes its final results and ends the RPC,
// then cancels after DrainTimeout. Safe to call more than once.
func (p *STTProcessor) finish(st *stream) {
	st.mu.Lock()
	if st.closing {
		st.mu.Unlock()
		return
	}
	st.closing = true
	st.mu.Unlock()
	if err := st.closeSend(); err != nil {
		p.release(st)
		p.stop(st, nil)
		return
	}
	st.mu.Lock()
	st.draining = time.AfterFunc(p.cfg.DrainTimeout, func() { p.release(st); p.stop(st, nil) })
	st.mu.Unlock()
}

func (p *STTProcessor) stopTurn(turn string, gen uint64) {
	p.mu.Lock()
	var victims []*stream
	for st := range p.all {
		if t, g := st.owner(); t == turn && g == gen {
			victims = append(victims, st)
			delete(p.all, st)
			if p.active == st {
				p.active = nil
			}
		}
	}
	p.mu.Unlock()
	for _, st := range victims {
		p.stop(st, nil)
	}
}

func (p *STTProcessor) release(st *stream) {
	p.mu.Lock()
	delete(p.all, st)
	if p.active == st {
		p.active = nil
	}
	p.mu.Unlock()
}

// stop cancels the RPC exactly once and ends the trace with err.
func (p *STTProcessor) stop(st *stream, err error) {
	st.mu.Lock()
	if st.draining != nil {
		st.draining.Stop()
	}
	st.mu.Unlock()
	st.stopOnce.Do(st.cancel)
	st.endTrace(err)
}
