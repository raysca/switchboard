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
	stream STTStream
	cancel context.CancelFunc
	ctx    context.Context
	id     int
}

// STTProcessor implements core.Processor for xAI Speech-to-Text.
type STTProcessor struct {
	cfg     STTConfig
	session core.SessionController
	index   int

	mu        sync.Mutex
	active    *activeSTTStream
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
	defer p.mu.Unlock()
	p.closed = true
	if p.active != nil {
		p.active.cancel()
		_ = p.active.stream.Close()
		p.active = nil
	}
	return nil
}

// ProcessFrame processes frames traversing the pipeline.
func (p *STTProcessor) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	switch frame := f.(type) {
	case *core.StartFrame:
		return p.ensureStream(ctx)

	case *core.UserStartedSpeakingFrame:
		p.mu.Lock()
		p.turnID = fmt.Sprintf("turn-%d-%d", frame.Gen, frame.Seq)
		p.streamGen = frame.Gen
		p.mu.Unlock()
		if err := p.ensureStream(ctx); err != nil {
			p.ErrorsTotal.Add(1)
		}
		return e.Push(f)

	case *core.InputAudioFrame:
		return p.handleAudio(ctx, frame, e)

	case *core.UserStoppedSpeakingFrame:
		p.mu.Lock()
		active := p.active
		p.mu.Unlock()
		if active != nil {
			_ = active.stream.CloseSend(ctx)
		}
		return e.Push(f)

	case *core.InterruptionFrame:
		p.mu.Lock()
		if p.active != nil {
			p.active.cancel()
			_ = p.active.stream.Close()
			p.active = nil
		}
		p.mu.Unlock()
		return e.Push(f)

	case *core.CancelFrame:
		p.mu.Lock()
		if p.active != nil {
			p.active.cancel()
			_ = p.active.stream.Close()
			p.active = nil
		}
		p.mu.Unlock()
		return e.Push(f)

	default:
		return e.Push(f)
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

	sendCtx, sendCancel := context.WithTimeout(st.ctx, 500*time.Millisecond)
	defer sendCancel()

	if err := st.stream.SendAudio(sendCtx, f.Samples); err != nil {
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
	p.streamID++
	active := &activeSTTStream{
		stream: st,
		cancel: cancel,
		ctx:    streamCtx,
		id:     p.streamID,
	}
	p.active = active
	p.mu.Unlock()

	p.spawnReceiver(active)
	return nil
}

func (p *STTProcessor) spawnReceiver(st *activeSTTStream) {
	if p.session == nil {
		return
	}

	_ = p.session.Go(func(ctx context.Context) {
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
				p.mu.Lock()
				if p.active == st {
					p.active = nil
				}
				p.mu.Unlock()

				if errors.Is(err, context.Canceled) || st.ctx.Err() != nil || errors.Is(err, io.EOF) {
					return
				}
				if strings.Contains(err.Error(), "closed") || strings.Contains(err.Error(), "1006") || strings.Contains(err.Error(), "1000") {
					return
				}
				p.ErrorsTotal.Add(1)
				if p.session != nil {
					_ = p.session.Reenter(&core.ErrorFrame{
						Err: MapError("stt", err),
					}, core.Downstream, p.index+1, p.streamGen)
				}
				return
			}

			if msg == nil {
				continue
			}

			switch msg.Type {
			case "transcript.partial":
				if msg.Text == "" {
					continue
				}

				p.mu.Lock()
				turn := p.turnID
				turnGen := p.streamGen
				if turnGen == 0 && p.session != nil {
					turnGen = p.session.CurrentGeneration()
				}
				p.mu.Unlock()

				if msg.SpeechFinal || msg.IsFinal {
					frame := &core.FinalTranscriptionFrame{
						Text:      msg.Text,
						Turn:      turn,
						Gen:       turnGen,
						Timestamp: time.Now(),
					}
					_ = p.session.Reenter(frame, core.Downstream, p.index+1, turnGen)
				} else {
					frame := &core.InterimTranscriptionFrame{
						Text:      msg.Text,
						Turn:      turn,
						Gen:       turnGen,
						Timestamp: time.Now(),
					}
					_ = p.session.Reenter(frame, core.Downstream, p.index+1, turnGen)
				}

			case "transcript.done":
				// Stream turn concluded
				p.mu.Lock()
				if p.active == st {
					p.active = nil
				}
				p.mu.Unlock()
				return

			case "error":
				p.ErrorsTotal.Add(1)
				errMsg := msg.Message
				if errMsg == "" {
					errMsg = msg.Code
				}
				if p.session != nil {
					_ = p.session.Reenter(&core.ErrorFrame{
						Err: MapError("stt", errors.New(errMsg)),
					}, core.Downstream, p.index+1, p.streamGen)
				}
			}
		}
	})
}
