package ws

import "time"

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
