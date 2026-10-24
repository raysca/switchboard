package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"govoice/server/api"
	"govoice/server/storage"
)

func TestOperationsRouteUsesManagementBoundaryAndServesDiagnostics(t *testing.T) {
	s := apiTestServer(t)
	store, err := storage.Open(context.Background(), storage.Config{
		Dialect: storage.SQLite,
		DSN:     filepath.Join(t.TempDir(), "operations.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s.setStore(store)

	key := "management-test-key-32-bytes-long-and-separate"
	paths := []string{"/api/v1/operations/status"}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			// 1. Unauthenticated request rejected with 401
			unauth := apiRequest(s, http.MethodGet, path, "", "")
			if unauth.Code != http.StatusUnauthorized {
				t.Fatalf("unauthenticated status=%d, want 401", unauth.Code)
			}

			// 2. Wrong management key rejected with 401
			wrong := apiRequest(s, http.MethodGet, path, "invalid-management-key", "")
			if wrong.Code != http.StatusUnauthorized {
				t.Fatalf("wrong key status=%d, want 401", wrong.Code)
			}

			// 3. Voice token rejected on management route
			voice := apiRequest(s, http.MethodGet, path, s.cfg.Token.Secret, "")
			if voice.Code != http.StatusUnauthorized {
				t.Fatalf("voice token authenticated management API: %d", voice.Code)
			}

			// 4. Public listener returns 404
			public := httptest.NewRecorder()
			s.publicMux.ServeHTTP(public, httptest.NewRequest(http.MethodGet, path, nil))
			if public.Code != http.StatusNotFound {
				t.Fatalf("operations route exposed on public listener: %d", public.Code)
			}

			// 5. Method not allowed returns 405
			postReq := apiRequest(s, http.MethodPost, path, key, `{"action":"start"}`)
			if postReq.Code != http.StatusMethodNotAllowed {
				t.Fatalf("POST status=%d, want 405", postReq.Code)
			}

			// 6. Authorized GET succeeds
			authed := apiRequest(s, http.MethodGet, path, key, "")
			if authed.Code != http.StatusOK {
				t.Fatalf("authorized GET %s failed status=%d: %s", path, authed.Code, authed.Body.String())
			}

			if cc := authed.Header().Get("Cache-Control"); cc != "no-store" {
				t.Errorf("expected Cache-Control no-store, got %s", cc)
			}
			if reqID := authed.Header().Get("X-Request-ID"); reqID == "" {
				t.Error("missing X-Request-ID header")
			}

			var resp api.SuccessResponse[api.OperationsStatus]
			if err := json.Unmarshal(authed.Body.Bytes(), &resp); err != nil {
				t.Fatalf("json decode failed: %v", err)
			}

			data := resp.Data
			if data.Profile != string(s.cfg.Profile) {
				t.Errorf("got profile %s, want %s", data.Profile, s.cfg.Profile)
			}
			if data.Health != "healthy" {
				t.Errorf("got health %s, want healthy", data.Health)
			}
			if data.Database.Dialect != "sqlite" {
				t.Errorf("got dialect %s, want sqlite", data.Database.Dialect)
			}
			if data.Database.Status != "connected" {
				t.Errorf("got database status %s, want connected", data.Database.Status)
			}
			if data.Database.MigrationVersion != 10 {
				t.Errorf("got migration version %d, want 10", data.Database.MigrationVersion)
			}
			if data.VAD.WorkerStatus != "ready" {
				t.Errorf("got vad status %s, want ready", data.VAD.WorkerStatus)
			}
			if data.VAD.SampleRate != 16000 || data.VAD.FrameSize != 512 {
				t.Errorf("unexpected vad settings: %+v", data.VAD)
			}

			// 7. Verify no secrets leaked
			bodyStr := authed.Body.String()
			for _, sensitive := range []string{key, s.cfg.Token.Secret, "dsn", "password"} {
				if strings.Contains(strings.ToLower(bodyStr), strings.ToLower(sensitive)) {
					t.Errorf("leaked sensitive text %q in body: %s", sensitive, bodyStr)
				}
			}
		})
	}
}

func TestOperationsRouteWithoutStore_ReportsUnconfigured(t *testing.T) {
	s := apiTestServer(t)
	key := "management-test-key-32-bytes-long-and-separate"

	resp := apiRequest(s, http.MethodGet, "/api/v1/operations/status", key, "")
	if resp.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", resp.Code)
	}

	var statusResp api.SuccessResponse[api.OperationsStatus]
	if err := json.Unmarshal(resp.Body.Bytes(), &statusResp); err != nil {
		t.Fatal(err)
	}

	if statusResp.Data.Database.Status != "unconfigured" {
		t.Errorf("got db status %s, want unconfigured", statusResp.Data.Database.Status)
	}
	if statusResp.Data.Database.Dialect != "none" {
		t.Errorf("got dialect %s, want none", statusResp.Data.Database.Dialect)
	}
}
