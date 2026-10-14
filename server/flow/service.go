// Package flow contains persistent flow product behavior. Framework runtime
// construction is intentionally left to the server runtime layer.
package flow

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

type Service struct {
	store *storage.Store
	av    Availability
}

func NewService(store *storage.Store, av Availability) *Service {
	return &Service{store: store, av: av}
}

type CreateRequest struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Config      json.RawMessage `json:"config"`
	Enabled     *bool           `json:"enabled,omitempty"`
	MCPTools    []ToolSelection `json:"mcp_tools,omitempty"`
}

type UpdateRequest struct {
	Name        *string         `json:"name,omitempty"`
	Description *string         `json:"description,omitempty"`
	Config      json.RawMessage `json:"config,omitempty"`
	Enabled     *bool           `json:"enabled,omitempty"`
	// MCPTools replaces the flow's MCP tool selection in the same version as Config; nil keeps
	// the current selection, an empty list clears it. Without Config, the current config is republished.
	MCPTools *[]ToolSelection `json:"mcp_tools,omitempty"`
}

type Version struct {
	ID                string          `json:"id"`
	Number            int             `json:"number"`
	Schema            int             `json:"schema_version"`
	Config            json.RawMessage `json:"config"`
	ConfigHash        string          `json:"config_hash"`
	CreatedAt         time.Time       `json:"created_at"`
	MCPTools          []ToolSelection `json:"mcp_tools"`
	MCPReviewRequired bool            `json:"mcp_review_required,omitempty"`
}

type ToolSelection struct {
	ServerID   string `json:"mcp_server_id"`
	RemoteName string `json:"remote_name"`
	SchemaHash string `json:"schema_hash"`
	Required   bool   `json:"required"`
}

type MCPToolSnapshot struct {
	ServerID    string          `json:"mcp_server_id"`
	ServerName  string          `json:"server_name"`
	Endpoint    string          `json:"endpoint"`
	Transport   string          `json:"transport"`
	AuthEnv     string          `json:"auth_env,omitempty"`
	RemoteName  string          `json:"remote_name"`
	Description string          `json:"description"`
	SchemaJSON  json.RawMessage `json:"input_schema"`
	SchemaHash  string          `json:"schema_hash"`
	Required    bool            `json:"required"`
}

type ToolDependency struct {
	ServerID           string `json:"mcp_server_id"`
	RemoteName         string `json:"remote_name"`
	Required           bool   `json:"required"`
	State              string `json:"state"`
	SelectedSchemaHash string `json:"selected_schema_hash"`
	CurrentSchemaHash  string `json:"current_schema_hash,omitempty"`
}

type Readiness struct {
	State            string           `json:"state"`
	Reasons          []string         `json:"reasons"`
	MissingEnv       []string         `json:"missing_env,omitempty"`
	ToolDependencies []ToolDependency `json:"tool_dependencies,omitempty"`
}

type Flow struct {
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
	FlowID     string            `json:"flow_id"`
	Version    Version           `json:"version"`
	ConfigHash string            `json:"config_hash"`
	MCPTools   []MCPToolSnapshot `json:"mcp_tools"`
	// Draft marks a snapshot of an unpublished config (ResolveDraft): Version is the version it is
	// based on, with the draft's config and hash.
	Draft bool `json:"draft,omitempty"`
}

type SessionPin struct {
	ID            string
	FlowID        string
	FlowVersionID string
	ConfigHash    string
}

func (s *Service) Create(ctx context.Context, req CreateRequest) (Flow, error) {
	return s.create(ctx, req, false)
}

func (s *Service) create(ctx context.Context, req CreateRequest, mcpReviewRequired bool) (Flow, error) {
	name := strings.TrimSpace(req.Name)
	if err := validateDetails(name, req.Description); err != nil {
		return Flow{}, err
	}
	config, err := s.attachedConfig(ctx, req.Config)
	if err != nil {
		return Flow{}, err
	}
	g, err := Decode(config)
	if err != nil {
		return Flow{}, err
	}
	if err := checkStepTools(g, req.MCPTools); err != nil {
		return Flow{}, err
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	row := storage.Flow{ID: storage.NewID(), Name: name, Description: req.Description, Enabled: enabled}
	selected, err := toStorageSelections(req.MCPTools)
	if err != nil {
		return Flow{}, err
	}
	if _, err := s.store.Flows().CreateWithVersionAndToolsAndReviewRequired(ctx, row, config, selected, mcpReviewRequired); err != nil {
		return Flow{}, err
	}
	return s.Get(ctx, row.ID)
}

func (s *Service) Get(ctx context.Context, id string) (Flow, error) {
	row, err := s.store.Flows().Current(ctx, id)
	if err != nil {
		return Flow{}, err
	}
	return s.fromRow(ctx, row)
}

func (s *Service) List(ctx context.Context, limit, offset int) ([]Flow, error) {
	rows, err := s.store.Flows().List(ctx, limit, offset)
	if err != nil {
		return nil, err
	}
	out := make([]Flow, 0, len(rows))
	for _, row := range rows {
		a, err := s.fromRow(ctx, row)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

func (s *Service) Update(ctx context.Context, id string, req UpdateRequest) (Flow, error) {
	var config json.RawMessage
	var err error
	if len(req.Config) > 0 {
		config, err = s.attachedConfig(ctx, req.Config)
		if err != nil {
			return Flow{}, err
		}
	}
	mutation := storage.FlowMutation{Name: req.Name, Description: req.Description, Enabled: req.Enabled}
	if req.MCPTools != nil {
		selected, err := toStorageSelections(*req.MCPTools)
		if err != nil {
			return Flow{}, err
		}
		mutation.Tools = &selected
		if len(config) == 0 { // a selection alone republishes the current config
			config, err = s.currentConfig(ctx, id)
			if err != nil {
				return Flow{}, err
			}
		}
	}
	if len(config) > 0 {
		if err := s.checkConfigTools(ctx, id, config, req.MCPTools); err != nil {
			return Flow{}, err
		}
	}
	if _, _, err := s.store.Flows().MutateAndPublish(ctx, id, mutation, config); err != nil {
		return Flow{}, err
	}
	return s.Get(ctx, id)
}

func (s *Service) currentConfig(ctx context.Context, id string) (json.RawMessage, error) {
	row, err := s.store.Flows().Current(ctx, id)
	if err != nil {
		return nil, err
	}
	if row.Archived || row.CurrentVersionID == "" {
		return nil, errors.New("flow unavailable")
	}
	v, err := s.store.Flows().Version(ctx, row.CurrentVersionID)
	if err != nil {
		return nil, err
	}
	return v.ConfigJSON, nil
}

// checkConfigTools verifies a flow's step tools against the tools the publish will select (selected,
// when given) or else the tools the flow's current version has (a publish copies them forward).
func (s *Service) checkConfigTools(ctx context.Context, id string, config json.RawMessage, selected *[]ToolSelection) error {
	g, err := Decode(config)
	if err != nil {
		return err
	}
	if selected != nil {
		return checkStepTools(g, *selected)
	}
	row, err := s.store.Flows().Current(ctx, id)
	if err != nil {
		return err
	}
	var current []ToolSelection
	if row.CurrentVersionID != "" {
		rows, err := s.store.Flows().SelectedTools(ctx, row.CurrentVersionID)
		if err != nil {
			return err
		}
		current = fromStorageSelections(rows)
	}
	return checkStepTools(g, current)
}

func (s *Service) Archive(ctx context.Context, id string) error {
	return s.store.Flows().Archive(ctx, id)
}

func (s *Service) Duplicate(ctx context.Context, id, name string) (Flow, error) {
	source, err := s.Get(ctx, id)
	if err != nil {
		return Flow{}, err
	}
	if source.Version == nil {
		return Flow{}, errors.New("flow has no current version")
	}
	if strings.TrimSpace(name) == "" {
		name = source.Name + " copy"
	}
	return s.create(ctx, CreateRequest{Name: name, Description: source.Description, Config: source.Version.Config, Enabled: boolPointer(source.Enabled), MCPTools: source.Version.MCPTools}, source.Version.MCPReviewRequired)
}

// ResolveDraft validates config exactly as a publish of flow id would and returns what a call on it
// runs: the flow's current version with the draft's config and hash, and the flow's attached MCP
// tools. Nothing is written. A draft cannot change tool selections, only use the attached ones.
func (s *Service) ResolveDraft(ctx context.Context, id string, config json.RawMessage) (Snapshot, Readiness, error) {
	row, err := s.store.Flows().Current(ctx, id)
	if err != nil {
		return Snapshot{}, Readiness{}, err
	}
	if row.Archived || !row.Enabled || row.CurrentVersionID == "" {
		return Snapshot{}, Readiness{}, fmt.Errorf("%w: flow unavailable", sql.ErrNoRows)
	}
	canonical, err := s.attachedConfig(ctx, config)
	if err != nil {
		if !errors.Is(err, ErrNotAttached) {
			err = fmt.Errorf("%w: %w", ErrInvalidConfig, err)
		}
		return Snapshot{}, Readiness{}, err
	}
	if err := s.checkConfigTools(ctx, id, canonical, nil); err != nil {
		return Snapshot{}, Readiness{}, err
	}
	base, err := s.store.Flows().Version(ctx, row.CurrentVersionID)
	if err != nil {
		return Snapshot{}, Readiness{}, err
	}
	hash, canonical, err := storage.ConfigHash(canonical)
	if err != nil {
		return Snapshot{}, Readiness{}, err
	}
	draft := base
	draft.ConfigJSON, draft.ConfigHash = canonical, hash
	readiness, tools := s.flowReadiness(ctx, draft, row.Enabled, row.Archived)
	return Snapshot{FlowID: row.ID, Version: toVersion(draft), ConfigHash: hash, MCPTools: tools, Draft: true}, readiness, nil
}

func (s *Service) ResolveCurrent(ctx context.Context, id string) (Snapshot, error) {
	row, err := s.store.Flows().Current(ctx, id)
	if err != nil {
		return Snapshot{}, err
	}
	if row.Archived || !row.Enabled || row.CurrentVersionID == "" {
		return Snapshot{}, errors.New("flow unavailable")
	}
	v, err := s.store.Flows().Version(ctx, row.CurrentVersionID)
	if err != nil {
		return Snapshot{}, err
	}
	version := toVersion(v)
	readiness, tools := s.flowReadiness(ctx, v, row.Enabled, row.Archived)
	if readiness.State == ReadinessNotReady || readiness.State == ReadinessNeedsToolReview || readiness.State == ReadinessToolUnavailable {
		return Snapshot{}, errors.New("flow not ready")
	}
	return Snapshot{FlowID: row.ID, Version: version, ConfigHash: v.ConfigHash, MCPTools: tools}, nil
}

// StartSession resolves and persists one immutable version reference. A later
// flow update only moves the current pointer; this session remains pinned.
func (s *Service) StartSession(ctx context.Context, id, sessionID string) (SessionPin, error) {
	snapshot, err := s.ResolveCurrent(ctx, id)
	if err != nil {
		return SessionPin{}, err
	}
	if sessionID == "" {
		sessionID = storage.NewID()
	}
	if len(snapshot.Version.MCPTools) > 0 {
		return SessionPin{}, errors.New("flow MCP tools require runtime session capture")
	}
	return s.StartCapturedSession(ctx, snapshot, sessionID, "", nil)
}

func (s *Service) StartCapturedSession(ctx context.Context, snapshot Snapshot, sessionID, runtimeJSON string, references []ToolSelection) (SessionPin, error) {
	if sessionID == "" {
		sessionID = storage.NewID()
	}
	if runtimeJSON == "" {
		raw, _ := json.Marshal(snapshot.MCPTools)
		runtimeJSON = string(raw)
	}
	runtimeTools := make([]storage.SelectedTool, 0, len(references))
	for _, tool := range references {
		runtimeTools = append(runtimeTools, storage.SelectedTool{ServerID: tool.ServerID, RemoteName: tool.RemoteName, SchemaHash: tool.SchemaHash, Required: tool.Required})
	}
	if err := s.store.Sessions().Create(ctx, storage.SessionRecord{
		ID: sessionID, FlowID: snapshot.FlowID, FlowVersionID: snapshot.Version.ID,
		ConfigHash: snapshot.ConfigHash, StartedAt: time.Now().UTC(), Status: "active", RuntimeSnapshotJSON: runtimeJSON, RuntimeTools: runtimeTools, Draft: snapshot.Draft,
	}); err != nil {
		return SessionPin{}, err
	}
	return SessionPin{ID: sessionID, FlowID: snapshot.FlowID, FlowVersionID: snapshot.Version.ID, ConfigHash: snapshot.ConfigHash}, nil
}

func (s *Service) EndSession(ctx context.Context, sessionID string) error {
	return s.store.Sessions().End(ctx, sessionID)
}

func (s *Service) ResolveSession(ctx context.Context, sessionID string) (Snapshot, error) {
	session, err := s.store.Sessions().Get(ctx, sessionID)
	if err != nil {
		return Snapshot{}, err
	}
	v, err := s.store.Flows().Version(ctx, session.FlowVersionID)
	if err != nil {
		return Snapshot{}, err
	}
	if v.FlowID != session.FlowID || v.ConfigHash != session.ConfigHash {
		return Snapshot{}, errors.New("session flow snapshot is inconsistent")
	}
	version := toVersion(v)
	var runtimeTools []MCPToolSnapshot
	if strings.TrimSpace(session.RuntimeSnapshotJSON) != "" && strings.TrimSpace(session.RuntimeSnapshotJSON) != "{}" {
		if err := json.Unmarshal([]byte(session.RuntimeSnapshotJSON), &runtimeTools); err != nil {
			return Snapshot{}, errors.New("session runtime snapshot is invalid")
		}
	}
	if runtimeTools == nil {
		_, runtimeTools = s.flowReadiness(ctx, v, true, false)
	}
	return Snapshot{FlowID: session.FlowID, Version: version, ConfigHash: session.ConfigHash, MCPTools: runtimeTools}, nil
}

// SetMCPTools publishes a new immutable flow version. Existing versions and
// sessions keep their prior associations; every requested schema must match
// the server's latest discovery at publication time.
func (s *Service) SetMCPTools(ctx context.Context, id string, tools []ToolSelection) (Version, error) {
	row, err := s.store.Flows().Current(ctx, id)
	if err != nil {
		return Version{}, err
	}
	if row.Archived || row.CurrentVersionID == "" {
		return Version{}, errors.New("flow unavailable")
	}
	current, err := s.store.Flows().Version(ctx, row.CurrentVersionID)
	if err != nil {
		return Version{}, err
	}
	selected, err := toStorageSelections(tools)
	if err != nil {
		return Version{}, err
	}
	g, err := Decode(current.ConfigJSON)
	if err != nil {
		return Version{}, err
	}
	if err := checkStepTools(g, tools); err != nil {
		return Version{}, err
	}
	version, err := s.store.Flows().PublishVersion(ctx, id, current.Version+1, current.ConfigJSON, selected)
	if err != nil {
		return Version{}, err
	}
	return toVersionWithTools(version, selected), nil
}

func (s *Service) fromRow(ctx context.Context, row storage.Flow) (Flow, error) {
	a := Flow{ID: row.ID, Name: row.Name, Description: row.Description, Enabled: row.Enabled, Archived: row.Archived, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
	if row.CurrentVersionID != "" {
		v, err := s.store.Flows().Version(ctx, row.CurrentVersionID)
		if err != nil {
			return Flow{}, err
		}
		converted := toVersion(v)
		a.Readiness, _ = s.flowReadiness(ctx, v, row.Enabled, row.Archived)
		a.Version = &converted
	} else {
		a.Readiness = Readiness{State: ReadinessNotReady, Reasons: []string{"missing_configuration"}}
	}
	return a, nil
}

const (
	ReadinessReady           = "ready"
	ReadinessNotReady        = "not_ready"
	ReadinessWithToolWarning = "ready_with_tool_warning"
	ReadinessNeedsToolReview = "needs_tool_review"
	ReadinessToolUnavailable = "tool_dependency_unavailable"
)

// flowReadiness evaluates an already-loaded version (with its MCPTools)
// against the current provider availability.
func (s *Service) flowReadiness(ctx context.Context, version storage.FlowVersion, enabled, archived bool) (Readiness, []MCPToolSnapshot) {
	if version.MCPReviewRequired {
		return Readiness{State: ReadinessNeedsToolReview, Reasons: []string{"mcp_tools_detached_review_required"}, ToolDependencies: []ToolDependency{}}, nil
	}
	parsed, parseErr := Decode(version.ConfigJSON)
	if parseErr != nil {
		return Readiness{State: ReadinessNotReady, Reasons: []string{"invalid_config"}}, nil
	}
	// A stored flow can stop being callable later, e.g. when the environment
	// no longer carries a provider's credentials.
	problems, missingEnv := parsed.Problems(s.av)
	reasons := append(make([]string, 0, maxReadinessReasons), problems...)
	if archived {
		reasons = append(reasons, "archived")
	} else if !enabled {
		reasons = append(reasons, "disabled")
	}
	if len(reasons) > maxReadinessReasons {
		reasons = reasons[:maxReadinessReasons]
	}
	if len(reasons) > 0 {
		return Readiness{State: ReadinessNotReady, Reasons: reasons, MissingEnv: missingEnv}, nil
	}
	selections := version.MCPTools
	dependencies := make([]ToolDependency, 0, len(selections))
	snapshots := make([]MCPToolSnapshot, 0, len(selections))
	requiredFailure := ""
	optionalWarning := false
	for _, selection := range selections {
		state := "ready"
		currentHash := ""
		server, serverErr := s.store.MCP().Server(ctx, selection.ServerID)
		if serverErr != nil {
			state = "server_unavailable"
		} else if !server.Enabled {
			state = "server_disabled"
		} else if server.ValidationState != "healthy" || server.LatestDiscoveryID == "" {
			state = "server_unavailable"
		} else {
			discovery, discoveryErr := s.store.MCP().LatestDiscovery(ctx, selection.ServerID)
			if discoveryErr != nil {
				state = "server_unavailable"
			} else {
				for _, tool := range discovery.Tools {
					if tool.RemoteName == selection.RemoteName {
						currentHash = tool.SchemaHash
						break
					}
				}
				if currentHash == "" {
					state = "tool_missing"
				} else if currentHash != selection.SchemaHash {
					state = "schema_drift"
				}
			}
		}
		dependencies = append(dependencies, ToolDependency{ServerID: selection.ServerID, RemoteName: selection.RemoteName, Required: selection.Required, State: state, SelectedSchemaHash: selection.SchemaHash, CurrentSchemaHash: currentHash})
		if state != "ready" {
			if selection.Required {
				if state == "schema_drift" || state == "tool_missing" {
					requiredFailure = ReadinessNeedsToolReview
				} else if requiredFailure == "" {
					requiredFailure = ReadinessToolUnavailable
				}
			} else {
				optionalWarning = true
			}
			continue
		}
		schema, schemaErr := s.store.MCP().Schema(ctx, selection.ServerID, selection.RemoteName, selection.SchemaHash)
		if schemaErr != nil {
			if selection.Required {
				requiredFailure = ReadinessNeedsToolReview
			} else {
				optionalWarning = true
			}
			continue
		}
		snapshots = append(snapshots, MCPToolSnapshot{ServerID: server.ID, ServerName: server.Name, Endpoint: server.Endpoint, Transport: server.Transport, AuthEnv: server.AuthEnv, RemoteName: selection.RemoteName, SchemaJSON: json.RawMessage(schema), SchemaHash: selection.SchemaHash, Required: selection.Required})
	}
	base := Readiness{State: ReadinessReady, Reasons: []string{}, ToolDependencies: dependencies}
	if requiredFailure != "" {
		base.State = requiredFailure
		base.Reasons = []string{"required_tool_unavailable"}
		return base, snapshots
	}
	if optionalWarning {
		base.State = ReadinessWithToolWarning
		base.Reasons = []string{"optional_tool_unavailable"}
	}
	return base, snapshots
}

func validateDetails(name, description string) error {
	if name == "" || len(name) > 128 || len(description) > 1024 {
		return errors.New("flow name must be 1 to 128 characters and description at most 1024 characters")
	}
	return nil
}

// ErrNotAttached is returned when a flow config is not attached to a usable
// set of providers.
var ErrNotAttached = errors.New("flow must be attached to providers")

// attachedConfig validates that raw names a provider and an available
// available provider for every stage, then returns its canonical form.
func (s *Service) attachedConfig(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	canonical, err := normalizeConfig(raw)
	if err != nil {
		return nil, err
	}
	g, err := Decode(canonical)
	if err != nil {
		return nil, err
	}
	problems, _ := g.Problems(s.av)
	if len(problems) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrNotAttached, strings.Join(problems, ", "))
	}
	return canonical, nil
}

func normalizeConfig(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, errors.New("flow config is required")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, errors.New("flow config must be a JSON object")
	}
	if _, ok := object["nodes"]; ok { // a pre-rename config (also a duplicated old version) is written with the new keys
		if err := upgradeKeys(object); err != nil {
			return nil, err
		}
		raw, _ = json.Marshal(object)
	}
	if _, err := Decode(raw); err != nil {
		return nil, err
	}
	canonical, err := storage.CanonicalJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("normalize flow config: %w", err)
	}
	return canonical, nil
}

func toVersion(v storage.FlowVersion) Version {
	tools := make([]ToolSelection, 0, len(v.MCPTools))
	for _, tool := range v.MCPTools {
		tools = append(tools, ToolSelection{ServerID: tool.ServerID, RemoteName: tool.RemoteName, SchemaHash: tool.SchemaHash, Required: tool.Required})
	}
	return Version{ID: v.ID, Number: v.Version, Schema: v.SchemaVersion, Config: append(json.RawMessage(nil), v.ConfigJSON...), ConfigHash: v.ConfigHash, CreatedAt: v.CreatedAt, MCPTools: tools, MCPReviewRequired: v.MCPReviewRequired}
}

func fromStorageSelections(selections []storage.SelectedTool) []ToolSelection {
	out := make([]ToolSelection, 0, len(selections))
	for _, selection := range selections {
		out = append(out, ToolSelection{ServerID: selection.ServerID, RemoteName: selection.RemoteName, SchemaHash: selection.SchemaHash, Required: selection.Required})
	}
	return out
}

func toStorageSelections(tools []ToolSelection) ([]storage.SelectedTool, error) {
	selected := make([]storage.SelectedTool, 0, len(tools))
	seen := map[string]bool{}
	for _, tool := range tools {
		key := tool.ServerID + "\x00" + tool.RemoteName
		if tool.ServerID == "" || tool.RemoteName == "" || tool.SchemaHash == "" || seen[key] {
			return nil, errors.New("invalid MCP tool selection")
		}
		seen[key] = true
		selected = append(selected, storage.SelectedTool{ServerID: tool.ServerID, RemoteName: tool.RemoteName, SchemaHash: tool.SchemaHash, Required: tool.Required})
	}
	return selected, nil
}

func toVersionWithTools(v storage.FlowVersion, selections []storage.SelectedTool) Version {
	version := toVersion(v)
	version.MCPTools = fromStorageSelections(selections)
	return version
}

func boolPointer(value bool) *bool { return &value }
