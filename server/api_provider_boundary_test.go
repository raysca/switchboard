package server

import (
	"context"
	"encoding/json"
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
	service, err := credential.NewService(store.Credentials(), credential.Keyring{ActiveID: "api-k1", Keys: map[string][]byte{"api-k1": []byte(strings.Repeat("a", 32))}}, registry)
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
	request := httptest.NewRequest(http.MethodGet, "/api/v1/providers/no-such-route", nil)
	request.Header.Set("Authorization", "Bearer management-test-key-32-bytes-long-and-separate")
	request.Header.Set("X-Request-ID", "client-forged-request-id")
	responseRecorder := httptest.NewRecorder()
	s.adminMux.ServeHTTP(responseRecorder, request)
	var failure struct {
		Error struct {
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(responseRecorder.Body.Bytes(), &failure); err != nil {
		t.Fatal(err)
	}
	if responseRecorder.Code != http.StatusNotFound || responseRecorder.Header().Get("X-Request-ID") == "client-forged-request-id" || failure.Error.RequestID != responseRecorder.Header().Get("X-Request-ID") {
		t.Fatalf("outer request ID mismatch: %d header=%q error=%q body=%s", responseRecorder.Code, responseRecorder.Header().Get("X-Request-ID"), failure.Error.RequestID, responseRecorder.Body.String())
	}
}
