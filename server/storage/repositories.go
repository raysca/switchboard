package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
)

type Agent struct {
	ID, Name, Description, CurrentVersionID string
	Enabled, Archived                       bool
	CreatedAt, UpdatedAt                    time.Time
}
type AgentVersion struct {
	ID, AgentID            string
	Version, SchemaVersion int
	ConfigJSON             []byte
	ConfigHash             string
	CreatedAt              time.Time
	MCPReviewRequired      bool
	MCPTools               []SelectedTool
}
type AgentRepository struct{ s *Store }
type SelectedTool struct {
	ServerID, RemoteName, SchemaHash string
	Required                         bool
}

func (r AgentRepository) SelectedTools(ctx context.Context, versionID string) ([]SelectedTool, error) {
	rows, err := r.s.db.QueryContext(ctx, r.s.bind("SELECT mcp_server_id,remote_name,schema_hash,required FROM agent_mcp_tools WHERE agent_version_id=? ORDER BY mcp_server_id,remote_name"), versionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SelectedTool
	for rows.Next() {
		var tool SelectedTool
		if err := rows.Scan(&tool.ServerID, &tool.RemoteName, &tool.SchemaHash, &tool.Required); err != nil {
			return nil, err
		}
		out = append(out, tool)
	}
	return out, rows.Err()
}

func (s *Store) Agents() AgentRepository { return AgentRepository{s} }
func (r AgentRepository) Create(ctx context.Context, a Agent) error {
	if err := validID(a.ID); err != nil {
		return err
	}
	now := utc(a.CreatedAt)
	_, err := r.s.db.ExecContext(ctx, r.s.bind("INSERT INTO agents(id,name,description,enabled,archived,created_at,updated_at) VALUES(?,?,?,?,?,?,?)"), a.ID, a.Name, a.Description, boolValue(r.s.dialect, a.Enabled), boolValue(r.s.dialect, a.Archived), now, now)
	return err
}
func (r AgentRepository) AddVersion(ctx context.Context, agentID string, version int, config []byte) (AgentVersion, error) {
	return r.PublishVersion(ctx, agentID, version, config, nil)
}
func (r AgentRepository) PublishVersion(ctx context.Context, agentID string, version int, config []byte, selections []SelectedTool) (AgentVersion, error) {
	var out AgentVersion
	if err := validID(agentID); err != nil {
		return out, err
	}
	if version < 1 {
		return out, errors.New("version must be positive")
	}
	hash, canonical, err := hashJSON(config)
	if err != nil {
		return out, err
	}
	out = AgentVersion{ID: NewID(), AgentID: agentID, Version: version, SchemaVersion: 1, ConfigJSON: canonical, ConfigHash: hash, CreatedAt: time.Now().UTC()}
	err = r.s.WithTx(ctx, func(tx *sql.Tx) error {
		// Serialize version allocation per agent. Version numbers are contiguous
		// and never change once published, including under concurrent writers.
		lockAgent := "SELECT archived FROM agents WHERE id=?"
		if r.s.dialect == Postgres {
			lockAgent += " FOR UPDATE"
		}
		var archived bool
		if err := tx.QueryRowContext(ctx, r.s.bind(lockAgent), agentID).Scan(&archived); err != nil {
			return err
		}
		if archived {
			return errors.New("agent unavailable")
		}
		var latest int
		if err := tx.QueryRowContext(ctx, r.s.bind("SELECT COALESCE(MAX(version),0) FROM agent_versions WHERE agent_id=?"), agentID).Scan(&latest); err != nil {
			return err
		}
		if version != latest+1 {
			return errors.New("agent version must increment monotonically")
		}
		// The same transaction validates every selected schema and publishes the version.
		// SQLite begins IMMEDIATE; PostgreSQL row locks block refresh or delete races.
		for _, sel := range selections {
			if err := validID(sel.ServerID); err != nil {
				return err
			}
			query := "SELECT latest_discovery_id FROM mcp_servers WHERE id=? AND enabled=?"
			if r.s.dialect == Postgres {
				query += " FOR SHARE"
			}
			var discovery sql.NullString
			if err := tx.QueryRowContext(ctx, r.s.bind(query), sel.ServerID, boolValue(r.s.dialect, true)).Scan(&discovery); err != nil {
				return err
			}
			if !discovery.Valid {
				return errors.New("MCP server has no discovery")
			}
			var actual string
			if err := tx.QueryRowContext(ctx, r.s.bind("SELECT schema_hash FROM mcp_tools WHERE discovery_id=? AND remote_name=?"), discovery.String, sel.RemoteName).Scan(&actual); err != nil {
				return err
			}
			if actual != sel.SchemaHash {
				return errors.New("MCP schema hash changed")
			}
		}
		_, err := tx.ExecContext(ctx, r.s.bind("INSERT INTO agent_versions(id,agent_id,version,schema_version,config_json,config_hash,created_at) VALUES(?,?,?,?,?,?,?)"), out.ID, agentID, version, out.SchemaVersion, string(canonical), hash, utc(out.CreatedAt))
		if err != nil {
			return err
		}
		for _, sel := range selections {
			if _, err := tx.ExecContext(ctx, r.s.bind("INSERT INTO agent_mcp_tools(agent_version_id,mcp_server_id,remote_name,schema_hash,required) VALUES(?,?,?,?,?)"), out.ID, sel.ServerID, sel.RemoteName, sel.SchemaHash, boolValue(r.s.dialect, sel.Required)); err != nil {
				return err
			}
		}
		result, err := tx.ExecContext(ctx, r.s.bind("UPDATE agents SET current_version_id=?,updated_at=? WHERE id=? AND archived=?"), out.ID, utc(time.Now()), agentID, boolValue(r.s.dialect, false))
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return errors.New("agent unavailable")
		}
		return nil
	})
	return out, err
}
func (r AgentRepository) Current(ctx context.Context, id string) (Agent, error) {
	var a Agent
	var created, updated string
	var current sql.NullString
	err := r.s.db.QueryRowContext(ctx, r.s.bind("SELECT id,name,description,enabled,archived,current_version_id,created_at,updated_at FROM agents WHERE id=?"), id).Scan(&a.ID, &a.Name, &a.Description, &a.Enabled, &a.Archived, &current, &created, &updated)
	if err != nil {
		return a, err
	}
	a.CurrentVersionID = current.String
	if a.CreatedAt, err = parseTime(created); err != nil {
		return a, err
	}
	if a.UpdatedAt, err = parseTime(updated); err != nil {
		return a, err
	}
	return a, nil
}

// List returns a stable ID-ordered page of agents, including archived ones.
func (r AgentRepository) List(ctx context.Context, limit, offset int) ([]Agent, error) {
	if limit < 1 || limit > 100 || offset < 0 {
		return nil, errors.New("invalid agent pagination")
	}
	rows, err := r.s.db.QueryContext(ctx, r.s.bind("SELECT id,name,description,enabled,archived,current_version_id,created_at,updated_at FROM agents ORDER BY id LIMIT ? OFFSET ?"), limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var agents []Agent
	for rows.Next() {
		var a Agent
		var current sql.NullString
		var created, updated string
		if err := rows.Scan(&a.ID, &a.Name, &a.Description, &a.Enabled, &a.Archived, &current, &created, &updated); err != nil {
			return nil, err
		}
		a.CurrentVersionID = current.String
		if a.CreatedAt, err = parseTime(created); err != nil {
			return nil, err
		}
		if a.UpdatedAt, err = parseTime(updated); err != nil {
			return nil, err
		}
		agents = append(agents, a)
	}
	return agents, rows.Err()
}

// UpdateDetails changes mutable agent metadata without rewriting any version.
func (r AgentRepository) UpdateDetails(ctx context.Context, id, name, description string, enabled *bool) error {
	if strings.TrimSpace(name) == "" || len(name) > 128 || len(description) > 1024 {
		return errors.New("invalid agent details")
	}
	query := "UPDATE agents SET name=?,description=?,updated_at=? WHERE id=? AND archived=?"
	args := []any{strings.TrimSpace(name), description, utc(time.Now()), id, boolValue(r.s.dialect, false)}
	if enabled != nil {
		query = "UPDATE agents SET name=?,description=?,enabled=?,updated_at=? WHERE id=? AND archived=?"
		args = []any{strings.TrimSpace(name), description, boolValue(r.s.dialect, *enabled), utc(time.Now()), id, boolValue(r.s.dialect, false)}
	}
	result, err := r.s.db.ExecContext(ctx, r.s.bind(query), args...)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
func (r AgentRepository) Version(ctx context.Context, id string) (AgentVersion, error) {
	var v AgentVersion
	var created string
	err := r.s.db.QueryRowContext(ctx, r.s.bind("SELECT id,agent_id,version,schema_version,config_json,config_hash,created_at,mcp_review_required FROM agent_versions WHERE id=?"), id).Scan(&v.ID, &v.AgentID, &v.Version, &v.SchemaVersion, &v.ConfigJSON, &v.ConfigHash, &created, &v.MCPReviewRequired)
	if err != nil {
		return v, err
	}
	if v.CreatedAt, err = parseTime(created); err != nil {
		return v, err
	}
	v.MCPTools, err = r.SelectedTools(ctx, id)
	if err != nil {
		return v, err
	}
	return v, nil
}
func (r AgentRepository) Archive(ctx context.Context, id string) error {
	result, err := r.s.db.ExecContext(ctx, r.s.bind("UPDATE agents SET archived=?,enabled=?,updated_at=? WHERE id=?"), boolValue(r.s.dialect, true), boolValue(r.s.dialect, false), utc(time.Now()), id)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

type Credential struct {
	ID, Provider, MaskedSuffix, ValidationState, CapabilitiesJSON, KeyID string
	ValidatedAt, CreatedAt, UpdatedAt                                    time.Time
	Ciphertext                                                           []byte
}
type CredentialRepository struct{ s *Store }

func (s *Store) Credentials() CredentialRepository { return CredentialRepository{s} }
func (r CredentialRepository) Put(ctx context.Context, c Credential) error {
	if err := validID(c.ID); err != nil {
		return err
	}
	if c.ID == "" || len(c.Ciphertext) == 0 || c.KeyID == "" {
		return errors.New("credential id, ciphertext and key id required")
	}
	cap := c.CapabilitiesJSON
	if cap == "" {
		cap = "{}"
	}
	canonical, err := CanonicalJSON([]byte(cap))
	if err != nil {
		return err
	}
	now := utc(time.Now())
	_, err = r.s.db.ExecContext(ctx, r.s.bind("INSERT INTO provider_credentials(id,provider,masked_suffix,validation_state,validated_at,capabilities_json,ciphertext,key_id,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)"), c.ID, c.Provider, c.MaskedSuffix, c.ValidationState, nullTime(c.ValidatedAt), string(canonical), c.Ciphertext, c.KeyID, now, now)
	return err
}
func (r CredentialRepository) Get(ctx context.Context, id string) (Credential, error) {
	var c Credential
	var validated sql.NullString
	var created, updated string
	err := r.s.db.QueryRowContext(ctx, r.s.bind("SELECT id,provider,masked_suffix,validation_state,validated_at,capabilities_json,ciphertext,key_id,created_at,updated_at FROM provider_credentials WHERE id=?"), id).Scan(&c.ID, &c.Provider, &c.MaskedSuffix, &c.ValidationState, &validated, &c.CapabilitiesJSON, &c.Ciphertext, &c.KeyID, &created, &updated)
	if err != nil {
		return c, err
	}
	if validated.Valid {
		if c.ValidatedAt, err = parseTime(validated.String); err != nil {
			return c, err
		}
	}
	if c.CreatedAt, err = parseTime(created); err != nil {
		return c, err
	}
	if c.UpdatedAt, err = parseTime(updated); err != nil {
		return c, err
	}
	return c, nil
}

// ReplaceIfUnchanged prevents validation or key rotation from overwriting a
// credential that was concurrently replaced or deleted.
func (r CredentialRepository) ReplaceIfUnchanged(ctx context.Context, expectedKeyID string, expectedCiphertext []byte, c Credential) error {
	if err := validID(c.ID); err != nil {
		return err
	}
	if expectedKeyID == "" || len(expectedCiphertext) == 0 || len(c.Ciphertext) == 0 || c.KeyID == "" {
		return errors.New("credential ciphertext and key id required")
	}
	canonical, err := CanonicalJSON([]byte(c.CapabilitiesJSON))
	if err != nil {
		return err
	}
	result, err := r.s.db.ExecContext(ctx, r.s.bind("UPDATE provider_credentials SET provider=?,masked_suffix=?,validation_state=?,validated_at=?,capabilities_json=?,ciphertext=?,key_id=?,updated_at=? WHERE id=? AND key_id=? AND ciphertext=?"), c.Provider, c.MaskedSuffix, c.ValidationState, nullTime(c.ValidatedAt), string(canonical), c.Ciphertext, c.KeyID, utc(time.Now()), c.ID, expectedKeyID, expectedCiphertext)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// RotateIfUnchanged changes only encryption material and the masked suffix.
// It deliberately preserves validation and capability metadata written after
// the caller read its rotation snapshot.
func (r CredentialRepository) RotateIfUnchanged(ctx context.Context, id, expectedKeyID string, expectedCiphertext []byte, keyID string, ciphertext []byte, maskedSuffix string) error {
	if err := validID(id); err != nil {
		return err
	}
	if expectedKeyID == "" || len(expectedCiphertext) == 0 || keyID == "" || len(ciphertext) == 0 {
		return errors.New("credential ciphertext and key id required")
	}
	result, err := r.s.db.ExecContext(ctx, r.s.bind("UPDATE provider_credentials SET key_id=?,ciphertext=?,masked_suffix=?,updated_at=? WHERE id=? AND key_id=? AND ciphertext=?"), keyID, ciphertext, maskedSuffix, utc(time.Now()), id, expectedKeyID, expectedCiphertext)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ErrCredentialInUse is returned when deleting a credential that an active
// agent is attached to.
var ErrCredentialInUse = errors.New("credential is attached to an agent")

// CredentialAgentReference names an active agent attached to a credential.
type CredentialAgentReference struct {
	AgentID   string `json:"agent_id"`
	AgentName string `json:"agent_name"`
}

// CredentialInUseError carries the agents that block a credential deletion.
type CredentialInUseError struct{ Agents []CredentialAgentReference }

func (e *CredentialInUseError) Error() string { return ErrCredentialInUse.Error() }
func (e *CredentialInUseError) Unwrap() error { return ErrCredentialInUse }

// Delete removes a credential unless the current version of a non-archived
// agent is attached to it. The check and the delete share one transaction.
func (r CredentialRepository) Delete(ctx context.Context, id string) error {
	if err := validID(id); err != nil {
		return err
	}
	return r.s.WithTx(ctx, func(tx *sql.Tx) error {
		// Agent configs are canonical compact JSON, so an attachment is always
		// "credential_id":"<id>"; ids are UUIDs and contain no LIKE wildcards.
		rows, err := tx.QueryContext(ctx, r.s.bind(`SELECT a.id, a.name FROM agents a JOIN agent_versions v ON v.id = a.current_version_id
WHERE a.archived = ? AND v.config_json LIKE ? ORDER BY a.name, a.id`), boolValue(r.s.dialect, false), `%"credential_id":"`+id+`"%`)
		if err != nil {
			return err
		}
		var refs []CredentialAgentReference
		for rows.Next() {
			var ref CredentialAgentReference
			if err := rows.Scan(&ref.AgentID, &ref.AgentName); err != nil {
				rows.Close()
				return err
			}
			refs = append(refs, ref)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if len(refs) > 0 {
			return &CredentialInUseError{Agents: refs}
		}
		result, err := tx.ExecContext(ctx, r.s.bind("DELETE FROM provider_credentials WHERE id=?"), id)
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return sql.ErrNoRows
		}
		return nil
	})
}

func (r CredentialRepository) List(ctx context.Context) ([]Credential, error) {
	rows, err := r.s.db.QueryContext(ctx, "SELECT id,provider,masked_suffix,validation_state,validated_at,capabilities_json,ciphertext,key_id,created_at,updated_at FROM provider_credentials ORDER BY created_at,id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var credentials []Credential
	for rows.Next() {
		var c Credential
		var validated sql.NullString
		var created, updated string
		if err := rows.Scan(&c.ID, &c.Provider, &c.MaskedSuffix, &c.ValidationState, &validated, &c.CapabilitiesJSON, &c.Ciphertext, &c.KeyID, &created, &updated); err != nil {
			return nil, err
		}
		if validated.Valid {
			if c.ValidatedAt, err = parseTime(validated.String); err != nil {
				return nil, err
			}
		}
		if c.CreatedAt, err = parseTime(created); err != nil {
			return nil, err
		}
		if c.UpdatedAt, err = parseTime(updated); err != nil {
			return nil, err
		}
		credentials = append(credentials, c)
	}
	return credentials, rows.Err()
}

type MCPServer struct {
	ID, Name, Endpoint, Transport, AuthCredentialID, ValidationState, LatestDiscoveryID string
	Description                                                                         string
	Enabled, PendingDelete                                                              bool
	LastConnectedAt, LastDiscoveredAt, CreatedAt, UpdatedAt                             time.Time
}
type MCPTool struct {
	RemoteName  string
	Description string
	SchemaJSON  []byte
	SchemaHash  string
}
type MCPDiscovery struct {
	ID, ServerID, DiscoveryHash string
	Tools                       []MCPTool
	CreatedAt                   time.Time
}
type MCPRepository struct{ s *Store }

func (s *Store) MCP() MCPRepository { return MCPRepository{s} }
func (r MCPRepository) CreateServer(ctx context.Context, m MCPServer) error {
	if err := validID(m.ID); err != nil {
		return err
	}
	if err := optionalID(m.AuthCredentialID); err != nil {
		return err
	}
	now := utc(time.Now())
	_, err := r.s.db.ExecContext(ctx, r.s.bind("INSERT INTO mcp_servers(id,name,endpoint,transport,auth_credential_id,enabled,validation_state,created_at,updated_at,description) VALUES(?,?,?,?,?,?,?,?,?,?)"), m.ID, m.Name, m.Endpoint, m.Transport, nullable(m.AuthCredentialID), boolValue(r.s.dialect, m.Enabled), m.ValidationState, now, now, m.Description)
	return err
}

func (r MCPRepository) Server(ctx context.Context, id string) (MCPServer, error) {
	var m MCPServer
	var auth, latest, connected, discovered sql.NullString
	var enabled bool
	var created, updated string
	err := r.s.db.QueryRowContext(ctx, r.s.bind("SELECT id,name,description,endpoint,transport,auth_credential_id,enabled,validation_state,latest_discovery_id,last_connected_at,last_discovered_at,created_at,updated_at,pending_delete FROM mcp_servers WHERE id=?"), id).Scan(&m.ID, &m.Name, &m.Description, &m.Endpoint, &m.Transport, &auth, &enabled, &m.ValidationState, &latest, &connected, &discovered, &created, &updated, &m.PendingDelete)
	if err != nil {
		return m, err
	}
	m.AuthCredentialID, m.LatestDiscoveryID, m.Enabled = auth.String, latest.String, enabled
	if m.CreatedAt, err = parseTime(created); err != nil {
		return m, err
	}
	if m.UpdatedAt, err = parseTime(updated); err != nil {
		return m, err
	}
	if connected.Valid {
		if m.LastConnectedAt, err = parseTime(connected.String); err != nil {
			return m, err
		}
	}
	if discovered.Valid {
		if m.LastDiscoveredAt, err = parseTime(discovered.String); err != nil {
			return m, err
		}
	}
	return m, nil
}

func (r MCPRepository) ListServers(ctx context.Context) ([]MCPServer, error) {
	rows, err := r.s.db.QueryContext(ctx, "SELECT id,name,description,endpoint,transport,auth_credential_id,enabled,validation_state,latest_discovery_id,last_connected_at,last_discovered_at,created_at,updated_at,pending_delete FROM mcp_servers ORDER BY name,id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MCPServer
	for rows.Next() {
		var m MCPServer
		var auth, latest, connected, discovered sql.NullString
		var enabled bool
		var created, updated string
		if err := rows.Scan(&m.ID, &m.Name, &m.Description, &m.Endpoint, &m.Transport, &auth, &enabled, &m.ValidationState, &latest, &connected, &discovered, &created, &updated, &m.PendingDelete); err != nil {
			return nil, err
		}
		m.AuthCredentialID, m.LatestDiscoveryID, m.Enabled = auth.String, latest.String, enabled
		if m.CreatedAt, err = parseTime(created); err != nil {
			return nil, err
		}
		if m.UpdatedAt, err = parseTime(updated); err != nil {
			return nil, err
		}
		if connected.Valid {
			if m.LastConnectedAt, err = parseTime(connected.String); err != nil {
				return nil, err
			}
		}
		if discovered.Valid {
			if m.LastDiscoveredAt, err = parseTime(discovered.String); err != nil {
				return nil, err
			}
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (r MCPRepository) LatestDiscovery(ctx context.Context, serverID string) (MCPDiscovery, error) {
	var id sql.NullString
	err := r.s.db.QueryRowContext(ctx, r.s.bind("SELECT latest_discovery_id FROM mcp_servers WHERE id=?"), serverID).Scan(&id)
	if err != nil {
		return MCPDiscovery{}, err
	}
	if !id.Valid {
		return MCPDiscovery{}, sql.ErrNoRows
	}
	return r.Discovery(ctx, id.String)
}

func (r MCPRepository) SetValidation(ctx context.Context, id, state string, connected, discovered time.Time) error {
	result, err := r.s.db.ExecContext(ctx, r.s.bind("UPDATE mcp_servers SET validation_state=?,last_connected_at=COALESCE(?,last_connected_at),last_discovered_at=COALESCE(?,last_discovered_at),updated_at=? WHERE id=?"), state, nullTime(connected), nullTime(discovered), utc(time.Now()), id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return sql.ErrNoRows
	}
	return nil
}

var ErrMCPReferenced = errors.New("MCP server is referenced by a current agent or active session")

type MCPAgentReference struct {
	AgentID, AgentName, AgentVersionID, RemoteName string
	Required                                       bool
}

func (r MCPRepository) CurrentReferences(ctx context.Context, id string) ([]MCPAgentReference, error) {
	rows, err := r.s.db.QueryContext(ctx, r.s.bind("SELECT a.id,a.name,a.current_version_id,t.remote_name,t.required FROM agents a JOIN agent_mcp_tools t ON t.agent_version_id=a.current_version_id WHERE t.mcp_server_id=? ORDER BY a.name,a.id,t.remote_name"), id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MCPAgentReference
	for rows.Next() {
		var ref MCPAgentReference
		if err := rows.Scan(&ref.AgentID, &ref.AgentName, &ref.AgentVersionID, &ref.RemoteName, &ref.Required); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

func (r MCPRepository) DisableServer(ctx context.Context, id string) error {
	result, err := r.s.db.ExecContext(ctx, r.s.bind("UPDATE mcp_servers SET enabled=?,validation_state=?,updated_at=? WHERE id=?"), boolValue(r.s.dialect, false), "disabled", utc(time.Now()), id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func (r MCPRepository) EnableServer(ctx context.Context, id string) error {
	result, err := r.s.db.ExecContext(ctx, r.s.bind("UPDATE mcp_servers SET enabled=?,validation_state=CASE WHEN validation_state='disabled' THEN 'not_validated' ELSE validation_state END,updated_at=? WHERE id=? AND pending_delete=?"), boolValue(r.s.dialect, true), utc(time.Now()), id, boolValue(r.s.dialect, false))
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func (r MCPRepository) DeleteServer(ctx context.Context, id string) error {
	return r.s.WithTx(ctx, func(tx *sql.Tx) error {
		if r.s.dialect == Postgres {
			var locked string
			if err := tx.QueryRowContext(ctx, r.s.bind("SELECT id FROM mcp_servers WHERE id=? FOR UPDATE"), id).Scan(&locked); err != nil {
				return err
			}
		}
		current, active, err := mcpReferenceCounts(ctx, tx, r.s, id)
		if err != nil {
			return err
		}
		if current != 0 || active != 0 {
			return ErrMCPReferenced
		}
		result, err := tx.ExecContext(ctx, r.s.bind("DELETE FROM mcp_servers WHERE id=?"), id)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return sql.ErrNoRows
		}
		return nil
	})
}

// DetachAndDelete creates a new immutable version for each current agent that
// removes this server's selections. Historical versions and sessions are kept.
// The live server remains disabled and pending deletion while old active
// sessions still depend on it.
func (r MCPRepository) DetachAndDelete(ctx context.Context, id string) error {
	if err := validID(id); err != nil {
		return err
	}
	return r.s.WithTx(ctx, func(tx *sql.Tx) error {
		// Match publication/session-start lock order: agent rows first, then the
		// MCP server row. This makes detach serialize with new assignments without
		// holding the server lock while waiting on an agent.
		rows, err := tx.QueryContext(ctx, r.s.bind("SELECT DISTINCT a.id FROM agents a JOIN agent_mcp_tools t ON t.agent_version_id=a.current_version_id WHERE t.mcp_server_id=? ORDER BY a.id"), id)
		if err != nil {
			return err
		}
		var agentIDs []string
		for rows.Next() {
			var agentID string
			if err := rows.Scan(&agentID); err != nil {
				rows.Close()
				return err
			}
			agentIDs = append(agentIDs, agentID)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if r.s.dialect == Postgres {
			for _, agentID := range agentIDs {
				var locked string
				if err := tx.QueryRowContext(ctx, r.s.bind("SELECT id FROM agents WHERE id=? FOR UPDATE"), agentID).Scan(&locked); err != nil {
					return err
				}
			}
		}
		var serverID string
		lock := "SELECT id FROM mcp_servers WHERE id=?"
		if r.s.dialect == Postgres {
			lock += " FOR UPDATE"
		}
		if err := tx.QueryRowContext(ctx, r.s.bind(lock), id).Scan(&serverID); err != nil {
			return err
		}
		rows, err = tx.QueryContext(ctx, r.s.bind("SELECT a.id,a.current_version_id FROM agents a JOIN agent_mcp_tools t ON t.agent_version_id=a.current_version_id WHERE t.mcp_server_id=? ORDER BY a.id"), id)
		if err != nil {
			return err
		}
		type currentAgent struct{ id, versionID string }
		var agents []currentAgent
		for rows.Next() {
			var a currentAgent
			if err := rows.Scan(&a.id, &a.versionID); err != nil {
				rows.Close()
				return err
			}
			agents = append(agents, a)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		for _, a := range agents {
			query := "SELECT config_json,config_hash FROM agent_versions WHERE id=?"
			var config, hash string
			if err := tx.QueryRowContext(ctx, r.s.bind(query), a.versionID).Scan(&config, &hash); err != nil {
				return err
			}
			var latest int
			if err := tx.QueryRowContext(ctx, r.s.bind("SELECT COALESCE(MAX(version),0) FROM agent_versions WHERE agent_id=?"), a.id).Scan(&latest); err != nil {
				return err
			}
			versionID := NewID()
			now := utc(time.Now())
			if _, err := tx.ExecContext(ctx, r.s.bind("INSERT INTO agent_versions(id,agent_id,version,schema_version,config_json,config_hash,created_at,mcp_review_required) VALUES(?,?,?,?,?,?,?,?)"), versionID, a.id, latest+1, 1, config, hash, now, boolValue(r.s.dialect, true)); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, r.s.bind("INSERT INTO agent_mcp_tools(agent_version_id,mcp_server_id,remote_name,schema_hash,required) SELECT ?,mcp_server_id,remote_name,schema_hash,required FROM agent_mcp_tools WHERE agent_version_id=? AND mcp_server_id<>?"), versionID, a.versionID, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, r.s.bind("UPDATE agents SET current_version_id=?,updated_at=? WHERE id=? AND current_version_id=?"), versionID, now, a.id, a.versionID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, r.s.bind("UPDATE mcp_servers SET enabled=?,validation_state='disabled',pending_delete=?,updated_at=? WHERE id=?"), boolValue(r.s.dialect, false), boolValue(r.s.dialect, true), utc(time.Now()), id); err != nil {
			return err
		}
		_, active, err := mcpReferenceCounts(ctx, tx, r.s, id)
		if err != nil {
			return err
		}
		if active != 0 {
			return nil
		}
		_, err = tx.ExecContext(ctx, r.s.bind("DELETE FROM mcp_servers WHERE id=?"), id)
		return err
	})
}

func (r MCPRepository) FinalizePendingDeletes(ctx context.Context) error {
	if r.s.finalizePendingDeletesHook != nil {
		if err := r.s.finalizePendingDeletesHook(ctx); err != nil {
			return err
		}
	}
	query := "SELECT id FROM mcp_servers WHERE pending_delete=TRUE ORDER BY id"
	if r.s.dialect == SQLite {
		query = "SELECT id FROM mcp_servers WHERE pending_delete=1 ORDER BY id"
	}
	rows, err := r.s.db.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, id := range ids {
		current, active, err := countMCPReferences(ctx, r.s, id)
		if err != nil {
			return err
		}
		if current == 0 && active == 0 {
			if err := r.DeleteServer(ctx, id); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
	}
	return nil
}

func countMCPReferences(ctx context.Context, s *Store, id string) (int, int, error) {
	var current, active int
	err := s.db.QueryRowContext(ctx, s.bind("SELECT count(*) FROM agents a JOIN agent_mcp_tools t ON t.agent_version_id=a.current_version_id WHERE t.mcp_server_id=?"), id).Scan(&current)
	if err != nil {
		return 0, 0, err
	}
	err = s.db.QueryRowContext(ctx, s.bind("SELECT count(*) FROM sessions s JOIN session_mcp_tools t ON t.session_id=s.id WHERE t.mcp_server_id=? AND s.status IN ('active','draining')"), id).Scan(&active)
	return current, active, err
}
func mcpReferenceCounts(ctx context.Context, tx *sql.Tx, s *Store, id string) (int, int, error) {
	var current, active int
	err := tx.QueryRowContext(ctx, s.bind("SELECT count(*) FROM agents a JOIN agent_mcp_tools t ON t.agent_version_id=a.current_version_id WHERE t.mcp_server_id=?"), id).Scan(&current)
	if err != nil {
		return 0, 0, err
	}
	err = tx.QueryRowContext(ctx, s.bind("SELECT count(*) FROM sessions s JOIN session_mcp_tools t ON t.session_id=s.id WHERE t.mcp_server_id=? AND s.status IN ('active','draining')"), id).Scan(&active)
	return current, active, err
}

func (r MCPRepository) Schema(ctx context.Context, serverID, name, schemaHash string) ([]byte, error) {
	var schema string
	err := r.s.db.QueryRowContext(ctx, r.s.bind("SELECT t.schema_json FROM mcp_tools t JOIN mcp_discoveries d ON d.id=t.discovery_id WHERE d.server_id=? AND t.remote_name=? AND t.schema_hash=? ORDER BY d.created_at DESC LIMIT 1"), serverID, name, schemaHash).Scan(&schema)
	return []byte(schema), err
}
func (r MCPRepository) AddDiscovery(ctx context.Context, serverID string, tools []MCPTool) (MCPDiscovery, error) {
	var out MCPDiscovery
	if err := validID(serverID); err != nil {
		return out, err
	}
	sorted := append([]MCPTool(nil), tools...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].RemoteName < sorted[j].RemoteName })
	for i := range sorted {
		h, c, err := hashJSON(sorted[i].SchemaJSON)
		if err != nil {
			return out, err
		}
		sorted[i].SchemaHash = h
		sorted[i].SchemaJSON = c
		if i > 0 && sorted[i-1].RemoteName == sorted[i].RemoteName {
			return out, errors.New("duplicate MCP tool name")
		}
	}
	// The ordered name/schema-hash pairs are the immutable discovery identity.
	pairs := make([][2]string, len(sorted))
	for i, t := range sorted {
		pairs[i] = [2]string{t.RemoteName, t.SchemaHash}
	}
	raw, err := jsonMarshal(pairs)
	if err != nil {
		return out, err
	}
	hash, _, err := hashJSON(raw)
	if err != nil {
		return out, err
	}
	out = MCPDiscovery{ID: NewID(), ServerID: serverID, DiscoveryHash: hash, Tools: sorted, CreatedAt: time.Now().UTC()}
	err = r.s.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, r.s.bind("INSERT INTO mcp_discoveries(id,server_id,discovery_hash,created_at) VALUES(?,?,?,?)"), out.ID, serverID, hash, utc(out.CreatedAt))
		if err != nil {
			return err
		}
		for _, t := range sorted {
			if _, err := tx.ExecContext(ctx, r.s.bind("INSERT INTO mcp_tools(discovery_id,remote_name,schema_json,schema_hash,description) VALUES(?,?,?,?,?)"), out.ID, t.RemoteName, string(t.SchemaJSON), t.SchemaHash, t.Description); err != nil {
				return err
			}
		}
		result, err := tx.ExecContext(ctx, r.s.bind("UPDATE mcp_servers SET latest_discovery_id=?,updated_at=? WHERE id=?"), out.ID, utc(time.Now()), serverID)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return sql.ErrNoRows
		}
		return nil
	})
	return out, err
}
func (r MCPRepository) Discovery(ctx context.Context, id string) (MCPDiscovery, error) {
	var d MCPDiscovery
	var created string
	err := r.s.db.QueryRowContext(ctx, r.s.bind("SELECT id,server_id,discovery_hash,created_at FROM mcp_discoveries WHERE id=?"), id).Scan(&d.ID, &d.ServerID, &d.DiscoveryHash, &created)
	if err != nil {
		return d, err
	}
	if d.CreatedAt, err = parseTime(created); err != nil {
		return d, err
	}
	rows, err := r.s.db.QueryContext(ctx, r.s.bind("SELECT remote_name,description,schema_json,schema_hash FROM mcp_tools WHERE discovery_id=? ORDER BY remote_name"), id)
	if err != nil {
		return d, err
	}
	defer rows.Close()
	for rows.Next() {
		var t MCPTool
		if err := rows.Scan(&t.RemoteName, &t.Description, &t.SchemaJSON, &t.SchemaHash); err != nil {
			return d, err
		}
		d.Tools = append(d.Tools, t)
	}
	return d, rows.Err()
}
func (r MCPRepository) SelectTool(ctx context.Context, versionID, serverID, name, hash string, required bool) error {
	return errors.New("selected tools must be published with an agent version")
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }
