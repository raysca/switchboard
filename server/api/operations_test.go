package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"govoice/server/api"
)

type mockOperationsProvider struct {
	status api.OperationsStatus
	err    error
}

func (m *mockOperationsProvider) GetOperationsStatus(ctx context.Context) (api.OperationsStatus, error) {
	return m.status, m.err
}

func TestOperationsHandler_Success(t *testing.T) {
	now := time.Now().UTC()
	expected := api.OperationsStatus{
		UptimeSeconds:       3600.5,
		StartedAt:           now,
		Profile:             "development",
		ActiveSessions:      3,
		MaxSessions:         100,
		CapacityUtilization: 0.03,
		SessionsTotal:       42,
		TurnsTotal:          150,
		ErrorsTotal:         1,
		SessionRate:         0.012,
		Database: api.DatabaseStatus{
			Dialect:          "sqlite",
			Status:           "connected",
			OpenConnections:  1,
			InUse:            0,
			MigrationVersion: 5,
		},
		VAD: api.VADStatus{
			WorkerStatus: "ready",
			SampleRate:   16000,
			FrameSize:    512,
		},
		Health: "healthy",
	}

	handler := api.NewOperationsHandler(&mockOperationsProvider{status: expected})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/operations/status", nil)
	req = req.WithContext(api.WithRequestID(req.Context(), "test-req-123"))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("expected application/json, got %s", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("expected no-store, got %s", cc)
	}

	var resp api.SuccessResponse[api.OperationsStatus]
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	data := resp.Data
	if data.Profile != "development" {
		t.Errorf("expected profile development, got %s", data.Profile)
	}
	if data.ActiveSessions != 3 {
		t.Errorf("expected active sessions 3, got %d", data.ActiveSessions)
	}
	if data.MaxSessions != 100 {
		t.Errorf("expected max sessions 100, got %d", data.MaxSessions)
	}
	if data.Health != "healthy" {
		t.Errorf("expected health healthy, got %s", data.Health)
	}
	if data.Database.Dialect != "sqlite" || data.Database.Status != "connected" {
		t.Errorf("unexpected database status: %+v", data.Database)
	}
	if data.VAD.WorkerStatus != "ready" || data.VAD.SampleRate != 16000 {
		t.Errorf("unexpected vad status: %+v", data.VAD)
	}

	// Verify absence of sensitive words / credentials
	body := rec.Body.String()
	for _, forbidden := range []string{"password", "secret", "bearer", "token", "DSN", "private"} {
		if strings.Contains(strings.ToLower(body), forbidden) {
			t.Errorf("response body leaked sensitive keyword %q: %s", forbidden, body)
		}
	}
}

func TestOperationsHandler_MethodNotAllowed(t *testing.T) {
	handler := api.NewOperationsHandler(&mockOperationsProvider{})
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req := httptest.NewRequest(method, "/api/v1/operations/status", nil)
		req = req.WithContext(api.WithRequestID(req.Context(), "test-req-405"))
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("method %s: expected status 405, got %d", method, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow != "GET" {
			t.Errorf("method %s: expected Allow GET header, got %s", method, allow)
		}
	}
}

func TestOperationsHandler_ProviderUnavailable(t *testing.T) {
	handler := api.NewOperationsHandler(nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/operations/status", nil)
	req = req.WithContext(api.WithRequestID(req.Context(), "test-req-nil"))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected status 503, got %d", rec.Code)
	}
}

func TestOperationsHandler_ProviderError(t *testing.T) {
	handler := api.NewOperationsHandler(&mockOperationsProvider{err: errors.New("db query failed")})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/operations/status", nil)
	req = req.WithContext(api.WithRequestID(req.Context(), "test-req-err"))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected status 500, got %d", rec.Code)
	}
}

func TestOperationsHandler_DegradedAndUnhealthyStates(t *testing.T) {
	tests := []struct {
		name       string
		status     api.OperationsStatus
		wantHealth string
	}{
		{
			name: "degraded capacity",
			status: api.OperationsStatus{
				Health:              "degraded",
				ActiveSessions:      100,
				MaxSessions:         100,
				CapacityUtilization: 1.0,
			},
			wantHealth: "degraded",
		},
		{
			name: "unhealthy database",
			status: api.OperationsStatus{
				Health: "unhealthy",
				Database: api.DatabaseStatus{
					Dialect: "postgres",
					Status:  "error",
				},
			},
			wantHealth: "unhealthy",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := api.NewOperationsHandler(&mockOperationsProvider{status: tc.status})
			req := httptest.NewRequest(http.MethodGet, "/api/v1/operations/status", nil)
			req = req.WithContext(api.WithRequestID(req.Context(), "test-req-state"))
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("expected status 200, got %d", rec.Code)
			}
			var resp api.SuccessResponse[api.OperationsStatus]
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("json unmarshal failed: %v", err)
			}
			if resp.Data.Health != tc.wantHealth {
				t.Errorf("expected health %s, got %s", tc.wantHealth, resp.Data.Health)
			}
		})
	}
}
