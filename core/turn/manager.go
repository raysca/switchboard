package turn

import (
	"errors"
	"sync"
	"time"

	"govoice/core"
)

var (
	ErrInvalidConfig       = errors.New("turn: invalid configuration")
	ErrInvalidObservation  = errors.New("turn: invalid observation")
	ErrTimestampRegression = errors.New("turn: timestamp regressed")
	ErrNotStarted          = errors.New("turn: manager not started")
	ErrClosed              = errors.New("turn: manager closed")
)

// Config contains deterministic user-speech timing policy. All durations are
// evaluated against caller-supplied observation and advance timestamps.
type Config struct {
	InitialSilenceTimeout time.Duration
	SilenceTimeout        time.Duration
	MaxTurnDuration       time.Duration
	StartDebounce         time.Duration
	StopDebounce          time.Duration
	MinimumSpeechDuration time.Duration
	SemanticMaxAge        time.Duration
}

func DefaultConfig() Config {
	return Config{
		InitialSilenceTimeout: 10 * time.Second,
		SilenceTimeout:        700 * time.Millisecond,
		MaxTurnDuration:       60 * time.Second,
		StartDebounce:         80 * time.Millisecond,
		StopDebounce:          120 * time.Millisecond,
		MinimumSpeechDuration: 150 * time.Millisecond,
		SemanticMaxAge:        time.Second,
	}
}

func (c Config) validate() error {
	if c.InitialSilenceTimeout <= 0 || c.SilenceTimeout <= 0 || c.MaxTurnDuration <= 0 ||
		c.StartDebounce < 0 || c.StopDebounce < 0 || c.MinimumSpeechDuration < 0 || c.SemanticMaxAge < 0 {
		return ErrInvalidConfig
	}
	return nil
}

// Observation is a provider-neutral VAD/activity snapshot. Muting is explicit
// manager state, controlled with SetMuted, so muted frames are never replayed.
type Observation struct {
	At         time.Time
	TurnID     core.TurnID
	Generation core.Generation
	Speaking   bool
	Activity   bool
	Semantic   *SemanticEvidence
}

// Manager is a session-owned, serialized state machine. It creates no timers
// or goroutines and emits framework frames for the caller to route normally.
type Manager struct {
	mu       sync.Mutex
	cfg      Config
	detector TurnDetector

	started bool
	closed  bool
	muted   bool
	lastAt  time.Time
	hasLast bool

	noSpeechSince  time.Time
	noSpeechFired  bool
	speechAccepted bool

	currentGen      core.Generation
	currentID       core.TurnID
	lastTerminalID  core.TurnID
	lifecycle       core.TurnLifecycle
	active          bool
	turnStartedAt   time.Time
	speaking        bool
	startCandidate  bool
	startSince      time.Time
	startTurn       core.TurnID
	startGen        core.Generation
	stopCandidate   bool
	stopSince       time.Time
	silenceSince    time.Time
	hasSilenceSince bool
	muteSince       time.Time
	semantic        *SemanticEvidence
}

func NewManager(config Config, detector TurnDetector) (*Manager, error) {
	if err := config.validate(); err != nil || detector == nil {
		return nil, ErrInvalidConfig
	}
	return &Manager{cfg: config, detector: detector}, nil
}

// Start sets the initial-silence origin explicitly; it never reads wall time.
func (m *Manager) Start(at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}
	if m.started {
		return nil
	}
	m.started, m.lastAt, m.hasLast = true, at, true
	m.noSpeechSince = at
	return nil
}

// Observe applies one timestamped VAD/activity observation.
func (m *Manager) Observe(observation Observation) ([]core.Frame, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.checkReady(); err != nil {
		return nil, err
	}
	if observation.Generation != 0 && observation.Generation < m.currentGen {
		return nil, nil
	}
	if observation.Speaking && (observation.TurnID == "" || observation.Generation == 0) {
		return nil, ErrInvalidObservation
	}
	if m.active && observation.Generation <= m.currentGen && (observation.TurnID != m.currentID || observation.Generation != m.currentGen) {
		return nil, nil
	}
	if m.startCandidate && !observation.Speaking && (observation.TurnID != m.startTurn || observation.Generation != m.startGen) {
		return nil, nil
	}
	if observation.TurnID != "" && observation.TurnID == m.lastTerminalID && observation.Generation <= m.currentGen {
		return nil, nil
	}
	if err := m.acceptTimestamp(observation.At); err != nil {
		return nil, err
	}
	if m.muted {
		return nil, nil
	}
	if observation.Semantic == nil {
		m.semantic = nil
	} else {
		evidence := *observation.Semantic
		m.semantic = &evidence
	}
	var frames []core.Frame
	if observation.Generation > m.currentGen {
		if m.active {
			frames = append(frames, m.interruptLocked(m.currentID, m.currentGen, observation.At)...)
		}
		m.clearPendingLocked()
		m.currentGen = observation.Generation
		m.currentID = ""
	}
	if observation.Activity || observation.Speaking {
		m.noSpeechSince = observation.At
	}
	if observation.Speaking {
		if m.active {
			if observation.TurnID != m.currentID || observation.Generation != m.currentGen {
				return frames, nil
			}
			m.stopCandidate = false
			m.speaking = true
			m.hasSilenceSince = false
			return append(frames, m.advanceLocked(observation.At)...), nil
		}
		if observation.TurnID == m.lastTerminalID {
			return frames, nil
		}
		if !m.startCandidate || m.startTurn != observation.TurnID || m.startGen != observation.Generation {
			m.startCandidate = true
			m.startSince = observation.At
			m.startTurn, m.startGen = observation.TurnID, observation.Generation
		}
		m.speaking = true
		return append(frames, m.advanceLocked(observation.At)...), nil
	}
	if m.startCandidate {
		if observation.At.Sub(m.startSince) >= maxDuration(m.cfg.StartDebounce, m.cfg.MinimumSpeechDuration) {
			frames = append(frames, m.startLocked(observation.At)...)
		}
		m.startCandidate = false
	}
	if m.active {
		if !m.stopCandidate {
			m.stopCandidate, m.stopSince = true, observation.At
		}
		frames = append(frames, m.advanceLocked(observation.At)...)
	} else {
		frames = append(frames, m.advanceLocked(observation.At)...)
	}
	return frames, nil
}

// Advance applies deadlines through at. It is the only way time progresses in
// the manager when no new observation arrives.
func (m *Manager) Advance(at time.Time) ([]core.Frame, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.checkReady(); err != nil {
		return nil, err
	}
	if err := m.acceptTimestamp(at); err != nil {
		return nil, err
	}
	if m.muted {
		return nil, nil
	}
	return m.advanceLocked(at), nil
}

// SetMuted pauses all turn and timeout advancement. Unmute discards muted
// speech state and resumes silence timing from the explicit unmute timestamp.
func (m *Manager) SetMuted(muted bool, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.checkReady(); err != nil {
		return err
	}
	if err := m.acceptTimestamp(at); err != nil {
		return err
	}
	if muted == m.muted {
		return nil
	}
	if muted {
		m.muted, m.muteSince = true, at
		m.startCandidate = false
		m.stopCandidate = false
		m.semantic = nil
		return nil
	}
	paused := at.Sub(m.muteSince)
	m.noSpeechSince = m.noSpeechSince.Add(paused)
	if m.active {
		m.turnStartedAt = m.turnStartedAt.Add(paused)
		m.speaking = false
		m.stopCandidate = false
		m.silenceSince, m.hasSilenceSince = at, true
	}
	m.muted = false
	return nil
}

// Interrupt commits a one-shot interruption for the expected active turn.
func (m *Manager) Interrupt(id core.TurnID, generation core.Generation, at time.Time) ([]core.Frame, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.checkReady(); err != nil {
		return nil, err
	}
	if generation < m.currentGen || !m.active || id != m.currentID || generation != m.currentGen {
		return nil, nil
	}
	if err := m.acceptTimestamp(at); err != nil {
		return nil, err
	}
	return m.interruptLocked(id, generation, at), nil
}

// Reset is idempotent and begins a fresh caller-timestamped state epoch.
func (m *Manager) Reset(at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}
	m.started, m.muted, m.noSpeechFired, m.speechAccepted = true, false, false, false
	m.lastAt, m.hasLast = at, true
	m.noSpeechSince = at
	m.currentGen, m.currentID, m.lastTerminalID = 0, "", ""
	m.semantic = nil
	m.lifecycle = core.TurnLifecycle{}
	m.active, m.speaking, m.hasSilenceSince = false, false, false
	m.clearPendingLocked()
	return nil
}

// Close is idempotent and releases no external resources.
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	m.clearPendingLocked()
	return nil
}

func (m *Manager) checkReady() error {
	if m.closed {
		return ErrClosed
	}
	if !m.started {
		return ErrNotStarted
	}
	return nil
}

func (m *Manager) acceptTimestamp(at time.Time) error {
	if m.hasLast && at.Before(m.lastAt) {
		return ErrTimestampRegression
	}
	m.lastAt, m.hasLast = at, true
	return nil
}

func (m *Manager) advanceLocked(at time.Time) []core.Frame {
	if m.muted {
		return nil
	}
	var frames []core.Frame
	if !m.active && m.startCandidate && m.speaking && at.Sub(m.startSince) >= maxDuration(m.cfg.StartDebounce, m.cfg.MinimumSpeechDuration) {
		frames = append(frames, m.startLocked(at)...)
	}
	if m.active && m.stopCandidate && at.Sub(m.stopSince) >= m.cfg.StopDebounce {
		m.stopCandidate, m.speaking = false, false
		m.silenceSince, m.hasSilenceSince = m.stopSince, true
		frames = append(frames, event(m.currentID, m.currentGen, core.TurnEventStopped, m.stopSince, core.TurnReasonNone, 0, false))
	}
	if m.active {
		if at.Sub(m.turnStartedAt) >= m.cfg.MaxTurnDuration {
			frames = append(frames, m.completeLocked(at, core.TurnReasonMaxDuration, 0, false)...)
			return frames
		}
		if m.hasSilenceSince {
			input := DetectionInput{
				At: at, TurnStartedAt: m.turnStartedAt, TurnID: m.currentID, Generation: m.currentGen,
				SilenceFor: at.Sub(m.silenceSince), SilenceAfter: m.cfg.SilenceTimeout,
				SemanticMaxAge: m.cfg.SemanticMaxAge, Semantic: m.semantic,
			}
			decision, done := m.detector.Detect(input)
			if done {
				frames = append(frames, m.completeLocked(at, decision.Reason, decision.Confidence, decision.HasConfidence)...)
				return frames
			}
		}
	}
	if !m.speechAccepted && !m.noSpeechFired && at.Sub(m.noSpeechSince) >= m.cfg.InitialSilenceTimeout {
		m.noSpeechFired = true
		frames = append(frames, event("", m.currentGen, core.TurnEventNoSpeechTimeout, at, core.TurnReasonInitialNoSpeech, 0, false))
	}
	return frames
}

func (m *Manager) startLocked(at time.Time) []core.Frame {
	if !m.startCandidate || m.active || m.startTurn == "" || m.startGen == 0 {
		return nil
	}
	if !m.lifecycle.Start(m.startTurn, m.startGen) {
		if m.lifecycle.State == core.TurnCompleted || m.lifecycle.State == core.TurnInterrupted {
			m.lifecycle = core.TurnLifecycle{}
			if !m.lifecycle.Start(m.startTurn, m.startGen) {
				return nil
			}
		} else {
			return nil
		}
	}
	m.active, m.speechAccepted = true, true
	m.currentID, m.currentGen = m.startTurn, m.startGen
	m.turnStartedAt = m.startSince
	m.speaking, m.startCandidate, m.stopCandidate = true, false, false
	m.hasSilenceSince = false
	return []core.Frame{event(m.currentID, m.currentGen, core.TurnEventStarted, at, core.TurnReasonNone, 0, false)}
}

func (m *Manager) completeLocked(at time.Time, reason core.TurnReason, confidence float64, hasConfidence bool) []core.Frame {
	if !m.active || !m.lifecycle.Complete(m.currentID) {
		return nil
	}
	e := event(m.currentID, m.currentGen, core.TurnEventCompleted, at, reason, confidence, hasConfidence)
	m.active = false
	m.lastTerminalID = m.currentID
	m.speaking, m.stopCandidate, m.startCandidate, m.hasSilenceSince = false, false, false, false
	return []core.Frame{e}
}

func (m *Manager) interruptLocked(id core.TurnID, generation core.Generation, at time.Time) []core.Frame {
	if !m.active || !m.lifecycle.Interrupt(id) {
		return nil
	}
	e := event(id, generation, core.TurnEventInterrupted, at, core.TurnReasonInterrupted, 0, false)
	m.active = false
	m.lastTerminalID = id
	m.speaking, m.stopCandidate, m.startCandidate, m.hasSilenceSince = false, false, false, false
	return []core.Frame{e}
}

func (m *Manager) clearPendingLocked() {
	m.startCandidate, m.stopCandidate = false, false
	m.speaking = false
	m.hasSilenceSince = false
}

func event(id core.TurnID, generation core.Generation, kind core.TurnEventKind, at time.Time, reason core.TurnReason, confidence float64, hasConfidence bool) *core.TurnEventFrame {
	return &core.TurnEventFrame{Kind: kind, Turn: id, Gen: generation, At: at, Reason: reason, Confidence: confidence, HasConfidence: hasConfidence}
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
