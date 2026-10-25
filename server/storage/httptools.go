package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// HTTPToolRefPrefix starts the pseudo server id under which an agent version pins an HTTP tool in agent_mcp_tools.
const HTTPToolRefPrefix = "http:"

var (
	ErrHTTPToolReferenced = errors.New("HTTP tool is referenced by a current agent")
	ErrHTTPToolNameTaken  = errors.New("an HTTP tool with that name exists")
)

// HTTPTool is the current state of a tool; its definitions are immutable rows kept by hash.
type HTTPTool struct {
	ID, Name, CurrentHash string
	Enabled               bool
	CreatedAt, UpdatedAt  time.Time
}

// HTTPToolReference is a current agent version that pins the tool.
type HTTPToolReference struct {
	AgentID        string `json:"agent_id"`
	AgentName      string `json:"agent_name"`
	AgentVersionID string `json:"agent_version_id"`
	Required       bool   `json:"required"`
}

type HTTPToolRepository struct{ s *Store }

func (s *Store) HTTPTools() HTTPToolRepository { return HTTPToolRepository{s} }

// Create stores a new tool with its first definition.
func (r HTTPToolRepository) Create(ctx context.Context, t HTTPTool, definition []byte) error {
	if err := validID(t.ID); err != nil {
		return err
	}
	err := r.s.WithTx(ctx, func(tx *sql.Tx) error {
		if err := r.nameFree(ctx, tx, t.Name, t.ID); err != nil {
			return err
		}
		now := utc(time.Now())
		if _, err := tx.ExecContext(ctx, r.s.bind("INSERT INTO http_tools(id,name,enabled,current_hash,created_at,updated_at) VALUES(?,?,?,?,?,?)"), t.ID, t.Name, boolValue(r.s.dialect, t.Enabled), t.CurrentHash, now, now); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, r.s.bind("INSERT INTO http_tool_definitions(tool_id,hash,definition_json,created_at) VALUES(?,?,?,?)"), t.ID, t.CurrentHash, string(definition), now)
		return err
	})
	return r.nameTakenAfter(ctx, err, t.Name, t.ID)
}

// nameTakenAfter turns a failed write into ErrHTTPToolNameTaken when a concurrent request took the name first (the
// UNIQUE constraint's driver error says nothing portable).
func (r HTTPToolRepository) nameTakenAfter(ctx context.Context, err error, name, id string) error {
	if err == nil || errors.Is(err, ErrHTTPToolNameTaken) {
		return err
	}
	var other string
	if r.s.db.QueryRowContext(ctx, r.s.bind("SELECT id FROM http_tools WHERE name=? AND id<>?"), name, id).Scan(&other) == nil {
		return ErrHTTPToolNameTaken
	}
	return err
}

// NewDefinition makes a definition the tool's current one (an edit). A definition that was seen before is reused.
// sql.ErrNoRows: the tool does not exist.
func (r HTTPToolRepository) NewDefinition(ctx context.Context, id, name, hash string, definition []byte) error {
	err := r.s.WithTx(ctx, func(tx *sql.Tx) error {
		if err := r.nameFree(ctx, tx, name, id); err != nil {
			return err
		}
		now := utc(time.Now())
		res, err := tx.ExecContext(ctx, r.s.bind("UPDATE http_tools SET name=?,current_hash=?,updated_at=? WHERE id=?"), name, hash, now, id)
		if err := oneRow(res, err); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, r.s.bind("INSERT INTO http_tool_definitions(tool_id,hash,definition_json,created_at) VALUES(?,?,?,?) ON CONFLICT(tool_id,hash) DO NOTHING"), id, hash, string(definition), now)
		return err
	})
	return r.nameTakenAfter(ctx, err, name, id)
}

func (r HTTPToolRepository) nameFree(ctx context.Context, tx *sql.Tx, name, id string) error {
	var other string
	err := tx.QueryRowContext(ctx, r.s.bind("SELECT id FROM http_tools WHERE name=? AND id<>?"), name, id).Scan(&other)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return ErrHTTPToolNameTaken
}

const httpToolColumns = "id,name,enabled,current_hash,created_at,updated_at"

func scanHTTPTool(row rowScanner) (HTTPTool, error) {
	var t HTTPTool
	var created, updated string
	if err := row.Scan(&t.ID, &t.Name, &t.Enabled, &t.CurrentHash, &created, &updated); err != nil {
		return t, err
	}
	var err error
	if t.CreatedAt, err = parseTime(created); err != nil {
		return t, err
	}
	t.UpdatedAt, err = parseTime(updated)
	return t, err
}

// Get returns sql.ErrNoRows when the tool does not exist.
func (r HTTPToolRepository) Get(ctx context.Context, id string) (HTTPTool, error) {
	return scanHTTPTool(r.s.db.QueryRowContext(ctx, r.s.bind("SELECT "+httpToolColumns+" FROM http_tools WHERE id=?"), id))
}

func (r HTTPToolRepository) List(ctx context.Context) ([]HTTPTool, error) {
	rows, err := r.s.db.QueryContext(ctx, "SELECT "+httpToolColumns+" FROM http_tools ORDER BY name,id")
	if err != nil {
		return nil, err
	}
	return collect(rows, nil, scanHTTPTool)
}

// Definition returns the stored definition JSON of a tool at a hash; sql.ErrNoRows when there is none.
func (r HTTPToolRepository) Definition(ctx context.Context, id, hash string) ([]byte, error) {
	var raw string
	err := r.s.db.QueryRowContext(ctx, r.s.bind("SELECT definition_json FROM http_tool_definitions WHERE tool_id=? AND hash=?"), id, hash).Scan(&raw)
	return []byte(raw), err
}

// StoredDefinition is one definition row with its tool's name.
type StoredDefinition struct {
	ToolID, Name, Hash string
	JSON               []byte
}

// AllDefinitions returns every stored definition of every tool, current or not.
func (r HTTPToolRepository) AllDefinitions(ctx context.Context) ([]StoredDefinition, error) {
	rows, err := r.s.db.QueryContext(ctx, "SELECT t.id,t.name,d.hash,d.definition_json FROM http_tool_definitions d JOIN http_tools t ON t.id=d.tool_id ORDER BY t.name,d.created_at")
	if err != nil {
		return nil, err
	}
	return collect(rows, nil, func(row rowScanner) (StoredDefinition, error) {
		var d StoredDefinition
		var raw string
		err := row.Scan(&d.ToolID, &d.Name, &d.Hash, &raw)
		d.JSON = []byte(raw)
		return d, err
	})
}

// SetEnabled returns sql.ErrNoRows when the tool does not exist.
func (r HTTPToolRepository) SetEnabled(ctx context.Context, id string, enabled bool) error {
	res, err := r.s.db.ExecContext(ctx, r.s.bind("UPDATE http_tools SET enabled=?,updated_at=? WHERE id=?"), boolValue(r.s.dialect, enabled), utc(time.Now()), id)
	return oneRow(res, err)
}

// CurrentReferences lists the agents whose current version pins the tool.
func (r HTTPToolRepository) CurrentReferences(ctx context.Context, id string) ([]HTTPToolReference, error) {
	return r.references(ctx, r.s.db, id)
}

type rowQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func (r HTTPToolRepository) references(ctx context.Context, q rowQuerier, id string) ([]HTTPToolReference, error) {
	rows, err := q.QueryContext(ctx, r.s.bind("SELECT a.id,a.name,a.current_version_id,t.required FROM agents a JOIN agent_mcp_tools t ON t.agent_version_id=a.current_version_id WHERE t.mcp_server_id=? ORDER BY a.name,a.id"), HTTPToolRefPrefix+id)
	if err != nil {
		return nil, err
	}
	return collect(rows, nil, func(row rowScanner) (HTTPToolReference, error) {
		var ref HTTPToolReference
		err := row.Scan(&ref.AgentID, &ref.AgentName, &ref.AgentVersionID, &ref.Required)
		return ref, err
	})
}

// Delete removes a tool no agent version has ever pinned, with its definitions; a tool with history is disabled
// instead, so every pinned definition stays readable. ErrHTTPToolReferenced: a version pins it (CurrentReferences
// lists the current ones). sql.ErrNoRows: the tool does not exist.
func (r HTTPToolRepository) Delete(ctx context.Context, id string) error {
	return r.s.WithTx(ctx, func(tx *sql.Tx) error {
		if r.s.dialect == Postgres {
			// A publish takes the row FOR SHARE, so this waits for it and then sees its pin.
			var locked string
			if err := tx.QueryRowContext(ctx, "SELECT id FROM http_tools WHERE id=$1 FOR UPDATE", id).Scan(&locked); err != nil {
				return err
			}
		}
		var pins int
		if err := tx.QueryRowContext(ctx, r.s.bind("SELECT count(*) FROM agent_mcp_tools WHERE mcp_server_id=?"), HTTPToolRefPrefix+id).Scan(&pins); err != nil {
			return err
		}
		if pins > 0 {
			return ErrHTTPToolReferenced
		}
		if _, err := tx.ExecContext(ctx, r.s.bind("DELETE FROM http_tool_definitions WHERE tool_id=?"), id); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, r.s.bind("DELETE FROM http_tools WHERE id=?"), id)
		return oneRow(res, err)
	})
}

// validateHTTPSelection checks a pinned HTTP tool: it exists, is enabled, the pinned name is its name and the pinned
// hash is its current definition (a selection made against an older definition must be reviewed again).
func (s *Store) validateHTTPSelection(ctx context.Context, q queryer, sel SelectedTool) error {
	id := strings.TrimPrefix(sel.ServerID, HTTPToolRefPrefix)
	if err := validID(id); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidToolSelection, err)
	}
	query := "SELECT name,current_hash FROM http_tools WHERE id=? AND enabled=?"
	if s.dialect == Postgres {
		query += " FOR SHARE"
	}
	var name, hash string
	if err := q.QueryRowContext(ctx, s.bind(query), id, boolValue(s.dialect, true)).Scan(&name, &hash); errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: HTTP tool %s is missing or disabled", ErrInvalidToolSelection, id)
	} else if err != nil {
		return err
	}
	if sel.RemoteName != name {
		return fmt.Errorf("%w: unknown HTTP tool name %q", ErrInvalidToolSelection, sel.RemoteName)
	}
	if hash != sel.SchemaHash {
		return fmt.Errorf("%w: HTTP tool %q changed", ErrInvalidToolSelection, sel.RemoteName)
	}
	return nil
}

// httpToolStartable checks, inside a session-start transaction, that a captured HTTP tool is still enabled and that
// the pinned definition is stored. The tool may have been edited since the agent pinned it; the call runs the pinned,
// reviewed definition.
func (s *Store) httpToolStartable(ctx context.Context, q queryer, id, hash string) error {
	query := "SELECT t.enabled FROM http_tools t JOIN http_tool_definitions d ON d.tool_id=t.id WHERE t.id=? AND d.hash=?"
	if s.dialect == Postgres {
		query += " FOR SHARE OF t"
	}
	var enabled bool
	if err := q.QueryRowContext(ctx, s.bind(query), id, hash).Scan(&enabled); err != nil || !enabled {
		return errors.New("HTTP tool is unavailable for new sessions")
	}
	return nil
}
