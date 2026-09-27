package google

import (
	"sync"
)

// QuotaManager manages STT stream concurrency and guarantees rotation headroom.
// v1-lock.json specifies quota_headroom_multiplier: 2.0.
type QuotaManager struct {
	mu                 sync.Mutex
	maxStreams         int
	headroomMultiplier float64
	activeSessions     int
	activeStreams      int
	reservedStreams    int
}

// NewQuotaManager creates a new STT QuotaManager with the specified max concurrent streams.
func NewQuotaManager(maxStreams int, multiplier float64) *QuotaManager {
	if multiplier < 1.0 {
		multiplier = 2.0
	}
	return &QuotaManager{
		maxStreams:         maxStreams,
		headroomMultiplier: multiplier,
	}
}

// CanAdmitSession reports whether a new voice session can be admitted while guaranteeing
// that every active session has sufficient headroom (2x multiplier) to rotate its STT stream.
func (qm *QuotaManager) CanAdmitSession() bool {
	qm.mu.Lock()
	defer qm.mu.Unlock()

	if qm.maxStreams <= 0 {
		return true // Unbounded in test mode
	}

	// Projected required stream capacity if one more session is admitted
	projectedSessions := qm.activeSessions + 1
	neededCapacity := int(float64(projectedSessions) * qm.headroomMultiplier)
	return neededCapacity <= qm.maxStreams
}

// AdmitSession registers a newly admitted session.
func (qm *QuotaManager) AdmitSession() error {
	qm.mu.Lock()
	defer qm.mu.Unlock()

	if qm.maxStreams > 0 {
		neededCapacity := int(float64(qm.activeSessions+1) * qm.headroomMultiplier)
		if neededCapacity > qm.maxStreams {
			return ErrQuotaExhausted
		}
	}
	qm.activeSessions++
	qm.activeStreams++
	return nil
}

// ReleaseSession decrements an active session.
func (qm *QuotaManager) ReleaseSession() {
	qm.mu.Lock()
	defer qm.mu.Unlock()

	if qm.activeSessions > 0 {
		qm.activeSessions--
	}
	if qm.activeStreams > 0 {
		qm.activeStreams--
	}
}

// ReserveRotationStream attempts to reserve a second concurrent stream for rotation.
func (qm *QuotaManager) ReserveRotationStream() error {
	qm.mu.Lock()
	defer qm.mu.Unlock()

	if qm.maxStreams > 0 && qm.activeStreams+1 > qm.maxStreams {
		return ErrQuotaExhausted
	}
	qm.activeStreams++
	qm.reservedStreams++
	return nil
}

// ReleaseRotationStream releases the temporary second stream after rotation completes.
func (qm *QuotaManager) ReleaseRotationStream() {
	qm.mu.Lock()
	defer qm.mu.Unlock()

	if qm.reservedStreams > 0 {
		qm.reservedStreams--
	}
	if qm.activeStreams > 0 {
		qm.activeStreams--
	}
}

// ActiveSessions returns current active session count.
func (qm *QuotaManager) ActiveSessions() int {
	qm.mu.Lock()
	defer qm.mu.Unlock()
	return qm.activeSessions
}

// ActiveStreams returns total currently opened streams (including rotation streams).
func (qm *QuotaManager) ActiveStreams() int {
	qm.mu.Lock()
	defer qm.mu.Unlock()
	return qm.activeStreams
}

// ReservedStreams returns active rotation streams.
func (qm *QuotaManager) ReservedStreams() int {
	qm.mu.Lock()
	defer qm.mu.Unlock()
	return qm.reservedStreams
}
