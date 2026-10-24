// Package secrets resolves a secret name to its value at the moment a tool or MCP server needs it. Backends, in
// lookup order: the process environment (MCP_TOKEN_* and HTTP_TOKEN_*), a directory of files (any platform that
// mounts secrets as files), and an encrypted table in the server's own database. A value is never returned by an API,
// logged or stored in plaintext; the encrypted store seals each one with the deployment master key.
package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"govoice/server/storage"
)

const (
	// MaxValueBytes bounds a secret value; tokens and API keys are far smaller.
	MaxValueBytes = 4096
	sourceEnv     = "environment"
	sourceDir     = "directory"
	sourceStore   = "store"
)

var (
	// Environment secrets keep the prefix rule MCP tokens always had: a caller can never make the server send an
	// unrelated variable (a provider key, the management key) to an endpoint they choose.
	envNameRe = regexp.MustCompile(`^(MCP|HTTP)_TOKEN_[A-Z0-9_]{1,48}$`)
	// Directory and store secrets are named in lower case, so the two name spaces cannot overlap.
	slugNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

	ErrInvalidName = errors.New("secrets: invalid name")
	ErrInvalidKey  = errors.New("secrets: master key must be 32 bytes, base64 encoded")
	ErrNoKey       = errors.New("secrets: no master key is configured, so the encrypted store is disabled")
	ErrBadValue    = errors.New("secrets: value is empty, too long, or contains whitespace or control characters")
	ErrNotFound    = errors.New("secrets: not found")
)

// ValidName reports whether name is a legal environment secret (MCP_TOKEN_X) or a stored or directory secret (crm-api).
func ValidName(name string) bool { return envNameRe.MatchString(name) || slugNameRe.MatchString(name) }

// ValidStoredName is ValidName for secrets the encrypted store may hold (lower case only).
func ValidStoredName(name string) bool { return slugNameRe.MatchString(name) }

// ValidValue reports whether v is usable as a header value.
func ValidValue(v string) bool {
	return v != "" && len(v) <= MaxValueBytes && strings.IndexFunc(v, func(c rune) bool { return c <= ' ' || c == 0x7f }) < 0
}

// Resolver turns a secret name into its value. ok is false when the name does not resolve for any reason; callers
// report only "missing", never why, so a response cannot reveal which backend holds a name.
type Resolver interface {
	Resolve(ctx context.Context, name string) (value string, ok bool)
}

// Info describes a secret without its value.
type Info struct {
	Name   string
	Source string // "environment", "directory" or "store"
}

// Chain tries each resolver in order.
type Chain []Resolver

func (c Chain) Resolve(ctx context.Context, name string) (string, bool) {
	for _, r := range c {
		if v, ok := r.Resolve(ctx, name); ok {
			return v, true
		}
	}
	return "", false
}

// Env resolves MCP_TOKEN_* and HTTP_TOKEN_* variables.
type Env struct{ Getenv func(string) string }

func (e Env) Resolve(_ context.Context, name string) (string, bool) {
	if !envNameRe.MatchString(name) {
		return "", false
	}
	v := strings.TrimSpace(e.Getenv(name))
	return v, ValidValue(v)
}

// List returns the environment secrets that are set to a usable value.
func (e Env) List(environ []string) []Info {
	var out []Info
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if _, ok := e.Resolve(context.Background(), name); ok {
			out = append(out, Info{name, sourceEnv})
		}
	}
	return out
}

// Dir resolves a secret from the file of that name in a directory. Symbolic links are followed only while they stay
// inside the directory (Kubernetes and Docker mount secrets that way), and the file is read afresh every time, so
// a rotated secret takes effect without a restart.
type Dir struct{ Path string }

func (d Dir) Resolve(_ context.Context, name string) (string, bool) {
	if d.Path == "" || !slugNameRe.MatchString(name) {
		return "", false
	}
	root, err := filepath.EvalSymlinks(d.Path)
	if err != nil {
		return "", false
	}
	file, err := filepath.EvalSymlinks(filepath.Join(root, name))
	if err != nil || !strings.HasPrefix(file, root+string(filepath.Separator)) {
		return "", false
	}
	f, err := os.Open(file)
	if err != nil {
		return "", false
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return "", false
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxValueBytes+1))
	if err != nil {
		return "", false
	}
	v := strings.TrimRight(string(b), "\r\n")
	return v, ValidValue(v)
}

// List returns the readable secrets in the directory.
func (d Dir) List() []Info {
	if d.Path == "" {
		return nil
	}
	entries, err := os.ReadDir(d.Path)
	if err != nil {
		return nil
	}
	var out []Info
	for _, e := range entries {
		if _, ok := d.Resolve(context.Background(), e.Name()); ok {
			out = append(out, Info{e.Name(), sourceDir})
		}
	}
	return out
}

// Keyring holds the master keys. The first key seals new values; every key can open old ones, so a key is rotated by
// putting the new one first, re-sealing (Store.Reseal), and then dropping the old one.
type Keyring struct {
	ids  []string
	keys map[string]cipher.AEAD
}

// ParseKeyring reads "id:base64key[,id:base64key...]". An empty spec gives a nil keyring (the store is disabled).
func ParseKeyring(spec string) (*Keyring, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}
	kr := &Keyring{keys: map[string]cipher.AEAD{}}
	for _, part := range strings.Split(spec, ",") {
		id, enc, ok := strings.Cut(strings.TrimSpace(part), ":")
		if !ok || !slugNameRe.MatchString(id) {
			return nil, fmt.Errorf("%w: each entry is id:key with a lower-case id", ErrInvalidKey)
		}
		raw, err := base64.StdEncoding.DecodeString(enc)
		if err != nil || len(raw) != 32 {
			return nil, ErrInvalidKey
		}
		block, err := aes.NewCipher(raw)
		if err != nil {
			return nil, ErrInvalidKey
		}
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			return nil, ErrInvalidKey
		}
		if _, dup := kr.keys[id]; dup {
			return nil, fmt.Errorf("%w: duplicate key id %q", ErrInvalidKey, id)
		}
		kr.ids = append(kr.ids, id)
		kr.keys[id] = gcm
	}
	return kr, nil
}

// associated binds a ciphertext to its secret name and key id, so a row copied under another name does not open.
func associated(keyID, name string) []byte { return []byte(keyID + "\x00" + name) }

func (k *Keyring) seal(name, value string) (keyID string, nonce, ct []byte, err error) {
	keyID = k.ids[0]
	aead := k.keys[keyID]
	nonce = make([]byte, aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return "", nil, nil, err
	}
	return keyID, nonce, aead.Seal(nil, nonce, []byte(value), associated(keyID, name)), nil
}

func (k *Keyring) open(name, keyID string, nonce, ct []byte) (string, error) {
	aead, ok := k.keys[keyID]
	if !ok || len(nonce) != aead.NonceSize() {
		return "", ErrNotFound
	}
	pt, err := aead.Open(nil, nonce, ct, associated(keyID, name))
	if err != nil {
		return "", ErrNotFound
	}
	return string(pt), nil
}

// Store is the encrypted table. A nil keyring leaves it disabled: Resolve finds nothing and Put fails with ErrNoKey.
type Store struct {
	Repo storage.SecretsRepository
	Keys *Keyring
}

func (s Store) Enabled() bool { return s.Keys != nil }

func (s Store) Resolve(ctx context.Context, name string) (string, bool) {
	if s.Keys == nil || !slugNameRe.MatchString(name) {
		return "", false
	}
	row, err := s.Repo.Get(ctx, name)
	if err != nil {
		return "", false
	}
	v, err := s.Keys.open(row.Name, row.KeyID, row.Nonce, row.Ciphertext)
	if err != nil || !ValidValue(v) {
		return "", false
	}
	_ = s.Repo.Touch(ctx, name) // best effort: a failed use stamp must not fail the call
	return v, true
}

// Put seals value under name, creating or replacing the secret.
func (s Store) Put(ctx context.Context, name, value string) error {
	if s.Keys == nil {
		return ErrNoKey
	}
	if !slugNameRe.MatchString(name) {
		return ErrInvalidName
	}
	if !ValidValue(value) {
		return ErrBadValue
	}
	keyID, nonce, ct, err := s.Keys.seal(name, value)
	if err != nil {
		return err
	}
	return s.Repo.Put(ctx, storage.SecretRow{Name: name, KeyID: keyID, Nonce: nonce, Ciphertext: ct})
}

// Delete removes a stored secret; the caller checks first that nothing references it.
func (s Store) Delete(ctx context.Context, name string) error { return s.Repo.Delete(ctx, name) }

// List returns the stored secrets without values.
func (s Store) List(ctx context.Context) ([]Info, error) {
	rows, err := s.Repo.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Info, 0, len(rows))
	for _, r := range rows {
		out = append(out, Info{r.Name, sourceStore})
	}
	return out, nil
}

// Reseal re-encrypts every secret under the first key and returns how many it rewrote. A secret that no key opens is
// reported as an error and left alone.
func (s Store) Reseal(ctx context.Context) (int, error) {
	if s.Keys == nil {
		return 0, ErrNoKey
	}
	rows, err := s.Repo.List(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range rows {
		if r.KeyID == s.Keys.ids[0] {
			continue
		}
		v, err := s.Keys.open(r.Name, r.KeyID, r.Nonce, r.Ciphertext)
		if err != nil {
			return n, fmt.Errorf("secret %q: no configured key opens it", r.Name)
		}
		if err := s.Put(ctx, r.Name, v); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
