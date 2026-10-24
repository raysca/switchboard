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
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"
	"golang.org/x/time/rate"
	"govoice/core"
	"govoice/core/observe"
	"govoice/core/record"
	"govoice/core/redact"
	"govoice/core/resilience"
	"govoice/server/adminui"
	"govoice/server/agent"
	"govoice/server/api"
	"govoice/server/auth"
	"govoice/server/catalog"
	"govoice/server/mcp"
	"govoice/server/otelexport"
	"govoice/server/storage"
	"govoice/smartturn"
	"govoice/transport/token"
	"govoice/transport/ws"
	"govoice/vad"
)

// adminUIFS is the admin UI served with public_admin (the embedded build; tests replace it).
var adminUIFS = adminui.Embedded()

// Server represents the config-driven Switchboard server.
type Server struct {
	authFails             *rate.Limiter // global failed-management-auth allowance: burst 10, one more per 6 s
	authNow               func() time.Time
	clientIP              func(*http.Request) string // proxy-aware client address
	secure                func(*http.Request) bool   // request arrived over TLS (directly or via a trusted proxy)
	otel                  *otelexport.Exporter       // nil unless otel.enabled
	redactor              *redact.Redactor           // sink redaction; never applied to the live LLM context
	cfg                   *Config
	adjustCall            func(*CallSpec) // tests only: changes the resolved call before it is built
	catalog               *catalog.Catalog
	metrics               *Metrics
	timelines             *TimelineRegistry
	admission             *AdmissionController
	registry              *Registry
	env                   func(string) string
	resolver              *catalog.Resolver
	availability          providerAvailability
	validator             *token.Validator
	management            *auth.Management
	providerManagementAPI http.Handler
	agentManagementAPI    http.Handler
	mcpManagementAPI      http.Handler
	secretsManagementAPI  http.Handler
	sessionManagementAPI  http.Handler
	store                 *storage.Store
	startedAt             time.Time
	voiceSignKey          []byte
	voiceKeyID            string
	voiceEpoch            string
	drafts                *draftCalls
	probes                probeGate
	vadExecutor           vad.SharedExecutor
	turnBackend           smartturn.Backend
	mgmt                  atomic.Pointer[management]
	agents                *agent.Service
	mcpService            *mcp.Service
	logger                *slog.Logger

	ready        atomic.Bool
	shuttingDown atomic.Bool

	pingMu        sync.Mutex // guards pingAt/pingOK only, never held across the ping
	pingSF        singleflight.Group
	pingDB        func(context.Context) error // test seam; nil pings the store
	sessionBudget core.ResourceBudget
	pingAt        time.Time
	pingOK        bool

	publicMux    *http.ServeMux
	adminMux     http.Handler
	probeMux     http.Handler // /health, /ready, /metrics only (probe_listen_addr)
	publicServer *http.Server
	adminServer  *http.Server
	probeServer  *http.Server
	publicAddr   string // bound addresses after Start (useful with ":0")
	wsServer     *ws.Server
	backgrounds  map[string]background         // the sound catalogue (audio.backgrounds)
	cues         map[string]func() ws.Underlay // handoff cue sounds by name

	mu            sync.Mutex
	activeCalls   map[uint64]*activeCall
	circuitMu     sync.Mutex                       // guards circuitStates only (never held with mu)
	circuitStates map[providerKey]resilience.State // last state each circuit reported (recordCircuit)
	live          *liveCalls                       // stored session id -> context injector of a call running here
	connecting    sync.WaitGroup                   // calls admitted but not yet in activeCalls
	nextCallID    atomic.Uint64
	textTurns     atomic.Uint64
	pruneStop     chan struct{}
	demoSF        singleflight.Group // collapses overlapping demo installs
	stopDemo      context.CancelFunc // cancels the startup demo install; guarded by mu
	stopFlight    context.CancelFunc // cancels the running demo install flight; guarded by mu
	demoWG        sync.WaitGroup     // the startup demo install goroutine
	recSink       *record.LocalSink  // nil unless recording.enabled
}

type activeCall struct {
	sessionID uint64
	principal string
	conn      *ws.Connection
	session   *core.Session
	timeline  *SessionTimeline
	ending    *endReason
	providers map[providerKey]bool // every candidate (stage, provider) of the start step
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
	if cfg.Observability.JSONFormat || cfg.Profile == ProfileProduction { // production logs are always JSON
		logHandler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		logHandler = slog.NewTextHandler(os.Stdout, opts)
	}
	logger := slog.New(logHandler)

	cat, err := catalog.LoadWithOverlay(cfg.Providers.CatalogDir, func(format string, args ...any) {
		logger.Warn(fmt.Sprintf(format, args...))
	})
	if err != nil {
		return nil, err
	}

	metrics := NewMetrics()
	timelines := NewTimelineRegistry(
		cfg.Observability.TimelineMaxEvents,
		cfg.Observability.TimelineTTL,
	)
	admission := NewAdmissionController(cfg, metrics)
	registry := NewRegistry()
	registry.PruneOffline(cfg)
	registry.BindMetrics(metrics)
	cfg.Warmup.Apply()
	if cfg.Warmup.OnStart {
		go registry.WarmOnStart(context.Background(), cfg.Warmup.WarmTimeout())
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
	if cfg.PublicAdmin.Enabled && !management.Enabled() {
		return nil, fmt.Errorf("public_admin.enabled needs the management API key in %s", managementEnv)
	}
	if managementKey != "" && managementKey == tokenSecret {
		return nil, errors.New("management API key must differ from voice token signing key")
	}

	s := &Server{
		catalog:       cat,
		sessionBudget: core.DefaultResourceBudget(),
		cfg:           cfg,
		metrics:       metrics,
		timelines:     timelines,
		live:          newLiveCalls(),
		admission:     admission,
		registry:      registry,
		env:           os.Getenv,
		validator:     validator,
		management:    management,
		voiceSignKey:  []byte(tokenSecret),
		voiceKeyID:    tokenCfg.KeyID,
		voiceEpoch:    validator.ScopeEpoch(),
		drafts:        newDraftCalls(),
		logger:        logger,
		startedAt:     metrics.StartTime,
		activeCalls:   make(map[uint64]*activeCall),
		authFails:     rate.NewLimiter(rate.Every(6*time.Second), 10),
		authNow:       time.Now,
	}
	s.resolver = catalog.NewResolver(s.catalog, func(k string) string { return s.env(k) })
	s.availability = providerAvailability{cat: s.catalog, res: s.resolver, supports: s.registry.Supports, policy: policyFrom(cfg)}
	circuit := s.recordCircuit
	s.registry.onCircuit.Store(&circuit)
	if lowVADStopWithoutManager(cfg) {
		logger.Warn("turn.manager is off with a short vad.stop_ms: turns end on the raw VAD stop and cut callers off mid-sentence; set turn.manager: true or vad.stop_ms: 800")
	}

	if s.redactor, err = redact.New(cfg.Privacy.Redaction); err != nil {
		return nil, err
	}
	if cfg.OTel.Enabled {
		if s.otel, err = otelexport.New(cfg.OTel, os.Getenv); err != nil {
			return nil, fmt.Errorf("otel: %w", err)
		}
		s.otel.SetRedactor(s.redactor.Redact)
		metrics.OTelStats = s.otel.Stats
	}

	if s.backgrounds, err = loadBackgrounds(cfg, ws.FixedPlayerSampleRate); err != nil {
		return nil, err
	}
	if s.cues, err = loadCues(cfg, ws.FixedPlayerSampleRate); err != nil {
		return nil, err
	}
	s.setupRoutes()
	return s, nil
}

func voiceTokenConfig(cfg *Config, tokenSecret string) token.Config {
	tokenCfg := token.DefaultConfig(cfg.Token.Issuer, cfg.Token.Audience, []byte(tokenSecret))
	tokenCfg.MaxLifetime = voiceTokenLifetime(cfg) // the same value the mint side uses
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

// SetTurnBackend attaches the smart-turn model backend (turn.stop:
// turn-analyzer); the server closes it on shutdown. A crashed or overloaded
// model only degrades turn endpointing to speech-timeout, so it is reported
// through metrics, not /ready.
func (s *Server) SetTurnBackend(b smartturn.Backend) {
	s.turnBackend = b
	s.registry.SetTurnBackend(b)
	if p, ok := b.(*smartturn.Pool); ok {
		s.metrics.SmartTurnWorkers = p.Capacity
	}
}

// setProviderManagementAPI installs the authenticated management route handler.
// Configure it before serving requests; the outer API boundary always performs
// administrator authentication before this handler is called.
func (s *Server) setProviderManagementAPI(handler http.Handler) {
	s.providerManagementAPI = handler
}

// setAgentManagementAPI installs the agent route handler behind management
// authentication. Configure it before serving requests.
func (s *Server) setAgentManagementAPI(handler http.Handler) {
	s.agentManagementAPI = handler
}

// setMCPManagementAPI installs the MCP handler behind the management listener
// authentication boundary. Configure it before serving requests.
func (s *Server) setMCPManagementAPI(handler http.Handler) {
	s.mcpManagementAPI = handler
}

// setSecretsManagementAPI installs the secrets handler behind the management
// listener authentication boundary. Configure it before serving requests.
func (s *Server) setSecretsManagementAPI(handler http.Handler) {
	s.secretsManagementAPI = handler
}

// setSessionManagementAPI installs the session and traces route handler behind
// management authentication. Configure it before serving requests.
func (s *Server) setSessionManagementAPI(handler http.Handler) {
	s.sessionManagementAPI = handler
}

// setStore attaches a durable SQL store for operations inspection and diagnostics.
func (s *Server) setStore(store *storage.Store) {
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
	} else if util >= 1.0 || (sessionsTotal > 0 && errorsTotal > 10 && float64(errorsTotal)/float64(sessionsTotal) > 0.5) {
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
		SessionRate:                  sessionRate,
		MaxCallDurationSeconds:       int(s.cfg.Limits.MaxCallDuration.Seconds()),
		VoiceTokenMaxLifetimeSeconds: int(s.effectiveVoiceTokenLifetime().Seconds()),
		Database:                     dbStatus,
		VAD:                          vadStatus,
		Health:                       health,
	}
}

func (s *Server) setupRoutes() {
	// Configure WS Transport Server
	directTLS := s.cfg.TLS.CertFile != "" && s.cfg.TLS.KeyFile != ""
	proxyTLS := s.cfg.TLS.TrustedProxyMode && len(s.cfg.TLS.TrustedProxies) > 0
	wsCfg := ws.ServerConfig{
		Security: ws.SecurityConfig{
			AllowedOrigins:     s.cfg.OriginAllowlist,
			AllowSameHost:      s.cfg.OriginAllowSameHost,
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
		PingInterval:               s.cfg.Limits.PingInterval,
		PingTimeout:                s.cfg.Limits.PingTimeout,
		MaxConnectionLifetime:      0, // the max-call-duration timer in onSessionReady is the single owner (it counts and tells the client)
		MaxConcurrentSessions:      s.cfg.Limits.MaxSessions,
		MaxConcurrentSessionsPerIP: s.cfg.Limits.MaxSessionsPerIP,
		MaxPendingHandshakes:       s.cfg.Limits.MaxPendingHandshakes,
		HandshakeRateLimit:         s.cfg.Limits.HandshakeRatePerIP, // 0 -> ws default (20)
		FixedPlayerRate:            24000,
	}

	sec := wsCfg.Security
	s.clientIP = func(r *http.Request) string {
		if ip := sec.ClientIP(r); ip != "" {
			return ip
		}
		return "unknown" // an unparseable peer shares one bucket
	}

	s.secure = sec.IsSecure
	wsCfg.Cues = len(s.cues) > 0 || len(s.backgrounds) > 0 // the call's background is set once its agent is known
	s.wsServer = ws.NewServer(wsCfg)
	s.wsServer.OnSessionReady = s.onSessionReady
	s.wsServer.OnText = s.onText
	s.wsServer.OnDTMF = s.onDTMF

	// Public Listener: /health and /ws
	s.publicMux = http.NewServeMux()
	s.publicMux.HandleFunc("GET /health", s.handleHealth)
	if s.cfg.PublicAdmin.Enabled {
		s.logger.Warn("public_admin is on: the management API is served on the public port behind the management key")
		s.publicMux.HandleFunc("/api/v1", s.handleAPI)
		s.publicMux.HandleFunc("/api/v1/", s.handleAPI)
		s.publicMux.Handle("/", adminui.Handler(adminUIFS)) // the catch-all (no method: "GET /" would conflict with the method-less /api/ patterns; the handler refuses non-reads itself). /ws, /health and /api/v1 are more specific and win
		if !adminui.Built(adminUIFS) {
			s.logger.Warn("public_admin is on but this binary has no admin UI (run make ui-embed before building)")
		}
	}
	s.publicMux.HandleFunc("GET /ws", func(w http.ResponseWriter, r *http.Request) {
		if s.shuttingDown.Load() { // before auth, so the token is not consumed; it is still bound to this process (issuer_epoch), so the client fetches a new one
			http.Error(w, DrainReason, http.StatusServiceUnavailable)
			return
		}
		s.wsServer.ServeHTTP(w, r)
	})

	// Admin Listener: /health, /ready, /metrics, /debug/timeline
	adminRoutes := http.NewServeMux()
	adminRoutes.HandleFunc("GET /health", s.handleHealth)
	adminRoutes.HandleFunc("GET /ready", s.handleReady)
	adminRoutes.HandleFunc("GET /metrics", s.handleMetrics)
	adminRoutes.Handle("GET /debug/timeline", s.timelines.Handler())
	// Probe listener: the only surface safe to bind beyond loopback (kubelet probes, Prometheus).
	// No management API and no /debug/timeline (per-call principals).
	probe := http.NewServeMux()
	probe.HandleFunc("GET /health", s.handleHealth)
	probe.HandleFunc("GET /ready", s.handleReady)
	probe.HandleFunc("GET /metrics", s.handleMetrics)
	s.probeMux = probe
	s.adminMux = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1" || strings.HasPrefix(r.URL.Path, "/api/v1/") {
			s.handleAPI(w, r)
			return
		}
		adminRoutes.ServeHTTP(w, r)
	})
}

// onSessionReady turns a panic in call setup (agent resolution, a provider
// constructor) into a refused call: the connection closes with an error, the
// process lives. The panic value is redacted before it is logged.
func (s *Server) onSessionReady(conn *ws.Connection) (session *core.Session, err error) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("panic during call setup", "panic", s.redactor.Redact(fmt.Sprint(r)))
			session, err = nil, errors.New("call setup panicked")
		}
	}()
	return s.startCall(conn)
}

func (s *Server) startCall(conn *ws.Connection) (*core.Session, error) {
	// Under s.mu so Shutdown either refuses this call or waits for it to
	// register: a call still resolving its agent is not yet in activeCalls.
	s.mu.Lock()
	if s.shuttingDown.Load() {
		s.mu.Unlock()
		return nil, ws.ErrDraining
	}
	s.connecting.Add(1)
	s.mu.Unlock()
	defer s.connecting.Done()

	principal := conn.Claims().Principal
	if principal == "" {
		principal = conn.Claims().Subject
	}

	agentID := conn.AgentID()

	// 1. Admission Control check
	vadCapacity := true
	if s.vadExecutor != nil {
		avail, _ := s.vadExecutor.Capacity()
		vadCapacity = s.vadExecutor.IsReady() && avail > 0
	}

	if err := s.admission.AcquireSession(principal, agentID, vadCapacity); err != nil {
		return nil, &ws.RejectError{Reason: admissionReason(err), Err: err}
	}

	// A panic from here until the call is registered (agent resolution, a provider
	// constructor) releases what setup acquired, then propagates to onSessionReady.
	sessionID := s.nextCallID.Add(1)
	var fail func(error) (*core.Session, error)
	var releaseCall func()
	releaseSlot := sync.OnceFunc(func() { s.admission.ReleaseSession(principal, agentID) }) // fail may run twice
	registered := false
	defer func() {
		if r := recover(); r != nil {
			switch { // once registered, finishCall owns the release
			case registered:
			case fail != nil:
				_, _ = fail(errors.New("call setup panicked"))
			default: // resolved, but setup has no fail path yet
				s.recoverEach(sessionID, "releaseCall", releaseCall)
				releaseSlot()
			}
			panic(r)
		}
	}()

	timeline := s.timelines.GetOrCreate(sessionID)
	timeline.Record(TimelineEvent{Type: EventConnect, Details: "websocket connected"})
	timeline.Record(TimelineEvent{Type: EventAuthenticate, Details: fmt.Sprintf("principal=%s", principal)})
	timeline.Record(TimelineEvent{Type: EventReady, Details: "rtvi ready"})

	// 2. Pin the token's stored agent and resolve providers and their environment secrets
	resolveCtx, cancelResolve := context.WithTimeout(conn.Context(), resolveTimeout)
	tokenID := ""
	if claims := conn.Claims(); claims != nil {
		tokenID = claims.ID
	}
	ending := &endReason{transport: conn.CloseCause}
	call, rel, err := s.resolveCall(resolveCtx, conn.AgentID(), tokenID, principal, ending)
	cancelResolve()
	releaseCall = rel
	if err != nil {
		releaseSlot()
		s.metrics.SessionsFailedTotal.Add(1)
		timeline.Record(TimelineEvent{Type: EventError, Details: s.redactor.Redact(err.Error())})
		s.logSetupFailure(sessionID, agentID, "resolve", err)
		return nil, fmt.Errorf("resolve agent: %w", err)
	}
	call.PushToTalk = conn.PushToTalk()
	conn.SetBackground(s.backgroundFor(call.Audio))
	var injector ContextInjector
	call.OnInjector = func(i ContextInjector) { injector = i }
	var greet func(string)
	call.OnGreeter = func(speak func(string)) { greet = speak }
	call.OnGreetFail = func(err error) {
		s.metrics.GreetingFailedTotal.Add(1)
		s.logger.Warn("greeting not queued", "session", call.StoredSessionID, "err", s.redactor.Redact(err.Error()))
	}
	call.OnHandoff = s.onHandoff(timeline, call.StoredSessionID)
	call.OnEndCall = s.onEndCall(call.StoredSessionID)
	call.OnProviderRetry = func(stage, provider string, retry int) {
		timeline.Record(TimelineEvent{Type: EventProviderRetry, Provider: provider, Details: fmt.Sprintf("%s retry %d", stage, retry)})
	}
	if hook := s.handoffBackground(call.Agent, call.Audio, conn.SetBackground); hook != nil {
		base := call.OnHandoff
		call.OnHandoff = func(e core.HandoffEvent) { base(e); hook(e) }
	}
	cue := s.newCallCue(call.Agent, conn.Cue(), timeline)
	hold := s.newCallHold(call.Audio, conn.Cue(), timeline, cue)
	call.toolHold = hold
	if cue != nil || hold != nil {
		base := call.OnHandoff
		// The hold finishes first when a handoff starts preparing: the two sounds share one player.
		call.OnHandoff = func(e core.HandoffEvent) { base(e); hold.onHandoff(e); cue.onHandoff(e) }
	}
	phase := "build_pipeline"
	var stopRecording func()
	var pipe *core.Pipeline
	var session *core.Session
	fail = func(err error) (*core.Session, error) {
		defer releaseSlot() // even if a step below panics
		// Release provider clients: the session closes its processors, and
		// before a session exists the built pipeline's processors are closed here.
		if session != nil {
			_ = session.Close()
		} else if pipe != nil {
			for _, p := range pipe.Processors() {
				if c, ok := p.(core.Closer); ok {
					func() {
						defer func() { _ = recover() }()
						_ = c.Close()
					}()
				}
			}
		}
		ending.set(storage.EndSetupFailed)
		s.recoverEach(sessionID, "releaseCall", releaseCall)
		s.recoverEach(sessionID, "stopRecording", stopRecording)
		s.metrics.SessionsFailedTotal.Add(1)
		timeline.Record(TimelineEvent{Type: EventError, Details: s.redactor.Redact(err.Error())})
		s.logSetupFailure(sessionID, agentID, phase, err)
		return nil, err
	}

	// 3. Build VAD processor and the agent's pipeline
	vadProc := vad.NewProcessor(s.vadExecutor, s.cfg.VADProcessorConfigForStopMS(call.VADStopMS))
	pipe, err = s.registry.BuildCallPipeline(s.cfg, call, vadProc, conn)
	call.wipeSecrets()
	if err != nil {
		pipe = nil // BuildCallPipeline already closed what it built
		return fail(fmt.Errorf("build pipeline: %w", err))
	}
	errProviders := stageProviders(call)

	// 4. Construct Core Session
	session, err = core.NewSession(sessionID, pipe, s.sessionBudget)
	if err != nil {
		session = nil
		phase = "create_session"
		return fail(fmt.Errorf("create session: %w", err))
	}

	active := &activeCall{
		sessionID: sessionID,
		principal: principal,
		conn:      conn,
		session:   session,
		timeline:  timeline,
		ending:    ending,
		providers: callProviders(call),
	}

	var (
		speechActive       bool
		lastSTTTurn        string
		firstTokenRecorded bool
		lastLLMTurn        string
		lastTTSUnit        uint64
		lastInterruption   uint64
		lastFlushAck       uint64
		outage             = sttOutage{window: s.cfg.Turn.NoTranscriptTimeout}
	)

	session.OnFrame = func(f core.Frame, dir core.Direction) {
		if outage.observe(f) {
			// The STT stayed unreachable for consecutive turns (an STT failover chain, if any, is
			// exhausted): a deaf call is worse than a clear hang-up. Like the idle end, off this
			// observer, since Session.Close waits for it.
			ending.set(storage.EndError)
			go func() {
				defer func() {
					if r := recover(); r != nil {
						s.logger.Error("panic during stt hang-up", slog.Uint64("session", sessionID), "panic", s.redactor.Redact(fmt.Sprint(r)))
					}
				}()
				timeline.Record(TimelineEvent{Type: EventDisconnect, Details: "terminated: stt unavailable"})
				conn.Hangup("stt_unavailable", 2*time.Second) // final RTVI error with the reason, 1000 close
				conn.Close()
				_ = session.Close()
			}()
		}
		switch f.Type() {
		case core.FrameUserStartedSpeaking:
			cue.userSpeech(true)
			hold.userSpeech(true)
			if !speechActive {
				speechActive = true
				firstTokenRecorded = false
				timeline.Record(TimelineEvent{Type: EventSpeechStart})
			}
		case core.FrameUserStoppedSpeaking:
			cue.userSpeech(false)
			hold.userSpeech(false)
			if speechActive {
				speechActive = false
				timeline.Record(TimelineEvent{Type: EventSpeechStop})
			}
		case core.FrameUserMessage:
			um := f.(*core.UserMessageFrame)
			if core.IsSayTurn(um.Turn) { // a spoken greeting, not a caller turn
				break
			}
			if um.Turn != lastSTTTurn {
				lastSTTTurn = um.Turn
				timeline.Record(TimelineEvent{Type: EventSTTResult, TurnID: um.Turn, Gen: um.Gen})
			}
		case core.FrameLLMTextDelta:
			if core.IsSayTurn(f.(*core.LLMTextDeltaFrame).Turn) {
				break
			}
			if !firstTokenRecorded {
				firstTokenRecorded = true
				timeline.Record(TimelineEvent{Type: EventLLMFirstToken})
			}
		case core.FrameLLMText:
			lt := f.(*core.LLMTextFrame)
			if core.IsSayTurn(lt.Turn) { // a spoken line, not a model reply
				break
			}
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
			cue.botAudio()
			hold.botAudio()
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
			msg := "unknown error"
			if ef.Err != nil {
				msg = s.redactor.Redact(ef.Err.Error()) // provider errors may echo request text
			}
			timeline.Record(TimelineEvent{Type: EventError, Error: msg})
		}
	}

	for _, p := range pipe.Processors() {
		if ic, ok := p.(*core.IdleController); ok {
			ic.SetOnEnd(func(reason string) {
				ending.set(reasonFromEnd(reason))
				// Off the controller's task: Session.Close waits for it.
				go func() {
					timeline.Record(TimelineEvent{Type: EventDisconnect, Details: "terminated: " + reason})
					conn.Hangup(reason, 2*time.Second) // final, not "reconnect elsewhere": RTVI error with the reason, 1000 close
					conn.Close()
					_ = session.Close()
				}()
			})
		}
	}

	var otelTracer *otelexport.SessionTracer
	if s.otel != nil {
		otelTracer = s.otel.Session(agentID, call.AgentVersionID)
		if err := session.SetTracer(otelTracer, s.otel.Policy()); err != nil {
			return fail(fmt.Errorf("otel tracer: %w", err))
		}
	}

	stopRecording = s.attachRecording(call, agentID, sessionID, conn, session)

	// Latency observer: attach before Start so startup timing is captured.
	if _, err := observe.Attach(session, func(m observe.Measurement) {
		if otelTracer != nil {
			otelTracer.Latency(m)
		}
		s.metrics.ObserveLatency(m.Stage, m.Value)
		if call.TurnLatencies != nil {
			call.TurnLatencies.observe(m)
		}
		timeline.Record(TimelineEvent{Type: EventLatency, Timestamp: m.At, TurnID: string(m.Turn), Details: string(m.Stage), DurationMs: m.Value.Milliseconds()})
	}); err != nil {
		return fail(fmt.Errorf("attach latency observer: %w", err))
	}

	if call.Greeting != "" && greet != nil {
		// The opening line: said once, as soon as the session is running (the transport starts it after this returns).
		if err := session.Go(func(ctx context.Context) {
			select {
			case <-session.Ready():
				greet(call.Greeting)
			case <-ctx.Done():
			}
		}); err != nil {
			return fail(fmt.Errorf("start greeting: %w", err))
		}
	}

	s.mu.Lock()
	s.activeCalls[sessionID] = active
	s.mu.Unlock()
	// Registered only once the pipeline is built; the done goroutine below removes it.
	if injector != nil && call.StoredSessionID != "" {
		s.live.Put(call.StoredSessionID, &sessionInjector{ContextInjector: injector, sess: session})
	}

	// 5. Cleanup when session closes
	go func() {
		<-session.Done()
		s.live.Delete(call.StoredSessionID) // gone for operators before teardown work starts
		s.finishCall(sessionID, principal, agentID, session, conn, timeline, errProviders, ending, releaseCall, stopRecording)
	}()
	registered = true

	s.metrics.SessionsStartedTotal.Add(1)

	// 6. Max call duration cost guard (spec §32, §35)
	if call.MaxCallDuration > 0 {
		maxDurTimer := s.callLimitTimer(call)
		go func() {
			defer maxDurTimer.Stop()
			select {
			case <-session.Done():
				return
			case <-maxDurTimer.C:
				ending.set(storage.EndTimeLimit)
				s.metrics.MaxDurationTermTotal.Add(1)
				timeline.Record(TimelineEvent{
					Type:    EventDisconnect,
					Details: "terminated: max call duration reached",
				})
				conn.Hangup("max_call_duration", 2*time.Second) // final RTVI error with the reason, 1000 close (like idle)
				conn.Close()
				_ = session.Close()
			}
		}()
	}

	return session, nil
}

// callLimitTimer is the call's cost-guard timer: it runs for the CallSpec's limit (the agent's, already
// clamped to the server's by callSpec), nil when there is none.
func (s *Server) callLimitTimer(call CallSpec) *time.Timer {
	if call.MaxCallDuration <= 0 {
		return nil
	}
	return time.NewTimer(call.MaxCallDuration)
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	s.sampleVADMetrics()
	s.metrics.Handler().ServeHTTP(w, r)
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

	// Check VAD executor capacity
	if s.vadExecutor != nil {
		if !s.vadExecutor.IsReady() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "unready", "reason": "zero VAD worker capacity"})
			return
		}
	}

	if s.agents == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "unready", "reason": "agent storage is not enabled"})
		return
	}

	if !s.storeHealthy(r.Context()) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "unready", "reason": "agent store unreachable"})
		return
	}

	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":   "ready",
		"circuits": s.circuitReport(),
	})
}

// circuitReport lists the circuits of providers built into this binary, for
// diagnosis; calls are per agent, so an open circuit only affects agents using
// that provider. Nvidia breakers are per endpoint: "nvidia_<stage>:<endpoint>".
func (s *Server) circuitReport() map[string]string {
	m := s.metrics
	out := map[string]string{}
	add := func(provider, stage string, v *atomic.Int64) {
		if s.registry.Supports(provider, stage) {
			out[provider+"_"+stage] = circuitStateName(v.Load())
		}
	}
	add("google", "stt", &m.STTCircuitStateGoogle)
	add("google", "llm", &m.LLMCircuitStateGoogle)
	add("google", "tts", &m.TTSCircuitStateGoogle)
	add("xai", "stt", &m.STTCircuitStateXAI)
	add("xai", "llm", &m.LLMCircuitStateXAI)
	add("xai", "tts", &m.TTSCircuitStateXAI)
	for k, v := range m.circuits.snapshot() {
		out[k.provider+"_"+k.stage+":"+k.endpoint] = circuitStateName(v)
	}
	return out
}

// storeHealthy pings the agent store, caching the verdict for a second so a
// probe storm does not become a query storm.
func (s *Server) storeHealthy(ctx context.Context) bool {
	if s.store == nil || s.store.DB() == nil {
		return true
	}
	s.pingMu.Lock()
	if time.Since(s.pingAt) < time.Second {
		ok := s.pingOK
		s.pingMu.Unlock()
		return ok
	}
	s.pingMu.Unlock()
	// Concurrent callers share one in-flight ping; no lock is held during it.
	v, _, _ := s.pingSF.Do("ping", func() (any, error) {
		// WithoutCancel: one aborted probe must not cache an "unhealthy" verdict for every other caller.
		pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer cancel()
		ping := s.pingDB
		if ping == nil {
			ping = s.store.DB().PingContext
		}
		ok := ping(pctx) == nil
		s.pingMu.Lock()
		s.pingOK, s.pingAt = ok, time.Now()
		s.pingMu.Unlock()
		return ok, nil
	})
	return v.(bool)
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
	var probeListener net.Listener
	if s.cfg.ProbeListenAddr != "" {
		if probeListener, err = net.Listen("tcp", s.cfg.ProbeListenAddr); err != nil {
			pubListener.Close()
			adminListener.Close()
			return fmt.Errorf("listen probe %s: %w", s.cfg.ProbeListenAddr, err)
		}
		s.probeServer = &http.Server{
			Handler:           s.probeMux,
			ReadHeaderTimeout: 5 * time.Second,
			IdleTimeout:       60 * time.Second,
		}
	}

	certFile, keyFile := s.cfg.TLS.CertFile, s.cfg.TLS.KeyFile
	if certFile != "" && keyFile != "" {
		if _, err := tls.LoadX509KeyPair(certFile, keyFile); err != nil {
			pubListener.Close()
			adminListener.Close()
			if probeListener != nil {
				probeListener.Close()
			}
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
	if probeListener != nil {
		go func() { _ = s.probeServer.Serve(probeListener) }()
	}

	s.publicAddr = pubListener.Addr().String()
	s.pruneStop = make(chan struct{})
	go s.pruneTimelines()

	if s.cfg.Demo.AutoInstall == "dental" {
		s.autoInstallDemo()
	}
	s.ready.Store(true)
	s.logger.Info("server started",
		slog.String("public_addr", s.cfg.PublicListenAddr),
		slog.String("admin_addr", s.cfg.AdminListenAddr),
		slog.String("probe_addr", s.cfg.ProbeListenAddr),
		slog.String("version", Version),
		slog.String("commit", Commit),
		slog.String("profile", string(s.cfg.Profile)),
	)
	return nil
}

// Logger is the configured structured logger (JSON when observability.json_format).
func (s *Server) Logger() *slog.Logger { return s.logger }

// Shutdown performs orderly drain-then-cancel shutdown per spec §37.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.shuttingDown.Store(true)
	s.stopDemoLocked()
	s.mu.Unlock()
	s.ready.Store(false)
	s.metrics.Draining.Store(1)
	if s.pruneStop != nil {
		select {
		case <-s.pruneStop:
		default:
			close(s.pruneStop)
		}
	}

	s.logger.Info("server shutting down: marked unready, stopping admission")

	// 1. The public listener stays open so late arrivals (LB endpoint lag) get
	// 503 server_draining instead of connection refused; /ws checks shuttingDown.

	// Calls admitted before shutdown finish registering (bounded by resolveTimeout).
	waitUntil(ctx, resolveTimeout+time.Second, func() bool { s.connecting.Wait(); return true })

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
			s.metrics.DrainRemaining.Store(int64(count))
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
	// Snapshot under the lock and close concurrently: Session.Close can take seconds per wedged
	// session, and serial closes under s.mu would overrun the process budget (M2-03).
	type target struct {
		conn   *ws.Connection
		sess   interface{ Close() error }
		ending *endReason
	}
	s.mu.Lock()
	var targets []target
	for _, call := range s.activeCalls {
		t := target{conn: call.conn, ending: call.ending}
		if call.session != nil {
			t.sess = call.session
		}
		targets = append(targets, t)
	}
	s.mu.Unlock()
	var goAway sync.WaitGroup
	for _, t := range targets {
		t.ending.set(storage.EndServerShutdown)
		if t.conn != nil {
			// Tell the client to reconnect (reason server_draining, close 1001).
			goAway.Add(1)
			go func(c *ws.Connection) { defer goAway.Done(); c.GoAway(DrainReason, 2*time.Second) }(t.conn)
		}
		if t.sess != nil {
			goAway.Add(1)
			go func(c interface{ Close() error }) { defer goAway.Done(); _ = c.Close() }(t.sess)
		}
	}
	// GoAway and Session.Close each bound themselves (~2 s); ctx caps the whole phase (M2-03).
	waitUntil(ctx, 3*time.Second, func() bool { goAway.Wait(); return true })
	if s.publicServer != nil {
		_ = s.publicServer.Close() // upgraded sockets are hijacked, so this only drops HTTP
	}

	// Their teardown ends each session row (bounded by releaseTimeout); wait for
	// it before closing the store.
	waitUntil(ctx, releaseTimeout+time.Second, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.activeCalls) == 0
	})

	// 4. Release VAD Executor
	if s.vadExecutor != nil {
		_ = s.vadExecutor.Close()
	}

	if s.turnBackend != nil {
		_ = s.turnBackend.Close()
	}

	// 5. Close admin server, then the management plane it served
	if s.adminServer != nil {
		_ = s.adminServer.Close()
	}
	if s.probeServer != nil {
		_ = s.probeServer.Close()
	}
	// The install must not run against a closed store. A short grace on a context that survives an
	// already-expired shutdown ctx: the install was cancelled above and ends promptly.
	graceCtx, graceCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer graceCancel()
	if !s.waitDemo(graceCtx) {
		s.logger.Warn("demo install still running at shutdown")
	}
	s.closeManagement()
	if s.otel != nil { // after calls drained so their final spans are flushed
		if err := s.otel.Shutdown(context.WithoutCancel(ctx)); err != nil {
			s.logger.Warn("otel flush incomplete", slog.String("err", err.Error()))
		}
	}

	if drainTimedOut {
		return fmt.Errorf("server shutdown timed out: forced cancellation of %d sessions", remainingCount)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("shutdown deadline exceeded: %w", err)
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
	case "ping_failure":
		s.metrics.WSPingFailuresTotal.Add(n)
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

// callProviders is "stage/provider" for every candidate (primary and fallbacks) of the call's start step.
// providerKey names one provider circuit; a struct key so a breaker report allocates nothing.
type providerKey struct{ stage, provider string }

func callProviders(c CallSpec) map[providerKey]bool {
	m := map[providerKey]bool{}
	add := func(stage string, primary StageSpec, fallbacks []StageSpec) {
		m[providerKey{stage, primary.Provider}] = true
		for _, f := range fallbacks {
			m[providerKey{stage, f.Provider}] = true
		}
	}
	add("stt", c.STT, c.STTFallbacks)
	add("llm", c.LLM, c.LLMFallbacks)
	add("tts", c.TTS, c.TTSFallbacks)
	return m
}

// stageProviders names the provider each stage's errors are attributed to: its
// provider, or "failover" when the stage has fallbacks (the chain's processor
// sums its candidates, so the failing one is unknown).
func stageProviders(c CallSpec) map[string]string {
	pick := func(p string, fallbacks int) string {
		if fallbacks > 0 {
			return "failover"
		}
		return p
	}
	return map[string]string{
		"stt": pick(c.STT.Provider, len(c.STTFallbacks)),
		"llm": pick(c.LLM.Provider, len(c.LLMFallbacks)),
		"tts": pick(c.TTS.Provider, len(c.TTSFallbacks)),
	}
}

// recordProviderErrors adds a finished call's stage errors to voice_provider_errors_total.
func (s *Server) recordProviderErrors(proc core.Processor, prov map[string]string) {
	switch p := proc.(type) {
	case core.STTCounters:
		_, _, errs := p.STTCounts()
		s.metrics.providerErrors.add(prov["stt"], "stt", errs)
	case core.LLMCounters:
		_, errs, _, _ := p.LLMCounts()
		s.metrics.providerErrors.add(prov["llm"], "llm", errs)
	case core.TTSCounters:
		_, errs, _, _ := p.TTSCounts()
		s.metrics.providerErrors.add(prov["tts"], "tts", errs)
	}
}

func (s *Server) recordProcessorMetrics(proc core.Processor) {
	switch p := proc.(type) {
	case *core.AssistantContextProcessor:
		s.metrics.FlushTimeoutsTotal.Add(p.FlushTimeouts.Load())
	case *core.IdleController:
		s.metrics.UserIdlePromptsTotal.Add(p.UserPrompts.Load())
		s.metrics.UserIdleHangupsTotal.Add(p.UserHangups.Load())
		s.metrics.BotIdleStuckTotal.Add(p.BotStuck.Load())
		s.metrics.BotIdleHangupsTotal.Add(p.BotHangups.Load())
	case *core.UserContextProcessor:
		if n := p.EvictionCount(); n > 0 {
			s.metrics.ContextEvictionsTotal.Add(uint64(n))
		}
	case core.STTCounters: // google and the failover wrapper
		rot, rotFail, errs := p.STTCounts()
		s.metrics.STTRotationsTotal.Add(rot)
		s.metrics.STTRotationFailuresTotal.Add(rotFail)
		s.metrics.STTErrorsTotal.Add(errs)
	case core.LLMCounters: // google, xai, openai and the failover wrapper
		req, errs, retries, stale := p.LLMCounts()
		s.metrics.LLMRequestsTotal.Add(req)
		s.metrics.LLMErrorsTotal.Add(errs)
		s.metrics.LLMRetriesTotal.Add(retries)
		s.metrics.LLMStaleDeltasDiscarded.Add(stale)
	case core.TTSCounters: // google, xai, ttsunit-based (cartesia, elevenlabs) and the failover wrapper
		req, errs, retries, stale := p.TTSCounts()
		s.metrics.TTSRequestsTotal.Add(req)
		s.metrics.TTSErrorsTotal.Add(errs)
		s.metrics.TTSRetriesTotal.Add(retries)
		s.metrics.TTSStaleAudioDiscarded.Add(stale)
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

// onText enters a client text turn into the call as a committed user turn: it
// skips VAD/STT and reaches the conversation context and LLM exactly like a
// finalized spoken transcript, so the reply is text and audio as usual.
func (s *Server) onText(conn *ws.Connection, text string) error {
	session := conn.Session()
	if session == nil {
		return errors.New("no active session")
	}
	gen := session.CurrentGeneration()
	frame := &core.FinalTranscriptionFrame{
		Text:      text,
		Timestamp: time.Now(),
		Turn:      fmt.Sprintf("text-%d", s.textTurns.Add(1)),
		Gen:       gen,
		Committed: true,
	}
	idx := CallUserContextIndex
	for i, p := range session.Processors() {
		if _, ok := p.(*core.UserContextProcessor); ok {
			idx = i // shifts by one when the turn processor is installed
		}
	}
	return session.Reenter(frame, core.Downstream, idx, gen)
}

// waitUntil polls done (or, if it blocks, waits for it) until it reports true,
// max elapses, or ctx ends.
func waitUntil(ctx context.Context, max time.Duration, done func() bool) {
	finished := make(chan struct{})
	stop := make(chan struct{})
	defer close(stop) // the poller ends with the wait (a blocking done ends when it returns)
	go func() {
		for !done() {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
		close(finished)
	}()
	timer := time.NewTimer(max)
	defer timer.Stop()
	select {
	case <-finished:
	case <-timer.C:
	case <-ctx.Done():
	}
}

func (s *Server) recoverEach(sessionID uint64, label string, fn func()) {
	if fn == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("panic during "+label, slog.Uint64("session", sessionID), "panic", s.redactor.Redact(fmt.Sprint(r)))
		}
	}()
	fn()
}

func (s *Server) finishCall(sessionID uint64, principal, agentID string, session *core.Session, conn *ws.Connection, timeline *SessionTimeline, errProviders map[string]string, ending *endReason, release func(), stopRecording func()) {
	recDone := make(chan struct{})
	go func() {
		defer close(recDone)
		s.recoverEach(sessionID, "stopRecording", stopRecording)
	}()
	defer func() {
		<-recDone
		if r := recover(); r != nil {
			s.logger.Error("panic during session cleanup", slog.Uint64("session", sessionID), "panic", s.redactor.Redact(fmt.Sprint(r)))
		}
		s.mu.Lock()
		delete(s.activeCalls, sessionID)
		s.mu.Unlock()
		if timeline != nil {
			timeline.Record(TimelineEvent{Type: EventDisconnect, Details: "session closed"})
		}
		s.metrics.SessionsStoppedTotal.Add(1)
	}()
	defer s.admission.ReleaseSession(principal, agentID)

	if session != nil {
		if n := session.LeakedTasks(); n > 0 {
			s.metrics.LeakedSessionsTotal.Add(1)
			s.metrics.LeakedWorkTotal.Add(uint64(n))
		}
		for _, proc := range session.Processors() {
			s.recordProcessorMetrics(proc)
			s.recordProviderErrors(proc, errProviders)
		}
		if err := session.Err(); err != nil {
			ending.set(storage.EndError)
			s.metrics.SessionsFailedTotal.Add(1)
			s.logger.Error("session failed", slog.Uint64("session", sessionID), slog.String("err", s.redactor.Redact(err.Error())))
			if conn != nil {
				conn.Close()
			}
		}
	}
	s.recoverEach(sessionID, "releaseCall", release)
}

// logSetupFailure logs one warning for a call that could not be set up, so
// operators see why (bad credential, refused endpoint, incomplete agent) rather
// than only a rising failure counter. It names the phase, category and failing
// provider stage but never the error text: that may echo endpoints, credential
// fragments or provider messages, and s.redactor masks PII, not secrets. The
// redacted text is on the call timeline.
func (s *Server) logSetupFailure(session uint64, agentID, phase string, err error) {
	category := phase
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		category = "timeout"
	}
	s.logger.Warn("call setup failed", slog.Uint64("session", session), slog.String("agent", agentID),
		slog.String("category", category), slog.String("stage", setupStage(err.Error())))
}

// setupStageRe matches the "<provider>.<stage>" prefix registry errors carry.
var setupStageRe = regexp.MustCompile(`(?:^|: )([a-z0-9_]+\.(?:llm|stt|tts|vad))(?: tools)?: `)

// setupStage returns the provider stage a setup error names ("nvidia.stt"), or "".
func setupStage(msg string) string {
	if m := setupStageRe.FindStringSubmatch(msg); m != nil {
		return m[1]
	}
	return ""
}
