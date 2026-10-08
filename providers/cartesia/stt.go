package cartesia

import (
	"context"
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

// Protocol (docs.cartesia.ai STT websocket reference): binary PCM frames up;
// text "finalize" (flush buffered audio -> server "flush_done") and "close"
// (flush and end -> server "done"). Server JSON: transcript {text, is_final,
// words?}, flush_done, done, error {status_code, title, message}. A transcript's
// text is a delta: finals are concatenated verbatim (no added whitespace), an
// interim replaces the previous interim.

// STTConfig configures the Cartesia (Ink) streaming transcription processor.
type STTConfig struct {
	Endpoint   string // default "wss://api.cartesia.ai/stt/websocket"
	Version    string // Cartesia-Version (header and cartesia_version query), default "2026-08-14"
	Model      string // default "ink-whisper"; docs also list "ink-2", "ink-preview"
	Language   string // default "en"; sent for ink-whisper only (per the docs)
	SampleRate int    // default 16000; input is 16-bit mono PCM (pcm_s16le)
	// MinVolume (0-1) and MaxSilenceSecs tune Cartesia's own silence
	// auto-finalization (ink-whisper only); zero leaves the server default.
	MinVolume      float64
	MaxSilenceSecs float64
	APIKey         string
	// RetryPolicy bounds opening the websocket (default 2 tries x 5 s).
	RetryPolicy    resilience.Policy
	CircuitBreaker *resilience.Breaker
	// DrainTimeout bounds the wait for the final results after the turn ends
	// before the socket is torn down.
	DrainTimeout time.Duration
	Now          func() time.Time
	// Pool dials the websocket; default is the process-wide "cartesia"/"stt" pool.
	Pool *netpool.Pool
}

func DefaultSTTConfig() STTConfig {
	return STTConfig{
		Endpoint: "wss://api.cartesia.ai/stt/websocket", Version: "2026-08-14", Model: "ink-whisper",
		Language: "en", SampleRate: 16000, DrainTimeout: 2 * time.Second,
		RetryPolicy: resilience.Policy{MaxAttempts: 2, AttemptTimeout: 5 * time.Second, InitialBackoff: 100 * time.Millisecond},
	}
}

// sttMessage is the subset of Cartesia's STT server messages we read.
type sttMessage struct {
	Type       string `json:"type"`
	Text       string `json:"text"`
	IsFinal    bool   `json:"is_final"`
	StatusCode int    `json:"status_code"`
	Title      string `json:"title"`
	Message    string `json:"message"`
}

type sttStream struct {
	conn   *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc

	mu        sync.Mutex
	turn      string // "" until a turn adopts the stream
	gen       uint64
	committed string // final deltas of this turn, concatenated verbatim
	interim   string // the current non-final delta
	draining  *time.Timer
	closing   bool

	stopOnce, traceFinal, traceEnd sync.Once
	span                           core.Span
	policy                         core.TracePolicy
	traceSession                   core.SessionID
}

func (st *sttStream) owner() (string, uint64) {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.turn, st.gen
}

func (st *sttStream) event(name string) {
	if st.span != nil {
		turn, gen := st.owner()
		st.span.AddEvent(core.NewTraceEvent(name, time.Now(), st.traceSession, core.TurnID(turn), core.Generation(gen), st.policy, nil))
	}
}

func (st *sttStream) endTrace(err error) {
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

// STTProcessor implements core.Processor for Cartesia speech-to-text. One
// websocket per turn, opened on the first speech/audio frame of the turn.
type STTProcessor struct {
	cfg     STTConfig
	session core.SessionController
	index   int

	mu     sync.Mutex
	active *sttStream
	all    map[*sttStream]struct{} // active plus draining, for teardown
	closed bool
	turn   string
	gen    uint64

	errs atomic.Uint64
}

// STTCounts implements core.STTCounters (no rotation or dedup in this adapter).
func (p *STTProcessor) STTCounts() (rot, rotFail, dedup, errs uint64) {
	return 0, 0, 0, p.errs.Load()
}

func NewSTTProcessor(cfg STTConfig) (*STTProcessor, error) {
	if cfg.APIKey == "" {
		return nil, ErrMissingAPIKey
	}
	def := DefaultSTTConfig()
	if cfg.Endpoint == "" {
		cfg.Endpoint = def.Endpoint
	}
	if cfg.Version == "" {
		cfg.Version = def.Version
	}
	if cfg.Model == "" {
		cfg.Model = def.Model
	}
	if cfg.SampleRate <= 0 {
		cfg.SampleRate = def.SampleRate
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
		cfg.Pool = netpool.Get(netpool.Options{Provider: "cartesia", Stage: "stt", HTTP1Only: true})
	}
	return &STTProcessor{cfg: cfg, all: map[*sttStream]struct{}{}}, nil
}

// Warm pre-dials TCP+TLS only: an unused websocket would just idle out.
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

func (p *STTProcessor) detachAllLocked() []*sttStream {
	out := make([]*sttStream, 0, len(p.all))
	for st := range p.all {
		out = append(out, st)
	}
	p.all, p.active = map[*sttStream]struct{}{}, nil
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
// A stream owned by another turn is finalized and drains on its own.
func (p *STTProcessor) beginTurn(turn string, gen uint64) {
	p.mu.Lock()
	p.turn, p.gen = turn, gen
	var old *sttStream
	if a := p.active; a != nil {
		a.mu.Lock()
		if a.turn == "" || (a.turn == turn && a.gen < gen) {
			// Unbound, or a turn manager barged in mid-utterance: the stream
			// moves to the new generation instead of being cut.
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
	sendCtx, cancel := context.WithTimeout(st.ctx, 500*time.Millisecond)
	defer cancel()
	if err := st.conn.Write(sendCtx, websocket.MessageBinary, f.Samples); err != nil {
		if st.ctx.Err() != nil { // torn down concurrently: not a provider failure
			return e.Push(f)
		}
		perr := MapError("stt", err, nil)
		p.errs.Add(1)
		p.release(st)
		p.stop(st, errors.New("provider_failure"))
		if p.cfg.CircuitBreaker != nil {
			p.cfg.CircuitBreaker.Report(perr, classify)
		}
		return perr
	}
	return e.Push(f)
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

// wsURL carries no credential; the key travels in a header only.
func (p *STTProcessor) wsURL() (string, error) {
	u, err := url.Parse(p.cfg.Endpoint)
	if err != nil {
		return "", errors.New("cartesia: invalid stt endpoint")
	}
	q := u.Query()
	q.Set("model", p.cfg.Model)
	q.Set("encoding", "pcm_s16le")
	q.Set("sample_rate", strconv.Itoa(p.cfg.SampleRate))
	q.Set("cartesia_version", p.cfg.Version)
	// language, min_volume and max_silence_duration_secs are documented as
	// ink-whisper only; other models (ink-2, ink-preview) do not get them.
	whisper := strings.HasPrefix(p.cfg.Model, "ink-whisper")
	if whisper && p.cfg.Language != "" {
		q.Set("language", p.cfg.Language)
	}
	if whisper && p.cfg.MinVolume > 0 {
		q.Set("min_volume", strconv.FormatFloat(p.cfg.MinVolume, 'f', -1, 64))
	}
	if whisper && p.cfg.MaxSilenceSecs > 0 {
		q.Set("max_silence_duration_secs", strconv.FormatFloat(p.cfg.MaxSilenceSecs, 'f', -1, 64))
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
	span := core.StartContextSpan(ctx, "stt.stream", traceSession, core.TurnID(turn), core.Generation(gen), map[string]any{"provider": "cartesia"})
	policy := core.TraceContextFromContext(ctx).Policy
	fail := func(err error) error {
		if span != nil {
			span.AddEvent(core.NewTraceEvent("stt.failure", time.Now(), traceSession, core.TurnID(turn), core.Generation(gen), policy, nil))
			span.End(errors.New("provider_failure"))
		}
		return err
	}

	br := p.cfg.CircuitBreaker
	if br != nil && br.WouldRefuse() {
		return fail(ErrCircuitOpen)
	}
	// Retries wrap only the dial; the breaker gets one report per open.
	opts := &websocket.DialOptions{HTTPClient: p.cfg.Pool.Client(0),
		HTTPHeader: http.Header{"X-API-Key": {p.cfg.APIKey}, "Cartesia-Version": {p.cfg.Version}}}
	conn, err := resilience.OpenBounded(ctx, p.cfg.RetryPolicy, classify, func(actx context.Context) (*websocket.Conn, error) {
		c, resp, derr := websocket.Dial(actx, target, opts)
		if derr != nil {
			return nil, MapError("stt", derr, resp)
		}
		c.SetReadLimit(1 << 20)
		return c, nil
	})
	if err != nil {
		p.errs.Add(1)
		if errors.Is(err, resilience.ErrTimeout) { // a hang is an outage
			err = &ProviderError{Capability: "stt", Code: "TIMEOUT", Message: SanitizeString(err.Error()), Retryable: true, Cause: err}
		}
		if br != nil {
			br.Report(err, classify)
		}
		return fail(err)
	}
	if br != nil {
		br.RecordSuccess()
	}

	streamCtx, cancel := context.WithCancel(p.ioParent(ctx))
	st := &sttStream{conn: conn, ctx: streamCtx, cancel: cancel, turn: turn, gen: gen, span: span, policy: policy, traceSession: traceSession}
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

// idleClose reports whether err is the server dropping a connection that got
// no audio (our silence, not an outage). The exact idle close code/reason is
// undocumented, so match the reason text.
func idleClose(err error) bool {
	var ce websocket.CloseError
	if !errors.As(err, &ce) {
		return false
	}
	r := strings.ToLower(ce.Reason)
	return strings.Contains(r, "idle") || strings.Contains(r, "timeout") || strings.Contains(r, "timed out")
}

// receive reads server messages until the stream ends, then cleans up.
func (p *STTProcessor) receive(sctx context.Context, st *sttStream) {
	var traceErr error
	defer func() {
		p.release(st)
		p.stop(st, traceErr)
		st.endTrace(traceErr)
	}()
	for {
		_, data, err := st.conn.Read(st.ctx)
		if err != nil {
			if st.ctx.Err() != nil || sctx.Err() != nil {
				return
			}
			if s := websocket.CloseStatus(err); s == websocket.StatusNormalClosure || s == websocket.StatusGoingAway || idleClose(err) {
				return
			}
			traceErr = errors.New("provider_failure")
			p.errs.Add(1)
			perr := MapError("stt", err, nil)
			if p.cfg.CircuitBreaker != nil {
				p.cfg.CircuitBreaker.Report(perr, classify)
			}
			p.reportError(st, perr)
			return
		}
		var m sttMessage
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		switch m.Type {
		case "transcript":
			p.transcript(st, &m)
		case "flush_done":
			p.flushed(st)
		case "done":
			return
		case "error":
			traceErr = errors.New("provider_failure")
			p.errs.Add(1)
			msg := m.Message
			if msg == "" {
				msg = m.Title
			}
			perr := messageError("stt", m.StatusCode, msg)
			if p.cfg.CircuitBreaker != nil {
				p.cfg.CircuitBreaker.Report(perr, classify)
			}
			p.reportError(st, perr)
			return
		}
	}
}

func (p *STTProcessor) reportError(st *sttStream, err error) {
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

// transcript turns one transcript delta into a cumulative turn candidate: the
// finalized deltas so far plus the current interim.
func (p *STTProcessor) transcript(st *sttStream, m *sttMessage) {
	st.mu.Lock()
	if m.IsFinal {
		st.committed += m.Text
		st.interim = ""
	} else {
		st.interim = m.Text
	}
	text := strings.TrimSpace(st.committed + st.interim)
	turn, gen := st.turn, st.gen
	st.mu.Unlock()
	// Unbound (between turns): keep committed text for the adopting turn, but a
	// candidate needs a turn identity.
	if turn == "" || text == "" {
		return
	}
	p.emit(st, text, turn, gen, m.IsFinal, false)
}

// flushed handles flush_done: everything buffered at finalize is now final, so
// the cumulative text ends the utterance.
func (p *STTProcessor) flushed(st *sttStream) {
	st.mu.Lock()
	st.committed += st.interim
	st.interim = ""
	text := strings.TrimSpace(st.committed)
	turn, gen := st.turn, st.gen
	st.mu.Unlock()
	if turn == "" || text == "" {
		return
	}
	p.emit(st, text, turn, gen, true, true)
}

func (p *STTProcessor) emit(st *sttStream, text, turn string, gen uint64, final, eou bool) {
	gen = p.frameGen(gen)
	frame := &core.TranscriptCandidateFrame{
		Text: text, Turn: core.TurnID(turn), Gen: core.Generation(gen), Timestamp: p.cfg.Now(),
		Final: final, EndOfUtterance: eou, Provider: "cartesia",
	}
	if p.session != nil && p.session.Reenter(frame, core.Downstream, p.index+1, gen) == nil && final {
		st.traceFinal.Do(func() { st.event("stt.first_final") })
	}
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

// finish asks Cartesia to flush and close st, then force-closes after
// DrainTimeout. Safe to call more than once.
func (p *STTProcessor) finish(st *sttStream) {
	st.mu.Lock()
	if st.closing {
		st.mu.Unlock()
		return
	}
	st.closing = true
	st.mu.Unlock()
	wctx, cancel := context.WithTimeout(st.ctx, 500*time.Millisecond)
	defer cancel()
	for _, msg := range []string{"finalize", "close"} {
		if err := st.conn.Write(wctx, websocket.MessageText, []byte(msg)); err != nil {
			p.release(st)
			p.stop(st, nil)
			return
		}
	}
	st.mu.Lock()
	st.draining = time.AfterFunc(p.cfg.DrainTimeout, func() { p.release(st); p.stop(st, nil) })
	st.mu.Unlock()
}

func (p *STTProcessor) stopTurn(turn string, gen uint64) {
	p.mu.Lock()
	var victims []*sttStream
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

func (p *STTProcessor) release(st *sttStream) {
	p.mu.Lock()
	delete(p.all, st)
	if p.active == st {
		p.active = nil
	}
	p.mu.Unlock()
}

// stop closes the socket exactly once and ends the trace with err.
func (p *STTProcessor) stop(st *sttStream, err error) {
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
