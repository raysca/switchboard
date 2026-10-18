package ws

import "time"

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
