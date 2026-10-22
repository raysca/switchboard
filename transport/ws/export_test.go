package ws

import (
	"time"

	"govoice/transport/token"
)

// Pacer exposes the connection's PacedWriter to the ws_test package.
func (c *Connection) Pacer() *PacedWriter { return c.pacer }

// SetTestHooks installs the write-loop synchronization hooks (either may be nil).
func (w *PacedWriter) SetTestHooks(afterSelect, beforeAdmission func()) {
	w.afterSelect, w.beforeWriteAdmission = afterSelect, beforeAdmission
}

func (w *PacedWriter) QueuedDuration() time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.queuedDur
}

func (w *PacedWriter) QueuedBytes() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.queuedBytes
}

func (w *PacedWriter) PendingFinalMarkers() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.pendingFinals)
}

func (w *PacedWriter) ActiveGeneration() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.activeGen
}

// DefaultServerConfig returns a test/dev default: it allows any origin and a
// missing Origin header. The server builds its own ServerConfig.
func DefaultServerConfig(val *token.Validator) ServerConfig {
	return ServerConfig{
		Security: SecurityConfig{
			AllowedOrigins:     []string{"*"},
			AllowMissingOrigin: true,
		},
		TokenValidator:             val,
		Pacer:                      DefaultPacerConfig(),
		HandshakeTimeout:           5 * time.Second,
		RTVIReadyTimeout:           5 * time.Second,
		ReadTimeout:                30 * time.Second,
		PingInterval:               15 * time.Second,
		PingTimeout:                10 * time.Second,
		PingMaxFailures:            2,
		MaxConnectionLifetime:      60 * time.Minute,
		MaxConcurrentSessions:      100,
		MaxConcurrentSessionsPerIP: 10,
		MaxPendingHandshakes:       50,
		HandshakeRateLimit:         20.0,
		FixedPlayerRate:            FixedPlayerSampleRate,
	}
}
