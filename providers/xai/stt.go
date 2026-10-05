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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"govoice/core"
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
	CircuitBreaker *CircuitBreaker
	StreamOpener   STTStreamOpener
	Now            func() time.Time // Receipt clock; policy time is carried in candidate frames.
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
	}
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
		HTTPHeader: http.Header{
			"Authorization": []string{"Bearer " + cfg.APIKey},
		},
	}

	conn, _, err := websocket.Dial(ctx, u.String(), opts)
	if err != nil {
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
	drainTimer *time.Timer
	closing    bool
	stopOnce   sync.Once

	sendMu   sync.Mutex
	mu       sync.Mutex
	audioBuf [][]byte
	isReady  bool
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
		var old *activeSTTStream
		if p.active != nil {
			p.active.mu.Lock()
			owned := p.active.turnID != "" && (p.active.turnID != p.turnID || p.active.gen != frame.Gen)
			if p.active.turnID == "" {
				p.active.turnID, p.active.gen = p.turnID, frame.Gen
			}
			p.active.mu.Unlock()
			if owned {
				p.draining = append(p.draining, p.active)
				p.active = nil
				old = p.trimDrainingLocked()
			}
		}
		p.mu.Unlock()
		p.stopStream(old)
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
			var old *activeSTTStream
			if p.active != nil {
				p.active.mu.Lock()
				owned := p.active.turnID != "" && (p.active.turnID != p.turnID || p.active.gen != p.streamGen)
				if p.active.turnID == "" {
					p.active.turnID, p.active.gen = p.turnID, p.streamGen
				}
				p.active.mu.Unlock()
				if owned {
					p.draining = append(p.draining, p.active)
					p.active = nil
					old = p.trimDrainingLocked()
				}
			}
			p.mu.Unlock()
			p.stopStream(old)
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
		p.mu.Lock()
		active := p.active
		p.mu.Unlock()
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
		// Buffer audio until transcript.created arrives (bound to 50 chunks = 1s of 20ms audio)
		if len(st.audioBuf) < 50 {
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
		provErr := MapError("stt", err)
		if p.cfg.CircuitBreaker != nil {
			p.cfg.CircuitBreaker.RecordFailure()
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
	p.mu.Unlock()

	streamCtx, cancel := context.WithCancel(p.ioParent(ctx))
	st, err := p.cfg.StreamOpener.OpenStream(streamCtx, p.cfg)
	if err != nil {
		cancel()
		provErr := MapError("stt", err)
		if p.cfg.CircuitBreaker != nil {
			p.cfg.CircuitBreaker.RecordFailure()
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
		if closed {
			return core.ErrSessionClosed
		}
		return nil
	}
	p.streamID++
	active := &activeSTTStream{
		stream:   st,
		cancel:   cancel,
		ctx:      streamCtx,
		id:       p.streamID,
		readyCh:  make(chan struct{}),
		turnID:   p.turnID,
		gen:      p.streamGen,
		audioBuf: make([][]byte, 0, 16),
	}
	p.active = active
	p.mu.Unlock()

	if err := p.spawnReceiver(active); err != nil {
		p.mu.Lock()
		if p.active == active {
			p.active = nil
		}
		p.mu.Unlock()
		p.stopStream(active)
		return err
	}
	return nil
}

func (p *STTProcessor) spawnReceiver(st *activeSTTStream) error {
	if p.session == nil {
		return nil
	}

	return p.session.Go(func(ctx context.Context) {
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
				if errors.Is(err, context.Canceled) || st.ctx.Err() != nil || errors.Is(err, io.EOF) {
					return
				}
				if strings.Contains(err.Error(), "closed") || strings.Contains(err.Error(), "1006") || strings.Contains(err.Error(), "1000") {
					return
				}
				p.ErrorsTotal.Add(1)
				if p.session != nil {
					st.mu.Lock()
					turnGen := st.gen
					st.mu.Unlock()
					_ = p.session.Reenter(&core.ErrorFrame{
						Err: MapError("stt", err),
					}, core.Downstream, p.index+1, turnGen)
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
				for _, chunk := range buffered {
					sendCtx, cancel := context.WithTimeout(st.ctx, 500*time.Millisecond)
					err := st.stream.SendAudio(sendCtx, chunk)
					cancel()
					if err != nil {
						break
					}
				}
				st.sendMu.Unlock()

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
				st.lastText = msg.Text
				isProviderFinal := msg.IsFinal || msg.SpeechFinal
				isUtteranceFinal := msg.SpeechFinal
				st.mu.Unlock()

				// is_final closes a chunk but does not close the utterance;
				// speech_final is the utterance-final signal.
				frame := &core.TranscriptCandidateFrame{
					Text:           msg.Text,
					Turn:           core.TurnID(turn),
					Gen:            core.Generation(turnGen),
					Timestamp:      p.cfg.Now(),
					Final:          isProviderFinal,
					EndOfUtterance: isUtteranceFinal,
					Stability:      boolStability(msg.IsFinal),
					HasStability:   false,
					Provider:       "xai",
				}
				_ = p.session.Reenter(frame, core.Downstream, p.index+1, turnGen)

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
				st.mu.Unlock()

				// transcript.done closes the stream/utterance. Re-emitting the last
				// text as a raw final candidate is safe; common policy deduplicates it.
				if lastText != "" && p.session != nil {
					frame := &core.TranscriptCandidateFrame{
						Text: lastText, Turn: core.TurnID(turn), Gen: core.Generation(turnGen),
						Timestamp: p.cfg.Now(), Final: true, EndOfUtterance: true, Provider: "xai",
					}
					_ = p.session.Reenter(frame, core.Downstream, p.index+1, turnGen)
				}

				return

			case "error":
				p.ErrorsTotal.Add(1)
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
	st.mu.Unlock()
	st.sendMu.Lock()
	err := st.stream.CloseSend(ctx)
	st.sendMu.Unlock()
	if err != nil {
		st.mu.Lock()
		st.closing = false
		st.mu.Unlock()
		return err
	}
	st.mu.Lock()
	if st.drainTimer == nil {
		st.drainTimer = time.AfterFunc(500*time.Millisecond, func() {
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
	return nil
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

func boolStability(stable bool) float64 {
	if stable {
		return 1
	}
	return 0
}
