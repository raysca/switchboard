package server

import (
	"log/slog"
	"sync/atomic"
)

// queueGreeting speaks a greeting through say (the assistant path's Say). A greeting never fails a call:
// if it cannot be queued (the session is closing) the failure is counted and logged and the call goes on.
// A TTS failure after queueing is already counted by the TTS error metrics.
func queueGreeting(say func(string) error, text string, failed *atomic.Uint64, logger *slog.Logger, sessionID string) {
	if err := say(text); err != nil {
		failed.Add(1)
		if logger != nil {
			logger.Warn("greeting not queued", "session", sessionID, "err", err.Error())
		}
	}
}
