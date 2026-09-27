package voice

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

var ErrAdmission = errors.New("admission rejected")

// Manager is the host runtime: session registry, watchdog, admission.
type Manager struct {
	mu       sync.RWMutex
	sessions map[uint64]*Session
	nextID   atomic.Uint64

	// Admit, if set, is consulted before creating a session (nil error = admit).
	Admit atomic.Pointer[func() error]

	// Host-wide metrics.
	VADLatency     Hist // all sessions: chunk due -> VAD result applied
	managed        atomic.Int64
	loops          atomic.Int64
	leakedSessions atomic.Int64
	failures       atomic.Int64
	created        atomic.Int64
	rejected       atomic.Int64
	audioDropped   atomic.Int64
	vadDropped     atomic.Int64

	stop chan struct{}
}

func NewManager() *Manager {
	m := &Manager{sessions: map[uint64]*Session{}, stop: make(chan struct{})}
	go m.watchdog()
	return m
}

func (m *Manager) Close() { close(m.stop) }

type Spec struct {
	Budget Budget
	Build  func(s *Session) ([]Processor, error)
	Labels map[string]string
}

// StartTimings are cumulative from request receipt (spec §9.1).
type StartTimings struct {
	Allocated, PipelineCreated, Registered, Ready time.Duration
}

func (m *Manager) Create(sp Spec) (*Session, StartTimings, error) {
	t0 := time.Now()
	var tm StartTimings
	if a := m.Admit.Load(); a != nil {
		if err := (*a)(); err != nil {
			m.rejected.Add(1)
			return nil, tm, err
		}
	}
	b := sp.Budget
	if b.MaxQueuedFrames == 0 {
		b = DefaultBudget()
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Session{
		ID: m.nextID.Add(1), Budget: b, m: m, ctx: ctx, cancel: cancel,
		control:  make(chan envelope, b.MaxControlFrames),
		data:     make(chan envelope, b.MaxQueuedFrames),
		loopDone: make(chan struct{}), ready: make(chan struct{}), stopped: make(chan struct{}),
		Created: t0, Labels: sp.Labels,
	}
	tm.Allocated = time.Since(t0)

	procs, err := sp.Build(s)
	if err != nil {
		cancel()
		return nil, tm, err
	}
	s.procs = procs
	s.emitters = make([]emitter, len(procs))
	for i := range procs {
		s.emitters[i] = emitter{s, i}
	}
	tm.PipelineCreated = time.Since(t0)

	m.mu.Lock()
	m.sessions[s.ID] = s
	m.mu.Unlock()
	m.created.Add(1)
	for i, p := range procs { // registers with shared services (e.g. VAD stream)
		if a, ok := p.(Attacher); ok {
			if err := a.Attach(s, i); err != nil {
				s.Fail(err)
				go s.run() // let Stop observe a finished loop
				s.Stop()
				return nil, tm, err
			}
		}
	}
	tm.Registered = time.Since(t0)

	s.state.Store(int32(StateRunning))
	go s.run()
	<-s.ready
	tm.Ready = time.Since(t0)
	return s, tm, nil
}

func (m *Manager) remove(s *Session) {
	m.mu.Lock()
	delete(m.sessions, s.ID)
	m.mu.Unlock()
}

func (m *Manager) Get(id uint64) *Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessions[id]
}

func (m *Manager) List() []*Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s)
	}
	return out
}

func (m *Manager) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions)
}

// VADDropped is called by VAD processors when a chunk is stale/rejected.
func (m *Manager) CountVADDrop() { m.vadDropped.Add(1) }

type HostStats struct {
	Sessions, ManagedGoroutines, RunLoops, LeakedSessions int64
	Failures, Created, Rejected, AudioDropped, VADDropped int64
}

func (m *Manager) Stats() HostStats {
	return HostStats{
		Sessions: int64(m.Len()), ManagedGoroutines: m.managed.Load(), RunLoops: m.loops.Load(),
		LeakedSessions: m.leakedSessions.Load(), Failures: m.failures.Load(), Created: m.created.Load(),
		Rejected: m.rejected.Load(), AudioDropped: m.audioDropped.Load(), VADDropped: m.vadDropped.Load(),
	}
}

// watchdog: one goroutine for the host (not one per session).
func (m *Manager) watchdog() {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for tick := 0; ; tick++ {
		select {
		case <-m.stop:
			return
		case <-t.C:
		}
		now := time.Now().UnixNano()
		resetDrops := tick%10 == 0
		for _, s := range m.List() {
			if st := s.procStart.Load(); st != 0 && time.Duration(now-st) > s.Budget.MaxFrameProcessing {
				s.Fail(ErrWatchdog)
				continue
			}
			if resetDrops && s.dropWindow.Swap(0) > int64(s.Budget.MaxDroppedAudio) {
				s.Fail(ErrAudioShedding)
			}
		}
	}
}
