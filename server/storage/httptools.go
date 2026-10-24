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
	AgentID, AgentName, AgentVersionID string
	Required                           bool
}

type HTTPToolRepository struct{ s *Store }

func (s *Store) HTTPTools() HTTPToolRepository { return HTTPToolRepository{s} }

// Create stores a new tool with its first definition.
func (r HTTPToolRepository) Create(ctx context.Context, t HTTPTool, definition []byte) error {
	if err := validID(t.ID); err != nil {
		return err
	}
	return r.s.WithTx(ctx, func(tx *sql.Tx) error {
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
}

// NewDefinition makes a definition the tool's current one (an edit). A definition that was seen before is reused.
// sql.ErrNoRows: the tool does not exist.
func (r HTTPToolRepository) NewDefinition(ctx context.Context, id, name, hash string, definition []byte) error {
	return r.s.WithTx(ctx, func(tx *sql.Tx) error {
		if err := r.nameFree(ctx, tx, name, id); err != nil {
			return err
		}
		now := utc(time.Now())
		res, err := tx.ExecContext(ctx, r.s.bind("UPDATE http_tools SET name=?,current_hash=?,updated_at=? WHERE id=?"), name, hash, now, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return sql.ErrNoRows
		}
		_, err = tx.ExecContext(ctx, r.s.bind("INSERT INTO http_tool_definitions(tool_id,hash,definition_json,created_at) VALUES(?,?,?,?) ON CONFLICT(tool_id,hash) DO NOTHING"), id, hash, string(definition), now)
		return err
	})
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

func scanHTTPTool(row interface{ Scan(...any) error }) (HTTPTool, error) {
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
	defer rows.Close()
	var out []HTTPTool
	for rows.Next() {
		t, err := scanHTTPTool(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Definition returns the stored definition JSON of a tool at a hash; sql.ErrNoRows when there is none.
func (r HTTPToolRepository) Definition(ctx context.Context, id, hash string) ([]byte, error) {
	var raw string
	err := r.s.db.QueryRowContext(ctx, r.s.bind("SELECT definition_json FROM http_tool_definitions WHERE tool_id=? AND hash=?"), id, hash).Scan(&raw)
	return []byte(raw), err
}

// SetEnabled returns sql.ErrNoRows when the tool does not exist.
func (r HTTPToolRepository) SetEnabled(ctx context.Context, id string, enabled bool) error {
	res, err := r.s.db.ExecContext(ctx, r.s.bind("UPDATE http_tools SET enabled=?,updated_at=? WHERE id=?"), boolValue(r.s.dialect, enabled), utc(time.Now()), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
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
	defer rows.Close()
	var out []HTTPToolReference
	for rows.Next() {
		var ref HTTPToolReference
		if err := rows.Scan(&ref.AgentID, &ref.AgentName, &ref.AgentVersionID, &ref.Required); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// Delete removes a tool nothing current uses, with its definitions. ErrHTTPToolReferenced: a current agent pins it.
// sql.ErrNoRows: the tool does not exist.
func (r HTTPToolRepository) Delete(ctx context.Context, id string) error {
	return r.s.WithTx(ctx, func(tx *sql.Tx) error {
		refs, err := r.references(ctx, tx, id)
		if err != nil {
			return err
		}
		if len(refs) > 0 {
			return ErrHTTPToolReferenced
		}
		if _, err := tx.ExecContext(ctx, r.s.bind("DELETE FROM http_tool_definitions WHERE tool_id=?"), id); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, r.s.bind("DELETE FROM http_tools WHERE id=?"), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return sql.ErrNoRows
		}
		return nil
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
