package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"govoice/core"
	"govoice/providers/google"
	"govoice/providers/xai"
	"govoice/server/api"
	"govoice/server/auth"
	"govoice/server/storage"
	"govoice/transport/token"
	"govoice/transport/ws"
	"govoice/vad"
)

// Server represents the config-driven Voice Framework V1 server matching spec §27-37.
type Server struct {
	cfg                   *Config
	metrics               *Metrics
	timelines             *TimelineRegistry
	admission             *AdmissionController
	registry              *Registry
	validator             *token.Validator
	management            *auth.Management
	providerManagementAPI http.Handler
	agentManagementAPI    http.Handler
	mcpManagementAPI        http.Handler
	sessionManagementAPI    http.Handler
	operationsManagementAPI http.Handler
	store                   *storage.Store
	startedAt               time.Time
	voiceSignKey            []byte
	voiceKeyID            string
	voiceEpoch            string
	vadExecutor           vad.SharedExecutor
	logger                *slog.Logger

	ready        atomic.Bool
	shuttingDown atomic.Bool

	publicMux    *http.ServeMux
	adminMux     http.Handler
	publicServer *http.Server
	adminServer  *http.Server
	wsServer     *ws.Server

	mu          sync.Mutex
	activeCalls map[uint64]*activeCall
	nextCallID  atomic.Uint64
	pruneStop   chan struct{}
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
	)
	admission := NewAdmissionController(cfg, metrics)
	registry := NewRegistry()
	registry.BindMetrics(metrics)
	if err := registry.Prepare(cfg); err != nil {
		if cfg.Profile == ProfileProduction {
			return nil, fmt.Errorf("provider initialization: %w", err)
		}
		logger.Warn("providers not initialized; readiness will stay false", slog.String("err", err.Error()))
	}

	tokenSecret, err := ResolveTokenSecret(cfg)
	if err != nil {
		return nil, err
	}

	tokenCfg := voiceTokenConfig(cfg, tokenSecret)
	validator, err := token.NewValidator(tokenCfg, time.Now())
	if err != nil {
		return nil, fmt.Errorf("initialize token validator: %w", err)
	}
	managementKey := ""
	managementEnv := cfg.Management.APIKeyEnv
	if managementEnv == "" {
		managementEnv = "GOVOICE_MANAGEMENT_API_KEY"
	}
	managementKey = os.Getenv(managementEnv)
	management, err := auth.NewManagement(managementKey)
	if err != nil {
		return nil, err
	}
	if managementKey != "" && managementKey == tokenSecret {
		return nil, errors.New("management API key must differ from voice token signing key")
	}

	s := &Server{
		cfg:          cfg,
		metrics:      metrics,
		timelines:    timelines,
		admission:    admission,
		registry:     registry,
		validator:    validator,
		management:   management,
		voiceSignKey: []byte(tokenSecret),
		voiceKeyID:   tokenCfg.KeyID,
		voiceEpoch:   validator.ScopeEpoch(),
		logger:       logger,
		startedAt:    metrics.StartTime,
		activeCalls:  make(map[uint64]*activeCall),
	}

	s.setupRoutes()
	return s, nil
}

func voiceTokenConfig(cfg *Config, tokenSecret string) token.Config {
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
	if cfg.Profile == ProfileProduction {
		tokenCfg.RequireKeyID = true
		keyID := cfg.Token.KeyID
		if keyID == "" {
			keyID = "v1"
		}
		tokenCfg.KeyID = keyID
	} else if cfg.Token.KeyID != "" {
		tokenCfg.RequireKeyID = true
		tokenCfg.KeyID = cfg.Token.KeyID
	}

	return tokenCfg
}

// SetVADExecutor attaches a custom or mocked VAD executor.
func (s *Server) SetVADExecutor(exec vad.SharedExecutor) {
	s.vadExecutor = exec
}

// Registry returns the provider registry.
func (s *Server) Registry() *Registry {
	return s.registry
}

// SetProviderManagementAPI installs the authenticated management route handler.
// Configure it before serving requests; the outer API boundary always performs
// administrator authentication before this handler is called.
func (s *Server) SetProviderManagementAPI(handler http.Handler) {
	s.providerManagementAPI = handler
}

// SetAgentManagementAPI installs the agent route handler behind management
// authentication. Configure it before serving requests.
func (s *Server) SetAgentManagementAPI(handler http.Handler) {
	s.agentManagementAPI = handler
}

// SetMCPManagementAPI installs the MCP handler behind the management listener
// authentication boundary. Configure it before serving requests.
func (s *Server) SetMCPManagementAPI(handler http.Handler) {
	s.mcpManagementAPI = handler
}

// SetSessionManagementAPI installs the session and traces route handler behind
// management authentication. Configure it before serving requests.
func (s *Server) SetSessionManagementAPI(handler http.Handler) {
	s.sessionManagementAPI = handler
}

// SetTraceManagementAPI is an alias for SetSessionManagementAPI.
func (s *Server) SetTraceManagementAPI(handler http.Handler) {
	s.sessionManagementAPI = handler
}

// SetOperationsManagementAPI installs the operations status route handler behind
// management authentication. Configure it before serving requests.
func (s *Server) SetOperationsManagementAPI(handler http.Handler) {
	s.operationsManagementAPI = handler
}

// SetStore attaches a durable SQL store for operations inspection and diagnostics.
func (s *Server) SetStore(store *storage.Store) {
	s.store = store
}

func (s *Server) getOperationsStatus(ctx context.Context) api.OperationsStatus {
	now := time.Now()
	uptime := now.Sub(s.startedAt).Seconds()
	if uptime < 0 {
		uptime = 0
	}
	active := int(s.metrics.ActiveSessions.Load())
	maxSessions := s.cfg.Limits.MaxSessions
	var util float64
	if maxSessions > 0 {
		util = float64(active) / float64(maxSessions)
	}

	sessionsTotal := s.metrics.SessionsStartedTotal.Load()
	turnsTotal := s.metrics.TurnsTotal.Load()
	errorsTotal := s.metrics.ErrorsTotal()
	avgLatency, p95Latency := s.metrics.LatencyStats()

	var sessionRate float64
	if uptime > 0 {
		sessionRate = float64(sessionsTotal) / uptime
	}

	// Database diagnostics
	dbStatus := api.DatabaseStatus{
		Dialect: "none",
		Status:  "unconfigured",
	}
	if s.store != nil {
		diag := s.store.Diagnostics(ctx)
		dbStatus.Dialect = string(diag.Dialect)
		dbStatus.Status = diag.Status
		dbStatus.OpenConnections = diag.OpenConnections
		dbStatus.InUse = diag.InUse
		dbStatus.MigrationVersion = diag.MigrationVersion
	}

	// VAD diagnostics
	vadStatus := api.VADStatus{
		WorkerStatus: "ready",
		SampleRate:   16000,
		FrameSize:    512,
	}
	if s.vadExecutor != nil {
		if !s.vadExecutor.IsReady() {
			vadStatus.WorkerStatus = "unavailable"
		} else {
			avail, total := s.vadExecutor.Capacity()
			if avail == 0 && total > 0 {
				vadStatus.WorkerStatus = "active"
			}
			st := s.vadExecutor.Stats()
			if st.Streams > 0 {
				vadStatus.WorkerStatus = "active"
			}
		}
	}

	// Health determination: healthy, degraded, or unhealthy
	health := "healthy"
	if dbStatus.Status == "error" || vadStatus.WorkerStatus == "unavailable" {
		health = "unhealthy"
	} else if util >= 1.0 || vadStatus.WorkerStatus == "degraded" || (sessionsTotal > 0 && errorsTotal > 10 && float64(errorsTotal)/float64(sessionsTotal) > 0.5) {
		health = "degraded"
	}

	return api.OperationsStatus{
		UptimeSeconds:                uptime,
		StartedAt:                    s.startedAt,
		Profile:                      string(s.cfg.Profile),
		ActiveSessions:               active,
		MaxSessions:                  maxSessions,
		CapacityUtilization:          util,
		SessionsTotal:                sessionsTotal,
		TurnsTotal:                   turnsTotal,
		ErrorsTotal:                  errorsTotal,
		P95LatencyMs:                 p95Latency,
		AverageLatencyMs:             avgLatency,
		SessionRate:                  sessionRate,
		MaxCallDurationSeconds:       int(s.cfg.Limits.MaxCallDuration.Seconds()),
		VoiceTokenMaxLifetimeSeconds: int(s.effectiveVoiceTokenLifetime().Seconds()),
		Database:                     dbStatus,
		VAD:                          vadStatus,
		Health:                       health,
	}
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
	directTLS := s.cfg.TLS.CertFile != "" && s.cfg.TLS.KeyFile != ""
	proxyTLS := s.cfg.TLS.TrustedProxyMode && len(s.cfg.TLS.TrustedProxies) > 0
	wsCfg := ws.ServerConfig{
		Security: ws.SecurityConfig{
			AllowedOrigins:     s.cfg.OriginAllowlist,
			AllowMissingOrigin: s.cfg.Profile == ProfileDevelopment || hasOriginWildcard(s.cfg.OriginAllowlist) || len(s.cfg.OriginAllowlist) == 0,
			RequireTLS:         s.cfg.Profile == ProfileProduction && !s.cfg.InsecureOverrides.AllowPlainWSInProduction && (directTLS || proxyTLS),
			TrustedProxies:     s.cfg.TLS.TrustedProxies,
		},
		Observe:                    s.observeTransport,
		TokenValidator:             s.validator,
		Pacer:                      ws.DefaultPacerConfig(),
		HandshakeTimeout:           s.cfg.Limits.UpgradeTimeout,
		RTVIReadyTimeout:           s.cfg.Limits.ReadyTimeout,
		ReadTimeout:                s.cfg.Limits.IdleTimeout,
		MaxConnectionLifetime:      s.cfg.Limits.MaxCallDuration,
		MaxConcurrentSessions:      s.cfg.Limits.MaxSessions,
		MaxConcurrentSessionsPerIP: s.cfg.Limits.MaxSessionsPerIP,
		MaxPendingHandshakes:       s.cfg.Limits.MaxPendingHandshakes,
		FixedPlayerRate:            24000,
	}

	s.wsServer = ws.NewServer(wsCfg)
	s.wsServer.OnSessionReady = s.onSessionReady

	// Public Listener: /health and /ws
	s.publicMux = http.NewServeMux()
	s.publicMux.HandleFunc("GET /health", s.handleHealth)
	s.publicMux.Handle("GET /ws", s.wsServer)

	// Admin Listener: /health, /ready, /metrics, /debug/timeline
	adminRoutes := http.NewServeMux()
	adminRoutes.HandleFunc("GET /health", s.handleHealth)
	adminRoutes.HandleFunc("GET /ready", s.handleReady)
	adminRoutes.Handle("GET /metrics", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.sampleVADMetrics()
		s.metrics.Handler().ServeHTTP(w, r)
	}))
	adminRoutes.Handle("GET /debug/timeline", s.timelines.Handler())
	s.adminMux = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1" || strings.HasPrefix(r.URL.Path, "/api/v1/") {
			s.handleAPI(w, r)
			return
		}
		adminRoutes.ServeHTTP(w, r)
	})
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
	pipe, _, err := s.registry.BuildPipeline(s.cfg, conn.Pipeline(), vadProc, conn)
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

	var (
		speechActive       bool
		lastSTTTurn        string
		firstTokenRecorded bool
		lastLLMTurn        string
		lastTTSUnit        uint64
		lastInterruption   uint64
		lastFlushAck       uint64
	)

	session.OnFrame = func(f core.Frame, dir core.Direction) {
		switch f.Type() {
		case core.FrameUserStartedSpeaking:
			if !speechActive {
				speechActive = true
				firstTokenRecorded = false
				timeline.Record(TimelineEvent{Type: EventSpeechStart})
			}
		case core.FrameUserStoppedSpeaking:
			if speechActive {
				speechActive = false
				timeline.Record(TimelineEvent{Type: EventSpeechStop})
			}
		case core.FrameUserMessage:
			um := f.(*core.UserMessageFrame)
			if um.Turn != lastSTTTurn {
				lastSTTTurn = um.Turn
				timeline.Record(TimelineEvent{Type: EventSTTResult, TurnID: um.Turn, Gen: um.Gen})
			}
		case core.FrameLLMTextDelta:
			if !firstTokenRecorded {
				firstTokenRecorded = true
				timeline.Record(TimelineEvent{Type: EventLLMFirstToken})
			}
		case core.FrameLLMText:
			lt := f.(*core.LLMTextFrame)
			if lt.Turn != lastLLMTurn {
				lastLLMTurn = lt.Turn
				timeline.Record(TimelineEvent{Type: EventLLMComplete, TurnID: lt.Turn, Gen: lt.Gen})
			}
		case core.FrameSynthesisUnit:
			su := f.(*core.SynthesisUnitFrame)
			if su.UnitID != lastTTSUnit {
				lastTTSUnit = su.UnitID
				timeline.Record(TimelineEvent{Type: EventTTSUnit, TurnID: su.Turn, Gen: su.Gen})
			}
		case core.FrameOutputAudio:
			// Audio frames stream directly to client; first chunk per unit is covered by EventTTSUnit
		case core.FrameInterruption:
			intr := f.(*core.InterruptionFrame)
			if intr.Gen != lastInterruption {
				lastInterruption = intr.Gen
				speechActive = false
				timeline.Record(TimelineEvent{Type: EventInterruption, TurnID: intr.Turn, Gen: intr.Gen})
			}
		case core.FrameFlushProcessed:
			fp := f.(*core.FlushProcessedFrame)
			if fp.Gen != lastFlushAck {
				lastFlushAck = fp.Gen
				timeline.Record(TimelineEvent{Type: EventFlushAck, Gen: fp.Gen})
			}
		case core.FrameError:
			ef := f.(*core.ErrorFrame)
			timeline.Record(TimelineEvent{Type: EventError, Error: ef.Err.Error()})
		}
	}

	s.mu.Lock()
	s.activeCalls[sessionID] = active
	s.mu.Unlock()

	// 5. Cleanup when session closes
	go func() {
		<-session.Done()
		if n := session.LeakedTasks(); n > 0 {
			s.metrics.LeakedSessionsTotal.Add(1)
			s.metrics.LeakedWorkTotal.Add(uint64(n))
		}
		for _, proc := range session.Processors() {
			s.recordProcessorMetrics(proc)
		}
		if err := session.Err(); err != nil {
			s.metrics.SessionsFailedTotal.Add(1)
			s.logger.Error("session failed", slog.Uint64("session", sessionID), slog.String("err", err.Error()))
			conn.Close()
		}
		s.admission.ReleaseSession(principal)
		s.mu.Lock()
		delete(s.activeCalls, sessionID)
		s.mu.Unlock()
		timeline.Record(TimelineEvent{Type: EventDisconnect, Details: "session closed"})
		s.metrics.SessionsStoppedTotal.Add(1)
	}()

	s.metrics.SessionsStartedTotal.Add(1)

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

	if !s.registry.PipelinesLive(s.cfg) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "unready",
			"reason": "configured providers are not initialized",
		})
		return
	}

	// Check provider circuits and usable pipelines (spec §31)
	circuits := map[string]string{
		"google_stt": circuitStateName(s.metrics.STTCircuitStateGoogle.Load()),
		"google_llm": circuitStateName(s.metrics.LLMCircuitStateGoogle.Load()),
		"xai_llm":    circuitStateName(s.metrics.LLMCircuitStateXAI.Load()),
		"google_tts": circuitStateName(s.metrics.TTSCircuitStateGoogle.Load()),
		"xai_stt":    circuitStateName(s.metrics.STTCircuitStateXAI.Load()),
		"xai_tts":    circuitStateName(s.metrics.TTSCircuitStateXAI.Load()),
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
	if !s.registry.pipelineProvidersLive(pipe) {
		return false
	}
	if pipe.STT == "google.stt" && s.metrics.STTCircuitStateGoogle.Load() == 1 {
		return false
	}
	if pipe.STT == "xai.stt" && s.metrics.STTCircuitStateXAI.Load() == 1 {
		return false
	}
	if pipe.LLM == "google.llm" && s.metrics.LLMCircuitStateGoogle.Load() == 1 {
		return false
	}
	if pipe.LLM == "xai.llm" && s.metrics.LLMCircuitStateXAI.Load() == 1 {
		return false
	}
	if pipe.TTS == "google.tts" && s.metrics.TTSCircuitStateGoogle.Load() == 1 {
		return false
	}
	if pipe.TTS == "xai.tts" && s.metrics.TTSCircuitStateXAI.Load() == 1 {
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

	s.publicServer = &http.Server{
		Handler:           s.publicMux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	s.adminServer = &http.Server{
		Handler:           s.adminMux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	certFile, keyFile := s.cfg.TLS.CertFile, s.cfg.TLS.KeyFile
	if certFile != "" && keyFile != "" {
		if _, err := tls.LoadX509KeyPair(certFile, keyFile); err != nil {
			pubListener.Close()
			adminListener.Close()
			return fmt.Errorf("load tls certificate: %w", err)
		}
	}
	go func() {
		var err error
		if certFile != "" && keyFile != "" {
			err = s.publicServer.ServeTLS(pubListener, certFile, keyFile)
		} else {
			err = s.publicServer.Serve(pubListener)
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.Error("public listener stopped", slog.String("err", err.Error()))
			s.ready.Store(false)
		}
	}()
	go func() {
		_ = s.adminServer.Serve(adminListener)
	}()

	s.pruneStop = make(chan struct{})
	go s.pruneTimelines()

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
	if s.pruneStop != nil {
		select {
		case <-s.pruneStop:
		default:
			close(s.pruneStop)
		}
	}

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

	var drainTimedOut bool
	var remainingCount int

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
				drainTimedOut = true
				remainingCount = count
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
	if drainTimedOut {
		return fmt.Errorf("server shutdown timed out: forced cancellation of %d sessions", remainingCount)
	}

	s.logger.Info("server shutdown complete")
	return nil
}

func (s *Server) observeTransport(event string, n uint64) {
	switch event {
	case "auth_failure":
		s.metrics.AuthFailuresTotal.Add(n)
	case "replay":
		s.metrics.ReplayRejectionsTotal.Add(n)
	case "origin_reject":
		s.metrics.OriginRejectionsTotal.Add(n)
	case "frame_in":
		s.metrics.FramesInTotal.Add(1)
		s.metrics.BytesInTotal.Add(n)
	case "frame_out":
		s.metrics.FramesOutTotal.Add(1)
		s.metrics.BytesOutTotal.Add(n)
	case "flush_ack":
		s.metrics.FlushAcksTotal.Add(n)
	case "slow_client":
		s.metrics.SlowClientTermTotal.Add(n)
	case "client_connected":
		s.metrics.ConnectedClients.Add(int64(n))
	case "client_disconnected":
		s.metrics.ConnectedClients.Add(-int64(n))
	case "disconnect_normal":
		s.metrics.DisconnectsNormal.Add(n)
	case "disconnect_client":
		s.metrics.DisconnectsClientError.Add(n)
	case "disconnect_server":
		s.metrics.DisconnectsServerError.Add(n)
	case "disconnect_timeout":
		s.metrics.DisconnectsTimeout.Add(n)
	case "protocol_error":
		s.metrics.ProtocolErrorsTotal.Add(n)
	}
}

func (s *Server) sampleVADMetrics() {
	if s.vadExecutor == nil {
		return
	}
	st := s.vadExecutor.Stats()
	s.metrics.VADActiveStreams.Store(st.Streams)
	s.metrics.VADStaleChunksTotal.Store(uint64(st.Stale))
	dropped := st.RejectedFull + st.RejectedLimit
	if dropped < 0 {
		dropped = 0
	}
	s.metrics.VADDroppedChunksTotal.Store(uint64(dropped))
	if st.Restarts > 0 {
		s.metrics.VADWorkerRestartsTotal.Store(uint64(st.Restarts))
	}
}

func (s *Server) recordProcessorMetrics(proc core.Processor) {
	switch p := proc.(type) {
	case *core.AssistantContextProcessor:
		s.metrics.FlushTimeoutsTotal.Add(p.FlushTimeouts.Load())
		s.metrics.FlushAcksTotal.Add(p.FlushAcks.Load())
	case *core.UserContextProcessor:
		if n := p.EvictionCount(); n > 0 {
			s.metrics.ContextEvictionsTotal.Add(uint64(n))
		}
	case *google.STTProcessor:
		s.metrics.STTRotationsTotal.Add(p.RotationsCount.Load())
		s.metrics.STTRotationFailuresTotal.Add(p.RotationFailures.Load())
		s.metrics.STTDeduplicatedTotal.Add(p.DeduplicatedResults.Load())
		s.metrics.STTErrorsTotal.Add(p.Errors.Load())
	case *google.LLMProcessor:
		s.metrics.LLMRequestsTotal.Add(p.Requests.Load())
		s.metrics.LLMErrorsTotal.Add(p.Errors.Load())
		s.metrics.LLMRetriesTotal.Add(p.Retries.Load())
		s.metrics.LLMStaleDeltasDiscarded.Add(p.Stale.Load())
	case *xai.Processor:
		s.metrics.LLMRequestsTotal.Add(p.Requests.Load())
		s.metrics.LLMErrorsTotal.Add(p.Errors.Load())
		s.metrics.LLMRetriesTotal.Add(p.Retries.Load())
		s.metrics.LLMStaleDeltasDiscarded.Add(p.Stale.Load())
	case *google.TTSProcessor:
		s.metrics.TTSRequestsTotal.Add(p.Requests.Load())
		s.metrics.TTSErrorsTotal.Add(p.Errors.Load())
		s.metrics.TTSRetriesTotal.Add(p.Retries.Load())
		s.metrics.TTSStaleAudioDiscarded.Add(p.Stale.Load())
	}
}

func (s *Server) pruneTimelines() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-s.pruneStop:
			return
		case <-ticker.C:
			s.timelines.PruneExpired()
		}
	}
}

func hasOriginWildcard(origins []string) bool {
	for _, o := range origins {
		if o == "*" {
			return true
		}
	}
	return false
}
