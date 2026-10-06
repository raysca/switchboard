// Package storage owns the server's durable SQL records. It does not perform
// product validation, secret decryption, or framework runtime construction.
package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
	"govoice/server/migrations"
	_ "modernc.org/sqlite"
)

type Dialect string

const (
	SQLite   Dialect = "sqlite"
	Postgres Dialect = "postgres"
)

type Config struct {
	Dialect Dialect
	DSN     string
}
type Store struct {
	db      *sql.DB
	dialect Dialect
	// finalizePendingDeletesHook is a package-test fault injection seam for
	// exercising retry behavior after the session status transaction commits.
	finalizePendingDeletesHook func(context.Context) error
}

func Open(ctx context.Context, cfg Config) (*Store, error) {
	if cfg.DSN == "" {
		return nil, errors.New("storage DSN required")
	}
	var driver string
	var gooseDialect goose.Dialect
	var subdir string
	switch cfg.Dialect {
	case SQLite:
		driver = "sqlite"
		gooseDialect = goose.DialectSQLite3
		subdir = "sqlite"
	case Postgres:
		driver = "pgx"
		gooseDialect = goose.DialectPostgres
		subdir = "postgres"
	default:
		return nil, fmt.Errorf("unsupported storage dialect %q", cfg.Dialect)
	}
	if cfg.Dialect == SQLite {
		path, err := sqliteLockPath(cfg.DSN)
		if err != nil {
			return nil, err
		}
		if path != "" {
			guard := flock.New(path + ".migration.lock")
			ok, err := guard.TryLockContext(ctx, 50*time.Millisecond)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, errors.New("SQLite migration lock unavailable")
			}
			defer guard.Unlock()
		}
		driverDSN, err := sqliteConfiguredDSN(cfg.DSN)
		if err != nil {
			return nil, err
		}
		cfg.DSN = driverDSN
	}
	db, err := sql.Open(driver, cfg.DSN)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Store, error) { db.Close(); return nil, err }
	if cfg.Dialect == SQLite {
		db.SetMaxOpenConns(1)
	}
	if err = db.PingContext(ctx); err != nil {
		return fail(err)
	}
	fsys, err := fs.Sub(migrations.Files, subdir)
	if err != nil {
		return fail(err)
	}
	opts := []goose.ProviderOption{goose.WithTableName("goose_db_version")}
	if cfg.Dialect == Postgres {
		locker, err := lock.NewPostgresSessionLocker()
		if err != nil {
			return fail(err)
		}
		opts = append(opts, goose.WithSessionLocker(locker))
	}
	p, err := goose.NewProvider(gooseDialect, db, fsys, opts...)
	if err != nil {
		return fail(err)
	}
	if _, err = p.Up(ctx); err != nil {
		return fail(fmt.Errorf("storage migrations: %w", err))
	}
	return &Store{db: db, dialect: cfg.Dialect}, nil
}

func sqliteLockPath(dsn string) (string, error) {
	if dsn == ":memory:" {
		return "", nil
	}
	var path string
	if strings.HasPrefix(dsn, "file:") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", err
		}
		if u.Query().Get("mode") == "memory" {
			return "", nil
		}
		if u.Host != "" && u.Host != "localhost" {
			return "", errors.New("remote SQLite file URI unsupported")
		}
		if u.Opaque != "" {
			path, err = url.PathUnescape(u.Opaque)
			if err != nil {
				return "", err
			}
		} else {
			path = u.Path
		}
		if path == ":memory:" {
			return "", nil
		}
	} else {
		path = strings.SplitN(dsn, "?", 2)[0]
	}
	if path == "" {
		return "", errors.New("SQLite file path required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err == nil {
		abs = filepath.Join(parent, filepath.Base(abs))
	}
	return abs, nil
}

func sqliteConfiguredDSN(dsn string) (string, error) {
	base, query, found := strings.Cut(dsn, "?")
	if !found {
		query = ""
	}
	q, err := url.ParseQuery(query)
	if err != nil {
		return "", err
	}
	reserved := map[string]bool{"_foreign_keys": true, "_fk": true, "_busy_timeout": true, "_timeout": true, "_journal_mode": true, "_journal": true, "_pragma": true, "_txlock": true}
	for key := range q {
		if reserved[strings.ToLower(key)] {
			return "", fmt.Errorf("SQLite setting %q is managed by storage", key)
		}
	}
	q.Set("_foreign_keys", "on")
	q.Set("_busy_timeout", "5000")
	q.Set("_journal_mode", "WAL")
	q.Set("_txlock", "immediate")
	return base + "?" + q.Encode(), nil
}

func (s *Store) DB() *sql.DB      { return s.db }
func (s *Store) Close() error     { return s.db.Close() }
func (s *Store) Dialect() Dialect { return s.dialect }

// Diagnostics contains safe operational metrics for database health and connection pooling.
type Diagnostics struct {
	Dialect          Dialect
	Status           string
	OpenConnections  int
	InUse            int
	MigrationVersion int64
}

// Diagnostics gathers current connection pool stats, ping health, and applied migration version.
func (s *Store) Diagnostics(ctx context.Context) Diagnostics {
	diag := Diagnostics{
		Dialect: s.dialect,
		Status:  "connected",
	}
	if s.db == nil {
		diag.Status = "unconfigured"
		return diag
	}
	stats := s.db.Stats()
	diag.OpenConnections = stats.OpenConnections
	diag.InUse = stats.InUse
	if err := s.db.PingContext(ctx); err != nil {
		diag.Status = "error"
		return diag
	}
	var version int64
	row := s.db.QueryRowContext(ctx, "SELECT max(version_id) FROM goose_db_version WHERE is_applied")
	if err := row.Scan(&version); err == nil {
		diag.MigrationVersion = version
	}
	return diag
}

func (s *Store) bind(query string) string {
	if s.dialect != Postgres {
		return query
	}
	var b strings.Builder
	n := 0
	for _, c := range query {
		if c == '?' {
			n++
			fmt.Fprintf(&b, "$%d", n)
		} else {
			b.WriteRune(c)
		}
	}
	return b.String()
}
func (s *Store) WithTx(ctx context.Context, fn func(*sql.Tx) error) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
func utc(t time.Time) string {
	if t.IsZero() {
		t = time.Now()
	}
	return t.UTC().Truncate(time.Microsecond).Format("2006-01-02T15:04:05.000000Z")
}
func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	return t.UTC(), err
}
func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return utc(t)
}
func boolValue(d Dialect, b bool) any {
	if d == SQLite {
		if b {
			return 1
		}
		return 0
	}
	return b
}

// NewID returns an RFC 9562 UUIDv7 value; callers treat it as opaque.
func NewID() string {
	id, err := uuid.NewV7()
	if err != nil {
		panic(err)
	}
	return id.String()
}

// IsID reports whether id is a canonical UUIDv7, the form of every stored id.
func IsID(id string) bool { return validID(id) == nil }

func validID(id string) error {
	u, err := uuid.Parse(id)
	if err != nil || u.Version() != 7 || u.Variant() != uuid.RFC4122 || u.String() != id {
		return errors.New("UUIDv7 id required")
	}
	return nil
}
func optionalID(id string) error {
	if id == "" {
		return nil
	}
	return validID(id)
}

// CanonicalJSON normalizes valid JSON into a stable, map-key-sorted encoding.
func CanonicalJSON(raw []byte) ([]byte, error) {
	var v any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, errors.New("trailing JSON input")
	}
	return json.Marshal(v)
}
func hashJSON(raw []byte) (string, []byte, error) {
	c, err := CanonicalJSON(raw)
	if err != nil {
		return "", nil, err
	}
	h := sha256.Sum256(c)
	return hex.EncodeToString(h[:]), c, nil
}
