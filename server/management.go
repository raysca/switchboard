package server

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"govoice/server/agent"
	"govoice/server/api"
	"govoice/server/credential"
	"govoice/server/mcp"
	"govoice/server/provider"
	"govoice/server/retention"
	"govoice/server/storage"
	"govoice/server/trace"
)

// management owns the durable management plane so Shutdown can release it.
type management struct {
	store     *storage.Store
	traces    *trace.Adapter
	retention *retention.Job
}

// EnableManagement opens storage and installs the agent, provider, MCP and
// session APIs plus the retention job. It is a no-op when storage is not
// configured. Call it before Start.
func (s *Server) EnableManagement(ctx context.Context) error {
	if s.cfg.Storage.Dialect == "" {
		s.logger.Warn("storage not configured; management APIs disabled")
		return nil
	}
	dsnEnv := envOr(s.cfg.Storage.DSNEnv, "GOVOICE_DATABASE_DSN")
	dsn := os.Getenv(dsnEnv)
	if dsn == "" {
		return fmt.Errorf("storage enabled but %s is empty", dsnEnv)
	}
	keyring, err := parseKeyring(os.Getenv(envOr(s.cfg.Credentials.KeyringEnv, "GOVOICE_CREDENTIAL_KEYS")))
	if err != nil {
		return err
	}
	store, err := storage.Open(ctx, storage.Config{Dialect: storage.Dialect(s.cfg.Storage.Dialect), DSN: dsn})
	if err != nil {
		return fmt.Errorf("open storage: %w", err)
	}
	m := &management{store: store}
	fail := func(err error) error { m.close(); return err }

	registry := provider.NewRegistry(provider.Definitions())
	creds, err := credential.NewService(store.Credentials(), keyring, registry, nil)
	if err != nil {
		return fail(fmt.Errorf("credential service: %w", err))
	}
	m.traces = trace.NewAdapter(store.Traces())
	mcpService, err := mcp.NewService(mcp.ServiceConfig{Store: store, Remote: mcp.NewSDKClient(), Credentials: creds, Tracer: m.traces})
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

	s.SetStore(store)
	s.SetProviderManagementAPI(&api.ProviderManagement{Credentials: creds, Registry: registry})
	s.SetAgentManagementAPI(api.NewAgentsHandler(agent.NewService(store)))
	s.SetMCPManagementAPI(mcp.Handler(mcpService, s.management))
	s.SetSessionManagementAPI(api.NewTracesHandler(store.Sessions(), store.Traces()))

	if policy != nil {
		m.retention = retention.NewJob(store.Retention(), *policy, retention.WithInterval(s.cfg.Retention.Interval), retention.WithLogger(s.logger))
		m.retention.Start(context.Background())
	} else {
		s.logger.Warn("retention disabled: stored transcripts and traces are kept indefinitely")
	}
	s.mgmt = m
	s.logger.Info("management plane enabled", slog.String("dialect", s.cfg.Storage.Dialect), slog.Bool("retention", m.retention != nil))
	return nil
}

func (m *management) close() {
	if m == nil {
		return
	}
	if m.retention != nil {
		m.retention.Stop()
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

// parseKeyring reads "id:base64key[,id:base64key]"; the first entry is active.
func parseKeyring(raw string) (credential.Keyring, error) {
	kr := credential.Keyring{Keys: map[string][]byte{}}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		id, enc, ok := strings.Cut(entry, ":")
		key, err := base64.StdEncoding.DecodeString(enc)
		if !ok || id == "" || err != nil || len(key) != 32 {
			return kr, errors.New("credential keyring must be id:base64(32-byte key) entries")
		}
		if kr.ActiveID == "" {
			kr.ActiveID = id
		}
		kr.Keys[id] = key
	}
	if kr.ActiveID == "" {
		return kr, errors.New("storage enabled but credential keyring is empty")
	}
	return kr, nil
}

// closeManagement is called from Shutdown after calls have drained.
func (s *Server) closeManagement() {
	s.mgmt.close()
	s.mgmt = nil
}
