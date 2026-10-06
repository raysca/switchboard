package api

import (
	"context"
	"encoding/json"
	"net/http"
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
	service, err := credential.NewService(store.Credentials(), credential.Keyring{ActiveID: "key-1", Keys: map[string][]byte{"key-1": stringBytes()}}, registry)
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
	get := httptest.NewRequest("GET", "/api/v1/providers/credentials/"+response.Data.ID, nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, get)
	if strings.Contains(w.Body.String(), secret) {
		t.Fatal("credential retrieval returned plaintext")
	}
}

func TestProviderHandlerIgnoresUntrustedRequestIDHeader(t *testing.T) {
	registry := provider.NewRegistry(provider.Definitions())
	store, err := storage.Open(context.Background(), storage.Config{Dialect: storage.SQLite, DSN: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := credential.NewService(store.Credentials(), credential.Keyring{ActiveID: "key-1", Keys: map[string][]byte{"key-1": stringBytes()}}, registry)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/providers/google/discovery?capability=mcp", nil)
	request.Header.Set("X-Request-ID", "attacker-controlled-request-id")
	w := httptest.NewRecorder()
	(&ProviderManagement{Credentials: service, Registry: registry}).ServeHTTP(w, request)
	var response struct {
		Error struct {
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if w.Header().Get("X-Request-ID") == "" || w.Header().Get("X-Request-ID") == "attacker-controlled-request-id" || response.Error.RequestID != w.Header().Get("X-Request-ID") {
		t.Fatalf("header/body request IDs: %q / %q body=%s", w.Header().Get("X-Request-ID"), response.Error.RequestID, w.Body.String())
	}
}

func stringBytes() []byte { return []byte(strings.Repeat("k", 32)) }
