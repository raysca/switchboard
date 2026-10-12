package storage

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

type FlowMutation struct {
	Name        *string
	Description *string
	Enabled     *bool
}

// CreateWithVersionAndToolsAndReviewRequired creates the flow and its initial
// immutable version in one transaction. An initial publication failure leaves
// neither row behind.
func (r FlowRepository) CreateWithVersionAndToolsAndReviewRequired(ctx context.Context, a Flow, config []byte, selections []SelectedTool, mcpReviewRequired bool) (FlowVersion, error) {
	if err := validID(a.ID); err != nil {
		return FlowVersion{}, err
	}
	hash, canonical, err := hashJSON(config)
	if err != nil {
		return FlowVersion{}, err
	}
	now := time.Now().UTC()
	v := FlowVersion{ID: NewID(), FlowID: a.ID, Version: 1, SchemaVersion: 1, ConfigJSON: canonical, ConfigHash: hash, CreatedAt: now, MCPReviewRequired: mcpReviewRequired}
	a.Name = strings.TrimSpace(a.Name)
	if a.Name == "" {
		return FlowVersion{}, errors.New("agent name required")
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = now
	}
	err = r.s.WithTx(ctx, func(tx *sql.Tx) error {
		created := utc(a.CreatedAt)
		if _, err := tx.ExecContext(ctx, r.s.bind("INSERT INTO agents(id,name,description,enabled,archived,created_at,updated_at) VALUES(?,?,?,?,?,?,?)"), a.ID, a.Name, a.Description, boolValue(r.s.dialect, a.Enabled), boolValue(r.s.dialect, a.Archived), created, created); err != nil {
			return err
		}
		for _, sel := range selections {
			if err := validID(sel.ServerID); err != nil {
				return err
			}
			query := "SELECT latest_discovery_id FROM mcp_servers WHERE id=? AND enabled=? AND pending_delete=?"
			if r.s.dialect == Postgres {
				query += " FOR SHARE"
			}
			var discovery sql.NullString
			if err := tx.QueryRowContext(ctx, r.s.bind(query), sel.ServerID, boolValue(r.s.dialect, true), boolValue(r.s.dialect, false)).Scan(&discovery); err != nil {
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
		if _, err := tx.ExecContext(ctx, r.s.bind("INSERT INTO agent_versions(id,agent_id,version,schema_version,config_json,config_hash,created_at,mcp_review_required) VALUES(?,?,?,?,?,?,?,?)"), v.ID, a.ID, v.Version, v.SchemaVersion, string(v.ConfigJSON), v.ConfigHash, utc(v.CreatedAt), boolValue(r.s.dialect, v.MCPReviewRequired)); err != nil {
			return err
		}
		for _, sel := range selections {
			if _, err := tx.ExecContext(ctx, r.s.bind("INSERT INTO agent_mcp_tools(agent_version_id,mcp_server_id,remote_name,schema_hash,required) VALUES(?,?,?,?,?)"), v.ID, sel.ServerID, sel.RemoteName, sel.SchemaHash, boolValue(r.s.dialect, sel.Required)); err != nil {
				return err
			}
		}
		result, err := tx.ExecContext(ctx, r.s.bind("UPDATE agents SET current_version_id=?,updated_at=? WHERE id=?"), v.ID, utc(now), a.ID)
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
	return v, err
}

// MutateAndPublish applies optional metadata fields and an optional config
// publication atomically. Version allocation is derived while holding the
// flow row lock (PostgreSQL) or SQLite's immediate write transaction.
func (r FlowRepository) MutateAndPublish(ctx context.Context, id string, mutation FlowMutation, config []byte) (FlowVersion, bool, error) {
	if err := validID(id); err != nil {
		return FlowVersion{}, false, err
	}
	var next FlowVersion
	publish := len(config) > 0
	var hash string
	var canonical []byte
	var err error
	if publish {
		hash, canonical, err = hashJSON(config)
		if err != nil {
			return FlowVersion{}, false, err
		}
	}
	err = r.s.WithTx(ctx, func(tx *sql.Tx) error {
		query := "SELECT name,description,enabled,archived,current_version_id FROM agents WHERE id=?"
		if r.s.dialect == Postgres {
			query += " FOR UPDATE"
		}
		var name, description string
		var currentVersion sql.NullString
		var enabled, archived bool
		if err := tx.QueryRowContext(ctx, r.s.bind(query), id).Scan(&name, &description, &enabled, &archived, &currentVersion); err != nil {
			return err
		}
		if archived {
			return errors.New("agent unavailable")
		}
		if mutation.Name != nil {
			name = strings.TrimSpace(*mutation.Name)
		}
		if mutation.Description != nil {
			description = *mutation.Description
		}
		if mutation.Enabled != nil {
			enabled = *mutation.Enabled
		}
		if name == "" || len(name) > 128 || len(description) > 1024 {
			return errors.New("invalid agent details")
		}
		updatedAt := utc(time.Now())
		if publish {
			var latest int
			if err := tx.QueryRowContext(ctx, r.s.bind("SELECT COALESCE(MAX(version),0) FROM agent_versions WHERE agent_id=?"), id).Scan(&latest); err != nil {
				return err
			}
			reviewRequired := false
			if currentVersion.Valid {
				if err := tx.QueryRowContext(ctx, r.s.bind("SELECT mcp_review_required FROM agent_versions WHERE id=?"), currentVersion.String).Scan(&reviewRequired); err != nil {
					return err
				}
			}
			next = FlowVersion{ID: NewID(), FlowID: id, Version: latest + 1, SchemaVersion: 1, ConfigJSON: canonical, ConfigHash: hash, CreatedAt: time.Now().UTC(), MCPReviewRequired: reviewRequired}
			if _, err := tx.ExecContext(ctx, r.s.bind("INSERT INTO agent_versions(id,agent_id,version,schema_version,config_json,config_hash,created_at,mcp_review_required) VALUES(?,?,?,?,?,?,?,?)"), next.ID, next.FlowID, next.Version, next.SchemaVersion, string(next.ConfigJSON), next.ConfigHash, utc(next.CreatedAt), boolValue(r.s.dialect, reviewRequired)); err != nil {
				return err
			}
			if currentVersion.Valid {
				if _, err := tx.ExecContext(ctx, r.s.bind("INSERT INTO agent_mcp_tools(agent_version_id,mcp_server_id,remote_name,schema_hash,required) SELECT ?,mcp_server_id,remote_name,schema_hash,required FROM agent_mcp_tools WHERE agent_version_id=?"), next.ID, currentVersion.String); err != nil {
					return err
				}
			}
			result, err := tx.ExecContext(ctx, r.s.bind("UPDATE agents SET name=?,description=?,enabled=?,current_version_id=?,updated_at=? WHERE id=? AND archived=?"), name, description, boolValue(r.s.dialect, enabled), next.ID, updatedAt, id, boolValue(r.s.dialect, false))
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
		}
		_, err := tx.ExecContext(ctx, r.s.bind("UPDATE agents SET name=?,description=?,enabled=?,updated_at=? WHERE id=? AND archived=?"), name, description, boolValue(r.s.dialect, enabled), updatedAt, id, boolValue(r.s.dialect, false))
		return err
	})
	return next, publish && err == nil, err
}
