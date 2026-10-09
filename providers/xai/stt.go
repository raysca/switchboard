//go:build xaispeech

package xai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// STTConfig holds configuration for the xAI Speech-to-Text processor.
type STTConfig struct {
	Endpoint       string // WebSocket endpoint, default: "wss://api.x.ai/v1/stt"
	Model          string // Default: "grok-voice-transcribe-2.0"
	Language       string // Default: "en"
	SampleRate     int    // Default: 16000
	Encoding       string // Default: "pcm"
	InterimResults bool   // Default: true
	APIKey         string // Bearer token
	CircuitBreaker *resilience.Breaker
	// RetryPolicy bounds opening the websocket: MaxAttempts tries of
	// AttemptTimeout each (zero = DefaultSTTOpenPolicy).
	RetryPolicy  resilience.Policy
	StreamOpener STTStreamOpener
	Now          func() time.Time // Receipt clock; policy time is carried in candidate frames.
}

// DefaultSTTConfig returns recommended default settings for xAI STT.
func DefaultSTTConfig() STTConfig {
	return STTConfig{
		Endpoint:       "wss://api.x.ai/v1/stt",
		Model:          "grok-voice-transcribe-2.0",
		Language:       "en",
		SampleRate:     16000,
		Encoding:       "pcm",
		InterimResults: true,
		RetryPolicy:    DefaultSTTOpenPolicy(),
	}
}

// DefaultSTTOpenPolicy is two tries of 5 s to open the STT websocket.
func DefaultSTTOpenPolicy() resilience.Policy {
	return resilience.Policy{MaxAttempts: 2, AttemptTimeout: 5 * time.Second, InitialBackoff: 100 * time.Millisecond}
}

// STTMessage represents an event received from the xAI STT stream.
type STTMessage struct {
	Type        string  `json:"type"` // "transcript.created", "transcript.partial", "transcript.done", "error"
	ID          string  `json:"id,omitempty"`
	Text        string  `json:"text,omitempty"`
	IsFinal     bool    `json:"is_final,omitempty"`
	SpeechFinal bool    `json:"speech_final,omitempty"`
	Start       float64 `json:"start,omitempty"`
	Duration    float64 `json:"duration,omitempty"`
	Language    string  `json:"language,omitempty"`
	Message     string  `json:"message,omitempty"`
	Code        string  `json:"code,omitempty"`
}

// STTStream abstracts an open bidirectional streaming session to xAI STT.
type STTStream interface {
	SendAudio(ctx context.Context, data []byte) error
	CloseSend(ctx context.Context) error
	Recv(ctx context.Context) (*STTMessage, error)
	Close() error
}

// STTStreamOpener abstracts stream establishment.
type STTStreamOpener interface {
	OpenStream(ctx context.Context, cfg STTConfig) (STTStream, error)
}

// wsSTTStream implements STTStream over a WebSocket connection.
type wsSTTStream struct {
	conn *websocket.Conn
}

func (s *wsSTTStream) SendAudio(ctx context.Context, data []byte) error {
	return s.conn.Write(ctx, websocket.MessageBinary, data)
}

func (s *wsSTTStream) CloseSend(ctx context.Context) error {
	return s.conn.Write(ctx, websocket.MessageText, []byte(`{"type":"audio.done"}`))
}

func (s *wsSTTStream) Recv(ctx context.Context) (*STTMessage, error) {
	msgType, data, err := s.conn.Read(ctx)
	if err != nil {
		return nil, err
	}
	if msgType != websocket.MessageText {
		return nil, nil
	}
	var msg STTMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

func (s *wsSTTStream) Close() error {
	return s.conn.Close(websocket.StatusNormalClosure, "done")
}

func sttPool() *netpool.Pool {
	return netpool.Get(netpool.Options{Provider: "xai", Stage: "stt", HTTP1Only: true})
}

// Warm pre-dials TCP+TLS only (no websocket session) when the real opener is
// used. Skipped while the breaker is open; never reports to it.
func (p *STTProcessor) Warm(ctx context.Context) error {
	if _, ok := p.cfg.StreamOpener.(*RealXAISTTOpener); !ok {
		return nil
	}
	if br := p.cfg.CircuitBreaker; br != nil && br.WouldRefuse() {
		sttPool().NoteSkipped()
		return nil
	}
	return sttPool().Warm(ctx, p.cfg.Endpoint)
}

// RealXAISTTOpener establishes live WebSocket streams to xAI.
type RealXAISTTOpener struct{}

func (o *RealXAISTTOpener) OpenStream(ctx context.Context, cfg STTConfig) (STTStream, error) {
	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = "wss://api.x.ai/v1/stt"
	}

	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("parse xai stt endpoint: %w", err)
	}

	q := u.Query()
	sr := cfg.SampleRate
	if sr <= 0 {
		sr = 16000
	}
	q.Set("sample_rate", fmt.Sprintf("%d", sr))
	enc := cfg.Encoding
	if enc == "" {
		enc = "pcm"
	}
	q.Set("encoding", enc)
	if cfg.Language != "" {
		q.Set("language", cfg.Language)
	}
	if cfg.InterimResults {
		q.Set("interim_results", "true")
	}
	if cfg.Model != "" {
		q.Set("model", cfg.Model)
	}
	u.RawQuery = q.Encode()

	opts := &websocket.DialOptions{
		HTTPClient: sttPool().Client(0),
		HTTPHeader: http.Header{
			"Authorization": []string{"Bearer " + cfg.APIKey},
		},
	}

	pol := cfg.RetryPolicy
	if pol.MaxAttempts <= 0 {
		pol = DefaultSTTOpenPolicy()
	}
	// The dial ctx only governs the handshake, so a per-try deadline cannot
	// cut the stream that follows.
	conn, err := resilience.OpenBounded(ctx, pol, classify, func(actx context.Context) (*websocket.Conn, error) {
		c, resp, derr := websocket.Dial(actx, u.String(), opts)
		if derr != nil {
			return nil, dialError(actx, resp, derr)
		}
		return c, nil
	})
	if err != nil {
		if errors.Is(err, resilience.ErrTimeout) { // a hang is an outage, not the caller's fault
			return nil, &ProviderError{Capability: "stt", Code: "TIMEOUT", Message: SanitizeString(err.Error()), Class: ClassificationRetryable, Cause: err}
		}
		return nil, MapError("stt", err)
	}

	return &wsSTTStream{conn: conn}, nil
}

type activeSTTStream struct {
	stream     STTStream
	cancel     context.CancelFunc
	ctx        context.Context
	id         int
	readyCh    chan struct{}
	turnID     string
	gen        uint64
	lastText   string
	base       string // finished utterances of this turn (speech_final texts)
	chunks     string // locked chunk finals of the utterance in progress
	settled    bool   // the last partial was a speech_final: no utterance is open
	drainTimer *time.Timer
	closing    bool
	// pendingDone: audio.done is owed because the turn ended before the stream
	// was ready; the receiver sends it after flushing the buffered audio.
	pendingDone bool
	stopOnce    sync.Once

	sendMu       sync.Mutex
	mu           sync.Mutex
	audioBuf     [][]byte
	isReady      bool
	traceSpan    core.Span
	tracePolicy  core.TracePolicy
	traceSession core.SessionID
	traceFinal   sync.Once
	traceEnd     sync.Once
}

func (st *activeSTTStream) firstFinal(turn string, gen uint64) {
	if st.traceSpan == nil {
		return
	}
	st.traceFinal.Do(func() {
		st.traceSpan.AddEvent(core.NewTraceEvent("stt.first_final", time.Now(), st.traceSession, core.TurnID(turn), core.Generation(gen), st.tracePolicy, nil))
	})
}
func (st *activeSTTStream) endTrace(err error) {
	st.traceEnd.Do(func() {
		if st.traceSpan == nil {
			return
		}
		st.mu.Lock()
		turn, gen := st.turnID, st.gen
		st.mu.Unlock()
		name := "stt.complete"
		if err != nil {
			name = "stt.failure"
		}
		st.traceSpan.AddEvent(core.NewTraceEvent(name, time.Now(), st.traceSession, core.TurnID(turn), core.Generation(gen), st.tracePolicy, nil))
		st.traceSpan.End(err)
	})
}

// STTProcessor implements core.Processor for xAI Speech-to-Text.
type STTProcessor struct {
	cfg     STTConfig
	session core.SessionController
	index   int

	mu        sync.Mutex
	active    *activeSTTStream
	draining  []*activeSTTStream
	streamID  int
	closed    bool
	turnID    string
	streamGen uint64

	// Metrics
	RequestsTotal atomic.Uint64
	ErrorsTotal   atomic.Uint64
}

// NewSTTProcessor creates a new xAI STTProcessor.
func NewSTTProcessor(cfg STTConfig) (*STTProcessor, error) {
	if cfg.Endpoint == "" {
		cfg.Endpoint = "wss://api.x.ai/v1/stt"
	}
	if cfg.StreamOpener == nil {
		cfg.StreamOpener = &RealXAISTTOpener{}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &STTProcessor{
		cfg: cfg,
	}, nil
}

// Attach registers the processor with the session pipeline and supervisor.
func (p *STTProcessor) Attach(s core.SessionController, index int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.session = s
	p.index = index
	return nil
}

// Close gracefully closes the processor and terminates any active stream.
func (p *STTProcessor) Close() error {
	p.mu.Lock()
	p.closed = true
	streams := append([]*activeSTTStream(nil), p.draining...)
	if p.active != nil {
		streams = append(streams, p.active)
	}
	p.active = nil
	p.draining = nil
	p.mu.Unlock()
	for _, st := range streams {
		p.stopStream(st)
	}
	return nil
}

// ProcessFrame processes frames traversing the pipeline.
func (p *STTProcessor) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	switch frame := f.(type) {
	case *core.StartFrame:
		return e.PushDirection(f, dir)

	case *core.UserStartedSpeakingFrame:
		p.mu.Lock()
		p.turnID = fmt.Sprintf("turn-%d-%d", frame.Gen, frame.Seq)
		p.streamGen = frame.Gen
		var old, parked *activeSTTStream
		if p.active != nil {
			p.active.mu.Lock()
			owned := p.active.turnID != "" && (p.active.turnID != p.turnID || p.active.gen != frame.Gen)
			if p.active.turnID == "" {
				p.active.turnID, p.active.gen = p.turnID, frame.Gen
			}
			p.active.mu.Unlock()
			if owned {
				parked = p.active
				p.draining = append(p.draining, p.active)
				p.active = nil
				old = p.trimDrainingLocked()
			}
		}
		p.mu.Unlock()
		p.stopStream(old)
		_ = p.beginDrain(ctx, parked) // another turn's stream finalizes and drains on its own
		if err := p.ensureStream(ctx); err != nil {
			p.ErrorsTotal.Add(1)
			return err
		}
		return e.Push(f)

	case *core.TurnEventFrame:
		switch frame.Kind {
		case core.TurnEventStarted:
			p.mu.Lock()
			p.turnID = string(frame.Turn)
			p.streamGen = uint64(frame.Gen)
			var old, parked *activeSTTStream
			if p.active != nil {
				p.active.mu.Lock()
				if p.active.turnID == p.turnID && p.active.gen < p.streamGen {
					// A turn manager barged in mid-utterance: keep the stream, move it to the new generation.
					p.active.gen = p.streamGen
				}
				owned := p.active.turnID != "" && (p.active.turnID != p.turnID || p.active.gen != p.streamGen)
				if p.active.turnID == "" {
					p.active.turnID, p.active.gen = p.turnID, p.streamGen
				}
				p.active.mu.Unlock()
				if owned {
					parked = p.active
					p.draining = append(p.draining, p.active)
					p.active = nil
					old = p.trimDrainingLocked()
				}
			}
			p.mu.Unlock()
			p.stopStream(old)
			_ = p.beginDrain(ctx, parked)
		case core.TurnEventCompleted:
			if err := p.completeStream(ctx, string(frame.Turn), uint64(frame.Gen)); err != nil {
				return err
			}
		case core.TurnEventInterrupted:
			p.stopMatchingStream(string(frame.Turn), uint64(frame.Gen))
		}
		return e.PushDirection(f, dir)

	case *core.InputAudioFrame:
		return p.handleAudio(ctx, frame, e)

	case *core.UserStoppedSpeakingFrame:
		// Half-close and detach the stream, and release the turn (like Deepgram): audio after the
		// stop opens a fresh unbound stream that the next UserStarted adopts, so a quick resume
		// keeps its onset instead of feeding a stream that already got audio.done.
		p.mu.Lock()
		active, old := p.active, (*activeSTTStream)(nil)
		if active != nil {
			p.active = nil
			p.draining = append(p.draining, active)
			old = p.trimDrainingLocked()
		}
		p.turnID, p.streamGen = "", 0
		p.mu.Unlock()
		p.stopStream(old)
		if active != nil {
			if err := p.beginDrain(ctx, active); err != nil {
				return err
			}
		}
		return e.PushDirection(f, dir)

	case *core.InterruptionFrame:
		p.mu.Lock()
		generation := uint64(frame.Gen)
		if p.session != nil && p.session.CurrentGeneration() > generation {
			generation = p.session.CurrentGeneration()
		}
		if generation > p.streamGen {
			// The session has advanced after barge-in. New streams inherit the
			// new generation; streams below remain owned by their original one.
			p.streamGen = generation
		}
		streams := append([]*activeSTTStream(nil), p.draining...)
		if p.active != nil {
			streams = append(streams, p.active)
		}
		p.active = nil
		p.draining = nil
		p.mu.Unlock()
		for _, st := range streams {
			p.stopStream(st)
		}
		return e.PushDirection(f, dir)

	case *core.CancelFrame:
		p.mu.Lock()
		streams := append([]*activeSTTStream(nil), p.draining...)
		if p.active != nil {
			streams = append(streams, p.active)
		}
		p.active = nil
		p.draining = nil
		p.mu.Unlock()
		for _, st := range streams {
			p.stopStream(st)
		}
		return e.PushDirection(f, dir)

	default:
		return e.PushDirection(f, dir)
	}
}

func (p *STTProcessor) handleAudio(ctx context.Context, f *core.InputAudioFrame, e core.Emitter) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.mu.Unlock()

	if err := p.ensureStream(ctx); err != nil {
		p.ErrorsTotal.Add(1)
		return err
	}

	p.mu.Lock()
	st := p.active
	p.mu.Unlock()

	if st == nil {
		return ErrProviderUnavailable
	}

	st.mu.Lock()
	if !st.isReady {
		// Buffer audio until transcript.created arrives. Live audio only fills the
		// handshake time; an STT failover replays up to 60 s at once into a new
		// stream, so the bound (3000 chunks = 60 s of 20 ms audio) covers that burst.
		if len(st.audioBuf) < 3000 {
			st.audioBuf = append(st.audioBuf, append([]byte(nil), f.Samples...))
		}
		st.mu.Unlock()
		p.RequestsTotal.Add(1)
		return e.Push(f)
	}
	st.mu.Unlock()

	sendCtx, sendCancel := context.WithTimeout(st.ctx, 500*time.Millisecond)
	defer sendCancel()

	st.sendMu.Lock()
	err := st.stream.SendAudio(sendCtx, f.Samples)
	st.sendMu.Unlock()
	if err != nil {
		if st.ctx.Err() != nil { // torn down concurrently: not a provider failure
			return e.Push(f)
		}
		st.endTrace(errors.New("provider_failure"))
		p.stopStream(st)
		provErr := MapError("stt", err)
		if p.cfg.CircuitBreaker != nil {
			p.cfg.CircuitBreaker.Report(err, classify)
		}
		return provErr
	}

	p.RequestsTotal.Add(1)
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

func (p *STTProcessor) ensureStream(ctx context.Context) error {
	p.mu.Lock()
	if p.active != nil || p.closed {
		p.mu.Unlock()
		return nil
	}
	turn, gen := p.turnID, p.streamGen
	p.mu.Unlock()

	// An open breaker stops the dial. WouldRefuse does not take the half-open probe: a cooled-down
	// breaker lets this dial through, and its one Report/RecordSuccess below settles the state.
	if br := p.cfg.CircuitBreaker; br != nil && br.WouldRefuse() {
		return ErrCircuitOpen
	}
	traceSession := core.SessionID("")
	if p.session != nil {
		traceSession = core.SessionID(strconv.FormatUint(p.session.ID(), 10))
	}
	span := core.StartContextSpan(ctx, "stt.stream", traceSession, core.TurnID(turn), core.Generation(gen), map[string]any{"provider": "xai"})
	policy := core.TraceContextFromContext(ctx).Policy
	streamCtx, cancel := context.WithCancel(p.ioParent(ctx))
	st, err := func() (STTStream, error) {
		defer func() {
			if v := recover(); v != nil {
				cancel()
				span.AddEvent(core.NewTraceEvent("stt.failure", time.Now(), traceSession, core.TurnID(turn), core.Generation(gen), policy, nil))
				span.End(errors.New("provider_failure"))
				panic(v)
			}
		}()
		return p.cfg.StreamOpener.OpenStream(streamCtx, p.cfg)
	}()
	if err != nil {
		span.AddEvent(core.NewTraceEvent("stt.failure", time.Now(), traceSession, core.TurnID(turn), core.Generation(gen), policy, nil))
		span.End(errors.New("provider_failure"))
		cancelled := streamCtx.Err() != nil // session torn down / caller gave up: not a provider outage
		cancel()
		provErr := MapError("stt", err)
		if p.cfg.CircuitBreaker != nil && !cancelled {
			p.cfg.CircuitBreaker.Report(err, classify)
		}
		return provErr
	}

	if p.cfg.CircuitBreaker != nil {
		p.cfg.CircuitBreaker.RecordSuccess()
	}

	p.mu.Lock()
	if p.closed || p.active != nil {
		closed := p.closed
		p.mu.Unlock()
		cancel()
		_ = st.Close()
		span.End(errors.New("canceled"))
		if closed {
			return core.ErrSessionClosed
		}
		return nil
	}
	p.streamID++
	active := &activeSTTStream{
		stream:    st,
		cancel:    cancel,
		ctx:       streamCtx,
		id:        p.streamID,
		readyCh:   make(chan struct{}),
		turnID:    p.turnID,
		gen:       p.streamGen,
		audioBuf:  make([][]byte, 0, 16),
		traceSpan: span, tracePolicy: policy, traceSession: traceSession,
		// Without a session no receiver runs, so nothing would ever deliver
		// transcript.created: treat the stream as ready (bare-processor use).
		isReady: p.session == nil,
	}
	p.active = active
	p.mu.Unlock()
	span.AddEvent(core.NewTraceEvent("stt.start", time.Now(), traceSession, core.TurnID(active.turnID), core.Generation(active.gen), policy, nil))

	if err := p.spawnReceiver(active); err != nil {
		p.mu.Lock()
		if p.active == active {
			p.active = nil
		}
		p.mu.Unlock()
		p.stopStream(active)
		active.endTrace(errors.New("canceled"))
		return err
	}
	return nil
}

func (p *STTProcessor) spawnReceiver(st *activeSTTStream) error {
	if p.session == nil {
		return nil
	}

	return p.session.Go(func(ctx context.Context) {
		var traceFailure error
		defer func() {
			if v := recover(); v != nil {
				traceFailure = errors.New("provider_failure")
				st.endTrace(traceFailure)
				panic(v)
			}
			st.endTrace(traceFailure)
		}()
		defer p.releaseStream(st)
		defer p.stopStream(st)
		for {
			select {
			case <-st.ctx.Done():
				return
			case <-ctx.Done():
				return
			default:
			}

			msg, err := st.stream.Recv(st.ctx)
			if err != nil {
				if errors.Is(err, context.Canceled) || st.ctx.Err() != nil {
					traceFailure = errors.New("canceled")
					return
				}
				st.mu.Lock()
				bound := st.turnID != ""
				// A drop after audio.done but before transcript.done, with words heard and no
				// speech_final yet, still loses the utterance (M3-09 follow-up).
				closing := st.closing && !(bound && !st.settled && st.lastText != "")
				st.mu.Unlock()
				closeStatus := websocket.CloseStatus(err) != -1
				if closing || (!bound && (errors.Is(err, io.EOF) || closeStatus)) {
					// Drained after audio.done, or an idle pre-opened stream the
					// server closed: nothing was lost.
					return
				}
				// A drop while a turn is bound and audio.done is not sent loses the
				// utterance: retire the partial and raise a retryable error so
				// STTFailover fires (M3-09).
				p.ErrorsTotal.Add(1)
				traceFailure = errors.New("provider_failure")
				providerErr := MapError("stt", err)
				if bound {
					if p.cfg.CircuitBreaker != nil {
						p.cfg.CircuitBreaker.Report(err, classify)
					}
					p.failBoundStream(st, providerErr, "xai stream dropped mid-utterance")
				} else if p.session != nil {
					st.mu.Lock()
					turnGen := st.gen
					st.mu.Unlock()
					_ = p.session.Reenter(&core.ErrorFrame{Err: providerErr}, core.Downstream, p.index+1, turnGen)
				}
				return
			}

			if msg == nil {
				continue
			}

			switch msg.Type {
			case "transcript.created":
				st.sendMu.Lock()
				st.mu.Lock()
				st.isReady = true
				select {
				case <-st.readyCh:
				default:
					close(st.readyCh)
				}
				// Publish readiness while sendMu reserves the wire. New audio
				// waits until these earlier buffered chunks have been sent.
				buffered := st.audioBuf
				st.audioBuf = nil
				st.mu.Unlock()
				var sendErr error
				for _, chunk := range buffered {
					sendCtx, cancel := context.WithTimeout(st.ctx, 500*time.Millisecond)
					sendErr = st.stream.SendAudio(sendCtx, chunk)
					cancel()
					if sendErr != nil {
						break
					}
				}
				// The turn ended before the handshake finished: audio.done goes
				// after the buffered audio, never before it (M3-02).
				st.mu.Lock()
				owed := st.pendingDone
				st.pendingDone = false
				st.mu.Unlock()
				if owed && sendErr == nil {
					sendErr = st.stream.CloseSend(st.ctx)
					if sendErr == nil {
						p.armDrainTimer(st)
					}
				}
				st.sendMu.Unlock()
				if sendErr != nil {
					if st.ctx.Err() != nil {
						traceFailure = errors.New("canceled")
						return
					}
					p.ErrorsTotal.Add(1)
					traceFailure = errors.New("provider_failure")
					providerErr := MapError("stt", sendErr)
					if p.cfg.CircuitBreaker != nil {
						p.cfg.CircuitBreaker.Report(sendErr, classify)
					}
					p.failBoundStream(st, providerErr, "xai buffered audio send failed")
					return
				}

			case "transcript.partial":
				if msg.Text == "" {
					continue
				}

				st.mu.Lock()
				turn := st.turnID
				turnGen := st.gen
				if turnGen == 0 && p.session != nil {
					turnGen = p.session.CurrentGeneration()
				}
				// xAI sends per-segment text: interims and chunk finals carry only the current
				// chunk, speech_final the stitched utterance. Candidates are cumulative for the
				// turn, like the other STT adapters (interim = locked + chunk).
				seg := strings.TrimSpace(msg.Text)
				var text string
				switch {
				case msg.SpeechFinal:
					text = joinWords(st.base, seg)
					st.base, st.chunks = text, ""
				case msg.IsFinal:
					st.chunks = joinWords(st.chunks, seg)
					text = joinWords(st.base, st.chunks)
				default:
					text = joinWords(joinWords(st.base, st.chunks), seg)
				}
				st.lastText = text
				st.settled = msg.SpeechFinal
				isProviderFinal := msg.IsFinal || msg.SpeechFinal
				isUtteranceFinal := msg.SpeechFinal
				st.mu.Unlock()
				if turn == "" { // unbound (between turns): the adopting turn's next candidate carries the text
					continue
				}

				// is_final closes a chunk but does not close the utterance;
				// speech_final is the utterance-final signal.
				frame := &core.TranscriptCandidateFrame{
					Text:           text,
					Turn:           core.TurnID(turn),
					Gen:            core.Generation(turnGen),
					Timestamp:      p.cfg.Now(),
					Final:          isProviderFinal,
					EndOfUtterance: isUtteranceFinal,
					Stability:      boolStability(msg.IsFinal),
					HasStability:   false,
					Provider:       "xai",
				}
				if err := p.session.Reenter(frame, core.Downstream, p.index+1, turnGen); err == nil && isProviderFinal {
					st.firstFinal(turn, turnGen)
				}

			case "transcript.done":
				st.mu.Lock()
				turn := st.turnID
				turnGen := st.gen
				if turnGen == 0 && p.session != nil {
					turnGen = p.session.CurrentGeneration()
				}
				lastText := msg.Text
				if lastText == "" {
					lastText = st.lastText
				}
				// Nothing is pending when the last candidate was already a speech_final: a second
				// final would only be a late duplicate that lands in the next turn's window.
				pending := !st.settled
				st.mu.Unlock()

				// transcript.done closes the stream/utterance. An utterance still open at that
				// point (audio.done before the server endpointed) is finalized from its text.
				if lastText != "" && pending && turn != "" && p.session != nil {
					frame := &core.TranscriptCandidateFrame{
						Text: lastText, Turn: core.TurnID(turn), Gen: core.Generation(turnGen),
						Timestamp: p.cfg.Now(), Final: true, EndOfUtterance: true, Provider: "xai",
					}
					if err := p.session.Reenter(frame, core.Downstream, p.index+1, turnGen); err == nil {
						st.firstFinal(turn, turnGen)
					}
				}

				return

			case "error":
				p.ErrorsTotal.Add(1)
				traceFailure = errors.New("provider_failure")
				errMsg := msg.Message
				if errMsg == "" {
					errMsg = msg.Code
				}
				if p.session != nil {
					st.mu.Lock()
					turnGen := st.gen
					st.mu.Unlock()
					_ = p.session.Reenter(&core.ErrorFrame{
						Err: MapError("stt", errors.New(errMsg)),
					}, core.Downstream, p.index+1, turnGen)
				}
				return
			}
		}
	})
}

func (p *STTProcessor) completeStream(ctx context.Context, turn string, gen uint64) error {
	p.mu.Lock()
	st := p.active
	p.mu.Unlock()
	if st == nil {
		return nil
	}
	st.mu.Lock()
	matches := st.turnID == turn && st.gen == gen
	st.mu.Unlock()
	if !matches {
		return nil
	}
	return p.beginDrain(ctx, st)
}

func (p *STTProcessor) beginDrain(ctx context.Context, st *activeSTTStream) error {
	if st == nil {
		return nil
	}
	st.mu.Lock()
	if st.closing {
		st.mu.Unlock()
		return nil
	}
	st.closing = true
	if !st.isReady { // the receiver sends audio.done after flushing the buffer
		st.pendingDone = true
		st.mu.Unlock()
		return nil
	}
	st.mu.Unlock()
	st.sendMu.Lock()
	err := st.stream.CloseSend(ctx)
	st.sendMu.Unlock()
	if err != nil {
		st.endTrace(errors.New("provider_failure"))
		p.stopStream(st)
		st.mu.Lock()
		st.closing = false
		st.mu.Unlock()
		return err
	}
	p.armDrainTimer(st)
	return nil
}

// failBoundStream retires the partial transcript of the stream's turn and
// publishes a transcript-invalidating error (what STTFailover reacts to).
func (p *STTProcessor) failBoundStream(st *activeSTTStream, providerErr error, what string) {
	st.mu.Lock()
	turn, turnGen := st.turnID, st.gen
	st.mu.Unlock()
	if turnGen == 0 {
		turnGen = p.session.CurrentGeneration()
	}
	if turn != "" && turnGen != 0 {
		if err := p.session.Reenter(&core.TranscriptInvalidationFrame{
			Turn: core.TurnID(turn), Gen: core.Generation(turnGen), Err: providerErr,
		}, core.Downstream, p.index+1, turnGen); err != nil {
			return
		}
	}
	_ = p.session.Reenter(&core.ErrorFrame{
		Err:  fmt.Errorf("%w: %s: %v", core.ErrTranscriptInvalidated, what, providerErr),
		Turn: core.TurnID(turn), Gen: core.Generation(turnGen),
	}, core.Downstream, p.index+1, turnGen)
}

// armDrainTimer bounds how long a half-closed stream may wait for its final.
func (p *STTProcessor) armDrainTimer(st *activeSTTStream) {
	st.mu.Lock()
	if st.drainTimer == nil {
		// The stream is detached, so a long wait costs nothing; live finals land 0.2-0.4 s after
		// audio.done. ponytail: fixed bound like Deepgram's default DrainTimeout; config if needed.
		st.drainTimer = time.AfterFunc(2*time.Second, func() {
			st.mu.Lock()
			if st.drainTimer == nil {
				st.mu.Unlock()
				return
			}
			st.drainTimer = nil
			st.mu.Unlock()
			st.stopOnce.Do(func() {
				st.cancel()
				_ = st.stream.Close()
			})
			p.releaseStream(st)
		})
	}
	st.mu.Unlock()
}

func (p *STTProcessor) stopDrainTimer(st *activeSTTStream) {
	st.mu.Lock()
	if st.drainTimer != nil {
		st.drainTimer.Stop()
		st.drainTimer = nil
	}
	st.mu.Unlock()
}

func (p *STTProcessor) stopStream(st *activeSTTStream) {
	if st == nil {
		return
	}
	p.stopDrainTimer(st)
	st.stopOnce.Do(func() {
		st.cancel()
		_ = st.stream.Close()
	})
}

func (p *STTProcessor) stopMatchingStream(turn string, gen uint64) {
	p.mu.Lock()
	st := p.active
	if st != nil {
		st.mu.Lock()
		matches := st.turnID == turn && st.gen == gen
		st.mu.Unlock()
		if matches {
			p.active = nil
		} else {
			st = nil
		}
	}
	if st == nil {
		for i, candidate := range p.draining {
			candidate.mu.Lock()
			matches := candidate.turnID == turn && candidate.gen == gen
			candidate.mu.Unlock()
			if matches {
				st = candidate
				p.draining = append(p.draining[:i], p.draining[i+1:]...)
				break
			}
		}
	}
	p.mu.Unlock()
	p.stopStream(st)
}

func (p *STTProcessor) trimDrainingLocked() *activeSTTStream {
	const maxDrainingStreams = 8
	if len(p.draining) <= maxDrainingStreams {
		return nil
	}
	oldest := p.draining[0]
	p.draining = p.draining[1:]
	return oldest
}

func (p *STTProcessor) releaseStream(st *activeSTTStream) {
	p.mu.Lock()
	if p.active == st {
		p.active = nil
	}
	for i, candidate := range p.draining {
		if candidate == st {
			p.draining = append(p.draining[:i], p.draining[i+1:]...)
			break
		}
	}
	p.mu.Unlock()
}

func joinWords(a, b string) string {
	if a == "" || b == "" {
		return a + b
	}
	return a + " " + b
}

func boolStability(stable bool) float64 {
	if stable {
		return 1
	}
	return 0
}

// dialError classifies a failed websocket dial: an HTTP refusal by its status
// (4xx is the caller's fault), cancellation as non-retryable, and anything
// else (timeout, reset, DNS) as a retryable outage the breaker should count.
func dialError(ctx context.Context, resp *http.Response, err error) error {
	if resp != nil {
		return MapHTTPError("stt", resp, "", err)
	}
	if ctx.Err() != nil && !errors.Is(context.Cause(ctx), resilience.ErrTimeout) {
		return MapError("stt", err) // cancelled by the caller
	}
	return &ProviderError{Capability: "stt", Code: "ERROR", Message: SanitizeString(err.Error()), Class: ClassificationRetryable, Cause: err}
}
