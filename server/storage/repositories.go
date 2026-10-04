package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
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
}
type AgentRepository struct{ s *Store }
type SelectedTool struct {
	ServerID, RemoteName, SchemaHash string
	Required                         bool
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
func (r AgentRepository) Version(ctx context.Context, id string) (AgentVersion, error) {
	var v AgentVersion
	var created string
	err := r.s.db.QueryRowContext(ctx, r.s.bind("SELECT id,agent_id,version,schema_version,config_json,config_hash,created_at FROM agent_versions WHERE id=?"), id).Scan(&v.ID, &v.AgentID, &v.Version, &v.SchemaVersion, &v.ConfigJSON, &v.ConfigHash, &created)
	if err != nil {
		return v, err
	}
	if v.CreatedAt, err = parseTime(created); err != nil {
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

type MCPServer struct {
	ID, Name, Endpoint, Transport, AuthCredentialID, ValidationState, LatestDiscoveryID string
	Enabled                                                                             bool
}
type MCPTool struct {
	RemoteName string
	SchemaJSON []byte
	SchemaHash string
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
	_, err := r.s.db.ExecContext(ctx, r.s.bind("INSERT INTO mcp_servers(id,name,endpoint,transport,auth_credential_id,enabled,validation_state,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)"), m.ID, m.Name, m.Endpoint, m.Transport, nullable(m.AuthCredentialID), boolValue(r.s.dialect, m.Enabled), m.ValidationState, now, now)
	return err
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
			if _, err := tx.ExecContext(ctx, r.s.bind("INSERT INTO mcp_tools(discovery_id,remote_name,schema_json,schema_hash) VALUES(?,?,?,?)"), out.ID, t.RemoteName, string(t.SchemaJSON), t.SchemaHash); err != nil {
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
	rows, err := r.s.db.QueryContext(ctx, r.s.bind("SELECT remote_name,schema_json,schema_hash FROM mcp_tools WHERE discovery_id=? ORDER BY remote_name"), id)
	if err != nil {
		return d, err
	}
	defer rows.Close()
	for rows.Next() {
		var t MCPTool
		if err := rows.Scan(&t.RemoteName, &t.SchemaJSON, &t.SchemaHash); err != nil {
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
