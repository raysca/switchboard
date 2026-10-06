package credential

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"govoice/server/provider"
	"govoice/server/storage"
)

func TestCredentialEncryptsMasksAndNeverReturnsSecret(t *testing.T) {
	keys := Keyring{ActiveID: "primary", Keys: map[string][]byte{"primary": bytesOf(32, 1)}}
	registry := provider.NewRegistry(provider.Definitions())
	repo := newMemoryRepository()
	service, err := NewService(repo, keys, registry)
	if err != nil {
		t.Fatal(err)
	}
	const secret = "credential-sentinel-123456"
	got, err := service.Create(context.Background(), CreateInput{Provider: "google", Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	if got.MaskedValue != "••••••3456" || got.ValidationState != ValidationUnvalidated {
		t.Fatalf("unexpected DTO: %+v", got)
	}
	if strings.Contains(strings.ToLower(strings.TrimSpace(got.String())), secret) {
		t.Fatal("DTO string exposed secret")
	}
	row := repo.rows[got.ID]
	if strings.Contains(string(row.Ciphertext), secret) || row.KeyID != "primary" {
		t.Fatal("secret was not encrypted under the active key")
	}
	var logOutput strings.Builder
	logger := slog.New(slog.NewTextHandler(&logOutput, nil))
	logger.Info("credential stored", "credential", got)
	if strings.Contains(logOutput.String(), secret) {
		t.Fatal("credential log exposed secret")
	}
}

func TestCredentialRejectsTamperedCiphertextAndUnknownKey(t *testing.T) {
	keys := Keyring{ActiveID: "k1", Keys: map[string][]byte{"k1": bytesOf(32, 2)}}
	repo := newMemoryRepository()
	svc, _ := NewService(repo, keys, provider.NewRegistry(provider.Definitions()))
	created, err := svc.Create(context.Background(), CreateInput{Provider: "google", Secret: "safe-secret"})
	if err != nil {
		t.Fatal(err)
	}
	original := cloneCredential(repo.rows[created.ID])
	wrongKeyService, _ := NewService(repo, Keyring{ActiveID: "k1", Keys: map[string][]byte{"k1": bytesOf(32, 99)}}, provider.NewRegistry(provider.Definitions()))
	if _, err := wrongKeyService.decrypt(original); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("wrong key error=%v", err)
	}
	moved := cloneCredential(original)
	moved.Provider = "xai"
	if _, err := svc.decrypt(moved); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("associated provider change error=%v", err)
	}
	row := repo.rows[created.ID]
	row.Ciphertext[len(row.Ciphertext)-1] ^= 0xff
	repo.rows[created.ID] = row
	if _, err := svc.decrypt(repo.rows[created.ID]); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("tampering error=%v", err)
	}
	row = repo.rows[created.ID]
	row.Ciphertext = []byte("opaque")
	row.KeyID = "missing"
	repo.rows[created.ID] = row
	if _, err := svc.decrypt(repo.rows[created.ID]); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("unknown key error=%v", err)
	}
}

func TestCredentialRotationReencryptsWithoutChangingSecret(t *testing.T) {
	oldKeys := Keyring{ActiveID: "old", Keys: map[string][]byte{"old": bytesOf(32, 3)}}
	repo := newMemoryRepository()
	svc, _ := NewService(repo, oldKeys, provider.NewRegistry(provider.Definitions()))
	created, err := svc.Create(context.Background(), CreateInput{Provider: "google", Secret: "rotate-me-secret"})
	if err != nil {
		t.Fatal(err)
	}
	rotator, err := NewService(repo, Keyring{ActiveID: "new", Keys: map[string][]byte{"old": bytesOf(32, 3), "new": bytesOf(32, 4)}}, provider.NewRegistry(provider.Definitions()))
	if err != nil {
		t.Fatal(err)
	}
	if err := rotator.Rotate(context.Background(), created.ID); err != nil {
		t.Fatal(err)
	}
	if repo.rows[created.ID].KeyID != "new" {
		t.Fatal("rotation did not update key ID")
	}
	secret, err := rotator.decrypt(repo.rows[created.ID])
	if err != nil || string(secret) != "rotate-me-secret" {
		t.Fatalf("secret=%q err=%v", secret, err)
	}
}

func bytesOf(n int, value byte) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = value
	}
	return out
}

type memoryRepository struct{ rows map[string]storage.Credential }

func newMemoryRepository() *memoryRepository {
	return &memoryRepository{rows: make(map[string]storage.Credential)}
}
func (r *memoryRepository) Put(_ context.Context, c storage.Credential) error {
	if _, exists := r.rows[c.ID]; exists {
		return errors.New("duplicate")
	}
	c.CreatedAt, c.UpdatedAt = time.Now().UTC(), time.Now().UTC()
	r.rows[c.ID] = cloneCredential(c)
	return nil
}
func (r *memoryRepository) ReplaceIfUnchanged(_ context.Context, expectedKeyID string, expectedCiphertext []byte, c storage.Credential) error {
	old, exists := r.rows[c.ID]
	if !exists || old.KeyID != expectedKeyID || !bytes.Equal(old.Ciphertext, expectedCiphertext) {
		return errors.New("credential changed")
	}
	c.UpdatedAt = time.Now().UTC()
	r.rows[c.ID] = cloneCredential(c)
	return nil
}
func (r *memoryRepository) RotateIfUnchanged(_ context.Context, id, expectedKeyID string, expectedCiphertext []byte, keyID string, ciphertext []byte, maskedSuffix string) error {
	old, exists := r.rows[id]
	if !exists || old.KeyID != expectedKeyID || !bytes.Equal(old.Ciphertext, expectedCiphertext) {
		return errors.New("credential changed")
	}
	old.KeyID, old.Ciphertext, old.MaskedSuffix, old.UpdatedAt = keyID, append([]byte(nil), ciphertext...), maskedSuffix, time.Now().UTC()
	r.rows[id] = old
	return nil
}
func (r *memoryRepository) Get(_ context.Context, id string) (storage.Credential, error) {
	c, exists := r.rows[id]
	if !exists {
		return c, ErrNotFound
	}
	return cloneCredential(c), nil
}
func (r *memoryRepository) Delete(_ context.Context, id string) error {
	if _, exists := r.rows[id]; !exists {
		return ErrNotFound
	}
	delete(r.rows, id)
	return nil
}
func (r *memoryRepository) List(context.Context) ([]storage.Credential, error) {
	out := make([]storage.Credential, 0, len(r.rows))
	for _, c := range r.rows {
		out = append(out, cloneCredential(c))
	}
	return out, nil
}
func cloneCredential(c storage.Credential) storage.Credential {
	c.Ciphertext = append([]byte(nil), c.Ciphertext...)
	return c
}

func TestCredentialUpdateDeleteAndInputValidation(t *testing.T) {
	repo := newMemoryRepository()
	svc, _ := NewService(repo, Keyring{ActiveID: "new", Keys: map[string][]byte{"new": bytesOf(32, 8)}}, provider.NewRegistry(provider.Definitions()))
	for _, secret := range []string{"", " \t ", "bad\nsecret", strings.Repeat("x", MaxSecretBytes+1)} {
		if _, err := svc.Create(context.Background(), CreateInput{Provider: "google", Secret: secret}); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("secret %q error=%v", secret, err)
		}
	}
	if _, err := svc.Create(context.Background(), CreateInput{Provider: "made-up", Secret: "some-key"}); !errors.Is(err, ErrUnsupportedProvider) {
		t.Fatalf("unsupported provider error=%v", err)
	}
	created, err := svc.Create(context.Background(), CreateInput{Provider: "google", Secret: "first-secret"})
	if err != nil {
		t.Fatal(err)
	}
	persisted, _ := repo.Get(context.Background(), created.ID)
	if created.CreatedAt.IsZero() || created.UpdatedAt.IsZero() || !created.UpdatedAt.Equal(persisted.UpdatedAt) {
		t.Fatalf("create DTO timestamps are not persisted values: dto=%+v row=%+v", created, persisted)
	}
	updated, err := svc.Update(context.Background(), created.ID, UpdateInput{Secret: "replacement-secret"})
	if err != nil || updated.ValidationState != ValidationUnvalidated || updated.MaskedValue != "••••••cret" {
		t.Fatalf("updated=%+v err=%v", updated, err)
	}
	persisted, _ = repo.Get(context.Background(), created.ID)
	if updated.UpdatedAt.IsZero() || !updated.UpdatedAt.Equal(persisted.UpdatedAt) {
		t.Fatalf("update DTO timestamp stale: dto=%s row=%s", updated.UpdatedAt, persisted.UpdatedAt)
	}
	plain, err := svc.decrypt(repo.rows[created.ID])
	if err != nil || string(plain) != "replacement-secret" {
		t.Fatalf("updated secret=%q err=%v", plain, err)
	}
	if err := svc.Delete(context.Background(), created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get(context.Background(), created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted credential read error=%v", err)
	}
}

type rotationBarrierRepository struct {
	base         *memoryRepository
	atRotate     chan struct{}
	resumeRotate chan struct{}
	once         sync.Once
}

func (r *rotationBarrierRepository) Put(ctx context.Context, c storage.Credential) error {
	return r.base.Put(ctx, c)
}
func (r *rotationBarrierRepository) ReplaceIfUnchanged(ctx context.Context, k string, b []byte, c storage.Credential) error {
	return r.base.ReplaceIfUnchanged(ctx, k, b, c)
}
func (r *rotationBarrierRepository) RotateIfUnchanged(ctx context.Context, id, oldKey string, oldCipher []byte, newKey string, newCipher []byte, masked string) error {
	r.once.Do(func() { close(r.atRotate); <-r.resumeRotate })
	return r.base.RotateIfUnchanged(ctx, id, oldKey, oldCipher, newKey, newCipher, masked)
}
func (r *rotationBarrierRepository) Get(ctx context.Context, id string) (storage.Credential, error) {
	return r.base.Get(ctx, id)
}
func (r *rotationBarrierRepository) Delete(ctx context.Context, id string) error {
	return r.base.Delete(ctx, id)
}
func (r *rotationBarrierRepository) List(ctx context.Context) ([]storage.Credential, error) {
	return r.base.List(ctx)
}

func TestResolveOnlyReleasesMCPTokens(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, storage.Config{Dialect: storage.SQLite, DSN: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	svc, err := NewService(store.Credentials(), Keyring{ActiveID: "k", Keys: map[string][]byte{"k": []byte(strings.Repeat("k", 32))}}, provider.NewRegistry(provider.Definitions()))
	if err != nil {
		t.Fatal(err)
	}
	apiKey, err := svc.Create(ctx, CreateInput{Provider: "google", Secret: "provider-api-key-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Resolve(ctx, apiKey.ID); !errors.Is(err, ErrNotMCPCredential) {
		t.Fatalf("provider API key released to MCP: err=%v", err)
	}
	token, err := svc.Create(ctx, CreateInput{Provider: provider.MCPProviderID, Secret: "mcp-bearer-token"})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := svc.Resolve(ctx, token.ID); err != nil || got != "mcp-bearer-token" {
		t.Fatalf("mcp token resolve = %q, %v", got, err)
	}
	if _, err := svc.Resolve(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing credential err=%v", err)
	}
}

func TestProviderSecretIsProviderScoped(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, storage.Config{Dialect: storage.SQLite, DSN: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	svc, err := NewService(store.Credentials(), Keyring{ActiveID: "k", Keys: map[string][]byte{"k": []byte(strings.Repeat("k", 32))}}, provider.NewRegistry(provider.Definitions()))
	if err != nil {
		t.Fatal(err)
	}
	g, _ := svc.Create(ctx, CreateInput{Provider: "google", Secret: "google-api-key"})
	m, _ := svc.Create(ctx, CreateInput{Provider: provider.MCPProviderID, Secret: "mcp-token"})
	if got, err := svc.ProviderSecret(ctx, g.ID, "google"); err != nil || string(got) != "google-api-key" {
		t.Fatalf("google secret = %q, %v", got, err)
	}
	if _, err := svc.ProviderSecret(ctx, g.ID, "xai"); !errors.Is(err, ErrProviderMismatch) {
		t.Fatalf("cross-provider read err=%v", err)
	}
	if _, err := svc.ProviderSecret(ctx, m.ID, provider.MCPProviderID); !errors.Is(err, ErrProviderMismatch) {
		t.Fatalf("MCP token released on call path err=%v", err)
	}
	if _, err := svc.ProviderSecret(ctx, "missing", "google"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing err=%v", err)
	}
}

func TestMultilineSecretsAcceptedButOtherControlsRejected(t *testing.T) {
	if err := validateSecret("google", "{\n  \"type\": \"service_account\"\n}\n"); err != nil {
		t.Fatalf("pretty-printed JSON rejected: %v", err)
	}
	if err := validateSecret("google", "key\x00injected"); err == nil {
		t.Fatal("NUL accepted")
	}
	if err := validateSecret("google", "api-key\nX-Injected: 1"); err == nil {
		t.Fatal("newline accepted in a non-JSON key")
	}
	for _, p := range []string{"xai", "mcp"} {
		if err := validateSecret(p, "{\n\"token\": 1}"); err == nil {
			t.Fatalf("%s accepted a multi-line secret", p)
		}
	}
}

// A credential with no validated capabilities serialises "capabilities": [],
// never null (the admin UI's schema rejects null).
func TestCredentialDTOCapabilitiesNeverNull(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, storage.Config{Dialect: storage.SQLite, DSN: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	svc, err := NewService(store.Credentials(), Keyring{ActiveID: "k", Keys: map[string][]byte{"k": []byte(strings.Repeat("k", 32))}}, provider.NewRegistry(provider.Definitions()))
	if err != nil {
		t.Fatal(err)
	}
	dto, err := svc.Create(ctx, CreateInput{Provider: "google", Secret: "abc-key-123"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(dto)
	if !strings.Contains(string(b), `"capabilities":[]`) {
		t.Fatalf("capabilities not an empty array: %s", b)
	}
}
