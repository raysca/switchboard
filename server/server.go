package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"govoice/core"
	"govoice/transport/token"
	"govoice/transport/ws"
	"govoice/vad"
)

// Server represents the config-driven Voice Framework V1 server matching spec §27-37.
type Server struct {
	cfg         *Config
	metrics     *Metrics
	timelines   *TimelineRegistry
	admission   *AdmissionController
	registry    *Registry
	validator   *token.Validator
	vadExecutor vad.SharedExecutor
	logger      *slog.Logger

	ready        atomic.Bool
	shuttingDown atomic.Bool

	publicMux    *http.ServeMux
	adminMux     *http.ServeMux
	publicServer *http.Server
	adminServer  *http.Server
	wsServer     *ws.Server

	mu          sync.Mutex
	activeCalls map[uint64]*activeCall
	nextCallID  atomic.Uint64
}

type activeCall struct {
	sessionID  uint64
	principal  string
	conn       *ws.Connection
	session    *core.Session
	timeline   *SessionTimeline
	cancelFunc context.CancelFunc
}

// NewServer initializes the server from configuration.
func NewServer(cfg *Config) (*Server, error) {
	if err := ValidateConfig(cfg); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	// Structured Logger
	var logHandler slog.Handler
	logLevel := slog.LevelInfo
	if cfg.Observability.LogLevel == "debug" {
		logLevel = slog.LevelDebug
	} else if cfg.Observability.LogLevel == "warn" {
		logLevel = slog.LevelWarn
	} else if cfg.Observability.LogLevel == "error" {
		logLevel = slog.LevelError
	}

	opts := &slog.HandlerOptions{Level: logLevel}
	if cfg.Observability.JSONFormat {
		logHandler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		logHandler = slog.NewTextHandler(os.Stdout, opts)
	}
	logger := slog.New(logHandler)

	metrics := NewMetrics()
	timelines := NewTimelineRegistry(
		cfg.Observability.TimelineMaxEvents,
		cfg.Observability.TimelineTTL,
		cfg.Observability.EnableTranscriptLogging,
	)
	admission := NewAdmissionController(cfg, metrics)
	registry := NewRegistry()

	// Token Validator
	tokenSecret := cfg.Token.Secret
	if tokenSecret == "" && cfg.Token.SecretEnv != "" {
		tokenSecret = os.Getenv(cfg.Token.SecretEnv)
	}
	if tokenSecret == "" {
		tokenSecret = "dev-insecure-secret-key-for-testing-only-12345"
	}

	tokenCfg := token.DefaultConfig(cfg.Token.Issuer, cfg.Token.Audience, []byte(tokenSecret))
	if cfg.Token.MaxLifetime > 0 {
		tokenCfg.MaxLifetime = cfg.Token.MaxLifetime
	}
	if cfg.Token.ClockSkew > 0 {
		tokenCfg.ClockSkew = cfg.Token.ClockSkew
	}
	if cfg.Token.ReplayCacheLimit > 0 {
		tokenCfg.ReplayCacheSize = cfg.Token.ReplayCacheLimit
	}

	validator, err := token.NewValidator(tokenCfg, time.Now())
	if err != nil {
		return nil, fmt.Errorf("initialize token validator: %w", err)
	}

	s := &Server{
		cfg:         cfg,
		metrics:     metrics,
		timelines:   timelines,
		admission:   admission,
		registry:    registry,
		validator:   validator,
		logger:      logger,
		activeCalls: make(map[uint64]*activeCall),
	}

	s.setupRoutes()
	return s, nil
}

// SetVADExecutor attaches a custom or mocked VAD executor.
func (s *Server) SetVADExecutor(exec vad.SharedExecutor) {
	s.vadExecutor = exec
}

// Registry returns the provider registry.
func (s *Server) Registry() *Registry {
	return s.registry
}

// Metrics returns the server metrics.
func (s *Server) Metrics() *Metrics {
	return s.metrics
}

// Admission returns the admission controller.
func (s *Server) Admission() *AdmissionController {
	return s.admission
}

// Timelines returns the diagnostic timeline registry.
func (s *Server) Timelines() *TimelineRegistry {
	return s.timelines
}

func (s *Server) setupRoutes() {
	// Configure WS Transport Server
	wsCfg := ws.ServerConfig{
		Security: ws.SecurityConfig{
			AllowedOrigins:     s.cfg.OriginAllowlist,
			AllowMissingOrigin: s.cfg.Profile == ProfileDevelopment || hasOriginWildcard(s.cfg.OriginAllowlist) || len(s.cfg.OriginAllowlist) == 0,
			RequireTLS:         s.cfg.Profile == ProfileProduction && !s.cfg.InsecureOverrides.AllowPlainWSInProduction && !s.cfg.TLS.TrustedProxyMode,
		},
		TokenValidator:        s.validator,
		Pacer:                 ws.DefaultPacerConfig(),
		HandshakeTimeout:      s.cfg.Limits.UpgradeTimeout,
		RTVIReadyTimeout:      s.cfg.Limits.ReadyTimeout,
		ReadTimeout:           s.cfg.Limits.IdleTimeout,
		MaxConnectionLifetime: s.cfg.Limits.MaxCallDuration,
		MaxConcurrentSessions: s.cfg.Limits.MaxSessions,
		MaxPendingHandshakes:  s.cfg.Limits.MaxPendingHandshakes,
		FixedPlayerRate:       24000,
	}

	s.wsServer = ws.NewServer(wsCfg)
	s.wsServer.OnSessionReady = s.onSessionReady

	// Public Listener: /health and /ws
	s.publicMux = http.NewServeMux()
	s.publicMux.HandleFunc("GET /health", s.handleHealth)
	s.publicMux.Handle("GET /ws", s.wsServer)

	// Admin Listener: /health, /ready, /metrics, /debug/timeline
	s.adminMux = http.NewServeMux()
	s.adminMux.HandleFunc("GET /health", s.handleHealth)
	s.adminMux.HandleFunc("GET /ready", s.handleReady)
	s.adminMux.Handle("GET /metrics", s.metrics.Handler())
	s.adminMux.Handle("GET /debug/timeline", s.timelines.Handler())
}

func (s *Server) onSessionReady(conn *ws.Connection) (*core.Session, error) {
	if s.shuttingDown.Load() {
		return nil, errors.New("server shutting down")
	}

	principal := conn.Claims().Principal
	if principal == "" {
		principal = conn.Claims().Subject
	}

	// 1. Admission Control check
	vadCapacity := true
	if s.vadExecutor != nil {
		avail, _ := s.vadExecutor.Capacity()
		vadCapacity = s.vadExecutor.IsReady() && avail > 0
	}

	if err := s.admission.AcquireSession(principal, vadCapacity); err != nil {
		return nil, err
	}

	sessionID := s.nextCallID.Add(1)
	timeline := s.timelines.GetOrCreate(sessionID)
	timeline.Record(TimelineEvent{Type: EventConnect, Details: "websocket connected"})
	timeline.Record(TimelineEvent{Type: EventAuthenticate, Details: fmt.Sprintf("principal=%s", principal)})
	timeline.Record(TimelineEvent{Type: EventReady, Details: "rtvi ready"})

	// 2. Build VAD Processor
	vadProc := vad.NewProcessor(s.vadExecutor, vad.DefaultProcessorConfig())

	// 3. Assemble Pipeline
	pipe, _, err := s.registry.BuildPipeline(s.cfg, "default", vadProc, conn)
	if err != nil {
		s.admission.ReleaseSession(principal)
		s.metrics.SessionsFailedTotal.Add(1)
		timeline.Record(TimelineEvent{Type: EventError, Details: err.Error()})
		return nil, fmt.Errorf("build pipeline: %w", err)
	}

	// 4. Construct Core Session
	session, err := core.NewSession(sessionID, pipe, core.DefaultResourceBudget())
	if err != nil {
		s.admission.ReleaseSession(principal)
		s.metrics.SessionsFailedTotal.Add(1)
		return nil, fmt.Errorf("create session: %w", err)
	}

	active := &activeCall{
		sessionID: sessionID,
		principal: principal,
		conn:      conn,
		session:   session,
		timeline:  timeline,
	}

	s.mu.Lock()
	s.activeCalls[sessionID] = active
	s.mu.Unlock()

	// 5. Cleanup when session closes
	go func() {
		<-session.Done()
		s.admission.ReleaseSession(principal)
		s.mu.Lock()
		delete(s.activeCalls, sessionID)
		s.mu.Unlock()
		timeline.Record(TimelineEvent{Type: EventDisconnect, Details: "session closed"})
		s.metrics.SessionsStoppedTotal.Add(1)
	}()

	// 6. Max call duration cost guard (spec §32, §35)
	if s.cfg.Limits.MaxCallDuration > 0 {
		maxDurTimer := time.NewTimer(s.cfg.Limits.MaxCallDuration)
		go func() {
			defer maxDurTimer.Stop()
			select {
			case <-session.Done():
				return
			case <-maxDurTimer.C:
				s.metrics.MaxDurationTermTotal.Add(1)
				timeline.Record(TimelineEvent{
					Type:    EventDisconnect,
					Details: "terminated: max call duration reached",
				})
				conn.Close()
				_ = session.Close()
			}
		}()
	}

	return session, nil
}

// handleHealth serves cheap local process liveness (spec §31).
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"live"}`))
}

// handleReady serves compound readiness check (spec §31).
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if s.shuttingDown.Load() || !s.ready.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "unready", "reason": "server starting or shutting down"})
		return
	}

	if !s.admission.IsEnabled() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "unready", "reason": "admission globally disabled"})
		return
	}

	// Check VAD executor capacity
	if s.vadExecutor != nil {
		if !s.vadExecutor.IsReady() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "unready", "reason": "zero VAD worker capacity"})
			return
		}
	}

	// Check provider circuits and usable pipelines (spec §31)
	circuits := map[string]string{
		"google_llm": circuitStateName(s.metrics.LLMCircuitStateGoogle.Load()),
		"xai_llm":    circuitStateName(s.metrics.LLMCircuitStateXAI.Load()),
		"google_tts": circuitStateName(s.metrics.TTSCircuitStateGoogle.Load()),
	}

	usablePipelines := make([]string, 0, len(s.cfg.Pipelines))
	for name, pipe := range s.cfg.Pipelines {
		if s.isPipelineUsable(pipe) {
			usablePipelines = append(usablePipelines, name)
		}
	}

	if len(s.cfg.Pipelines) > 0 && len(usablePipelines) == 0 {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":   "unready",
			"reason":   "no configured pipeline can accept a call (provider circuits open)",
			"circuits": circuits,
		})
		return
	}

	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":           "ready",
		"usable_pipelines": usablePipelines,
		"circuits":         circuits,
	})
}

func (s *Server) isPipelineUsable(pipe PipelineConfig) bool {
	if pipe.LLM == "google.llm" && s.metrics.LLMCircuitStateGoogle.Load() == 1 {
		return false
	}
	if pipe.LLM == "xai.llm" && s.metrics.LLMCircuitStateXAI.Load() == 1 {
		return false
	}
	if pipe.TTS == "google.tts" && s.metrics.TTSCircuitStateGoogle.Load() == 1 {
		return false
	}
	return true
}

func circuitStateName(val int64) string {
	switch val {
	case 1:
		return "open"
	case 2:
		return "half_open"
	default:
		return "closed"
	}
}

// Start boots public and admin listeners.
func (s *Server) Start() error {
	pubListener, err := net.Listen("tcp", s.cfg.PublicListenAddr)
	if err != nil {
		return fmt.Errorf("listen public %s: %w", s.cfg.PublicListenAddr, err)
	}

	adminListener, err := net.Listen("tcp", s.cfg.AdminListenAddr)
	if err != nil {
		pubListener.Close()
		return fmt.Errorf("listen admin %s: %w", s.cfg.AdminListenAddr, err)
	}

	s.publicServer = &http.Server{Handler: s.publicMux}
	s.adminServer = &http.Server{Handler: s.adminMux}

	go func() {
		_ = s.publicServer.Serve(pubListener)
	}()
	go func() {
		_ = s.adminServer.Serve(adminListener)
	}()

	s.ready.Store(true)
	s.logger.Info("server started",
		slog.String("public_addr", s.cfg.PublicListenAddr),
		slog.String("admin_addr", s.cfg.AdminListenAddr),
		slog.String("profile", string(s.cfg.Profile)),
	)
	return nil
}

// Shutdown performs orderly drain-then-cancel shutdown per spec §37.
func (s *Server) Shutdown(ctx context.Context) error {
	s.shuttingDown.Store(true)
	s.ready.Store(false)

	s.logger.Info("server shutting down: marked unready, stopping admission")

	// 1. Close public server to stop new upgrades immediately
	if s.publicServer != nil {
		_ = s.publicServer.Close()
	}

	// 2. Drain active calls or immediate cancel
	drainDeadline := s.cfg.Limits.DrainTimeout
	if drainDeadline <= 0 {
		drainDeadline = 30 * time.Second
	}
	if s.cfg.Limits.ImmediateCancel {
		drainDeadline = 0
	}

	if drainDeadline > 0 {
		s.logger.Info("draining active calls", slog.Duration("drain_timeout", drainDeadline))
		drainTimer := time.NewTimer(drainDeadline)
		defer drainTimer.Stop()

	drainLoop:
		for {
			s.mu.Lock()
			count := len(s.activeCalls)
			s.mu.Unlock()
			if count == 0 {
				s.logger.Info("all active calls drained cleanly")
				break drainLoop
			}

			select {
			case <-drainTimer.C:
				s.logger.Warn("drain timeout reached; cancelling remaining active calls", slog.Int("remaining", count))
				break drainLoop
			case <-ctx.Done():
				s.logger.Warn("shutdown context cancelled during drain")
				break drainLoop
			case <-time.After(50 * time.Millisecond):
			}
		}
	}

	// 3. Cancel any remaining active calls
	s.mu.Lock()
	for _, call := range s.activeCalls {
		if call.conn != nil {
			call.conn.Close()
		}
		if call.session != nil {
			_ = call.session.Close()
		}
	}
	s.mu.Unlock()

	// 4. Release VAD Executor
	if s.vadExecutor != nil {
		_ = s.vadExecutor.Close()
	}

	// 5. Close admin server
	if s.adminServer != nil {
		_ = s.adminServer.Close()
	}

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("shutdown deadline exceeded: %w", err)
	}

	s.logger.Info("server shutdown complete")
	return nil
}

func hasOriginWildcard(origins []string) bool {
	for _, o := range origins {
		if o == "*" {
			return true
		}
	}
	return false
}
