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
	// Tools nil keeps the current version's MCP tool selection; non-nil (even empty) replaces it,
	// is validated like PublishVersion's, and clears mcp_review_required (choosing again is the review).
	Tools *[]SelectedTool
}

// CreateWithVersionAndToolsAndReviewRequired creates the flow and its initial
// immutable version in one transaction. An initial publication failure leaves
// neither row behind.
func (r FlowRepository) CreateWithVersionAndToolsAndReviewRequired(ctx context.Context, a Flow, config []byte, selections []SelectedTool, mcpReviewRequired bool) (FlowVersion, error) {
	if err := validID(a.ID); err != nil {
		return FlowVersion{}, err
	}
	hash, canonical, err := ConfigHash(config)
	if err != nil {
		return FlowVersion{}, err
	}
	now := time.Now().UTC()
	v := FlowVersion{ID: NewID(), FlowID: a.ID, Version: 1, SchemaVersion: 1, ConfigJSON: canonical, ConfigHash: hash, CreatedAt: now, MCPReviewRequired: mcpReviewRequired}
	a.Name = strings.TrimSpace(a.Name)
	if a.Name == "" {
		return FlowVersion{}, errors.New("flow name required")
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = now
	}
	err = r.s.WithTx(ctx, func(tx *sql.Tx) error {
		created := utc(a.CreatedAt)
		if _, err := tx.ExecContext(ctx, r.s.bind("INSERT INTO flows(id,name,description,enabled,archived,created_at,updated_at) VALUES(?,?,?,?,?,?,?)"), a.ID, a.Name, a.Description, boolValue(r.s.dialect, a.Enabled), boolValue(r.s.dialect, a.Archived), created, created); err != nil {
			return err
		}
		if err := r.s.validateSelections(ctx, tx, selections); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, r.s.bind("INSERT INTO flow_versions(id,flow_id,version,schema_version,config_json,config_hash,created_at,mcp_review_required) VALUES(?,?,?,?,?,?,?,?)"), v.ID, a.ID, v.Version, v.SchemaVersion, string(v.ConfigJSON), v.ConfigHash, utc(v.CreatedAt), boolValue(r.s.dialect, v.MCPReviewRequired)); err != nil {
			return err
		}
		for _, sel := range selections {
			if _, err := tx.ExecContext(ctx, r.s.bind("INSERT INTO flow_mcp_tools(flow_version_id,mcp_server_id,remote_name,schema_hash,required) VALUES(?,?,?,?,?)"), v.ID, sel.ServerID, sel.RemoteName, sel.SchemaHash, boolValue(r.s.dialect, sel.Required)); err != nil {
				return err
			}
		}
		result, err := tx.ExecContext(ctx, r.s.bind("UPDATE flows SET current_version_id=?,updated_at=? WHERE id=?"), v.ID, utc(now), a.ID)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return errors.New("flow unavailable")
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
		hash, canonical, err = ConfigHash(config)
		if err != nil {
			return FlowVersion{}, false, err
		}
	}
	err = r.s.WithTx(ctx, func(tx *sql.Tx) error {
		query := "SELECT name,description,enabled,archived,current_version_id FROM flows WHERE id=?"
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
			return errors.New("flow unavailable")
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
			return errors.New("invalid flow details")
		}
		updatedAt := utc(time.Now())
		if publish {
			var latest int
			if err := tx.QueryRowContext(ctx, r.s.bind("SELECT COALESCE(MAX(version),0) FROM flow_versions WHERE flow_id=?"), id).Scan(&latest); err != nil {
				return err
			}
			reviewRequired := false
			if mutation.Tools != nil {
				if err := r.s.validateSelections(ctx, tx, *mutation.Tools); err != nil {
					return err
				}
			} else if currentVersion.Valid {
				if err := tx.QueryRowContext(ctx, r.s.bind("SELECT mcp_review_required FROM flow_versions WHERE id=?"), currentVersion.String).Scan(&reviewRequired); err != nil {
					return err
				}
			}
			next = FlowVersion{ID: NewID(), FlowID: id, Version: latest + 1, SchemaVersion: 1, ConfigJSON: canonical, ConfigHash: hash, CreatedAt: time.Now().UTC(), MCPReviewRequired: reviewRequired}
			if _, err := tx.ExecContext(ctx, r.s.bind("INSERT INTO flow_versions(id,flow_id,version,schema_version,config_json,config_hash,created_at,mcp_review_required) VALUES(?,?,?,?,?,?,?,?)"), next.ID, next.FlowID, next.Version, next.SchemaVersion, string(next.ConfigJSON), next.ConfigHash, utc(next.CreatedAt), boolValue(r.s.dialect, reviewRequired)); err != nil {
				return err
			}
			if mutation.Tools != nil {
				for _, sel := range *mutation.Tools {
					if _, err := tx.ExecContext(ctx, r.s.bind("INSERT INTO flow_mcp_tools(flow_version_id,mcp_server_id,remote_name,schema_hash,required) VALUES(?,?,?,?,?)"), next.ID, sel.ServerID, sel.RemoteName, sel.SchemaHash, boolValue(r.s.dialect, sel.Required)); err != nil {
						return err
					}
				}
			} else if currentVersion.Valid {
				if _, err := tx.ExecContext(ctx, r.s.bind("INSERT INTO flow_mcp_tools(flow_version_id,mcp_server_id,remote_name,schema_hash,required) SELECT ?,mcp_server_id,remote_name,schema_hash,required FROM flow_mcp_tools WHERE flow_version_id=?"), next.ID, currentVersion.String); err != nil {
					return err
				}
			}
			result, err := tx.ExecContext(ctx, r.s.bind("UPDATE flows SET name=?,description=?,enabled=?,current_version_id=?,updated_at=? WHERE id=? AND archived=?"), name, description, boolValue(r.s.dialect, enabled), next.ID, updatedAt, id, boolValue(r.s.dialect, false))
			if err != nil {
				return err
			}
			n, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if n != 1 {
				return errors.New("flow unavailable")
			}
			return nil
		}
		_, err := tx.ExecContext(ctx, r.s.bind("UPDATE flows SET name=?,description=?,enabled=?,updated_at=? WHERE id=? AND archived=?"), name, description, boolValue(r.s.dialect, enabled), updatedAt, id, boolValue(r.s.dialect, false))
		return err
	})
	return next, publish && err == nil, err
}
