package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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

// ErrInvalidToolSelection: a selected MCP tool's server is missing, disabled or
// undiscovered, or the tool or its schema changed.
var ErrInvalidToolSelection = errors.New("invalid MCP tool selection")

func (s *Store) Agents() AgentRepository { return AgentRepository{s} }
func (r AgentRepository) Create(ctx context.Context, a Agent) error {
	if err := validID(a.ID); err != nil {
		return err
	}
	now := utc(a.CreatedAt)
	_, err := r.s.db.ExecContext(ctx, r.s.bind("INSERT INTO agents(id,name,description,enabled,archived,created_at,updated_at) VALUES(?,?,?,?,?,?,?)"), a.ID, a.Name, a.Description, boolValue(r.s.dialect, a.Enabled), boolValue(r.s.dialect, a.Archived), now, now)
	return err
}

// MaxSelectedTools bounds the MCP tools one agent version may select.
const MaxSelectedTools = 64

// queryer is what validateSelections reads through: a transaction (a publish) or the database (a draft).
type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// validateSelections checks every selected tool against its server's latest discovery: the server is
// enabled, has a discovery, offers the tool, and the pinned schema hash is still the offered one.
func (s *Store) validateSelections(ctx context.Context, q queryer, selections []SelectedTool) error {
	if len(selections) > MaxSelectedTools {
		return fmt.Errorf("%w: at most %d tools", ErrInvalidToolSelection, MaxSelectedTools)
	}
	for _, sel := range selections {
		if err := validID(sel.ServerID); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidToolSelection, err)
		}
		query := "SELECT latest_discovery_id FROM mcp_servers WHERE id=? AND enabled=?"
		if s.dialect == Postgres {
			query += " FOR SHARE"
		}
		var discovery sql.NullString
		if err := q.QueryRowContext(ctx, s.bind(query), sel.ServerID, boolValue(s.dialect, true)).Scan(&discovery); errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: MCP server %s is missing or disabled", ErrInvalidToolSelection, sel.ServerID)
		} else if err != nil {
			return err
		}
		if !discovery.Valid {
			return fmt.Errorf("%w: MCP server %s has no discovery", ErrInvalidToolSelection, sel.ServerID)
		}
		var actual string
		if err := q.QueryRowContext(ctx, s.bind("SELECT schema_hash FROM mcp_tools WHERE discovery_id=? AND remote_name=?"), discovery.String, sel.RemoteName).Scan(&actual); errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: unknown tool %q", ErrInvalidToolSelection, sel.RemoteName)
		} else if err != nil {
			return err
		}
		if actual != sel.SchemaHash {
			return fmt.Errorf("%w: tool %q schema changed", ErrInvalidToolSelection, sel.RemoteName)
		}
	}
	return nil
}

// ValidateSelections checks a selection the way a publish would, without writing anything (a draft test call).
func (r AgentRepository) ValidateSelections(ctx context.Context, selections []SelectedTool) error {
	return r.s.validateSelections(ctx, r.s.db, selections)
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

// VersionByNumber returns one numbered version of an agent with its selected tools, or sql.ErrNoRows.
// Versions are immutable, so this is what an earlier publish looked like.
func (r AgentRepository) VersionByNumber(ctx context.Context, agentID string, number int) (AgentVersion, error) {
	var id string
	if err := r.s.db.QueryRowContext(ctx, r.s.bind("SELECT id FROM agent_versions WHERE agent_id=? AND version=?"), agentID, number).Scan(&id); err != nil {
		return AgentVersion{}, err
	}
	return r.Version(ctx, id)
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

type MCPServer struct {
	ID, Name, Endpoint, Transport, AuthEnv, ValidationState, LatestDiscoveryID string
	Description                                                                string
	Enabled, PendingDelete                                                     bool
	LastConnectedAt, LastDiscoveredAt, LastCheckedAt, CreatedAt, UpdatedAt     time.Time
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
	now := utc(time.Now())
	_, err := r.s.db.ExecContext(ctx, r.s.bind("INSERT INTO mcp_servers(id,name,endpoint,transport,auth_env,enabled,validation_state,created_at,updated_at,description) VALUES(?,?,?,?,?,?,?,?,?,?)"), m.ID, m.Name, m.Endpoint, m.Transport, m.AuthEnv, boolValue(r.s.dialect, m.Enabled), m.ValidationState, now, now, m.Description)
	return err
}

func (r MCPRepository) Server(ctx context.Context, id string) (MCPServer, error) {
	var m MCPServer
	var auth string
	var latest, connected, discovered, checked sql.NullString
	var enabled bool
	var created, updated string
	err := r.s.db.QueryRowContext(ctx, r.s.bind("SELECT id,name,description,endpoint,transport,auth_env,enabled,validation_state,latest_discovery_id,last_connected_at,last_discovered_at,last_checked_at,created_at,updated_at,pending_delete FROM mcp_servers WHERE id=?"), id).Scan(&m.ID, &m.Name, &m.Description, &m.Endpoint, &m.Transport, &auth, &enabled, &m.ValidationState, &latest, &connected, &discovered, &checked, &created, &updated, &m.PendingDelete)
	if err != nil {
		return m, err
	}
	m.AuthEnv, m.LatestDiscoveryID, m.Enabled = auth, latest.String, enabled
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
	if checked.Valid {
		if m.LastCheckedAt, err = parseTime(checked.String); err != nil {
			return m, err
		}
	}
	return m, nil
}

func (r MCPRepository) ListServers(ctx context.Context) ([]MCPServer, error) {
	rows, err := r.s.db.QueryContext(ctx, "SELECT id,name,description,endpoint,transport,auth_env,enabled,validation_state,latest_discovery_id,last_connected_at,last_discovered_at,last_checked_at,created_at,updated_at,pending_delete FROM mcp_servers ORDER BY name,id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MCPServer
	for rows.Next() {
		var m MCPServer
		var auth string
		var latest, connected, discovered, checked sql.NullString
		var enabled bool
		var created, updated string
		if err := rows.Scan(&m.ID, &m.Name, &m.Description, &m.Endpoint, &m.Transport, &auth, &enabled, &m.ValidationState, &latest, &connected, &discovered, &checked, &created, &updated, &m.PendingDelete); err != nil {
			return nil, err
		}
		m.AuthEnv, m.LatestDiscoveryID, m.Enabled = auth, latest.String, enabled
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
		if checked.Valid {
			if m.LastCheckedAt, err = parseTime(checked.String); err != nil {
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
	result, err := r.s.db.ExecContext(ctx, r.s.bind("UPDATE mcp_servers SET validation_state=?,last_connected_at=COALESCE(?,last_connected_at),last_discovered_at=COALESCE(?,last_discovered_at),last_checked_at=?,updated_at=? WHERE id=?"), state, nullTime(connected), nullTime(discovered), utc(time.Now()), utc(time.Now()), id)
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

// MCPAgentReference is the 409 mcp_server_in_use detail: its JSON keys are a wire contract the console reads.
type MCPAgentReference struct {
	AgentID        string `json:"agent_id"`
	AgentName      string `json:"agent_name"`
	AgentVersionID string `json:"agent_version_id"`
	RemoteName     string `json:"remote_name"`
	Required       bool   `json:"required"`
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

// UpdateServer stores an edit's name, description, endpoint and auth_env. With resetValidation
// (the endpoint or auth_env changed) an enabled row also goes back to not_validated, decided in
// the same statement; otherwise validation_state is left as it is now.
// sql.ErrNoRows: the server is missing or its deletion is pending.
func (r MCPRepository) UpdateServer(ctx context.Context, m MCPServer, resetValidation bool) error {
	set, args := "name=?,description=?,endpoint=?,auth_env=?,updated_at=?", []any{m.Name, m.Description, m.Endpoint, m.AuthEnv, utc(time.Now())}
	if resetValidation {
		set += ",validation_state=CASE WHEN enabled=? THEN 'not_validated' ELSE validation_state END"
		args = append(args, boolValue(r.s.dialect, true))
	}
	args = append(args, m.ID, boolValue(r.s.dialect, false))
	result, err := r.s.db.ExecContext(ctx, r.s.bind("UPDATE mcp_servers SET "+set+" WHERE id=? AND pending_delete=?"), args...)
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
		current, active, err := countMCPReferences(ctx, tx, r.s, id)
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
	if validID(id) != nil { // a malformed id cannot name a server
		return sql.ErrNoRows
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
		_, active, err := countMCPReferences(ctx, tx, r.s, id)
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
		current, active, err := countMCPReferences(ctx, r.s.db, r.s, id)
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

// countMCPReferences counts the agents whose current version and the live sessions that use an MCP server.
func countMCPReferences(ctx context.Context, q queryer, s *Store, id string) (int, int, error) {
	var current, active int
	err := q.QueryRowContext(ctx, s.bind("SELECT count(*) FROM agents a JOIN agent_mcp_tools t ON t.agent_version_id=a.current_version_id WHERE t.mcp_server_id=?"), id).Scan(&current)
	if err != nil {
		return 0, 0, err
	}
	err = q.QueryRowContext(ctx, s.bind("SELECT count(*) FROM sessions s JOIN session_mcp_tools t ON t.session_id=s.id WHERE t.mcp_server_id=? AND s.status IN ('active','draining')"), id).Scan(&active)
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
		h, c, err := ConfigHash(sorted[i].SchemaJSON)
		if err != nil {
			return out, err
		}
		sorted[i].SchemaHash = h
		sorted[i].SchemaJSON = c
		if i > 0 && sorted[i-1].RemoteName == sorted[i].RemoteName {
			return out, errors.New("duplicate MCP tool name")
		}
	}
	// The ordered name/schema-hash/description triples are the immutable discovery identity (the model sees
	// descriptions). Descriptions joined the hash later: the first sync after that deploy stores one new discovery per server.
	pairs := make([][3]string, len(sorted))
	for i, t := range sorted {
		pairs[i] = [3]string{t.RemoteName, t.SchemaHash, t.Description}
	}
	raw, err := jsonMarshal(pairs)
	if err != nil {
		return out, err
	}
	hash, _, err := ConfigHash(raw)
	if err != nil {
		return out, err
	}
	out = MCPDiscovery{ID: NewID(), ServerID: serverID, DiscoveryHash: hash, Tools: sorted, CreatedAt: time.Now().UTC()}
	var existing string
	err = r.s.WithTx(ctx, func(tx *sql.Tx) error {
		// An unchanged tool list is not a new discovery: reuse the latest one.
		var latestID, latestHash string
		switch err := tx.QueryRowContext(ctx, r.s.bind("SELECT d.id,d.discovery_hash FROM mcp_discoveries d JOIN mcp_servers s ON s.latest_discovery_id=d.id WHERE s.id=?"), serverID).Scan(&latestID, &latestHash); {
		case err == nil && latestHash == hash:
			existing = latestID
			return nil
		case err != nil && !errors.Is(err, sql.ErrNoRows):
			return err
		}
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
	if err == nil && existing != "" {
		return r.Discovery(ctx, existing)
	}
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

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }
