package ws

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"govoice/core"
	"govoice/core/transport/resample"
	"govoice/transport/token"
)

var (
	ErrHandshakeLimitExceeded = errors.New("ws: maximum pending handshakes exceeded")
	ErrMaxSessionsExceeded    = errors.New("ws: maximum concurrent sessions exceeded")
	ErrConnectionClosed       = errors.New("ws: connection closed")
)

// ServerConfig configures WebSocket connection lifecycle, timeouts, limits, and security.
type ServerConfig struct {
	Security              SecurityConfig
	TokenValidator        *token.Validator
	Pacer                 PacerConfig
	HandshakeTimeout      time.Duration // Max handshake duration (default: 5s)
	ReadTimeout           time.Duration // Idle read timeout (default: 30s)
	PingInterval          time.Duration // Keepalive ping interval (default: 10s)
	MaxConnectionLifetime time.Duration // Max duration for a voice call (default: 60m)
	MaxConcurrentSessions int           // Max active voice sessions (default: 100)
	MaxPendingHandshakes  int           // Max handshakes in flight (default: 50)
	FixedPlayerRate       int           // Fixed client player rate (default: 24000)
}

// DefaultServerConfig returns production-ready server parameters.
func DefaultServerConfig(val *token.Validator) ServerConfig {
	return ServerConfig{
		Security: SecurityConfig{
			AllowedOrigins:     []string{"*"},
			AllowMissingOrigin: true,
		},
		TokenValidator:        val,
		Pacer:                 DefaultPacerConfig(),
		HandshakeTimeout:      5 * time.Second,
		ReadTimeout:           30 * time.Second,
		PingInterval:          10 * time.Second,
		MaxConnectionLifetime: 60 * time.Minute,
		MaxConcurrentSessions: 100,
		MaxPendingHandshakes:  50,
		FixedPlayerRate:       FixedPlayerSampleRate,
	}
}

// Connection represents a live, authenticated WebSocket voice connection.
type Connection struct {
	id         uint64
	conn       *websocket.Conn
	claims     *token.Claims
	cfg        ServerConfig
	pacer      *PacedWriter
	session    *core.Session
	seqCounter atomic.Uint64

	// Resamplers
	resamplersMu sync.Mutex
	inResamplers map[int]*resample.Resampler // inRate -> 16000
	outResampler *resample.Resampler         // providerRate -> 24000

	ctx    context.Context
	cancel context.CancelFunc

	closed    atomic.Bool
	closeOnce sync.Once
}

// Server handles HTTP upgrades to WebSocket voice connections.
type Server struct {
	cfg        ServerConfig
	activeMu   sync.Mutex
	activeSess int
	pendingMu  sync.Mutex
	pendingHsh int
	idCounter  atomic.Uint64

	// Session factory callback
	OnSessionReady func(conn *Connection) (*core.Session, error)
}

// NewServer creates a new WebSocket voice server.
func NewServer(cfg ServerConfig) *Server {
	if cfg.HandshakeTimeout <= 0 {
		cfg.HandshakeTimeout = 5 * time.Second
	}
	if cfg.ReadTimeout <= 0 {
		cfg.ReadTimeout = 30 * time.Second
	}
	if cfg.MaxConcurrentSessions <= 0 {
		cfg.MaxConcurrentSessions = 100
	}
	if cfg.MaxPendingHandshakes <= 0 {
		cfg.MaxPendingHandshakes = 50
	}
	if cfg.FixedPlayerRate <= 0 {
		cfg.FixedPlayerRate = FixedPlayerSampleRate
	}

	return &Server{cfg: cfg}
}

// ServeHTTP processes WebSocket upgrade requests with pre-upgrade token and origin checks.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 1. Pending handshakes limit
	s.pendingMu.Lock()
	if s.pendingHsh >= s.cfg.MaxPendingHandshakes {
		s.pendingMu.Unlock()
		http.Error(w, ErrHandshakeLimitExceeded.Error(), http.StatusTooManyRequests)
		return
	}
	s.pendingHsh++
	s.pendingMu.Unlock()
	defer func() {
		s.pendingMu.Lock()
		s.pendingHsh--
		s.pendingMu.Unlock()
	}()

	// 2. Concurrency limit
	s.activeMu.Lock()
	if s.activeSess >= s.cfg.MaxConcurrentSessions {
		s.activeMu.Unlock()
		http.Error(w, ErrMaxSessionsExceeded.Error(), http.StatusTooManyRequests)
		return
	}
	s.activeMu.Unlock()

	// 3. Security: TLS enforcement
	if s.cfg.Security.RequireTLS && !s.cfg.Security.IsSecure(r) {
		http.Error(w, ErrInsecureScheme.Error(), http.StatusBadRequest)
		return
	}

	// 4. Security: Origin check
	if err := s.cfg.Security.CheckOrigin(r); err != nil {
		if errors.Is(err, ErrMissingOrigin) {
			http.Error(w, "missing origin header", http.StatusForbidden)
			return
		}
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}

	// 5. Authentication: Validate and atomically consume single-use token from query
	tokenStr := r.URL.Query().Get("token")
	if tokenStr == "" {
		http.Error(w, "missing admission token", http.StatusUnauthorized)
		return
	}

	claims, err := s.cfg.TokenValidator.ValidateAndConsume(tokenStr)
	if err != nil {
		if errors.Is(err, token.ErrTokenReplayed) {
			http.Error(w, "token already consumed (single-use)", http.StatusUnauthorized)
			return
		}
		if errors.Is(err, token.ErrTokenExpired) {
			http.Error(w, "token expired", http.StatusUnauthorized)
			return
		}
		http.Error(w, fmt.Sprintf("invalid token: %v", err), http.StatusUnauthorized)
		return
	}

	// 6. Upgrade WebSocket connection
	connCtx, connCancel := context.WithTimeout(r.Context(), s.cfg.HandshakeTimeout)
	defer connCancel()
	r = r.WithContext(connCtx)

	wsConn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode:    websocket.CompressionDisabled, // Audio sessions must have compression disabled
		InsecureSkipVerify: true,                          // Origin already checked above
	})
	if err != nil {
		return
	}

	s.activeMu.Lock()
	s.activeSess++
	s.activeMu.Unlock()

	sessCtx, sessCancel := context.WithCancel(context.Background())
	if s.cfg.MaxConnectionLifetime > 0 {
		sessCtx, sessCancel = context.WithTimeout(sessCtx, s.cfg.MaxConnectionLifetime)
	}

	conn := &Connection{
		id:           s.idCounter.Add(1),
		conn:         wsConn,
		claims:       claims,
		cfg:          s.cfg,
		ctx:          sessCtx,
		cancel:       sessCancel,
		inResamplers: make(map[int]*resample.Resampler),
	}

	// Initialize PacedWriter
	pacer := NewPacedWriter(s.cfg.Pacer, func(ctx context.Context, data []byte) error {
		return wsConn.Write(ctx, websocket.MessageBinary, data)
	})
	conn.pacer = pacer

	// Hook write completion re-entry
	pacer.OnWriteComplete = func(f *core.OutputAudioFrame, err error) {
		if conn.session != nil {
			_ = conn.session.Reenter(&core.WriteCompletionFrame{
				Turn:    f.Turn,
				Gen:     f.Gen,
				UnitID:  f.UnitID,
				Seq:     f.Seq,
				IsFinal: f.IsFinal,
				Err:     err,
			}, core.Downstream, 0, f.Gen)
		}
	}

	defer func() {
		conn.Close()
		s.activeMu.Lock()
		s.activeSess--
		s.activeMu.Unlock()
	}()

	// Allocate session if factory provided
	if s.OnSessionReady != nil {
		session, err := s.OnSessionReady(conn)
		if err != nil {
			_ = wsConn.Close(websocket.StatusInternalError, "session allocation failed")
			return
		}
		conn.session = session
		if err := session.Start(); err != nil {
			_ = wsConn.Close(websocket.StatusInternalError, "session start failed")
			return
		}
	}

	// Run independent supervised read and write tasks
	var wg sync.WaitGroup
	wg.Add(2)

	// Writer task
	go func() {
		defer wg.Done()
		if err := pacer.Run(conn.ctx); err != nil && !errors.Is(err, context.Canceled) {
			if errors.Is(err, ErrSlowClient) {
				_ = wsConn.Close(websocket.StatusPolicyViolation, "slow client terminated")
			}
			conn.cancel()
		}
	}()

	// Reader task
	go func() {
		defer wg.Done()
		conn.readLoop()
		conn.cancel()
	}()

	// Wait for connection close or error
	<-conn.ctx.Done()
	pacer.Close()
	_ = wsConn.Close(websocket.StatusNormalClosure, "call concluded")
	wg.Wait()

	if conn.session != nil {
		_ = conn.session.Close()
	}
}

func (c *Connection) readLoop() {
	for {
		readCtx := c.ctx
		if c.cfg.ReadTimeout > 0 {
			var cancel context.CancelFunc
			readCtx, cancel = context.WithTimeout(c.ctx, c.cfg.ReadTimeout)
			defer cancel()
		}

		msgType, data, err := c.conn.Read(readCtx)
		if err != nil {
			return
		}

		if msgType != websocket.MessageBinary {
			continue // Only binary protobuf is accepted
		}

		seq := c.seqCounter.Add(1)
		frame, err := DecodeBinaryFrame(data, seq, time.Now())
		if err != nil {
			// Malformed frame: close connection
			_ = c.conn.Close(websocket.StatusUnsupportedData, err.Error())
			return
		}

		// Audio input handling
		if audio, ok := frame.(*core.InputAudioFrame); ok {
			// Resample to 16 kHz internal format if needed
			if audio.SampleRate != InternalSampleRate {
				resampled, err := c.resampleInput(audio.Samples, audio.SampleRate)
				if err == nil {
					audio.Samples = resampled
					audio.SampleRate = InternalSampleRate
				}
			}
		}

		if c.session != nil {
			if err := c.session.Post(frame); err != nil {
				// Queue full or session closed
				if errors.Is(err, core.ErrControlQueueFull) {
					_ = c.conn.Close(websocket.StatusPolicyViolation, "control queue full")
					return
				}
			}
		}
	}
}

func (c *Connection) resampleInput(samples []byte, inRate int) ([]byte, error) {
	c.resamplersMu.Lock()
	defer c.resamplersMu.Unlock()

	r, exists := c.inResamplers[inRate]
	if !exists {
		var err error
		r, err = resample.NewResampler(inRate, InternalSampleRate)
		if err != nil {
			return nil, err
		}
		c.inResamplers[inRate] = r
	}

	return r.ResamplePCM16(samples)
}

// Close gracefully terminates the connection.
func (c *Connection) Close() {
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		c.cancel()
		if c.pacer != nil {
			c.pacer.Close()
		}
	})
}

// ProcessFrame implements core.Processor to serve as the transport audio sink.
func (c *Connection) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	switch f.Type() {
	case core.FrameOutputAudio:
		oaf := f.(*core.OutputAudioFrame)
		// Convert provider rate to fixed player rate if necessary
		if oaf.SampleRate != c.cfg.FixedPlayerRate {
			c.resamplersMu.Lock()
			if c.outResampler == nil {
				var err error
				c.outResampler, err = resample.NewResampler(oaf.SampleRate, c.cfg.FixedPlayerRate)
				if err != nil {
					c.resamplersMu.Unlock()
					return err
				}
			}
			resampled, err := c.outResampler.ResamplePCM16(oaf.Samples)
			c.resamplersMu.Unlock()
			if err != nil {
				return err
			}
			oaf.Samples = resampled
			oaf.SampleRate = c.cfg.FixedPlayerRate
		}

		return c.pacer.Enqueue(oaf)

	case core.FrameInterruption:
		intr := f.(*core.InterruptionFrame)
		c.pacer.Flush(intr.Gen)
		return e.PushDirection(f, dir)

	case core.FrameFlush:
		ff := f.(*core.FlushFrame)
		c.pacer.Flush(ff.Gen)
		// Acknowledge flush barrier
		return e.PushDirection(&core.FlushProcessedFrame{Gen: ff.Gen}, core.Upstream)

	default:
		return e.PushDirection(f, dir)
	}
}

// Pacer returns the underlying PacedWriter.
func (c *Connection) Pacer() *PacedWriter {
	return c.pacer
}

// Claims returns the authenticated token claims.
func (c *Connection) Claims() *token.Claims {
	return c.claims
}

// ID returns the unique connection identifier.
func (c *Connection) ID() uint64 {
	return c.id
}
