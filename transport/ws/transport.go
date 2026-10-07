package ws

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
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
)

// ServerConfig configures WebSocket connection lifecycle, timeouts, limits, and security.
type ServerConfig struct {
	Security         SecurityConfig
	TokenValidator   *token.Validator
	Pacer            PacerConfig
	HandshakeTimeout time.Duration // Max handshake duration (default: 5s)
	RTVIReadyTimeout time.Duration // Max wait for client-ready handshake (default: 5s)
	// ReadTimeout is the idle read deadline and the only dead-peer detector:
	// only data messages reset it (coder/websocket handles ping/pong inside Read
	// and expiry closes the conn), so a client that sends nothing for this long
	// is dropped, including a live one that stops sending audio while muted.
	// No server pings are sent; pings alone would not keep such a client alive.
	ReadTimeout                time.Duration // Idle read timeout (default: 30s)
	MaxConnectionLifetime      time.Duration // Max duration for a voice call (default: 60m)
	MaxConcurrentSessions      int           // Max active voice sessions (default: 100)
	MaxConcurrentSessionsPerIP int           // Max active voice sessions per source IP (default: 10)
	MaxPendingHandshakes       int           // Max handshakes in flight (default: 50)
	HandshakeRateLimit         float64       // Max handshake attempts per second per source IP (default: 20)
	FixedPlayerRate            int           // Fixed client player rate (default: 24000)
	Observe                    func(event string, n uint64)
	// Underlay builds one session's background ambience (nil: none). Its Rate
	// must equal FixedPlayerRate. See PacedWriter.Underlay.
	Underlay func() Underlay
}

// DefaultServerConfig returns production-ready server parameters.
func DefaultServerConfig(val *token.Validator) ServerConfig {
	return ServerConfig{
		Security: SecurityConfig{
			AllowedOrigins:     []string{"*"},
			AllowMissingOrigin: true,
		},
		TokenValidator:             val,
		Pacer:                      DefaultPacerConfig(),
		HandshakeTimeout:           5 * time.Second,
		RTVIReadyTimeout:           5 * time.Second,
		ReadTimeout:                30 * time.Second,
		MaxConnectionLifetime:      60 * time.Minute,
		MaxConcurrentSessions:      100,
		MaxConcurrentSessionsPerIP: 10,
		MaxPendingHandshakes:       50,
		HandshakeRateLimit:         20.0,
		FixedPlayerRate:            FixedPlayerSampleRate,
	}
}

// Connection represents a live, authenticated WebSocket voice connection.
type Connection struct {
	id         uint64
	conn       *websocket.Conn
	claims     *token.Claims
	cfg        ServerConfig
	pacer      *PacedWriter
	session    atomic.Pointer[core.Session] // published after Start succeeds; read by pacer callbacks
	tap        atomic.Pointer[AudioTap]     // optional call recorder; nil = none
	seqCounter atomic.Uint64
	rtvi       *rtvi.Handler

	writeMu   sync.Mutex
	readyCh   chan struct{}
	readyOnce sync.Once
	// Resamplers
	resamplersMu sync.Mutex
	inResamplers map[int]*resample.Resampler // inRate -> 16000
	outResampler *resample.Resampler         // providerRate -> 24000

	ctx    context.Context
	cancel context.CancelFunc

	closed       atomic.Bool
	closeOnce    sync.Once
	acceptedAt   time.Time
	traceMu      sync.Mutex
	traceSpan    core.Span
	traceSession core.SessionID
	tracePolicy  core.TracePolicy
}

func (c *Connection) startTrace() {
	session := c.session.Load()
	if session == nil {
		return
	}
	traceCtx := session.RootTraceContext()
	span := core.StartContextSpan(traceCtx, "transport.connection", core.SessionID(strconv.FormatUint(session.ID(), 10)), "", core.Generation(session.CurrentGeneration()), map[string]any{"component": "ws"})
	c.traceMu.Lock()
	c.traceSpan = span
	c.traceSession = core.SessionID(strconv.FormatUint(session.ID(), 10))
	c.tracePolicy = core.TraceContextFromContext(traceCtx).Policy
	c.traceMu.Unlock()
	c.traceEventAt("transport.connect", c.acceptedAt, nil)
	c.traceEvent("transport.rtvi_ready", nil)
}
func (c *Connection) traceEvent(name string, attrs map[string]any) {
	c.traceEventAt(name, time.Now(), attrs)
}
func (c *Connection) traceEventAt(name string, at time.Time, attrs map[string]any) {
	c.traceMu.Lock()
	span, session, policy := c.traceSpan, c.traceSession, c.tracePolicy
	c.traceMu.Unlock()
	if span != nil {
		span.AddEvent(core.NewTraceEvent(name, at, session, "", 0, policy, attrs))
	}
}
func (c *Connection) traceError(category string) {
	c.traceEvent("transport.error", map[string]any{"error.category": category})
}

type ipRateState struct {
	tokens    float64
	lastCheck time.Time
}

// Server handles HTTP upgrades to WebSocket voice connections.
type Server struct {
	cfg          ServerConfig
	activeMu     sync.Mutex
	activeSess   int
	activePerIP  map[string]int
	pendingMu    sync.Mutex
	pendingHsh   int
	pendingPerIP map[string]int
	rateMu       sync.Mutex
	ratePerIP    map[string]*ipRateState
	idCounter    atomic.Uint64

	// Session factory callback
	OnSessionReady func(conn *Connection) (*core.Session, error)
	// OnText receives a validated client text turn for conn's session. Nil
	// means text input is not supported and send-text is rejected.
	OnText func(conn *Connection, text string) error
	// OnDTMF receives validated client key presses for conn's session. Nil
	// rejects the message.
	OnDTMF func(conn *Connection, digits string) error
}

func (s *Server) observe(event string, n uint64) {
	if s.cfg.Observe != nil {
		s.cfg.Observe(event, n)
	}
}

func (s *Server) allowHandshakeRate(ip string) bool {
	rate := s.cfg.HandshakeRateLimit
	burst := int(rate * 2)
	if burst < 5 {
		burst = 5
	}

	s.rateMu.Lock()
	defer s.rateMu.Unlock()

	now := time.Now()
	state, exists := s.ratePerIP[ip]
	if !exists {
		if len(s.ratePerIP) >= 10000 {
			for k, v := range s.ratePerIP {
				if now.Sub(v.lastCheck) > time.Minute {
					delete(s.ratePerIP, k)
				}
			}
			if len(s.ratePerIP) >= 10000 {
				return false
			}
		}
		s.ratePerIP[ip] = &ipRateState{
			tokens:    float64(burst - 1),
			lastCheck: now,
		}
		return true
	}

	elapsed := now.Sub(state.lastCheck).Seconds()
	state.lastCheck = now
	state.tokens += elapsed * rate
	if state.tokens > float64(burst) {
		state.tokens = float64(burst)
	}

	if state.tokens < 1.0 {
		return false
	}
	state.tokens -= 1.0
	return true
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
	if cfg.MaxConcurrentSessionsPerIP <= 0 {
		cfg.MaxConcurrentSessionsPerIP = 10
	}
	if cfg.MaxPendingHandshakes <= 0 {
		cfg.MaxPendingHandshakes = 50
	}
	if cfg.HandshakeRateLimit <= 0 {
		cfg.HandshakeRateLimit = 20.0
	}
	if cfg.FixedPlayerRate <= 0 {
		cfg.FixedPlayerRate = FixedPlayerSampleRate
	}

	return &Server{
		cfg:          cfg,
		activePerIP:  make(map[string]int),
		pendingPerIP: make(map[string]int),
		ratePerIP:    make(map[string]*ipRateState),
	}
}

// ServeHTTP processes WebSocket upgrade requests with pre-upgrade token and origin checks.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	clientIP := s.cfg.Security.ClientIP(r)

	// 1a. Handshake rate limit per source IP
	if !s.allowHandshakeRate(clientIP) {
		http.Error(w, "handshake rate limit exceeded for source IP", http.StatusTooManyRequests)
		return
	}

	// 1b. Pending handshakes limit (global and per-source IP)
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
	pendingHeld := true
	releasePending := func() {
		if !pendingHeld {
			return
		}
		pendingHeld = false
		s.pendingMu.Lock()
		s.pendingHsh--
		s.pendingPerIP[clientIP]--
		if s.pendingPerIP[clientIP] <= 0 {
			delete(s.pendingPerIP, clientIP)
		}
		s.pendingMu.Unlock()
	}
	defer releasePending()

	// 2. Concurrency limit. Reserve the slot in the same critical section as the check.
	s.activeMu.Lock()
	if s.activeSess >= s.cfg.MaxConcurrentSessions || s.activePerIP[clientIP] >= s.cfg.MaxConcurrentSessionsPerIP {
		s.activeMu.Unlock()
		http.Error(w, ErrMaxSessionsExceeded.Error(), http.StatusTooManyRequests)
		return
	}
	s.activeSess++
	s.activePerIP[clientIP]++
	s.activeMu.Unlock()
	sessionHeld := true
	releaseSession := func() {
		if !sessionHeld {
			return
		}
		sessionHeld = false
		s.activeMu.Lock()
		s.activeSess--
		s.activePerIP[clientIP]--
		if s.activePerIP[clientIP] <= 0 {
			delete(s.activePerIP, clientIP)
		}
		s.activeMu.Unlock()
	}
	defer releaseSession()

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
		if errors.Is(err, token.ErrReplayCacheFull) {
			s.observe("replay_cache_full", 1)
			http.Error(w, "admission replay cache is full", http.StatusServiceUnavailable)
			return
		}
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

	// 7. Upgrade WebSocket connection
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
	// The handshake is finished. Pending slots must not be held for the call.
	releasePending()
	s.observe("client_connected", 1)

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
		rtvi:         rtvi.NewHandler(),
		readyCh:      make(chan struct{}),
		inResamplers: make(map[int]*resample.Resampler),
		acceptedAt:   time.Now(),
	}

	// Initialize PacedWriter with thread-safe writeFn
	pacer := NewPacedWriter(s.cfg.Pacer, func(ctx context.Context, data []byte) error {
		conn.writeMu.Lock()
		defer conn.writeMu.Unlock()
		return wsConn.Write(ctx, websocket.MessageBinary, data)
	})
	conn.pacer = pacer
	if s.cfg.Underlay != nil {
		pacer.Underlay = s.cfg.Underlay() // per-session cursor over shared ambience
	}

	// Completion and speaking callbacks run outside pacer locks. Speaking is
	// derived only from successful non-empty output writes and final drain.
	pacer.OnWriteComplete = func(f *core.OutputAudioFrame, err error) {
		if t := conn.tap.Load(); t != nil && err == nil {
			(*t).Bot(f) // only audio the pacer actually released, never a flushed tail
		}
		if session := conn.session.Load(); session != nil {
			_ = session.Reenter(&core.WriteCompletionFrame{
				Turn:        f.Turn,
				Gen:         f.Gen,
				UnitID:      f.UnitID,
				Seq:         f.Seq,
				IsFinal:     f.IsFinal,
				TurnFinal:   f.TurnFinal,
				Err:         err,
				Media:       f.Duration,
				WordTimings: f.WordTimings,
			}, core.Downstream, 0, f.Gen)
		}
	}
	pacer.OnSpeakingState = func(event core.SpeakingStateFrame) {
		msg, err := conn.rtvi.FormatSpeakingState(event)
		if err == nil {
			_ = conn.WriteWireMessage(conn.ctx, msg)
		}
		if session := conn.session.Load(); session != nil {
			frame := event
			_ = session.Reenter(&frame, core.Upstream, session.TailIndex(), 0)
		}
	}

	// Client text turns enter the conversation as a committed final transcription;
	// that frame reaches the client as a user transcription like a spoken turn.
	conn.rtvi.OnSendText = func(text string) error {
		session := conn.session.Load()
		if session == nil {
			return errors.New("no active session")
		}
		if s.OnText == nil {
			return errors.New("text input is not supported")
		}
		return s.OnText(conn, text)
	}

	conn.rtvi.OnDTMF = func(digits string) error {
		if s.OnDTMF == nil || conn.session.Load() == nil {
			return errors.New("dtmf input is not supported")
		}
		return s.OnDTMF(conn, digits)
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
			session.SetOutputGate(conn.awaitOutputRoom)
			if err := session.Start(); err != nil {
				_ = session.Close()
				return err
			}
			conn.session.Store(session)
			conn.startTrace()
		}
		return nil
	}

	defer conn.Close()

	// RTVI Ready watchdog: if client does not send valid client-ready within timeout,
	// terminate connection before heavy session allocation.
	go func() {
		select {
		case <-conn.readyCh:
			// Handshake completed successfully
		case <-time.After(s.cfg.RTVIReadyTimeout):
			if !conn.rtvi.IsReady() {
				conn.traceError("timeout")
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
			conn.traceError("transport_failure")
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
	conn.Close()

	if session := conn.session.Load(); session != nil {
		_ = session.Close()
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
			c.traceEvent("transport.disconnect", map[string]any{"reason": disconnectCategory(err)})
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
			c.traceError("protocol_failure")
			if c.cfg.Observe != nil {
				c.cfg.Observe("protocol_error", 1)
			}
			_ = c.conn.Close(websocket.StatusUnsupportedData, "invalid frame")
			return
		}

		switch f := frame.(type) {
		case *WireMessageFrame:
			respFrame, err := c.rtvi.HandleIncomingJSON(f.Data)
			if errors.Is(err, rtvi.ErrRejectedMessage) {
				// A refused client message is not a protocol failure: report it
				// and keep the call running.
				errJSON, _ := c.rtvi.FormatError(err.Error(), false)
				_ = c.WriteWireMessage(c.ctx, errJSON)
				continue
			}
			if err != nil {
				c.traceError("protocol_failure")
				if errors.Is(err, rtvi.ErrIncompatibleProtocol) {
					errJSON, _ := c.rtvi.FormatError(err.Error(), true)
					_ = c.WriteWireMessage(c.ctx, errJSON)
					_ = c.conn.Close(websocket.StatusPolicyViolation, "incompatible rtvi protocol")
					return
				}
				if errors.Is(err, ErrDraining) {
					// Server is draining: tell the client to reconnect elsewhere.
					c.GoAway(ErrDraining.Error(), 2*time.Second)
					return
				}
				var rej *RejectError
				if errors.As(err, &rej) {
					// Admission refused the call: a stable reason code and 1013, not a generic 1011.
					c.closeWith(rej.Reason, true, websocket.StatusTryAgainLater, 2*time.Second)
					return
				}
				if errors.Is(err, rtvi.ErrSessionSetup) {
					errJSON, _ := c.rtvi.FormatError("session_setup_failed", true)
					_ = c.WriteWireMessage(c.ctx, errJSON)
					_ = c.conn.Close(websocket.StatusInternalError, "session setup failed")
					return
				}
				_ = c.conn.Close(websocket.StatusUnsupportedData, "invalid rtvi message")
				return
			}
			if respFrame != nil {
				if _, ok := respFrame.(*core.EndFrame); ok {
					c.traceEvent("transport.rtvi_disconnect", nil)
					c.Close()
					return
				}
				if session := c.session.Load(); session != nil {
					_ = session.Post(respFrame)
				}
			}

		case *core.InputAudioFrame:
			// Audio input handling: require RTVI client-ready before accepting audio
			if !c.rtvi.IsReady() {
				c.traceError("protocol_failure")
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

			if t := c.tap.Load(); t != nil {
				(*t).User(f)
			}

			if session := c.session.Load(); session != nil {
				if err := session.Post(f); err != nil {
					// Queue full or session closed
					if errors.Is(err, core.ErrControlQueueFull) {
						c.traceError("backpressure")
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

func disconnectCategory(err error) string {
	switch websocket.CloseStatus(err) {
	case websocket.StatusNormalClosure, websocket.StatusGoingAway:
		return "normal"
	case websocket.StatusPolicyViolation, websocket.StatusUnsupportedData, websocket.StatusInternalError:
		return "server"
	default:
		if errors.Is(err, context.DeadlineExceeded) {
			return "timeout"
		}
		return "client"
	}
}

// ErrDraining, returned from OnSessionReady, refuses the call with GoAway.
var ErrDraining = errors.New("server_draining")

// RejectError, returned (possibly wrapped) from OnSessionReady, refuses the call
// because of capacity or rate limits: the client gets an RTVI error carrying
// Reason (a stable code such as capacity_exceeded) and a 1013 try-again-later close.
type RejectError struct {
	Reason string
	Err    error
}

func (e *RejectError) Error() string { return e.Reason + ": " + e.Err.Error() }
func (e *RejectError) Unwrap() error { return e.Err }

// GoAway tells the client to reconnect elsewhere: a fatal RTVI error carrying
// the stable reason code, then a 1001 going-away close. Bounded by timeout.
func (c *Connection) GoAway(reason string, timeout time.Duration) {
	c.closeWith(reason, true, websocket.StatusGoingAway, timeout)
}

// Hangup ends the call for good (the bot hung up, e.g. user_idle): a
// non-fatal RTVI error carrying the reason code, then a 1000 normal close, so
// clients do not treat it as "reconnect elsewhere". Bounded by timeout.
func (c *Connection) Hangup(reason string, timeout time.Duration) {
	c.closeWith(reason, false, websocket.StatusNormalClosure, timeout)
}

func (c *Connection) closeWith(reason string, fatal bool, code websocket.StatusCode, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if msg, err := c.rtvi.FormatError(reason, fatal); err == nil {
		_ = c.WriteWireMessage(ctx, msg)
	}
	done := make(chan struct{})
	go func() { _ = c.conn.Close(code, reason); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		_ = c.conn.CloseNow()
	}
}

func (c *Connection) Close() {
	c.closeOnce.Do(func() {
		c.traceEvent("transport.close", nil)
		c.traceMu.Lock()
		span := c.traceSpan
		c.traceMu.Unlock()
		if span != nil {
			span.End(nil)
		}
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

func replyKey(turn string, gen uint64) string { return turn + "/" + strconv.FormatUint(gen, 10) }

// ProcessFrame implements core.Processor to serve as the transport audio sink and RTVI emitter.
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

		return c.enqueueOutput(ctx, oaf)

	case core.FrameOutputTurnFinal:
		marker := f.(*core.OutputTurnFinalFrame)
		if err := c.pacer.MarkTurnFinal(marker.Turn, marker.Gen, marker.UnitID); err != nil {
			return err
		}
		return e.PushDirection(f, dir)

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
		for _, msg := range c.rtvi.BotLLMText(replyKey(ltf.Turn, ltf.Gen), ltf.Delta) {
			_ = c.WriteWireMessage(ctx, msg)
		}
		return e.PushDirection(f, dir)

	case core.FrameLLMText:
		// The complete reply text follows its deltas: tell the client the
		// turn's text is done.
		lf := f.(*core.LLMTextFrame)
		for _, msg := range c.rtvi.BotLLMStopped(replyKey(lf.Turn, lf.Gen)) {
			_ = c.WriteWireMessage(ctx, msg)
		}
		return e.PushDirection(f, dir)

	case core.FrameTTSStarted:
		msg, _ := c.rtvi.FormatBotTTSStarted()
		_ = c.WriteWireMessage(ctx, msg)
		return e.PushDirection(f, dir)

	case core.FrameTTSStopped:
		msg, _ := c.rtvi.FormatBotTTSStopped()
		_ = c.WriteWireMessage(ctx, msg)
		return e.PushDirection(f, dir)

	case core.FrameInterruption:
		// The following flush drives an output-derived stopped transition. The
		// pinned client still does not clear audio it already buffered.
		return e.PushDirection(f, dir)

	case core.FrameFlush:
		ff := f.(*core.FlushFrame)
		c.pacer.SetActiveGeneration(ff.Gen + 1)
		c.pacer.BeginFlush(ff.Gen, func(err error) {
			if err != nil {
				c.Close()
				return
			}
			if c.cfg.Observe != nil {
				c.cfg.Observe("flush_ack", 1)
			}
			if session := c.session.Load(); session != nil {
				// Start at the transport and walk upstream. Envelope generation 0
				// keeps the fence from dropping an ack for the generation just closed.
				_ = session.Reenter(&core.FlushProcessedFrame{Gen: ff.Gen}, core.Upstream, session.TailIndex(), 0)
			}
		})
		return e.PushDirection(f, dir)

	case core.FrameError:
		ef := f.(*core.ErrorFrame)
		// The client gets a category only: ef.Err can carry provider bodies and
		// project paths, and keeps travelling downstream for logs and the timeline.
		// ponytail: non-fatal errors are almost all provider failures; add a typed
		// category on ErrorFrame if the client needs finer detail.
		category := "provider_error"
		switch {
		case errors.Is(ef.Err, context.DeadlineExceeded):
			category = "timeout"
		case errors.Is(ef.Err, context.Canceled):
			category = "cancelled"
		case ef.Fatal:
			category = "pipeline_error"
		}
		msg, _ := c.rtvi.FormatError(category, ef.Fatal)
		_ = c.WriteWireMessage(ctx, msg)
		if ef.Fatal {
			c.Close()
		}
		return e.PushDirection(f, dir)

	default:
		return e.PushDirection(f, dir)
	}
}

// awaitOutputRoom is the session's output gate: the TTS producer, not the run
// loop, waits for the pacer to drain. Room is kept for the frame plus the two
// the session may already hold queued (core maxQueuedOutputAudio), so
// enqueueOutput below rarely has to block the run loop (and caller audio).
func (c *Connection) awaitOutputRoom(ctx context.Context, d time.Duration) {
	c.pacer.WaitRoom(ctx, 3*d, c.cfg.Pacer.WriteTimeout)
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
		// A session that ends (client hung up) while bot audio is backpressured
		// is not a slow client: returning the queue-full error here made the
		// session record it as its failure (loadtest: every hang-up mid-reply).
		if ctx.Err() != nil {
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil
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
	return c.session.Load()
}

// Pacer returns the underlying PacedWriter.
func (c *Connection) Pacer() *PacedWriter {
	return c.pacer
}

// AudioTap observes call audio for recording. Both methods must return
// quickly and must not retain the frame (its buffers are reused).
type AudioTap interface {
	User(*core.InputAudioFrame)
	Bot(*core.OutputAudioFrame)
}

// SetAudioTap attaches (or, with nil, detaches) the call's audio tap.
func (c *Connection) SetAudioTap(t AudioTap) {
	if t == nil {
		c.tap.Store(nil)
		return
	}
	c.tap.Store(&t)
}

// Claims returns the authenticated token claims.
func (c *Connection) Claims() *token.Claims {
	return c.claims
}

// Context is the connection's lifetime context; it ends when the client goes away.
func (c *Connection) Context() context.Context { return c.ctx }

// AgentID returns the stored agent the token authorizes this call to run.
func (c *Connection) AgentID() string {
	if c.claims == nil {
		return ""
	}
	return c.claims.AgentID
}

// ID returns the unique connection identifier.
func (c *Connection) ID() uint64 {
	return c.id
}
