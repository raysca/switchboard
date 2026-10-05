package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"govoice/server/credential"
	"govoice/server/provider"
	"govoice/server/storage"
)

func TestProviderCredentialAPIOnlyReturnsMaskedValues(t *testing.T) {
	store, err := storage.Open(context.Background(), storage.Config{Dialect: storage.SQLite, DSN: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	registry := provider.NewRegistry(provider.Definitions())
	service, err := credential.NewService(store.Credentials(), credential.Keyring{ActiveID: "key-1", Keys: map[string][]byte{"key-1": stringBytes()}}, registry, credential.ValidatorFunc(func(context.Context, string, string) (credential.ValidationResult, error) {
		return credential.ValidationResult{}, credential.ErrInvalidCredential
	}))
	if err != nil {
		t.Fatal(err)
	}
	handler := &ProviderManagement{Credentials: service, Registry: registry}
	const secret = "api-secret-sentinel-value"
	request := httptest.NewRequest("POST", "/api/v1/providers/credentials", strings.NewReader(`{"provider":"google","secret":"`+secret+`"}`))
	request.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, request)
	if w.Code != 201 || strings.Contains(w.Body.String(), secret) {
		t.Fatalf("create status/body=%d %s", w.Code, w.Body.String())
	}
	var response struct {
		Data credential.CredentialDTO `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Data.MaskedValue != "••••••alue" {
		t.Fatalf("value not masked: %q", response.Data.MaskedValue)
	}
	if stored, err := store.Credentials().Get(context.Background(), response.Data.ID); err != nil || strings.Contains(string(stored.Ciphertext), secret) {
		t.Fatalf("secret not encrypted: %v", err)
	}
	validate := httptest.NewRequest("POST", "/api/v1/providers/credentials/"+response.Data.ID+"/validate", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, validate)
	if w.Code != 200 || strings.Contains(w.Body.String(), secret) || !strings.Contains(w.Body.String(), `"validation_state":"invalid"`) {
		t.Fatalf("validation status/body=%d %s", w.Code, w.Body.String())
	}
	get := httptest.NewRequest("GET", "/api/v1/providers/credentials/"+response.Data.ID, nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, get)
	if strings.Contains(w.Body.String(), secret) {
		t.Fatal("credential retrieval returned plaintext")
	}
}

func TestProviderDiscoveryAPIHasStableUnsupportedResponse(t *testing.T) {
	registry := provider.NewRegistry(provider.Definitions())
	store, err := storage.Open(context.Background(), storage.Config{Dialect: storage.SQLite, DSN: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := credential.NewService(store.Credentials(), credential.Keyring{ActiveID: "key-1", Keys: map[string][]byte{"key-1": []byte(strings.Repeat("k", 32))}}, registry, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := &ProviderManagement{Credentials: service, Registry: registry}
	r := httptest.NewRequest("GET", "/api/v1/providers/google/discovery?capability=mcp", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 400 || !strings.Contains(w.Body.String(), `"code":"unsupported_capability"`) {
		t.Fatalf("status/body=%d %s", w.Code, w.Body.String())
	}
}

func stringBytes() []byte { return []byte(strings.Repeat("k", 32)) }
