package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"govoice/core/record"
	"govoice/server/agent"
	"govoice/server/api"
	"govoice/server/mcp"
	"govoice/server/retention"
	"govoice/server/storage"
	"govoice/server/trace"
)

// management owns the durable management plane so Shutdown can release it.
type management struct {
	store     *storage.Store
	traces    *trace.Adapter
	retention *retention.Job
	mcpSync   *mcp.Syncer
	recStop   context.CancelFunc // ends the recording janitor
}

// EnableManagement opens storage and installs the agent, provider, MCP and
// session APIs plus the retention job. It is a no-op when storage is not
// configured. Call it before Start.
func (s *Server) EnableManagement(ctx context.Context) error {
	if s.cfg.Storage.Dialect == "" {
		return errors.New("storage.dialect is required: calls run stored agents")
	}
	dsnEnv := envOr(s.cfg.Storage.DSNEnv, "GOVOICE_DATABASE_DSN")
	dsn := os.Getenv(dsnEnv)
	if s.cfg.Profile == ProfileDevelopment && dsn == "" {
		var err error
		if dsn, err = s.developmentStore(); err != nil {
			return err
		}
	}
	if dsn == "" {
		return fmt.Errorf("storage enabled but %s is empty", dsnEnv)
	}
	store, err := storage.Open(ctx, storage.Config{Dialect: storage.Dialect(s.cfg.Storage.Dialect), DSN: dsn})
	if err != nil {
		return fmt.Errorf("open storage: %w", err)
	}
	m := &management{store: store}
	fail := func(err error) error { m.close(); return err }

	m.traces = trace.NewAdapter(store.Traces())
	mcpTokens := mcp.EnvTokens(func(k string) string { return s.env(k) })
	mcpCfg := mcp.ServiceConfig{Store: store, Remote: mcp.NewSDKClient(), Tokens: mcpTokens, Tracer: m.traces, Timeout: s.cfg.MCP.Timeout}
	if s.cfg.MCP.AllowLocalHTTP {
		s.logger.Warn("MCP allows plain HTTP to loopback (development and tests)")
		mcpCfg.Remote = mcp.NewSDKClient(mcp.AllowLoopbackHTTP())
		mcpCfg.Destinations = mcp.LoopbackHTTPDestinationPolicy{}
	}
	mcpService, err := mcp.NewService(mcpCfg)
	if err != nil {
		return fail(fmt.Errorf("mcp service: %w", err))
	}

	// Validate everything that can fail before installing handlers, so a failed
	// enable never leaves routes pointing at a closed store.
	var policy *retention.Policy
	if rc := s.cfg.Retention; rc.Enabled {
		p := retention.Policy{
			ContentRetention:  time.Duration(rc.ContentRetentionDays) * 24 * time.Hour,
			MetadataRetention: time.Duration(rc.MetadataRetentionDays) * 24 * time.Hour,
			BatchSize:         rc.BatchSize,
		}
		if err := p.Validate(); err != nil {
			return fail(fmt.Errorf("retention policy: %w", err))
		}
		policy = &p
	}

	// The production provider check and the recording sink can still fail:
	// both run before anything points at the store fail() closes.
	agents := agent.NewService(store, s.availability)
	agents.SetTokenEnv(func(n string) bool { _, ok := mcpTokens.Token(n); return ok })
	if bad := s.unavailableAgentProviders(ctx, agents); len(bad) > 0 {
		for _, b := range bad {
			s.logger.Error("agent references a provider this build cannot run", slog.String("problem", b))
		}
		if s.cfg.Profile == ProfileProduction {
			return fail(fmt.Errorf("%d stored agent stage(s) use providers missing from this build (xAI STT/TTS need -tags xaispeech): %s", len(bad), strings.Join(bad, "; ")))
		}
	}
	var sink *record.LocalSink
	if rc := s.cfg.Recording; rc.Enabled {
		if sink, err = record.NewLocalSink(rc.Dir); err != nil {
			return fail(fmt.Errorf("recording sink: %w", err))
		}
	}

	s.setStore(store)
	s.endOrphanedSessions(ctx, store)
	s.mcpService = mcpService
	s.setProviderManagementAPI(s.providersRoutes())
	s.agents = agents
	s.setAgentManagementAPI(api.NewAgentsHandler(s.agents))
	s.setMCPManagementAPI(mcp.Handler(mcpService))
	s.setSessionManagementAPI(api.NewTracesHandler(store.Sessions(), store.Traces()))

	if policy != nil {
		m.retention = retention.NewJob(store.Retention(), *policy, retention.WithInterval(s.cfg.Retention.Interval), retention.WithLogger(s.logger))
		m.retention.Start(context.Background())
	} else {
		s.logger.Warn("retention disabled: stored transcripts and traces are kept indefinitely")
	}
	if d := s.cfg.MCP.SyncInterval; d > 0 {
		m.mcpSync = mcp.NewSyncer(mcpService, d, s.logger, func(ok bool) {
			if ok {
				s.metrics.MCPSyncOKTotal.Add(1)
			} else {
				s.metrics.MCPSyncFailedTotal.Add(1)
			}
		})
		m.mcpSync.Start()
	}
	if sink != nil {
		s.recSink = sink
		jctx, stop := context.WithCancel(context.Background())
		m.recStop = stop
		go sink.Janitor(jctx, time.Duration(s.cfg.Recording.RetentionDays)*24*time.Hour, time.Hour, s.logger)
	}
	s.mgmt.Store(m)
	s.logger.Info("management plane enabled", slog.String("dialect", s.cfg.Storage.Dialect), slog.Bool("retention", m.retention != nil))
	return nil
}

// unavailableAgentProviders lists "agent id stage provider" for every enabled,
// non-archived stored agent whose stage (or fallback) has no factory in this
// build, so a tagged-vs-untagged build mismatch fails startup, not calls.
func (s *Server) unavailableAgentProviders(ctx context.Context, agents *agent.Service) []string {
	var bad []string
	for offset := 0; ; offset += 100 {
		page, err := agents.List(ctx, 100, offset)
		if err != nil {
			s.logger.Warn("agent provider check skipped", slog.String("err", s.redactor.Redact(err.Error())))
			return bad
		}
		for _, a := range page {
			if !a.Enabled || a.Archived || a.Version == nil {
				continue
			}
			g, err := agent.Decode(a.Version.Config)
			if err != nil {
				continue
			}
			for _, n := range g.Steps {
				for _, stage := range []string{"stt", "llm", "tts"} {
					for _, c := range n.Config.Chain(stage) {
						if c.Provider != "" && !s.registry.Supports(c.Provider, stage) {
							bad = append(bad, fmt.Sprintf("agent %s %s=%s", a.ID, stage, c.Provider))
						}
					}
				}
			}
		}
		if len(page) < 100 {
			return bad
		}
	}
}

// stopSync ends the background MCP sync, waiting at most until ctx ends.
func (m *management) stopSync(ctx context.Context) {
	if m != nil && m.mcpSync != nil {
		_ = m.mcpSync.Stop(ctx)
	}
}

func (m *management) close() {
	if m == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	m.stopSync(ctx) // before the store closes
	cancel()
	if m.retention != nil {
		m.retention.Stop()
	}
	if m.recStop != nil {
		m.recStop()
	}
	if m.traces != nil {
		_ = m.traces.Close()
	}
	if m.store != nil {
		_ = m.store.Close()
	}
}

func envOr(name, fallback string) string {
	if name == "" {
		return fallback
	}
	return name
}

// closeManagement is called from Shutdown after calls have drained.
func (s *Server) closeManagement() {
	s.mgmt.Swap(nil).close() // nil-safe and idempotent: a second call closes nothing
}

// developmentStore returns a local SQLite DSN for the development profile, so a
// fresh checkout runs without setup. The directory defaults to .govoice-dev
// (override with GOVOICE_DEV_DIR). Production never reaches this path and fails
// closed without an explicit DSN.
func (s *Server) developmentStore() (string, error) {
	dir := os.Getenv("GOVOICE_DEV_DIR")
	if dir == "" {
		dir = ".govoice-dev"
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("development store: %w", err)
	}
	s.logger.Warn("development profile: using a local agent store; never use it in production", slog.String("dir", dir))
	return filepath.Join(dir, "switchboard.db"), nil
}

// providersRoutes serves the read-only provider listing and the on-demand provider test; every other
// path under /api/v1/providers/ is a 404.
func (s *Server) providersRoutes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/api/v1/providers", api.ProvidersHandler{Catalog: s.catalog, Resolver: s.resolver, Supports: s.registry.Supports})
	mux.HandleFunc("/api/v1/providers/{id}/test", s.handleProviderTest)
	return mux
}
