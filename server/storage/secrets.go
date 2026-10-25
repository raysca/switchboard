package storage

import (
	"context"
	"database/sql"
	"time"
)

// SecretRow is one sealed secret. The server/secrets package seals and opens the bytes; storage never sees plaintext.
type SecretRow struct {
	Name, KeyID                      string
	Nonce, Ciphertext                []byte
	CreatedAt, UpdatedAt, LastUsedAt time.Time // LastUsedAt is zero until the first use
}

type SecretsRepository struct{ s *Store }

func (s *Store) Secrets() SecretsRepository { return SecretsRepository{s} }

// SecretChange is one audited change to a stored secret: what happened to which name, and the request that did it.
type SecretChange struct {
	At                      time.Time
	Action, Name, RequestID string
}

// MaxSecretChanges bounds a list of changes.
const MaxSecretChanges = 100

func (r SecretsRepository) audit(ctx context.Context, tx *sql.Tx, action, name, requestID string) error {
	if requestID == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, r.s.bind("INSERT INTO secret_audit(id,at,action,name,request_id) VALUES(?,?,?,?,?)"), NewID(), utc(time.Now()), action, name, requestID)
	return err
}

// Put creates the secret or replaces its value, keeping CreatedAt. A non-empty requestID also records the change in
// the audit trail, in the same transaction (re-sealing under a new key passes none).
func (r SecretsRepository) Put(ctx context.Context, row SecretRow, requestID string) error {
	return r.s.WithTx(ctx, func(tx *sql.Tx) error {
		var exists int
		if err := tx.QueryRowContext(ctx, r.s.bind("SELECT count(*) FROM secrets WHERE name=?"), row.Name).Scan(&exists); err != nil {
			return err
		}
		now := utc(time.Now())
		if _, err := tx.ExecContext(ctx, r.s.bind("INSERT INTO secrets(name,key_id,nonce,ciphertext,created_at,updated_at) VALUES(?,?,?,?,?,?) ON CONFLICT(name) DO UPDATE SET key_id=excluded.key_id, nonce=excluded.nonce, ciphertext=excluded.ciphertext, updated_at=excluded.updated_at"),
			row.Name, row.KeyID, row.Nonce, row.Ciphertext, now, now); err != nil {
			return err
		}
		action := "create"
		if exists > 0 {
			action = "replace"
		}
		return r.audit(ctx, tx, action, row.Name, requestID)
	})
}

// Changes returns the audit trail, newest first, at most MaxSecretChanges.
func (r SecretsRepository) Changes(ctx context.Context, limit int) ([]SecretChange, error) {
	if limit <= 0 || limit > MaxSecretChanges {
		limit = MaxSecretChanges
	}
	rows, err := r.s.db.QueryContext(ctx, r.s.bind("SELECT at,action,name,request_id FROM secret_audit ORDER BY at DESC, id DESC LIMIT ?"), limit)
	if err != nil {
		return nil, err
	}
	return collect(rows, nil, func(row rowScanner) (SecretChange, error) {
		var c SecretChange
		var at string
		if err := row.Scan(&at, &c.Action, &c.Name, &c.RequestID); err != nil {
			return c, err
		}
		var err error
		c.At, err = parseTime(at)
		return c, err
	})
}

// Get returns sql.ErrNoRows when the secret does not exist.
func (r SecretsRepository) Get(ctx context.Context, name string) (SecretRow, error) {
	var row SecretRow
	var created, updated string
	var used sql.NullString
	err := r.s.db.QueryRowContext(ctx, r.s.bind("SELECT name,key_id,nonce,ciphertext,created_at,updated_at,last_used_at FROM secrets WHERE name=?"), name).
		Scan(&row.Name, &row.KeyID, &row.Nonce, &row.Ciphertext, &created, &updated, &used)
	if err != nil {
		return row, err
	}
	return finishSecret(row, created, updated, used)
}

// List returns every secret ordered by name; callers must not expose Nonce or Ciphertext.
func (r SecretsRepository) List(ctx context.Context) ([]SecretRow, error) {
	rows, err := r.s.db.QueryContext(ctx, "SELECT name,key_id,nonce,ciphertext,created_at,updated_at,last_used_at FROM secrets ORDER BY name")
	if err != nil {
		return nil, err
	}
	return collect(rows, nil, func(sc rowScanner) (SecretRow, error) {
		var row SecretRow
		var created, updated string
		var used sql.NullString
		if err := sc.Scan(&row.Name, &row.KeyID, &row.Nonce, &row.Ciphertext, &created, &updated, &used); err != nil {
			return row, err
		}
		return finishSecret(row, created, updated, used)
	})
}

// Delete returns sql.ErrNoRows when the secret does not exist. A non-empty requestID also records the change.
func (r SecretsRepository) Delete(ctx context.Context, name, requestID string) error {
	return r.s.WithTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, r.s.bind("DELETE FROM secrets WHERE name=?"), name)
		if err := oneRow(res, err); err != nil {
			return err
		}
		return r.audit(ctx, tx, "delete", name, requestID)
	})
}

// Touch records a use, at most once a minute per secret so a busy tool does not write on every call.
func (r SecretsRepository) Touch(ctx context.Context, name string) error {
	now := time.Now()
	_, err := r.s.db.ExecContext(ctx, r.s.bind("UPDATE secrets SET last_used_at=? WHERE name=? AND (last_used_at IS NULL OR last_used_at<?)"), utc(now), name, utc(now.Add(-time.Minute)))
	return err
}

func finishSecret(row SecretRow, created, updated string, used sql.NullString) (SecretRow, error) {
	var err error
	if row.CreatedAt, err = parseTime(created); err != nil {
		return row, err
	}
	if row.UpdatedAt, err = parseTime(updated); err != nil {
		return row, err
	}
	if used.Valid {
		if row.LastUsedAt, err = parseTime(used.String); err != nil {
			return row, err
		}
	}
	return row, nil
}

// SecretUse names something that refers to a secret by name and would stop working if it were deleted.
type SecretUse struct{ Kind, ID, Name string }

// Uses lists what refers to the secret: today the MCP servers that authenticate with it.
func (r SecretsRepository) Uses(ctx context.Context, name string) ([]SecretUse, error) {
	rows, err := r.s.db.QueryContext(ctx, r.s.bind("SELECT id,name FROM mcp_servers WHERE auth_env=? ORDER BY name,id"), name)
	if err != nil {
		return nil, err
	}
	return collect(rows, nil, func(row rowScanner) (SecretUse, error) {
		u := SecretUse{Kind: "mcp_server"}
		err := row.Scan(&u.ID, &u.Name)
		return u, err
	})
}
