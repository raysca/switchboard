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
	"govoice/transport/rtvi"
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
	RTVIReadyTimeout      time.Duration // Max wait for client-ready handshake (default: 5s)
	ReadTimeout           time.Duration // Idle read timeout (default: 30s)
	PingInterval          time.Duration // Keepalive ping interval (default: 10s)
	MaxConnectionLifetime time.Duration // Max duration for a voice call (default: 60m)
	MaxConcurrentSessions int           // Max active voice sessions (default: 100)
	MaxPendingHandshakes  int           // Max handshakes in flight (default: 50)
	FixedPlayerRate       int           // Fixed client player rate (default: 24000)
	Observe               func(event string, n uint64)
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
		RTVIReadyTimeout:      5 * time.Second,
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
	pipeline   string
	cfg        ServerConfig
	pacer      *PacedWriter
	session    *core.Session
	seqCounter atomic.Uint64
	rtvi       *rtvi.Handler

	writeMu   sync.Mutex
	readyCh   chan struct{}
	readyOnce sync.Once
	speaking  atomic.Bool

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
	cfg          ServerConfig
	activeMu     sync.Mutex
	activeSess   int
	pendingMu    sync.Mutex
	pendingHsh   int
	pendingPerIP map[string]int
	idCounter    atomic.Uint64

	// Session factory callback
	OnSessionReady func(conn *Connection) (*core.Session, error)
}

func (s *Server) observe(event string, n uint64) {
	if s.cfg.Observe != nil {
		s.cfg.Observe(event, n)
	}
}

// NewServer creates a new WebSocket voice server.
func NewServer(cfg ServerConfig) *Server {
	if cfg.HandshakeTimeout <= 0 {
		cfg.HandshakeTimeout = 5 * time.Second
	}
	if cfg.RTVIReadyTimeout <= 0 {
		cfg.RTVIReadyTimeout = 5 * time.Second
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

	return &Server{
		cfg:          cfg,
		pendingPerIP: make(map[string]int),
	}
}

// ServeHTTP processes WebSocket upgrade requests with pre-upgrade token and origin checks.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 1. Pending handshakes limit (global and per-source IP)
	clientIP := s.cfg.Security.ClientIP(r)
	maxPerSource := s.cfg.MaxPendingHandshakes / 4
	if maxPerSource < 5 {
		maxPerSource = 5
	}

	s.pendingMu.Lock()
	if s.pendingHsh >= s.cfg.MaxPendingHandshakes || s.pendingPerIP[clientIP] >= maxPerSource {
		s.pendingMu.Unlock()
		http.Error(w, ErrHandshakeLimitExceeded.Error(), http.StatusTooManyRequests)
		return
	}
	s.pendingHsh++
	s.pendingPerIP[clientIP]++
	s.pendingMu.Unlock()
	defer func() {
		s.pendingMu.Lock()
		s.pendingHsh--
		s.pendingPerIP[clientIP]--
		if s.pendingPerIP[clientIP] <= 0 {
			delete(s.pendingPerIP, clientIP)
		}
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
		s.observe("origin_reject", 1)
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
		s.observe("auth_failure", 1)
		http.Error(w, "missing admission token", http.StatusUnauthorized)
		return
	}

	claims, err := s.cfg.TokenValidator.ValidateAndConsume(tokenStr)
	if err != nil {
		if errors.Is(err, token.ErrTokenReplayed) {
			s.observe("replay", 1)
			http.Error(w, "token already consumed (single-use)", http.StatusUnauthorized)
			return
		}
		if errors.Is(err, token.ErrTokenExpired) {
			s.observe("auth_failure", 1)
			http.Error(w, "token expired", http.StatusUnauthorized)
			return
		}
		s.observe("auth_failure", 1)
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
	s.observe("client_connected", 1)

	sessCtx, sessCancel := context.WithCancel(context.Background())
	if s.cfg.MaxConnectionLifetime > 0 {
		sessCtx, sessCancel = context.WithTimeout(sessCtx, s.cfg.MaxConnectionLifetime)
	}

	pipeline := ""
	if claims != nil {
		pipeline = claims.Pipeline
	}
	if pipeline == "" {
		pipeline = r.URL.Query().Get("pipeline")
	}
	if pipeline == "" {
		pipeline = "default"
	}

	conn := &Connection{
		id:           s.idCounter.Add(1),
		conn:         wsConn,
		claims:       claims,
		pipeline:     pipeline,
		cfg:          s.cfg,
		ctx:          sessCtx,
		cancel:       sessCancel,
		rtvi:         rtvi.NewHandler(),
		readyCh:      make(chan struct{}),
		inResamplers: make(map[int]*resample.Resampler),
	}

	// Initialize PacedWriter with thread-safe writeFn
	pacer := NewPacedWriter(s.cfg.Pacer, func(ctx context.Context, data []byte) error {
		conn.writeMu.Lock()
		defer conn.writeMu.Unlock()
		return wsConn.Write(ctx, websocket.MessageBinary, data)
	})
	conn.pacer = pacer

	// Hook write completion re-entry and speech state
	pacer.OnWriteComplete = func(f *core.OutputAudioFrame, err error) {
		if f.IsFinal {
			if conn.speaking.CompareAndSwap(true, false) {
				msg, _ := conn.rtvi.FormatBotStoppedSpeaking()
				_ = conn.WriteWireMessage(conn.ctx, msg)
			}
		}
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

	// Protocol-ready handshake callback: allocate session ONLY after valid RTVI readiness!
	conn.rtvi.OnClientReady = func(version string) error {
		conn.readyOnce.Do(func() {
			close(conn.readyCh)
		})
		// 1. Format and send bot-ready message immediately
		botReadyMsg, err := conn.rtvi.FormatBotReady()
		if err != nil {
			return err
		}
		if err := conn.WriteWireMessage(conn.ctx, botReadyMsg); err != nil {
			return err
		}

		// 2. Allocate session only after token, protobuf, and RTVI readiness pass
		if s.OnSessionReady != nil {
			session, err := s.OnSessionReady(conn)
			if err != nil {
				return err
			}
			conn.session = session
			if err := session.Start(); err != nil {
				_ = session.Close()
				return err
			}
		}
		return nil
	}

	defer func() {
		conn.Close()
		s.activeMu.Lock()
		s.activeSess--
		s.activeMu.Unlock()
	}()

	// RTVI Ready watchdog: if client does not send valid client-ready within timeout,
	// terminate connection before heavy session allocation.
	go func() {
		select {
		case <-conn.readyCh:
			// Handshake completed successfully
		case <-time.After(s.cfg.RTVIReadyTimeout):
			if !conn.rtvi.IsReady() {
				_ = wsConn.Close(websocket.StatusPolicyViolation, "rtvi ready timeout exceeded")
				conn.cancel()
			}
		case <-conn.ctx.Done():
		}
	}()

	// Run independent supervised read and write tasks
	var wg sync.WaitGroup
	wg.Add(2)

	// Writer task
	go func() {
		defer wg.Done()
		if err := pacer.Run(conn.ctx); err != nil && !errors.Is(err, context.Canceled) {
			if errors.Is(err, ErrSlowClient) {
				s.observe("slow_client", 1)
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
		var cancel context.CancelFunc
		if c.cfg.ReadTimeout > 0 {
			readCtx, cancel = context.WithTimeout(c.ctx, c.cfg.ReadTimeout)
		}

		msgType, data, err := c.conn.Read(readCtx)
		if cancel != nil {
			cancel()
		}
		if err != nil {
			c.observeDisconnect(err)
			return
		}

		if msgType != websocket.MessageBinary {
			continue // Only binary protobuf is accepted
		}

		seq := c.seqCounter.Add(1)
		if c.cfg.Observe != nil {
			c.cfg.Observe("frame_in", uint64(len(data)))
		}
		frame, err := DecodeBinaryFrame(data, seq, time.Now())
		if err != nil {
			if c.cfg.Observe != nil {
				c.cfg.Observe("protocol_error", 1)
			}
			_ = c.conn.Close(websocket.StatusUnsupportedData, err.Error())
			return
		}

		switch f := frame.(type) {
		case *WireMessageFrame:
			respFrame, err := c.rtvi.HandleIncomingJSON(f.Data)
			if err != nil {
				if errors.Is(err, rtvi.ErrIncompatibleProtocol) {
					errJSON, _ := c.rtvi.FormatError(err.Error(), true)
					_ = c.WriteWireMessage(c.ctx, errJSON)
					_ = c.conn.Close(websocket.StatusPolicyViolation, err.Error())
					return
				}
				_ = c.conn.Close(websocket.StatusUnsupportedData, err.Error())
				return
			}
			if respFrame != nil {
				if _, ok := respFrame.(*core.EndFrame); ok {
					c.Close()
					return
				}
				if c.session != nil {
					_ = c.session.Post(respFrame)
				}
			}

		case *core.InputAudioFrame:
			// Audio input handling: require RTVI client-ready before accepting audio
			if !c.rtvi.IsReady() {
				_ = c.conn.Close(websocket.StatusPolicyViolation, "audio sent before rtvi client-ready handshake")
				return
			}

			// Resample to 16 kHz internal format if needed
			if f.SampleRate != InternalSampleRate {
				resampled, err := c.resampleInput(f.Samples, f.SampleRate)
				if err == nil {
					f.Samples = resampled
					f.SampleRate = InternalSampleRate
				}
			}

			if c.session != nil {
				if err := c.session.Post(f); err != nil {
					// Queue full or session closed
					if errors.Is(err, core.ErrControlQueueFull) {
						_ = c.conn.Close(websocket.StatusPolicyViolation, "control queue full")
						return
					}
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
func (c *Connection) observeDisconnect(err error) {
	if c.cfg.Observe == nil {
		return
	}
	switch websocket.CloseStatus(err) {
	case websocket.StatusNormalClosure, websocket.StatusGoingAway:
		c.cfg.Observe("disconnect_normal", 1)
	case websocket.StatusPolicyViolation, websocket.StatusUnsupportedData, websocket.StatusInternalError:
		c.cfg.Observe("disconnect_server", 1)
	default:
		if errors.Is(err, context.DeadlineExceeded) {
			c.cfg.Observe("disconnect_timeout", 1)
			return
		}
		c.cfg.Observe("disconnect_client", 1)
	}
}

func (c *Connection) Close() {
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		if c.cfg.Observe != nil {
			c.cfg.Observe("client_disconnected", 1)
		}
		c.cancel()
		if c.pacer != nil {
			c.pacer.Close()
		}
	})
}

// ProcessFrame implements core.Processor to serve as the transport audio sink and RTVI emitter.
func (c *Connection) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	switch f.Type() {
	case core.FrameOutputAudio:
		oaf := f.(*core.OutputAudioFrame)
		if c.speaking.CompareAndSwap(false, true) {
			msg, _ := c.rtvi.FormatBotStartedSpeaking()
			_ = c.WriteWireMessage(ctx, msg)
		}

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

		return c.enqueueOutput(ctx, oaf)

	case core.FrameUserStartedSpeaking:
		msg, _ := c.rtvi.FormatUserStartedSpeaking()
		_ = c.WriteWireMessage(ctx, msg)
		return e.PushDirection(f, dir)

	case core.FrameUserStoppedSpeaking:
		msg, _ := c.rtvi.FormatUserStoppedSpeaking()
		_ = c.WriteWireMessage(ctx, msg)
		return e.PushDirection(f, dir)

	case core.FrameInterimTranscription:
		itf := f.(*core.InterimTranscriptionFrame)
		ts := ""
		if !itf.Timestamp.IsZero() {
			ts = itf.Timestamp.Format(time.RFC3339Nano)
		}
		msg, _ := c.rtvi.FormatUserTranscriptionWithMeta(itf.Text, false, ts, itf.UserID)
		_ = c.WriteWireMessage(ctx, msg)
		return e.PushDirection(f, dir)

	case core.FrameFinalTranscription:
		ftf := f.(*core.FinalTranscriptionFrame)
		ts := ""
		if !ftf.Timestamp.IsZero() {
			ts = ftf.Timestamp.Format(time.RFC3339Nano)
		}
		msg, _ := c.rtvi.FormatUserTranscriptionWithMeta(ftf.Text, true, ts, ftf.UserID)
		_ = c.WriteWireMessage(ctx, msg)
		return e.PushDirection(f, dir)

	case core.FrameLLMTextDelta:
		ltf := f.(*core.LLMTextDeltaFrame)
		msg, _ := c.rtvi.FormatBotLLMText(ltf.Delta)
		_ = c.WriteWireMessage(ctx, msg)
		return e.PushDirection(f, dir)

	case core.FrameTTSStarted:
		if c.speaking.CompareAndSwap(false, true) {
			msg, _ := c.rtvi.FormatBotStartedSpeaking()
			_ = c.WriteWireMessage(ctx, msg)
		}
		return e.PushDirection(f, dir)

	case core.FrameTTSStopped:
		if c.speaking.CompareAndSwap(true, false) {
			msg, _ := c.rtvi.FormatBotStoppedSpeaking()
			_ = c.WriteWireMessage(ctx, msg)
		}
		return e.PushDirection(f, dir)

	case core.FrameInterruption:
		intr := f.(*core.InterruptionFrame)
		c.pacer.Flush(intr.Gen)
		c.speaking.Store(false)
		// Emit playback clear event (proven in Phase 0 / ADR-0001)
		msg, _ := c.rtvi.FormatBotStoppedSpeaking()
		_ = c.WriteWireMessage(ctx, msg)
		return e.PushDirection(f, dir)

	case core.FrameFlush:
		ff := f.(*core.FlushFrame)
		c.pacer.Flush(ff.Gen)
		c.pacer.SetActiveGeneration(ff.Gen + 1)
		if c.cfg.Observe != nil {
			c.cfg.Observe("flush_ack", 1)
		}
		// Acknowledge flush barrier
		return e.PushDirection(&core.FlushProcessedFrame{Gen: ff.Gen}, core.Upstream)

	case core.FrameError:
		ef := f.(*core.ErrorFrame)
		msg, _ := c.rtvi.FormatError(ef.Err.Error(), ef.Fatal)
		_ = c.WriteWireMessage(ctx, msg)
		if ef.Fatal {
			c.Close()
		}
		return e.PushDirection(f, dir)

	default:
		return e.PushDirection(f, dir)
	}
}

// enqueueOutput applies backpressure while the pacer drains at media time.
// A client that does not drain before the write timeout is terminated.
func (c *Connection) enqueueOutput(ctx context.Context, frame *core.OutputAudioFrame) error {
	deadline := time.Now().Add(c.cfg.Pacer.WriteTimeout)
	for {
		err := c.pacer.Enqueue(frame)
		if err == nil || !errors.Is(err, ErrSlowClient) {
			return err
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return err
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return err
		}
	}
}

// WriteWireMessage safely serializes and writes an RTVI JSON message to the client over WebSocket.
func (c *Connection) WriteWireMessage(ctx context.Context, rtviJSON string) error {
	data, err := EncodeMessageFrame(rtviJSON)
	if err != nil {
		return err
	}
	writeCtx := ctx
	var cancel context.CancelFunc
	if c.cfg.Pacer.WriteTimeout > 0 {
		writeCtx, cancel = context.WithTimeout(ctx, c.cfg.Pacer.WriteTimeout)
		defer cancel()
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	err = c.conn.Write(writeCtx, websocket.MessageBinary, data)
	if err == nil && c.cfg.Observe != nil {
		c.cfg.Observe("frame_out", uint64(len(data)))
	}
	return err
}

// RTVI returns the associated RTVI protocol handler.
func (c *Connection) RTVI() *rtvi.Handler {
	return c.rtvi
}

// Session returns the allocated core session, or nil if not ready yet.
func (c *Connection) Session() *core.Session {
	return c.session
}

// Pacer returns the underlying PacedWriter.
func (c *Connection) Pacer() *PacedWriter {
	return c.pacer
}

// Claims returns the authenticated token claims.
func (c *Connection) Claims() *token.Claims {
	return c.claims
}

// Pipeline returns the target pipeline requested by token or query parameter.
func (c *Connection) Pipeline() string {
	if c.pipeline == "" {
		return "default"
	}
	return c.pipeline
}

// ID returns the unique connection identifier.
func (c *Connection) ID() uint64 {
	return c.id
}
