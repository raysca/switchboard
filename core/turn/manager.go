package turn

import (
	"errors"
	"math"
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
	ErrRetiredTurnLimit    = errors.New("turn: retired turn limit reached for generation")
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
	MaxRetiredTurns       int
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
		MaxRetiredTurns:       128,
	}
}

func (c Config) validate() error {
	if c.InitialSilenceTimeout <= 0 || c.SilenceTimeout <= 0 || c.MaxTurnDuration <= 0 ||
		c.StartDebounce < 0 || c.StopDebounce < 0 || c.MinimumSpeechDuration < 0 || c.SemanticMaxAge < 0 || c.MaxRetiredTurns <= 0 {
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
	mu                sync.Mutex
	cfg               Config
	detector          TurnDetector
	detectionInFlight bool

	started  bool
	closed   bool
	muted    bool
	lastAt   time.Time
	hasLast  bool
	revision uint64
	epoch    uint64

	noSpeechSince  time.Time
	noSpeechFired  bool
	speechAccepted bool

	currentGen      core.Generation
	currentID       core.TurnID
	retiredTurns    map[core.TurnID]struct{}
	retiredOrder    []core.TurnID
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

type detectionSnapshot struct {
	input         DetectionInput
	revision      uint64
	epoch         uint64
	turn          core.TurnID
	generation    core.Generation
	turnStartedAt time.Time
	silenceSince  time.Time
}

func NewManager(config Config, detector TurnDetector) (*Manager, error) {
	if err := config.validate(); err != nil || detector == nil {
		return nil, ErrInvalidConfig
	}
	return &Manager{cfg: config, detector: detector, retiredTurns: make(map[core.TurnID]struct{})}, nil
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
	m.bumpEpochLocked()
	return nil
}

// Observe applies one timestamped VAD/activity observation.
func (m *Manager) Observe(observation Observation) ([]core.Frame, error) {
	m.mu.Lock()
	frames, snapshot, err := m.observeLocked(observation)
	m.mu.Unlock()
	if err != nil {
		return frames, err
	}
	return m.finishDetection(frames, snapshot), nil
}

// Advance applies deadlines through at. It is the only way time progresses in
// the manager when no new observation arrives.
func (m *Manager) Advance(at time.Time) ([]core.Frame, error) {
	m.mu.Lock()
	if err := m.checkReady(); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	previousAt := m.lastAt
	if err := m.acceptTimestamp(at); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	if at.After(previousAt) {
		m.bumpRevisionLocked()
	}
	if m.muted {
		m.mu.Unlock()
		return nil, nil
	}
	frames, snapshot := m.advanceLocked(at)
	m.mu.Unlock()
	return m.finishDetection(frames, snapshot), nil
}

func (m *Manager) observeLocked(observation Observation) ([]core.Frame, *detectionSnapshot, error) {
	if err := m.checkReady(); err != nil {
		return nil, nil, err
	}
	if observation.Generation != 0 && observation.Generation < m.currentGen {
		return nil, nil, nil
	}
	if observation.Speaking && (observation.TurnID == "" || observation.Generation == 0) {
		return nil, nil, ErrInvalidObservation
	}
	if observation.Generation == m.currentGen && (m.isRetiredLocked(observation.TurnID) ||
		(m.active && (observation.TurnID != m.currentID || observation.Generation != m.currentGen)) ||
		(m.startCandidate && !observation.Speaking && (observation.TurnID != m.startTurn || observation.Generation != m.startGen))) {
		return nil, nil, nil
	}
	if err := m.acceptTimestamp(observation.At); err != nil {
		return nil, nil, err
	}
	m.bumpRevisionLocked()

	var frames []core.Frame
	// A newer generation is authoritative even if this is a stop/idle snapshot
	// that cannot match a candidate or active turn from the previous generation.
	if observation.Generation > m.currentGen {
		if m.active {
			frames = append(frames, m.interruptLocked(m.currentID, m.currentGen, observation.At)...)
		}
		m.clearPendingLocked()
		m.currentGen, m.currentID = observation.Generation, ""
		m.lifecycle = core.TurnLifecycle{}
		m.retiredTurns = make(map[core.TurnID]struct{})
		m.retiredOrder = nil
		m.bumpEpochLocked()
	}
	if m.muted {
		return frames, nil, nil
	}

	if m.active && (observation.TurnID != m.currentID || observation.Generation != m.currentGen) {
		return frames, nil, nil
	}
	if m.startCandidate && !observation.Speaking && (observation.TurnID != m.startTurn || observation.Generation != m.startGen) {
		return frames, nil, nil
	}
	if m.isRetiredLocked(observation.TurnID) {
		return frames, nil, nil
	}
	if observation.Speaking && !m.active && len(m.retiredTurns) >= m.cfg.MaxRetiredTurns {
		return frames, nil, ErrRetiredTurnLimit
	}

	if observation.Semantic == nil {
		m.semantic = nil
	} else {
		evidence := *observation.Semantic
		m.semantic = &evidence
	}
	if observation.Activity || observation.Speaking {
		m.noSpeechSince = observation.At
	}
	if observation.Speaking {
		if m.active {
			m.stopCandidate = false
			m.speaking = true
			m.hasSilenceSince = false
		} else if !m.startCandidate || m.startTurn != observation.TurnID || m.startGen != observation.Generation {
			m.startCandidate = true
			m.startSince = observation.At
			m.startTurn, m.startGen = observation.TurnID, observation.Generation
			m.speaking = true
		}
		framesAfter, snapshot := m.advanceLocked(observation.At)
		return append(frames, framesAfter...), snapshot, nil
	}

	if m.startCandidate {
		if observation.At.Sub(m.startSince) >= maxDuration(m.cfg.StartDebounce, m.cfg.MinimumSpeechDuration) {
			frames = append(frames, m.startLocked(observation.At)...)
		}
		m.startCandidate = false
		if !m.active {
			m.speaking = false
		}
	}
	if m.active && m.speaking && !m.stopCandidate {
		m.stopCandidate, m.stopSince = true, observation.At
	}
	framesAfter, snapshot := m.advanceLocked(observation.At)
	return append(frames, framesAfter...), snapshot, nil
}

func (m *Manager) finishDetection(frames []core.Frame, snapshot *detectionSnapshot) []core.Frame {
	if snapshot == nil {
		return frames
	}
	m.mu.Lock()
	if !m.snapshotCurrentLocked(snapshot) {
		m.detectionInFlight = false
		m.mu.Unlock()
		return frames
	}
	detector := m.detector
	m.mu.Unlock()

	decision, done, panicked := detectSafely(detector, snapshot.input)
	if panicked || (done && !validDecision(decision)) {
		decision, done = (SilenceDetector{}).Detect(snapshot.input)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.detectionInFlight = false
	if !done {
		return frames
	}
	if !m.snapshotCurrentLocked(snapshot) {
		return frames
	}
	frames = append(frames, m.completeLocked(snapshot.input.At, decision.Reason, decision.Confidence, decision.HasConfidence)...)
	return frames
}

func (m *Manager) snapshotCurrentLocked(snapshot *detectionSnapshot) bool {
	return !m.closed && !m.muted && m.active && m.hasSilenceSince &&
		m.revision == snapshot.revision && m.epoch == snapshot.epoch &&
		m.currentID == snapshot.turn && m.currentGen == snapshot.generation &&
		m.turnStartedAt.Equal(snapshot.turnStartedAt) && m.silenceSince.Equal(snapshot.silenceSince)
}

func detectSafely(detector TurnDetector, input DetectionInput) (decision DetectionDecision, done bool, panicked bool) {
	defer func() {
		if recover() != nil {
			decision, done, panicked = DetectionDecision{}, false, true
		}
	}()
	decision, done = detector.Detect(input)
	return decision, done, false
}

func validDecision(decision DetectionDecision) bool {
	switch decision.Reason {
	case core.TurnReasonSilence:
	case core.TurnReasonSemantic:
		if !decision.HasConfidence {
			return false
		}
	default:
		return false
	}
	if decision.HasConfidence && (math.IsNaN(decision.Confidence) || math.IsInf(decision.Confidence, 0) || decision.Confidence < 0 || decision.Confidence > 1) {
		return false
	}
	return true
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
	m.bumpRevisionLocked()
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
	m.bumpRevisionLocked()
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
	m.currentGen, m.currentID = 0, ""
	m.retiredTurns = make(map[core.TurnID]struct{})
	m.retiredOrder = nil
	m.semantic = nil
	m.lifecycle = core.TurnLifecycle{}
	m.active, m.speaking, m.hasSilenceSince = false, false, false
	m.clearPendingLocked()
	m.bumpEpochLocked()
	return nil
}

// Close is idempotent and releases no external resources.
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	m.clearPendingLocked()
	m.bumpEpochLocked()
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

func (m *Manager) advanceLocked(at time.Time) ([]core.Frame, *detectionSnapshot) {
	if m.muted {
		return nil, nil
	}
	var frames []core.Frame
	if !m.active && m.startCandidate && m.speaking && at.Sub(m.startSince) >= maxDuration(m.cfg.StartDebounce, m.cfg.MinimumSpeechDuration) {
		frames = append(frames, m.startLocked(at)...)
	}
	if m.active && m.stopCandidate && at.Sub(m.stopSince) >= m.cfg.StopDebounce {
		m.stopCandidate, m.speaking = false, false
		m.silenceSince, m.hasSilenceSince = m.stopSince, true
		m.bumpRevisionLocked()
		frames = append(frames, event(m.currentID, m.currentGen, core.TurnEventStopped, m.stopSince, core.TurnReasonNone, 0, false))
	}
	if m.active {
		if at.Sub(m.turnStartedAt) >= m.cfg.MaxTurnDuration {
			frames = append(frames, m.completeLocked(at, core.TurnReasonMaxDuration, 0, false)...)
			return frames, nil
		}
		if m.hasSilenceSince {
			if m.detectionInFlight {
				return frames, nil
			}
			var semantic *SemanticEvidence
			if m.semantic != nil {
				evidence := *m.semantic
				semantic = &evidence
			}
			input := DetectionInput{
				At: at, TurnStartedAt: m.turnStartedAt, TurnID: m.currentID, Generation: m.currentGen,
				SilenceFor: at.Sub(m.silenceSince), SilenceAfter: m.cfg.SilenceTimeout,
				SemanticMaxAge: m.cfg.SemanticMaxAge, Semantic: semantic,
			}
			snapshot := &detectionSnapshot{
				input: input, revision: m.revision, epoch: m.epoch, turn: m.currentID,
				generation: m.currentGen, turnStartedAt: m.turnStartedAt, silenceSince: m.silenceSince,
			}
			m.detectionInFlight = true
			return frames, snapshot
		}
	}
	if !m.speechAccepted && !m.noSpeechFired && at.Sub(m.noSpeechSince) >= m.cfg.InitialSilenceTimeout {
		m.noSpeechFired = true
		m.bumpRevisionLocked()
		frames = append(frames, event("", m.currentGen, core.TurnEventNoSpeechTimeout, at, core.TurnReasonInitialNoSpeech, 0, false))
	}
	return frames, nil
}

func (m *Manager) startLocked(at time.Time) []core.Frame {
	if !m.startCandidate || m.active || m.startTurn == "" || m.startGen == 0 {
		return nil
	}
	if m.isRetiredLocked(m.startTurn) || len(m.retiredTurns) >= m.cfg.MaxRetiredTurns {
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
	m.bumpRevisionLocked()
	return []core.Frame{event(m.currentID, m.currentGen, core.TurnEventStarted, at, core.TurnReasonNone, 0, false)}
}

func (m *Manager) completeLocked(at time.Time, reason core.TurnReason, confidence float64, hasConfidence bool) []core.Frame {
	if !m.active || m.isRetiredLocked(m.currentID) || len(m.retiredTurns) >= m.cfg.MaxRetiredTurns || !m.lifecycle.Complete(m.currentID) {
		return nil
	}
	if !m.retireLocked(m.currentID) {
		return nil
	}
	e := event(m.currentID, m.currentGen, core.TurnEventCompleted, at, reason, confidence, hasConfidence)
	m.active = false
	m.speaking, m.stopCandidate, m.startCandidate, m.hasSilenceSince = false, false, false, false
	m.bumpRevisionLocked()
	return []core.Frame{e}
}

func (m *Manager) interruptLocked(id core.TurnID, generation core.Generation, at time.Time) []core.Frame {
	if !m.active || m.isRetiredLocked(id) || len(m.retiredTurns) >= m.cfg.MaxRetiredTurns || !m.lifecycle.Interrupt(id) {
		return nil
	}
	if !m.retireLocked(id) {
		return nil
	}
	e := event(id, generation, core.TurnEventInterrupted, at, core.TurnReasonInterrupted, 0, false)
	m.active = false
	m.speaking, m.stopCandidate, m.startCandidate, m.hasSilenceSince = false, false, false, false
	m.bumpRevisionLocked()
	return []core.Frame{e}
}

func (m *Manager) isRetiredLocked(id core.TurnID) bool {
	if id == "" {
		return false
	}
	_, ok := m.retiredTurns[id]
	return ok
}

func (m *Manager) retireLocked(id core.TurnID) bool {
	if id == "" || m.isRetiredLocked(id) {
		return false
	}
	if len(m.retiredTurns) >= m.cfg.MaxRetiredTurns {
		return false
	}
	m.retiredTurns[id] = struct{}{}
	m.retiredOrder = append(m.retiredOrder, id)
	return true
}

func (m *Manager) bumpRevisionLocked() { m.revision++ }

func (m *Manager) bumpEpochLocked() {
	m.epoch++
	m.bumpRevisionLocked()
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
