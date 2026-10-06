package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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
		return errors.New("storage.dialect is required: calls run stored agents")
	}
	dsnEnv := envOr(s.cfg.Storage.DSNEnv, "GOVOICE_DATABASE_DSN")
	keyEnv := envOr(s.cfg.Credentials.KeyringEnv, "GOVOICE_CREDENTIAL_KEYS")
	dsn, rawKeyring := os.Getenv(dsnEnv), os.Getenv(keyEnv)
	if s.cfg.Profile == ProfileDevelopment && (dsn == "" || rawKeyring == "") {
		var err error
		if dsn, rawKeyring, err = s.developmentStore(dsn, rawKeyring); err != nil {
			return err
		}
	}
	if dsn == "" {
		return fmt.Errorf("storage enabled but %s is empty", dsnEnv)
	}
	keyring, err := parseKeyring(rawKeyring)
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
	creds, err := credential.NewService(store.Credentials(), keyring, registry)
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
	s.credentials, s.mcpService, s.providerDefs = creds, mcpService, registry
	s.SetProviderManagementAPI(&api.ProviderManagement{Credentials: creds, Registry: registry})
	s.agents = agent.NewService(store)
	s.SetAgentManagementAPI(api.NewAgentsHandler(s.agents))
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

// developmentStore fills in a local SQLite DSN and a persisted credential key
// for the development profile, so a fresh checkout runs without setup. The
// directory defaults to .govoice-dev (override with GOVOICE_DEV_DIR). Production
// never reaches this path and fails closed without explicit secrets.
func (s *Server) developmentStore(dsn, rawKeyring string) (string, string, error) {
	dir := os.Getenv("GOVOICE_DEV_DIR")
	if dir == "" {
		dir = ".govoice-dev"
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", fmt.Errorf("development store: %w", err)
	}
	if dsn == "" {
		dsn = filepath.Join(dir, "govoice.db")
	}
	if rawKeyring == "" {
		key, err := loadOrCreateDevKey(filepath.Join(dir, "credential.key"))
		if err != nil {
			return "", "", fmt.Errorf("development credential key: %w", err)
		}
		rawKeyring = key
	}
	s.logger.Warn("development profile: using a local agent store and credential key; never use these in production", slog.String("dir", dir))
	return dsn, rawKeyring, nil
}

// loadOrCreateDevKey returns the keyring entry stored at path, creating a new
// random 32-byte key (mode 0600) on first use so stored credentials stay
// decryptable across restarts.
func loadOrCreateDevKey(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil {
		return strings.TrimSpace(string(b)), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", err
	}
	entry := "dev:" + base64.StdEncoding.EncodeToString(key)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) { // created concurrently: use that one
		return loadOrCreateDevKey(path)
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(entry + "\n"); err != nil {
		return "", err
	}
	return entry, nil
}
