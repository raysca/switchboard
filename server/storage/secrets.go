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

// Put creates the secret or replaces its value, keeping CreatedAt.
func (r SecretsRepository) Put(ctx context.Context, row SecretRow) error {
	now := utc(time.Now())
	_, err := r.s.db.ExecContext(ctx, r.s.bind("INSERT INTO secrets(name,key_id,nonce,ciphertext,created_at,updated_at) VALUES(?,?,?,?,?,?) ON CONFLICT(name) DO UPDATE SET key_id=excluded.key_id, nonce=excluded.nonce, ciphertext=excluded.ciphertext, updated_at=excluded.updated_at"),
		row.Name, row.KeyID, row.Nonce, row.Ciphertext, now, now)
	return err
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
	defer rows.Close()
	var out []SecretRow
	for rows.Next() {
		var row SecretRow
		var created, updated string
		var used sql.NullString
		if err := rows.Scan(&row.Name, &row.KeyID, &row.Nonce, &row.Ciphertext, &created, &updated, &used); err != nil {
			return nil, err
		}
		if row, err = finishSecret(row, created, updated, used); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// Delete returns sql.ErrNoRows when the secret does not exist.
func (r SecretsRepository) Delete(ctx context.Context, name string) error {
	res, err := r.s.db.ExecContext(ctx, r.s.bind("DELETE FROM secrets WHERE name=?"), name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
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
