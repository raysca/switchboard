package secrets_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"govoice/server/secrets"
	"govoice/server/storage"
)

func key(b byte) string { return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{b}, 32)) }

func newStore(t *testing.T, spec string) secrets.Store {
	t.Helper()
	db, err := storage.Open(t.Context(), storage.Config{Dialect: storage.SQLite, DSN: filepath.Join(t.TempDir(), "s.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	kr, err := secrets.ParseKeyring(spec)
	if err != nil {
		t.Fatal(err)
	}
	return secrets.Store{Repo: db.Secrets(), Keys: kr}
}

func TestStoreRoundTripAndNeverStoresPlaintext(t *testing.T) {
	s := newStore(t, "k1:"+key(1))
	ctx := t.Context()
	if err := s.Put(ctx, "crm-api", "sentinel-value-123"); err != nil {
		t.Fatal(err)
	}
	if v, ok := s.Resolve(ctx, "crm-api"); !ok || v != "sentinel-value-123" {
		t.Fatalf("resolve = %q, %v", v, ok)
	}
	row, err := s.Repo.Get(ctx, "crm-api")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(row.Ciphertext, []byte("sentinel")) || row.LastUsedAt.IsZero() {
		t.Fatalf("ciphertext leaks the value or use was not stamped: %+v", row)
	}
	// replacing keeps one row and the new value wins
	if err := s.Put(ctx, "crm-api", "second-value"); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Resolve(ctx, "crm-api"); v != "second-value" {
		t.Fatalf("replace failed: %q", v)
	}
	list, _ := s.List(ctx)
	if len(list) != 1 || list[0].Name != "crm-api" || list[0].Source != "store" {
		t.Fatalf("list = %+v", list)
	}
}

func TestStoreRejectsSwappedRowsWrongKeysAndBadInput(t *testing.T) {
	s := newStore(t, "k1:"+key(1))
	ctx := t.Context()
	_ = s.Put(ctx, "a-secret", "value-a")
	row, _ := s.Repo.Get(ctx, "a-secret")
	row.Name = "b-secret" // a copied row must not open under another name
	if err := s.Repo.Put(ctx, row); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Resolve(ctx, "b-secret"); ok {
		t.Fatal("a row copied to another name opened")
	}
	other := secrets.Store{Repo: s.Repo}
	other.Keys, _ = secrets.ParseKeyring("k1:" + key(2))
	if _, ok := other.Resolve(ctx, "a-secret"); ok {
		t.Fatal("a different master key opened the row")
	}
	for _, bad := range []string{"", "has space", "tab\there", strings.Repeat("x", secrets.MaxValueBytes+1)} {
		if err := s.Put(ctx, "c-secret", bad); !errors.Is(err, secrets.ErrBadValue) {
			t.Fatalf("value %q: %v", bad, err)
		}
	}
	if err := s.Put(ctx, "Bad Name", "v"); !errors.Is(err, secrets.ErrInvalidName) {
		t.Fatalf("name: %v", err)
	}
	if err := (secrets.Store{Repo: s.Repo}).Put(ctx, "ok-name", "v"); !errors.Is(err, secrets.ErrNoKey) {
		t.Fatalf("no key: %v", err)
	}
}

func TestKeyRotation(t *testing.T) {
	s := newStore(t, "k1:"+key(1))
	ctx := t.Context()
	_ = s.Put(ctx, "tok-a", "value-a")
	rotated := secrets.Store{Repo: s.Repo}
	rotated.Keys, _ = secrets.ParseKeyring("k2:" + key(2) + ",k1:" + key(1))
	if v, ok := rotated.Resolve(ctx, "tok-a"); !ok || v != "value-a" {
		t.Fatal("old key must still open old rows")
	}
	if n, err := rotated.Reseal(ctx); err != nil || n != 1 {
		t.Fatalf("reseal = %d, %v", n, err)
	}
	row, _ := s.Repo.Get(ctx, "tok-a")
	if row.KeyID != "k2" {
		t.Fatalf("key id after reseal = %q", row.KeyID)
	}
	onlyNew := secrets.Store{Repo: s.Repo}
	onlyNew.Keys, _ = secrets.ParseKeyring("k2:" + key(2))
	if v, ok := onlyNew.Resolve(ctx, "tok-a"); !ok || v != "value-a" {
		t.Fatal("old key can be dropped after reseal")
	}
}

func TestParseKeyringRejectsBadKeys(t *testing.T) {
	for _, spec := range []string{"nokey", "k1:" + base64.StdEncoding.EncodeToString([]byte("short")), "K1:" + key(1), "k1:" + key(1) + ",k1:" + key(2), "k1:!!!"} {
		if _, err := secrets.ParseKeyring(spec); !errors.Is(err, secrets.ErrInvalidKey) {
			t.Fatalf("spec %q accepted: %v", spec, err)
		}
	}
	if kr, err := secrets.ParseKeyring(" "); kr != nil || err != nil {
		t.Fatal("empty spec must disable the store")
	}
}

func TestEnvOnlyReadsTokenPrefixes(t *testing.T) {
	env := map[string]string{"MCP_TOKEN_A": " tok ", "HTTP_TOKEN_B": "has space", "GOVOICE_MANAGEMENT_API_KEY": "must-not-leak", "OPENAI_API_KEY": "x"}
	e := secrets.Env{Getenv: func(k string) string { return env[k] }}
	ctx := t.Context()
	if v, ok := e.Resolve(ctx, "MCP_TOKEN_A"); !ok || v != "tok" {
		t.Fatalf("trimmed token: %q %v", v, ok)
	}
	for _, name := range []string{"HTTP_TOKEN_B", "GOVOICE_MANAGEMENT_API_KEY", "OPENAI_API_KEY", "mcp_token_a", "MCP_TOKEN_"} {
		if _, ok := e.Resolve(ctx, name); ok {
			t.Fatalf("%s resolved", name)
		}
	}
	got := e.List([]string{"MCP_TOKEN_A=1", "OPENAI_API_KEY=2", "HTTP_TOKEN_B=3"})
	if len(got) != 1 || got[0].Name != "MCP_TOKEN_A" {
		t.Fatalf("list = %+v", got)
	}
}

func TestDirResolvesFilesAndStaysInside(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "secrets")
	_ = os.Mkdir(dir, 0o700)
	_ = os.WriteFile(filepath.Join(dir, "crm-api"), []byte("file-value\n"), 0o600)
	_ = os.WriteFile(filepath.Join(root, "outside"), []byte("outside-value"), 0o600)
	_ = os.Symlink(filepath.Join(root, "outside"), filepath.Join(dir, "escape")) // leaves the directory
	_ = os.Symlink(filepath.Join(dir, "crm-api"), filepath.Join(dir, "alias"))   // stays inside
	_ = os.WriteFile(filepath.Join(dir, "big"), bytes.Repeat([]byte("x"), secrets.MaxValueBytes+1), 0o600)
	d := secrets.Dir{Path: dir}
	ctx := t.Context()
	if v, ok := d.Resolve(ctx, "crm-api"); !ok || v != "file-value" {
		t.Fatalf("resolve = %q %v", v, ok)
	}
	if v, ok := d.Resolve(ctx, "alias"); !ok || v != "file-value" {
		t.Fatalf("inside symlink = %q %v", v, ok)
	}
	for _, name := range []string{"escape", "big", "../outside", "missing", ".hidden", "Crm-Api"} {
		if _, ok := d.Resolve(ctx, name); ok {
			t.Fatalf("%q resolved", name)
		}
	}
	_ = os.WriteFile(filepath.Join(dir, "crm-api"), []byte("rotated"), 0o600) // read afresh, no restart
	if v, _ := d.Resolve(ctx, "crm-api"); v != "rotated" {
		t.Fatalf("rotation not picked up: %q", v)
	}
	if (secrets.Dir{}).List() != nil {
		t.Fatal("empty path lists nothing")
	}
}

func TestChainOrder(t *testing.T) {
	first := secrets.Env{Getenv: func(string) string { return "from-env" }}
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "x-secret"), []byte("from-dir"), 0o600)
	c := secrets.Chain{first, secrets.Dir{Path: dir}}
	ctx := t.Context()
	if v, _ := c.Resolve(ctx, "MCP_TOKEN_X"); v != "from-env" {
		t.Fatal("env first")
	}
	if v, _ := c.Resolve(ctx, "x-secret"); v != "from-dir" {
		t.Fatal("falls through to dir")
	}
	if _, ok := c.Resolve(ctx, "nothing-here"); ok {
		t.Fatal("unknown name resolved")
	}
	if !secrets.ValidName("MCP_TOKEN_A") || !secrets.ValidName("crm-api") || secrets.ValidName("Crm") || secrets.ValidName("../x") {
		t.Fatal("name validation")
	}
	var _ context.Context = ctx
}
