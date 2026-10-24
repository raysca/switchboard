// Package agent contains persistent agent product behavior. Framework runtime
// construction is intentionally left to the server runtime layer.
package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"govoice/server/storage"
)

const maxReadinessReasons = 8

var ErrNotFound = sql.ErrNoRows

type Service struct {
	store *storage.Store
	av    Availability
	// tokenSet reports whether an MCP token variable has a value; nil counts
	// every variable as set.
	tokenSet func(name string) bool
	// secretSet reports whether an HTTP tool's secret name resolves; nil counts as set.
	secretSet func(name string) bool
}

// SetTokenEnv makes readiness report MCP servers whose token variable is unset.
func (s *Service) SetTokenEnv(isSet func(name string) bool) { s.tokenSet = isSet }

// SetSecretCheck makes readiness report HTTP tools whose secret name resolves to nothing; nil counts as resolved.
func (s *Service) SetSecretCheck(isSet func(name string) bool) { s.secretSet = isSet }

func NewService(store *storage.Store, av Availability) *Service {
	return &Service{store: store, av: av}
}

type CreateRequest struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Config      json.RawMessage `json:"config"`
	Enabled     *bool           `json:"enabled,omitempty"`
	MCPTools    []ToolSelection `json:"mcp_tools,omitempty"`

	fromStored bool // a copy of a stored version (Duplicate)
}

type UpdateRequest struct {
	Name        *string         `json:"name,omitempty"`
	Description *string         `json:"description,omitempty"`
	Config      json.RawMessage `json:"config,omitempty"`
	Enabled     *bool           `json:"enabled,omitempty"`
	// MCPTools replaces the agent's MCP tool selection in the same version as Config; nil keeps
	// the current selection, an empty list clears it. It must be sent with Config.
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
	AgentID    string            `json:"agent_id"`
	Version    Version           `json:"version"`
	ConfigHash string            `json:"config_hash"`
	MCPTools   []MCPToolSnapshot `json:"mcp_tools"`
	// Draft marks a snapshot of an unpublished config (ResolveDraft): Version is the version it is
	// based on, with the draft's config and hash.
	Draft bool `json:"draft,omitempty"`
}

type SessionPin struct {
	ID             string
	AgentID        string
	AgentVersionID string
	ConfigHash     string
}

func (s *Service) Create(ctx context.Context, req CreateRequest) (Agent, error) {
	return s.create(ctx, req, false)
}

func (s *Service) create(ctx context.Context, req CreateRequest, mcpReviewRequired bool) (Agent, error) {
	name := strings.TrimSpace(req.Name)
	if err := validateDetails(name, req.Description); err != nil {
		return Agent{}, err
	}
	config, err := s.attachedConfig(ctx, req.Config, req.fromStored)
	if err != nil {
		return Agent{}, err
	}
	g, err := Decode(config)
	if err != nil {
		return Agent{}, err
	}
	if err := checkStepTools(g, req.MCPTools); err != nil {
		return Agent{}, err
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	row := storage.Agent{ID: storage.NewID(), Name: name, Description: req.Description, Enabled: enabled}
	selected, err := toStorageSelections(req.MCPTools)
	if err != nil {
		return Agent{}, err
	}
	if _, err := s.store.Agents().CreateWithVersionAndToolsAndReviewRequired(ctx, row, config, selected, mcpReviewRequired); err != nil {
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
		config, err = s.attachedConfig(ctx, req.Config, false)
		if err != nil {
			return Agent{}, err
		}
	}
	mutation := storage.AgentMutation{Name: req.Name, Description: req.Description, Enabled: req.Enabled}
	if req.MCPTools != nil {
		selected, err := toStorageSelections(*req.MCPTools)
		if err != nil {
			return Agent{}, err
		}
		mutation.Tools = &selected
		if len(config) == 0 {
			return Agent{}, fmt.Errorf("%w: mcp_tools must be sent with config", ErrInvalidConfig)
		}
	}
	if len(config) > 0 {
		if err := s.checkConfigTools(ctx, id, config, req.MCPTools); err != nil {
			return Agent{}, err
		}
	}
	if _, _, err := s.store.Agents().MutateAndPublish(ctx, id, mutation, config); err != nil {
		return Agent{}, err
	}
	return s.Get(ctx, id)
}

// checkConfigTools verifies an agent's step tools against the tools the publish will select (selected,
// when given) or else the tools the agent's current version has (a publish copies them forward).
func (s *Service) checkConfigTools(ctx context.Context, id string, config json.RawMessage, selected *[]ToolSelection) error {
	g, err := Decode(config)
	if err != nil {
		return err
	}
	if selected != nil {
		return checkStepTools(g, *selected)
	}
	row, err := s.store.Agents().Current(ctx, id)
	if err != nil {
		return err
	}
	var current []ToolSelection
	if row.CurrentVersionID != "" {
		rows, err := s.store.Agents().SelectedTools(ctx, row.CurrentVersionID)
		if err != nil {
			return err
		}
		current = fromStorageSelections(rows)
	}
	return checkStepTools(g, current)
}

// VersionByNumber returns an earlier published version (config and tool selection) so a client can show it or
// publish it again with Update. Nothing is written: restoring is a normal publish of the old content.
func (s *Service) VersionByNumber(ctx context.Context, id string, number int) (Version, error) {
	v, err := s.store.Agents().VersionByNumber(ctx, id, number)
	if err != nil {
		return Version{}, err
	}
	return toVersion(v), nil
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
	return s.create(ctx, CreateRequest{Name: name, Description: source.Description, Config: withoutDeadStepSettings(source.Version.Config), Enabled: boolPointer(source.Enabled), MCPTools: source.Version.MCPTools, fromStored: true}, source.Version.MCPReviewRequired)
}

// ResolveDraft validates config (and selection, when given) exactly as a publish of agent id would and returns what a call on it
// runs: the agent's current version with the draft's config and hash, and the agent's attached MCP
// tools. Nothing is written. A draft cannot change tool selections, only use the attached ones.
func (s *Service) ResolveDraft(ctx context.Context, id string, config json.RawMessage, selection *[]ToolSelection) (Snapshot, Readiness, error) {
	row, err := s.store.Agents().Current(ctx, id)
	if err != nil {
		return Snapshot{}, Readiness{}, err
	}
	if row.Archived || !row.Enabled || row.CurrentVersionID == "" {
		return Snapshot{}, Readiness{}, fmt.Errorf("%w: agent unavailable", sql.ErrNoRows)
	}
	canonical, err := s.attachedConfig(ctx, config, false)
	if err != nil {
		if !errors.Is(err, ErrNotAttached) {
			err = fmt.Errorf("%w: %w", ErrInvalidConfig, err)
		}
		return Snapshot{}, Readiness{}, err
	}
	if err := s.checkConfigTools(ctx, id, canonical, selection); err != nil {
		return Snapshot{}, Readiness{}, err
	}
	base, err := s.store.Agents().Version(ctx, row.CurrentVersionID)
	if err != nil {
		return Snapshot{}, Readiness{}, err
	}
	hash, canonical, err := storage.ConfigHash(canonical)
	if err != nil {
		return Snapshot{}, Readiness{}, err
	}
	draft := base
	draft.ConfigJSON, draft.ConfigHash = canonical, hash
	if selection != nil { // the draft's own selection, validated like a publish's, replaces the version's
		selected, err := toStorageSelections(*selection)
		if err != nil {
			return Snapshot{}, Readiness{}, err
		}
		if err := s.store.Agents().ValidateSelections(ctx, selected); err != nil {
			return Snapshot{}, Readiness{}, err
		}
		draft.MCPTools = selected
		draft.MCPReviewRequired = false // choosing the tools again is the review, as in a publish
	}
	readiness, tools := s.agentReadiness(ctx, draft, row.Enabled, row.Archived)
	return Snapshot{AgentID: row.ID, Version: toVersion(draft), ConfigHash: hash, MCPTools: tools, Draft: true}, readiness, nil
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
	readiness, tools := s.agentReadiness(ctx, v, row.Enabled, row.Archived)
	if readiness.State == ReadinessNotReady || readiness.State == ReadinessNeedsToolReview || readiness.State == ReadinessToolUnavailable {
		return Snapshot{}, errors.New("agent not ready")
	}
	return Snapshot{AgentID: row.ID, Version: version, ConfigHash: v.ConfigHash, MCPTools: tools}, nil
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
	if len(snapshot.Version.MCPTools) > 0 {
		return SessionPin{}, errors.New("agent MCP tools require runtime session capture")
	}
	return s.StartCapturedSession(ctx, snapshot, sessionID, "", "", nil)
}

// StartCapturedSession persists a session. caller is the label the integration chose for it (the voice
// token's principal); empty is allowed.
func (s *Service) StartCapturedSession(ctx context.Context, snapshot Snapshot, sessionID, runtimeJSON, caller string, references []ToolSelection) (SessionPin, error) {
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
		ID: sessionID, AgentID: snapshot.AgentID, AgentVersionID: snapshot.Version.ID,
		ConfigHash: snapshot.ConfigHash, StartedAt: time.Now().UTC(), Status: "active", RuntimeSnapshotJSON: runtimeJSON, RuntimeTools: runtimeTools, Draft: snapshot.Draft, Caller: caller,
	}); err != nil {
		return SessionPin{}, err
	}
	return SessionPin{ID: sessionID, AgentID: snapshot.AgentID, AgentVersionID: snapshot.Version.ID, ConfigHash: snapshot.ConfigHash}, nil
}

// EndSession ends a session, recording why (one of storage.EndReasons).
func (s *Service) EndSession(ctx context.Context, sessionID, reason string) error {
	return s.store.Sessions().End(ctx, sessionID, reason)
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
	version := toVersion(v)
	var runtimeTools []MCPToolSnapshot
	if strings.TrimSpace(session.RuntimeSnapshotJSON) != "" && strings.TrimSpace(session.RuntimeSnapshotJSON) != "{}" {
		if err := json.Unmarshal([]byte(session.RuntimeSnapshotJSON), &runtimeTools); err != nil {
			return Snapshot{}, errors.New("session runtime snapshot is invalid")
		}
	}
	if runtimeTools == nil {
		_, runtimeTools = s.agentReadiness(ctx, v, true, false)
	}
	return Snapshot{AgentID: session.AgentID, Version: version, ConfigHash: session.ConfigHash, MCPTools: runtimeTools}, nil
}

// SetMCPTools publishes a new immutable agent version. Existing versions and
// sessions keep their prior associations; every requested schema must match
// the server's latest discovery at publication time.
func (s *Service) SetMCPTools(ctx context.Context, id string, tools []ToolSelection) (Version, error) {
	row, err := s.store.Agents().Current(ctx, id)
	if err != nil {
		return Version{}, err
	}
	if row.Archived || row.CurrentVersionID == "" {
		return Version{}, errors.New("agent unavailable")
	}
	current, err := s.store.Agents().Version(ctx, row.CurrentVersionID)
	if err != nil {
		return Version{}, err
	}
	updated, err := s.Update(ctx, id, UpdateRequest{Config: current.ConfigJSON, MCPTools: &tools})
	if err != nil {
		return Version{}, err
	}
	return *updated.Version, nil
}

func (s *Service) fromRow(ctx context.Context, row storage.Agent) (Agent, error) {
	a := Agent{ID: row.ID, Name: row.Name, Description: row.Description, Enabled: row.Enabled, Archived: row.Archived, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
	if row.CurrentVersionID != "" {
		v, err := s.store.Agents().Version(ctx, row.CurrentVersionID)
		if err != nil {
			return Agent{}, err
		}
		converted := toVersion(v)
		a.Readiness, _ = s.agentReadiness(ctx, v, row.Enabled, row.Archived)
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

// agentReadiness evaluates an already-loaded version (with its MCPTools)
// against the current provider availability.
func (s *Service) agentReadiness(ctx context.Context, version storage.AgentVersion, enabled, archived bool) (Readiness, []MCPToolSnapshot) {
	if version.MCPReviewRequired {
		return Readiness{State: ReadinessNeedsToolReview, Reasons: []string{"mcp_tools_detached_review_required"}, ToolDependencies: []ToolDependency{}}, nil
	}
	parsed, parseErr := Decode(version.ConfigJSON)
	if parseErr != nil {
		return Readiness{State: ReadinessNotReady, Reasons: []string{"invalid_config"}}, nil
	}
	// A stored agent can stop being callable later, e.g. when the environment
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
	var missing []string
	// noteFailure records an unusable tool: a required tool whose pin is stale or gone needs a review, any other
	// required failure makes the agent unavailable, and an optional tool only warns.
	noteFailure := func(selection storage.SelectedTool, state string) {
		if !selection.Required {
			optionalWarning = true
		} else if state == "schema_drift" || state == "tool_missing" || state == "tool_edited" {
			requiredFailure = ReadinessNeedsToolReview
		} else if requiredFailure == "" {
			requiredFailure = ReadinessToolUnavailable
		}
	}
	for _, selection := range selections {
		if strings.HasPrefix(selection.ServerID, storage.HTTPToolRefPrefix) {
			state, currentHash, snap, secret := s.httpToolDependency(ctx, selection)
			if secret != "" {
				missing = append(missing, secret)
			}
			dependencies = append(dependencies, ToolDependency{ServerID: selection.ServerID, RemoteName: selection.RemoteName, Required: selection.Required, State: state, SelectedSchemaHash: selection.SchemaHash, CurrentSchemaHash: currentHash})
			if state != "ready" {
				noteFailure(selection, state)
				continue
			}
			snapshots = append(snapshots, snap)
			continue
		}
		state := "ready"
		currentHash := ""
		server, serverErr := s.store.MCP().Server(ctx, selection.ServerID)
		if serverErr != nil {
			state = "server_unavailable"
		} else if !server.Enabled {
			state = "server_disabled"
		} else if server.AuthEnv != "" && s.tokenSet != nil && !s.tokenSet(server.AuthEnv) {
			state = "auth_env_missing"
			missing = append(missing, server.AuthEnv)
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
			noteFailure(selection, state)
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
	slices.Sort(missing)
	base := Readiness{State: ReadinessReady, Reasons: []string{}, ToolDependencies: dependencies, MissingEnv: slices.Compact(missing)}
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
		return errors.New("agent name must be 1 to 128 characters and description at most 1024 characters")
	}
	return nil
}

// ErrNotAttached is returned when an agent config is not attached to a usable
// set of providers.
var ErrNotAttached = errors.New("agent must be attached to providers")

// attachedConfig validates that raw names a provider and an available
// available provider for every stage, then returns its canonical form.
// stored: raw is a copy of a stored version (Duplicate), so a handoff sound the server dropped is not refused.
func (s *Service) attachedConfig(ctx context.Context, raw json.RawMessage, stored bool) (json.RawMessage, error) {
	canonical, err := normalizeConfig(raw)
	if err != nil {
		return nil, err
	}
	g, err := Decode(canonical)
	if err != nil {
		return nil, err
	}
	if err := g.deadStepSettings(); err != nil {
		return nil, err
	}
	if p, ok := policyOf(s.av); ok {
		for _, pp := range g.policyProblems(p) {
			if !(stored && pp.storedOK) {
				return nil, invalid("%s", pp.message)
			}
		}
	}
	problems, _ := g.Problems(s.av)
	if len(problems) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrNotAttached, strings.Join(problems, ", "))
	}
	return canonical, nil
}

func normalizeConfig(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, errors.New("agent config is required")
	}
	if _, err := Decode(raw); err != nil {
		return nil, err
	}
	canonical, err := storage.CanonicalJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("normalize agent config: %w", err)
	}
	return canonical, nil
}

func toVersion(v storage.AgentVersion) Version {
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
			return nil, fmt.Errorf("%w: invalid MCP tool selection", storage.ErrInvalidToolSelection)
		}
		seen[key] = true
		selected = append(selected, storage.SelectedTool{ServerID: tool.ServerID, RemoteName: tool.RemoteName, SchemaHash: tool.SchemaHash, Required: tool.Required})
	}
	return selected, nil
}

func boolPointer(value bool) *bool { return &value }

// httpToolDependency evaluates a pinned HTTP tool. The state is ready, tool_missing (deleted, or the pinned
// definition is gone), tool_disabled, secret_missing (the secret name resolves to nothing; its name is returned) or
// tool_edited (the tool has a newer definition than the one pinned, which must be reviewed again). The snapshot is
// built from the pinned definition.
func (s *Service) httpToolDependency(ctx context.Context, sel storage.SelectedTool) (state, currentHash string, snap MCPToolSnapshot, missingSecret string) {
	id := strings.TrimPrefix(sel.ServerID, storage.HTTPToolRefPrefix)
	tool, err := s.store.HTTPTools().Get(ctx, id)
	if err != nil {
		return "tool_missing", "", snap, ""
	}
	currentHash = tool.CurrentHash
	raw, err := s.store.HTTPTools().Definition(ctx, id, sel.SchemaHash)
	if err != nil {
		return "tool_missing", currentHash, snap, ""
	}
	var def struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		URL         string          `json:"url"`
		InputSchema json.RawMessage `json:"input_schema"`
		Auth        *struct {
			Secret string `json:"secret"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(raw, &def); err != nil || def.Name != sel.RemoteName {
		return "tool_missing", currentHash, snap, ""
	}
	if !tool.Enabled {
		return "tool_disabled", currentHash, snap, ""
	}
	secret := ""
	if def.Auth != nil {
		secret = def.Auth.Secret
		if s.secretSet != nil && !s.secretSet(secret) {
			return "secret_missing", currentHash, snap, secret
		}
	}
	if currentHash != sel.SchemaHash {
		return "tool_edited", currentHash, snap, ""
	}
	return "ready", currentHash, MCPToolSnapshot{ServerID: sel.ServerID, ServerName: def.Name, Endpoint: def.URL, Transport: "http", AuthEnv: secret, RemoteName: sel.RemoteName, Description: def.Description, SchemaJSON: def.InputSchema, SchemaHash: sel.SchemaHash, Required: sel.Required}, ""
}
