package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"govoice/server/api"
	"govoice/server/credential"
	"govoice/server/provider"
	"govoice/server/storage"
)

func TestProviderCredentialRoutesRemainManagementAuthenticated(t *testing.T) {
	s := apiTestServer(t)
	store, err := storage.Open(context.Background(), storage.Config{Dialect: storage.SQLite, DSN: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	registry := provider.NewRegistry(provider.Definitions())
	service, err := credential.NewService(store.Credentials(), credential.Keyring{ActiveID: "api-k1", Keys: map[string][]byte{"api-k1": []byte(strings.Repeat("a", 32))}}, registry, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.SetProviderManagementAPI(&api.ProviderManagement{Credentials: service, Registry: registry})
	const secret = "private-route-sentinel"
	path := "/api/v1/providers/credentials"
	if got := apiRequest(s, http.MethodPost, path, "", `{"provider":"google","secret":"`+secret+`"}`); got.Code != http.StatusUnauthorized || strings.Contains(got.Body.String(), secret) {
		t.Fatalf("unauthenticated provider route: %d %s", got.Code, got.Body.String())
	}
	response := apiRequest(s, http.MethodPost, path, "management-test-key-32-bytes-long-and-separate", `{"provider":"google","secret":"`+secret+`"}`)
	if response.Code != http.StatusCreated || strings.Contains(response.Body.String(), secret) {
		t.Fatalf("credential create: %d %s", response.Code, response.Body.String())
	}
	public := httptest.NewRecorder()
	s.publicMux.ServeHTTP(public, httptest.NewRequest("GET", path, nil))
	if public.Code != http.StatusNotFound {
		t.Fatalf("provider management route exposed on public listener: %d", public.Code)
	}
}
