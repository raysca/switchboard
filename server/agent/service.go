// Package agent contains persistent agent product behavior. Framework runtime
// construction is intentionally left to the server runtime layer.
package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"govoice/server/storage"
)

const maxReadinessReasons = 8

var ErrNotFound = sql.ErrNoRows

type ReadinessEvaluator func(context.Context, string, json.RawMessage) (bool, error)

type Service struct {
	store     *storage.Store
	evaluator ReadinessEvaluator
}

func NewService(store *storage.Store, evaluators ...ReadinessEvaluator) *Service {
	s := &Service{store: store}
	if len(evaluators) > 0 {
		s.evaluator = evaluators[0]
	}
	return s
}

type CreateRequest struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Config      json.RawMessage `json:"config"`
	Enabled     *bool           `json:"enabled,omitempty"`
}

type UpdateRequest struct {
	Name        *string         `json:"name,omitempty"`
	Description *string         `json:"description,omitempty"`
	Config      json.RawMessage `json:"config,omitempty"`
	Enabled     *bool           `json:"enabled,omitempty"`
}

type Version struct {
	ID         string          `json:"id"`
	Number     int             `json:"number"`
	Schema     int             `json:"schema_version"`
	Config     json.RawMessage `json:"config"`
	ConfigHash string          `json:"config_hash"`
	CreatedAt  time.Time       `json:"created_at"`
}

type Readiness struct {
	State   string   `json:"state"`
	Reasons []string `json:"reasons"`
}

type Agent struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Enabled     bool      `json:"enabled"`
	Archived    bool      `json:"archived"`
	Version     *Version  `json:"current_version,omitempty"`
	Readiness   Readiness `json:"readiness"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type Snapshot struct {
	AgentID    string  `json:"agent_id"`
	Version    Version `json:"version"`
	ConfigHash string  `json:"config_hash"`
}

type SessionPin struct {
	ID             string
	AgentID        string
	AgentVersionID string
	ConfigHash     string
}

// V1Pipeline is the import shape used by the existing config-driven server.
type V1Pipeline struct {
	STT         string `json:"stt"`
	LLM         string `json:"llm"`
	TTS         string `json:"tts"`
	MaxMessages int    `json:"max_messages,omitempty"`
	MaxTokens   int    `json:"max_tokens,omitempty"`
}

func (s *Service) Create(ctx context.Context, req CreateRequest) (Agent, error) {
	name := strings.TrimSpace(req.Name)
	if err := validateDetails(name, req.Description); err != nil {
		return Agent{}, err
	}
	config, err := normalizeConfig(req.Config)
	if err != nil {
		return Agent{}, err
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	row := storage.Agent{ID: storage.NewID(), Name: name, Description: req.Description, Enabled: enabled}
	if _, err := s.store.Agents().CreateWithVersion(ctx, row, config); err != nil {
		return Agent{}, err
	}
	return s.Get(ctx, row.ID)
}

func (s *Service) Get(ctx context.Context, id string) (Agent, error) {
	row, err := s.store.Agents().Current(ctx, id)
	if err != nil {
		return Agent{}, err
	}
	return s.fromRow(ctx, row)
}

func (s *Service) List(ctx context.Context, limit, offset int) ([]Agent, error) {
	rows, err := s.store.Agents().List(ctx, limit, offset)
	if err != nil {
		return nil, err
	}
	out := make([]Agent, 0, len(rows))
	for _, row := range rows {
		a, err := s.fromRow(ctx, row)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

func (s *Service) Update(ctx context.Context, id string, req UpdateRequest) (Agent, error) {
	var config json.RawMessage
	var err error
	if len(req.Config) > 0 {
		config, err = normalizeConfig(req.Config)
		if err != nil {
			return Agent{}, err
		}
	}
	if _, _, err := s.store.Agents().MutateAndPublish(ctx, id, storage.AgentMutation{
		Name: req.Name, Description: req.Description, Enabled: req.Enabled,
	}, config); err != nil {
		return Agent{}, err
	}
	return s.Get(ctx, id)
}

func (s *Service) Archive(ctx context.Context, id string) error {
	return s.store.Agents().Archive(ctx, id)
}

func (s *Service) Duplicate(ctx context.Context, id, name string) (Agent, error) {
	source, err := s.Get(ctx, id)
	if err != nil {
		return Agent{}, err
	}
	if source.Version == nil {
		return Agent{}, errors.New("agent has no current version")
	}
	if strings.TrimSpace(name) == "" {
		name = source.Name + " copy"
	}
	return s.Create(ctx, CreateRequest{Name: name, Description: source.Description, Config: source.Version.Config, Enabled: boolPointer(source.Enabled)})
}

func (s *Service) GetVersion(ctx context.Context, agentID, versionID string) (Version, error) {
	v, err := s.store.Agents().Version(ctx, versionID)
	if err != nil {
		return Version{}, err
	}
	if v.AgentID != agentID {
		return Version{}, ErrNotFound
	}
	return toVersion(v), nil
}

func (s *Service) ResolveCurrent(ctx context.Context, id string) (Snapshot, error) {
	row, err := s.store.Agents().Current(ctx, id)
	if err != nil {
		return Snapshot{}, err
	}
	if row.Archived || !row.Enabled || row.CurrentVersionID == "" {
		return Snapshot{}, errors.New("agent unavailable")
	}
	v, err := s.store.Agents().Version(ctx, row.CurrentVersionID)
	if err != nil {
		return Snapshot{}, err
	}
	version := toVersion(v)
	readiness := s.agentReadiness(ctx, row.ID, version.Config, row.Enabled, row.Archived)
	if readiness.State == ReadinessNotReady {
		return Snapshot{}, errors.New("agent not ready")
	}
	return Snapshot{AgentID: row.ID, Version: version, ConfigHash: v.ConfigHash}, nil
}

// StartSession resolves and persists one immutable version reference. A later
// agent update only moves the current pointer; this session remains pinned.
func (s *Service) StartSession(ctx context.Context, id, sessionID string) (SessionPin, error) {
	snapshot, err := s.ResolveCurrent(ctx, id)
	if err != nil {
		return SessionPin{}, err
	}
	if sessionID == "" {
		sessionID = storage.NewID()
	}
	if err := s.store.Sessions().Create(ctx, storage.SessionRecord{
		ID: sessionID, AgentID: snapshot.AgentID, AgentVersionID: snapshot.Version.ID,
		ConfigHash: snapshot.ConfigHash, StartedAt: time.Now().UTC(), Status: "active",
	}); err != nil {
		return SessionPin{}, err
	}
	return SessionPin{ID: sessionID, AgentID: snapshot.AgentID, AgentVersionID: snapshot.Version.ID, ConfigHash: snapshot.ConfigHash}, nil
}

func (s *Service) ResolveSession(ctx context.Context, sessionID string) (Snapshot, error) {
	session, err := s.store.Sessions().Get(ctx, sessionID)
	if err != nil {
		return Snapshot{}, err
	}
	v, err := s.store.Agents().Version(ctx, session.AgentVersionID)
	if err != nil {
		return Snapshot{}, err
	}
	if v.AgentID != session.AgentID || v.ConfigHash != session.ConfigHash {
		return Snapshot{}, errors.New("session agent snapshot is inconsistent")
	}
	return Snapshot{AgentID: session.AgentID, Version: toVersion(v), ConfigHash: session.ConfigHash}, nil
}

func (s *Service) ImportV1(ctx context.Context, legacyName, name, description string, pipeline V1Pipeline) (Agent, error) {
	if strings.TrimSpace(legacyName) == "" || len(legacyName) > 128 {
		return Agent{}, errors.New("invalid V1 pipeline name")
	}
	config, err := json.Marshal(pipeline)
	if err != nil {
		return Agent{}, errors.New("invalid V1 pipeline configuration")
	}
	return s.Create(ctx, CreateRequest{Name: name, Description: description, Config: config})
}

func (s *Service) fromRow(ctx context.Context, row storage.Agent) (Agent, error) {
	a := Agent{ID: row.ID, Name: row.Name, Description: row.Description, Enabled: row.Enabled, Archived: row.Archived, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
	if row.CurrentVersionID != "" {
		v, err := s.store.Agents().Version(ctx, row.CurrentVersionID)
		if err != nil {
			return Agent{}, err
		}
		converted := toVersion(v)
		a.Version = &converted
		a.Readiness = s.agentReadiness(ctx, row.ID, converted.Config, row.Enabled, row.Archived)
	} else {
		a.Readiness = Readiness{State: ReadinessNotReady, Reasons: []string{"missing_configuration"}}
	}
	return a, nil
}

const (
	ReadinessReady      = "ready"
	ReadinessConfigured = "configured"
	ReadinessNotReady   = "not_ready"
)

func (s *Service) agentReadiness(ctx context.Context, agentID string, config json.RawMessage, enabled, archived bool) Readiness {
	var settings map[string]json.RawMessage
	if json.Unmarshal(config, &settings) != nil {
		return Readiness{State: ReadinessNotReady, Reasons: []string{"invalid_configuration"}}
	}
	reasons := make([]string, 0, maxReadinessReasons)
	for _, field := range []string{"stt", "llm", "tts"} {
		var value string
		if json.Unmarshal(settings[field], &value) != nil || strings.TrimSpace(value) == "" {
			reasons = append(reasons, "missing_"+field)
		}
	}
	if archived {
		reasons = append(reasons, "archived")
	} else if !enabled {
		reasons = append(reasons, "disabled")
	}
	if len(reasons) > maxReadinessReasons {
		reasons = reasons[:maxReadinessReasons]
	}
	if len(reasons) > 0 {
		return Readiness{State: ReadinessNotReady, Reasons: reasons}
	}
	if s.evaluator == nil {
		// Configuration presence is not provider or MCP capability validation.
		return Readiness{State: ReadinessConfigured, Reasons: []string{}}
	}
	ready, err := s.evaluator(ctx, agentID, append(json.RawMessage(nil), config...))
	if err != nil {
		return Readiness{State: ReadinessNotReady, Reasons: []string{"readiness_unavailable"}}
	}
	if !ready {
		return Readiness{State: ReadinessNotReady, Reasons: []string{"external_dependency_unavailable"}}
	}
	return Readiness{State: ReadinessReady, Reasons: []string{}}
}

func validateDetails(name, description string) error {
	if name == "" || len(name) > 128 || len(description) > 1024 {
		return errors.New("agent name must be 1 to 128 characters and description at most 1024 characters")
	}
	return nil
}

func normalizeConfig(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, errors.New("agent config is required")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, errors.New("agent config must be a JSON object")
	}
	canonical, err := storage.CanonicalJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("normalize agent config: %w", err)
	}
	return canonical, nil
}

func toVersion(v storage.AgentVersion) Version {
	return Version{ID: v.ID, Number: v.Version, Schema: v.SchemaVersion, Config: append(json.RawMessage(nil), v.ConfigJSON...), ConfigHash: v.ConfigHash, CreatedAt: v.CreatedAt}
}

func boolPointer(value bool) *bool { return &value }
