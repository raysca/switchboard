package api

import "time"

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
	SessionRate                  float64        `json:"session_rate,omitempty"`
	MaxCallDurationSeconds       int            `json:"max_call_duration_seconds,omitempty"`
	VoiceTokenMaxLifetimeSeconds int            `json:"voice_token_max_lifetime_seconds,omitempty"`
	Database                     DatabaseStatus `json:"database"`
	VAD                          VADStatus      `json:"vad"`
	Health                       string         `json:"health"`
}
