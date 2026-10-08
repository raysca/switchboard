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
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"govoice/core"
	"govoice/core/netpool"
	"govoice/core/resilience"
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

type stream struct {
	conn   *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc

	mu         sync.Mutex
	turn       string // "" until a turn adopts the stream
	gen        uint64
	committed  string // committed segments of this turn, space-joined
	sent       bool   // audio went out: there is something to commit
	commitSent bool   // our manual commit is in flight: the next committed text ends the utterance
	lastKind   string // last committed message kind with no partial since (dedup of the two committed forms)
	lastText   string
	draining   *time.Timer
	closing    bool

	stopOnce, traceFinal, traceEnd sync.Once
	span                           core.Span
	policy                         core.TracePolicy
	traceSession                   core.SessionID
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

// STTProcessor implements core.Processor for ElevenLabs realtime speech-to-text.
// One websocket per turn, opened on the first speech/audio frame of the turn.
// Turn detection is ours: the stream uses the manual commit strategy and is
// committed when the turn ends.
type STTProcessor struct {
	cfg     STTConfig
	session core.SessionController
	index   int
	errs    atomic.Uint64

	mu     sync.Mutex
	active *stream
	all    map[*stream]struct{} // active plus draining, for teardown
	closed bool
	turn   string
	gen    uint64
}

// STTCounts implements core.STTCounters (only the error count is kept).
func (p *STTProcessor) STTCounts() (rot, rotFail, dedup, errs uint64) { return 0, 0, 0, p.errs.Load() }

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
	return &STTProcessor{cfg: cfg, all: map[*stream]struct{}{}}, nil
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

func (p *STTProcessor) Attach(s core.SessionController, index int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.session, p.index = s, index
	return nil
}

// Close tears down every stream; it is idempotent.
func (p *STTProcessor) Close() error {
	p.mu.Lock()
	p.closed = true
	streams := p.detachAllLocked()
	p.mu.Unlock()
	for _, st := range streams {
		p.stop(st, nil)
	}
	return nil
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
// turns) is adopted: it already carries the speech onset VAD reported late. A
// stream of the same turn at an older generation moves to the new one instead
// of being cut. A stream owned by another turn is committed and drains alone.
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
	sendCtx, cancel := context.WithTimeout(st.ctx, 500*time.Millisecond)
	defer cancel()
	if err := st.conn.Write(sendCtx, websocket.MessageText, p.chunk(f.Samples, false)); err != nil {
		if st.ctx.Err() != nil { // torn down concurrently: not a provider failure
			return e.Push(f)
		}
		perr := p.mapErr(err, nil)
		p.errs.Add(1)
		p.release(st)
		p.stop(st, errors.New("provider_failure"))
		p.report(perr)
		return perr
	}
	st.mu.Lock()
	st.sent = true
	st.mu.Unlock()
	return e.Push(f)
}

// ErrProviderUnavailable is returned when no stream could be used for audio.
var ErrProviderUnavailable = errors.New("elevenlabs: provider unavailable")

// mapErr classifies err and scrubs the API key from its text.
func (p *STTProcessor) mapErr(err error, resp *http.Response) *ProviderError {
	pe := MapError("stt", err, resp)
	pe.Message = strings.ReplaceAll(pe.Message, p.cfg.APIKey, "[REDACTED]")
	return pe
}

func (p *STTProcessor) report(err error) {
	if br := p.cfg.CircuitBreaker; br != nil {
		br.Report(err, classify)
	}
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

func (p *STTProcessor) ensureStream(ctx context.Context) error {
	p.mu.Lock()
	if p.active != nil || p.closed {
		p.mu.Unlock()
		return nil
	}
	turn, gen := p.turn, p.gen
	p.mu.Unlock()

	target, err := p.wsURL()
	if err != nil {
		return err
	}
	traceSession := core.SessionID("")
	if p.session != nil {
		traceSession = core.SessionID(strconv.FormatUint(p.session.ID(), 10))
	}
	span := core.StartContextSpan(ctx, "stt.stream", traceSession, core.TurnID(turn), core.Generation(gen), map[string]any{"provider": "elevenlabs"})
	policy := core.TraceContextFromContext(ctx).Policy
	fail := func(err error) error {
		if span != nil {
			span.AddEvent(core.NewTraceEvent("stt.failure", time.Now(), traceSession, core.TurnID(turn), core.Generation(gen), policy, nil))
			span.End(errors.New("provider_failure"))
		}
		return err
	}

	// The breaker and retries wrap only the dial (as Deepgram): nothing is
	// committed until audio flows, so a failed open is safe to retry. Execute
	// releases a half-open probe on client errors/cancel without closing the
	// breaker. The dial ctx governs the handshake only; the stream outlives it.
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
		if !errors.Is(err, ErrCircuitOpen) {
			p.errs.Add(1)
		}
		return fail(err)
	}

	streamCtx, cancel := context.WithCancel(p.ioParent(ctx))
	st := &stream{conn: conn, ctx: streamCtx, cancel: cancel, turn: turn, gen: gen, span: span, policy: policy, traceSession: traceSession}
	p.mu.Lock()
	if p.closed || p.active != nil {
		closed := p.closed
		p.mu.Unlock()
		cancel()
		conn.CloseNow()
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

// receive reads server messages until the stream ends, then cleans up.
func (p *STTProcessor) receive(sctx context.Context, st *stream) {
	var traceErr error
	defer func() {
		p.release(st)
		p.stop(st, traceErr)
		st.endTrace(traceErr)
	}()
	for {
		_, data, err := st.conn.Read(st.ctx)
		if err != nil {
			if st.ctx.Err() != nil || sctx.Err() != nil || quietClose(err) {
				return
			}
			traceErr = errors.New("provider_failure")
			perr := p.mapErr(err, nil)
			p.report(perr)
			p.reportError(st, perr)
			return
		}
		var m sttMessage
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		switch {
		case m.Type == "partial_transcript" || m.Type == "committed_transcript" || m.Type == "committed_transcript_with_timestamps":
			if p.transcript(st, &m) {
				return // committed after our commit: the turn is done
			}
		case m.Type == "session_time_limit_exceeded" || m.Type == "insufficient_audio_activity":
			// A very long session, or our own silence: end quietly (the server
			// closes next, possibly with 1008); the next audio opens a new stream.
			return
		case sttErrorKinds[m.Type]:
			traceErr = errors.New("provider_failure")
			perr := sttMessageError(m.Type, strings.ReplaceAll(m.Error, p.cfg.APIKey, "[REDACTED]"))
			p.report(perr)
			p.reportError(st, perr)
			return
		} // session_started, warning, entities, edited_transcript...: nothing to do
	}
}

func (p *STTProcessor) reportError(st *stream, err error) {
	p.errs.Add(1)
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

// transcript turns one transcript message into a cumulative turn candidate:
// committed segments so far plus this message's text. It reports whether the
// stream is finished (the committed text answering our commit).
func (p *STTProcessor) transcript(st *stream, m *sttMessage) (done bool) {
	seg := strings.TrimSpace(m.Text)
	final := m.Type != "partial_transcript"
	st.mu.Lock()
	if final && st.lastKind != "" && st.lastKind != m.Type && st.lastText == seg {
		st.mu.Unlock() // the same commit sent as plain and as with-timestamps
		return false
	}
	text := join(st.committed, seg)
	if final {
		st.committed, st.lastKind, st.lastText = text, m.Type, seg
	} else {
		st.lastKind = ""
	}
	eou := final && st.commitSent
	turn, gen := st.turn, st.gen
	st.mu.Unlock()
	if turn != "" && text != "" && !(seg == "" && !final) {
		gen = p.frameGen(gen)
		frame := &core.TranscriptCandidateFrame{
			Text: text, Turn: core.TurnID(turn), Gen: core.Generation(gen), Timestamp: p.cfg.Now(),
			Final: final, EndOfUtterance: eou, Provider: "elevenlabs",
		}
		if p.session != nil && p.session.Reenter(frame, core.Downstream, p.index+1, gen) == nil && final {
			st.traceFinal.Do(func() { st.event("stt.first_final") })
		}
	}
	return eou
}

func join(a, b string) string {
	if a == "" || b == "" {
		return a + b
	}
	return a + " " + b
}

// finishActive commits the active stream (turn "" = whichever is active, i.e.
// the user stopped speaking). The turn is released so audio after it opens an
// unbound stream for the next utterance.
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

// finish sends the manual commit; the stream closes when the committed text
// arrives, or after DrainTimeout. Safe to call more than once. A stream that
// never got audio has nothing to commit and is just closed.
func (p *STTProcessor) finish(st *stream) {
	st.mu.Lock()
	if st.closing {
		st.mu.Unlock()
		return
	}
	st.closing = true
	sent := st.sent
	st.commitSent = sent
	st.mu.Unlock()
	if !sent {
		p.release(st)
		p.stop(st, nil)
		return
	}
	wctx, cancel := context.WithTimeout(st.ctx, 500*time.Millisecond)
	defer cancel()
	// The commit is an empty audio chunk with commit=true, as the official SDKs send it.
	if err := st.conn.Write(wctx, websocket.MessageText, p.chunk(nil, true)); err != nil {
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

// stop closes the socket exactly once and ends the trace with err.
func (p *STTProcessor) stop(st *stream, err error) {
	st.mu.Lock()
	if st.draining != nil {
		st.draining.Stop()
	}
	st.mu.Unlock()
	st.stopOnce.Do(func() {
		st.cancel()
		st.conn.CloseNow()
	})
	st.endTrace(err)
}
