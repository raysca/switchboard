package api

import (
	"context"
	"net/http"
	"time"
)

// DatabaseStatus represents read-only storage diagnostics for operational visibility.
type DatabaseStatus struct {
	Dialect          string `json:"dialect"`
	Status           string `json:"status"`
	OpenConnections  int    `json:"open_connections"`
	InUse            int    `json:"in_use"`
	MigrationVersion int64  `json:"migration_version,omitempty"`
}

// VADStatus represents voice activity detection worker status and parameters.
type VADStatus struct {
	WorkerStatus string `json:"worker_status"`
	SampleRate   int    `json:"sample_rate"`
	FrameSize    int    `json:"frame_size"`
}

// OperationsStatus provides bounded-cardinality operational telemetry and runtime view.
// It strictly excludes any secrets, passwords, connection DSNs, or bearer tokens.
type OperationsStatus struct {
	UptimeSeconds                float64        `json:"uptime_seconds"`
	StartedAt                    time.Time      `json:"started_at"`
	Profile                      string         `json:"profile"`
	ActiveSessions               int            `json:"active_sessions"`
	MaxSessions                  int            `json:"max_sessions"`
	CapacityUtilization          float64        `json:"capacity_utilization"`
	SessionsTotal                uint64         `json:"sessions_total"`
	TurnsTotal                   uint64         `json:"turns_total"`
	ErrorsTotal                  uint64         `json:"errors_total"`
	P95LatencyMs                 float64        `json:"p95_latency_ms"`
	AverageLatencyMs             float64        `json:"average_latency_ms,omitempty"`
	SessionRate                  float64        `json:"session_rate,omitempty"`
	MaxCallDurationSeconds       int            `json:"max_call_duration_seconds,omitempty"`
	VoiceTokenMaxLifetimeSeconds int            `json:"voice_token_max_lifetime_seconds,omitempty"`
	Database                     DatabaseStatus `json:"database"`
	VAD                          VADStatus      `json:"vad"`
	Health                       string         `json:"health"`
}

// OperationsProvider returns the current operations status.
type OperationsProvider interface {
	GetOperationsStatus(ctx context.Context) (OperationsStatus, error)
}

// OperationsHandler serves read-only operations status over HTTP.
type OperationsHandler struct {
	provider OperationsProvider
}

// NewOperationsHandler constructs an operations HTTP handler.
func NewOperationsHandler(provider OperationsProvider) *OperationsHandler {
	return &OperationsHandler{provider: provider}
}

func (h *OperationsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := RequestIDFromContext(r.Context())
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not supported", requestID)
		return
	}
	if h.provider == nil {
		WriteError(w, http.StatusServiceUnavailable, "operations_unavailable", "operations status provider unavailable", requestID)
		return
	}
	status, err := h.provider.GetOperationsStatus(r.Context())
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "internal_error", "failed to retrieve operations status", requestID)
		return
	}
	WriteJSON(w, http.StatusOK, SuccessResponse[OperationsStatus]{Data: status})
}
