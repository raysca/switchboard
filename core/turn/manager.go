package turn

import (
	"errors"
	"math"
	"strings"
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
	SilenceTimeout        time.Duration
	MaxTurnDuration       time.Duration
	StartDebounce         time.Duration
	StopDebounce          time.Duration
	MinimumSpeechDuration time.Duration
	MaxRetiredTurns       int
	// StartStrategies is the first-yes-wins chain deciding when a user turn
	// starts. Nil means VAD only (the historical behavior). Instances are
	// stateful and must not be shared between managers.
	StartStrategies []UserTurnStartStrategy
}

func DefaultConfig() Config {
	return Config{
		SilenceTimeout:        700 * time.Millisecond,
		MaxTurnDuration:       60 * time.Second,
		StartDebounce:         80 * time.Millisecond,
		StopDebounce:          120 * time.Millisecond,
		MinimumSpeechDuration: 150 * time.Millisecond,
		MaxRetiredTurns:       128,
	}
}

func (c Config) validate() error {
	if c.SilenceTimeout <= 0 || c.MaxTurnDuration <= 0 ||
		c.StartDebounce < 0 || c.StopDebounce < 0 || c.MinimumSpeechDuration < 0 || c.MaxRetiredTurns <= 0 {
		return ErrInvalidConfig
	}
	for _, s := range c.StartStrategies {
		if s == nil {
			return ErrInvalidConfig
		}
	}
	return nil
}

// Observation is a provider-neutral VAD/activity snapshot. Muting is explicit
// manager state, controlled with SetMuted, so muted frames are never replayed.
type Observation struct {
	At           time.Time
	SilenceSince time.Time // audio silence onset if known (accounts for VAD hangover)
	TurnID       core.TurnID
	Generation   core.Generation
	Speaking     bool
	Activity     bool
	// Optional start-strategy evidence; ignored by the default VAD chain.
	Transcript      string
	TranscriptFinal bool
	BotSpeaking     bool
	ExternalStart   bool
	// ExternalStop ends the active turn when an ExternalStop strategy is in the chain.
	ExternalStop bool
}

// Manager is a session-owned, serialized state machine. It creates no timers
// or goroutines and emits framework frames for the caller to route normally.
type Manager struct {
	mu                sync.Mutex
	cfg               Config
	detector          TurnDetector
	detectionInFlight bool
	pendingDetection  *detectionSnapshot

	started bool
	closed  bool
	muted   bool
	lastAt  time.Time
	hasLast bool
	epoch   uint64

	currentGen      core.Generation
	currentID       core.TurnID
	retiredTurns    map[core.TurnID]struct{}
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

	starters     []UserTurnStartStrategy
	startByVAD   bool
	pendingText  string
	pendingFinal bool
	hasText      bool
	pendingExt   bool
	botSpeaking  bool

	text      string
	textFinal bool
	textAt    time.Time
	extStop   bool
}

type detectionSnapshot struct {
	input         DetectionInput
	epoch         uint64
	turn          core.TurnID
	generation    core.Generation
	turnStartedAt time.Time
	silenceSince  time.Time
}

const maxDetectorCallbacksPerDrain = 2

func NewManager(config Config, detector TurnDetector) (*Manager, error) {
	if err := config.validate(); err != nil || detector == nil {
		return nil, ErrInvalidConfig
	}
	starters := config.StartStrategies
	if len(starters) == 0 {
		starters = []UserTurnStartStrategy{VADStart()}
	}
	return &Manager{cfg: config, detector: detector, starters: starters, retiredTurns: make(map[core.TurnID]struct{})}, nil
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
	if err := m.acceptTimestamp(at); err != nil {
		m.mu.Unlock()
		return nil, err
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
	if observation.Generation <= m.currentGen && (m.isRetiredLocked(observation.TurnID) ||
		(m.active && (observation.TurnID != m.currentID || observation.Generation != m.currentGen)) ||
		(m.startCandidate && !observation.Speaking && (observation.TurnID != m.startTurn || observation.Generation != m.startGen))) {
		return nil, nil, nil
	}
	if !m.muted && observation.Generation <= m.currentGen && observation.Speaking && !m.active && len(m.retiredTurns) >= m.cfg.MaxRetiredTurns {
		return nil, nil, ErrRetiredTurnLimit
	}
	if err := m.acceptTimestamp(observation.At); err != nil {
		return nil, nil, err
	}

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
		m.bumpEpochLocked()
	}
	if m.muted {
		return frames, nil, nil
	}

	m.botSpeaking = observation.BotSpeaking
	if observation.Activity || observation.Speaking || observation.BotSpeaking || observation.Transcript != "" {
		for _, s := range m.starters {
			s.activity(observation.At)
		}
	}
	if !m.active {
		if strings.TrimSpace(observation.Transcript) != "" {
			m.pendingText, m.pendingFinal, m.hasText = observation.Transcript, observation.TranscriptFinal, true
		}
		m.pendingExt = m.pendingExt || observation.ExternalStart
	} else {
		if strings.TrimSpace(observation.Transcript) != "" {
			m.text, m.textFinal, m.textAt = observation.Transcript, observation.TranscriptFinal, observation.At
		}
		m.extStop = m.extStop || observation.ExternalStop
	}
	if observation.Speaking {
		if m.active {
			if m.hasSilenceSince {
				m.bumpEpochLocked()
			}
			m.stopCandidate = false
			m.speaking = true
			m.hasSilenceSince = false
			m.clearPendingDetectionLocked()
		} else if !m.startCandidate || m.startTurn != observation.TurnID || m.startGen != observation.Generation {
			m.startCandidate = true
			m.startSince = observation.At
			m.startTurn, m.startGen = observation.TurnID, observation.Generation
			m.startByVAD = true
			m.speaking = true
		}
		framesAfter, snapshot := m.advanceLocked(observation.At)
		return append(frames, framesAfter...), snapshot, nil
	}

	if !m.startCandidate && !m.active && (m.hasText || m.pendingExt) && observation.TurnID != "" && observation.Generation != 0 {
		// Transcript/external evidence without speech activity opens a
		// one-shot candidate; it never lingers if no strategy accepts it.
		m.startCandidate, m.startByVAD = true, false
		m.startSince, m.startTurn, m.startGen = observation.At, observation.TurnID, observation.Generation
	}
	if m.startCandidate {
		frames = append(frames, m.tryStartLocked(observation.At)...)
		m.startCandidate = false
		if !m.active {
			m.speaking = false
		}
	}
	m.pendingText, m.pendingFinal, m.hasText, m.pendingExt = "", false, false, false
	if m.active && m.speaking && !m.stopCandidate {
		m.stopCandidate = true
		if !observation.SilenceSince.IsZero() && observation.SilenceSince.Before(observation.At) {
			m.stopSince = observation.SilenceSince
			if m.stopSince.Before(m.turnStartedAt) {
				m.stopSince = m.turnStartedAt
			}
		} else {
			m.stopSince = observation.At
		}
	}
	framesAfter, snapshot := m.advanceLocked(observation.At)
	return append(frames, framesAfter...), snapshot, nil
}

func (m *Manager) finishDetection(frames []core.Frame, snapshot *detectionSnapshot) []core.Frame {
	if snapshot == nil {
		return frames
	}
	frameEpoch := snapshot.epoch
	detectorCalls := 0
	for snapshot != nil {
		m.mu.Lock()
		if !m.snapshotCurrentLocked(snapshot) {
			m.detectionInFlight = false
			if m.epoch != frameEpoch {
				frames = nil
			}
			next := m.takePendingDetectionLocked()
			m.detectionInFlight = next != nil
			m.mu.Unlock()
			snapshot = next
			continue
		}
		detector := m.detector
		useFallback := detectorCalls >= maxDetectorCallbacksPerDrain
		if !useFallback {
			detectorCalls++
		}
		m.mu.Unlock()

		var decision DetectionDecision
		var done, panicked bool
		if useFallback {
			decision, done = (SilenceDetector{}).Detect(snapshot.input)
		} else {
			decision, done, panicked = detectSafely(detector, snapshot.input)
		}
		if normalized, valid := normalizeDecision(decision); done && valid && !panicked {
			decision = normalized
		} else if panicked || (done && !valid) {
			decision, done = (SilenceDetector{}).Detect(snapshot.input)
		}
		m.mu.Lock()
		m.detectionInFlight = false
		current := m.snapshotCurrentLocked(snapshot)
		if done && current {
			m.clearPendingDetectionLocked()
			frames = append(frames, m.completeLocked(snapshot.input.At, decision.Reason, decision.Confidence, decision.HasConfidence)...)
			m.mu.Unlock()
			return frames
		}
		if m.epoch != frameEpoch {
			frames = nil
		}
		if current {
			// An identical queued input has already been evaluated, including
			// a negative result. Do not call the plugin again for that input.
			m.clearPendingDetectionLocked()
			m.mu.Unlock()
			return frames
		}
		next := m.takePendingDetectionLocked()
		m.detectionInFlight = next != nil
		m.mu.Unlock()
		snapshot = next
	}
	return frames
}

func sameDetectionInput(a, b DetectionInput) bool { return a == b }

// Current means the snapshot matches actual detector-relevant state, not just
// an observation counter or another queued snapshot. Control timestamps alone
// do not advance detection; an accepted Observe/Advance queues its own input.
func (m *Manager) snapshotCurrentLocked(snapshot *detectionSnapshot) bool {
	if m.closed || m.muted || !m.active || !(m.hasSilenceSince || m.extStop) ||
		m.epoch != snapshot.epoch || m.currentID != snapshot.turn || m.currentGen != snapshot.generation ||
		!m.turnStartedAt.Equal(snapshot.turnStartedAt) || !m.silenceSince.Equal(snapshot.silenceSince) {
		return false
	}
	if m.pendingDetection != nil && !sameDetectionInput(snapshot.input, m.pendingDetection.input) {
		return false
	}
	return sameDetectionInput(snapshot.input, m.newDetectionSnapshotLocked(snapshot.input.At).input)
}

func (m *Manager) takePendingDetectionLocked() *detectionSnapshot {
	snapshot := m.pendingDetection
	m.pendingDetection = nil
	return snapshot
}

func (m *Manager) clearPendingDetectionLocked() {
	m.pendingDetection = nil
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

func normalizeDecision(decision DetectionDecision) (DetectionDecision, bool) {
	if math.IsNaN(decision.Confidence) || math.IsInf(decision.Confidence, 0) {
		return DetectionDecision{}, false
	}
	switch decision.Reason {
	case core.TurnReasonSilence:
	case core.TurnReasonSemantic:
		if !decision.HasConfidence {
			return DetectionDecision{}, false
		}
	default:
		return DetectionDecision{}, false
	}
	if decision.HasConfidence && (decision.Confidence < 0 || decision.Confidence > 1) {
		return DetectionDecision{}, false
	}
	if !decision.HasConfidence {
		decision.Confidence = 0
	}
	return decision, true
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
	m.bumpEpochLocked()
	if muted {
		m.muted, m.muteSince = true, at
		m.startCandidate = false
		m.stopCandidate = false
		m.clearPendingDetectionLocked()
		return nil
	}
	paused := at.Sub(m.muteSince)
	if m.active {
		m.turnStartedAt = m.turnStartedAt.Add(paused)
		m.speaking = false
		m.stopCandidate = false
		m.silenceSince, m.hasSilenceSince = at, true
	}
	m.muted = false
	if m.detectionInFlight && m.active && m.hasSilenceSince {
		m.pendingDetection = m.newDetectionSnapshotLocked(at)
	}
	return nil
}

// Reset is idempotent and begins a fresh caller-timestamped state epoch.
func (m *Manager) Reset(at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}
	m.started, m.muted = true, false
	m.lastAt, m.hasLast = at, true
	m.currentGen, m.currentID = 0, ""
	m.retiredTurns = make(map[core.TurnID]struct{})
	m.pendingText, m.hasText, m.pendingExt = "", false, false
	for _, s := range m.starters {
		s.reset()
	}
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
	if !m.active && m.startCandidate && m.speaking {
		frames = append(frames, m.tryStartLocked(at)...)
	}
	if m.active && m.stopCandidate && at.Sub(m.stopSince) >= m.cfg.StopDebounce {
		m.stopCandidate, m.speaking = false, false
		m.silenceSince, m.hasSilenceSince = m.stopSince, true
		frames = append(frames, event(m.currentID, m.currentGen, core.TurnEventStopped, m.stopSince, core.TurnReasonNone, 0, false))
	}
	if m.active {
		if at.Sub(m.turnStartedAt) >= m.cfg.MaxTurnDuration {
			frames = append(frames, m.completeLocked(at, core.TurnReasonMaxDuration, 0, false)...)
			return frames, nil
		}
		if m.hasSilenceSince || m.extStop {
			if m.detectionInFlight {
				m.pendingDetection = m.newDetectionSnapshotLocked(at)
				return frames, nil
			}
			snapshot := m.newDetectionSnapshotLocked(at)
			m.detectionInFlight = true
			m.clearPendingDetectionLocked()
			return frames, snapshot
		}
	}
	return frames, nil
}

func (m *Manager) newDetectionSnapshotLocked(at time.Time) *detectionSnapshot {
	var silenceFor time.Duration
	if m.hasSilenceSince {
		silenceFor = at.Sub(m.silenceSince)
	}
	input := DetectionInput{
		At: at, TurnStartedAt: m.turnStartedAt, TurnID: m.currentID, Generation: m.currentGen,
		Text: m.text, TextFinal: m.textFinal, TextAt: m.textAt, ExternalStop: m.extStop,
		SilenceFor: silenceFor, SilenceAfter: m.cfg.SilenceTimeout,
	}
	return &detectionSnapshot{
		input: input, epoch: m.epoch, turn: m.currentID,
		generation: m.currentGen, turnStartedAt: m.turnStartedAt, silenceSince: m.silenceSince,
	}
}

// tryStartLocked consults the strategy chain (first yes wins) and consumes
// one-shot evidence. Strategies are in-package and callback-free.
func (m *Manager) tryStartLocked(at time.Time) []core.Frame {
	in := startInput{
		At: at, VADCandidate: m.startByVAD, SpeakingFor: at.Sub(m.startSince),
		Debounce: maxDuration(m.cfg.StartDebounce, m.cfg.MinimumSpeechDuration),
		Text:     m.pendingText, HasText: m.hasText, Final: m.pendingFinal,
		BotSpeaking: m.botSpeaking, External: m.pendingExt,
	}
	m.pendingText, m.pendingFinal, m.hasText, m.pendingExt = "", false, false, false
	for _, s := range m.starters {
		switch s.evaluate(in) {
		case startYes:
			frames := m.startLocked(at)
			if m.active && in.HasText {
				m.text, m.textFinal, m.textAt = in.Text, in.Final, at
			}
			return frames
		}
	}
	return nil
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
	m.active = true
	m.currentID, m.currentGen = m.startTurn, m.startGen
	m.turnStartedAt = m.startSince
	m.speaking, m.startCandidate, m.stopCandidate = true, false, false
	m.hasSilenceSince = false
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
	m.text, m.textFinal, m.textAt, m.extStop = "", false, time.Time{}, false
	m.speaking, m.stopCandidate, m.startCandidate, m.hasSilenceSince = false, false, false, false
	m.clearPendingDetectionLocked()
	m.bumpEpochLocked()
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
	m.text, m.textFinal, m.textAt, m.extStop = "", false, time.Time{}, false
	m.speaking, m.stopCandidate, m.startCandidate, m.hasSilenceSince = false, false, false, false
	m.clearPendingDetectionLocked()
	m.bumpEpochLocked()
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
	return true
}

func (m *Manager) bumpEpochLocked() {
	m.epoch++
	m.clearPendingDetectionLocked()
}

func (m *Manager) clearPendingLocked() {
	m.text, m.textFinal, m.textAt, m.extStop = "", false, time.Time{}, false
	m.startCandidate, m.stopCandidate = false, false
	m.speaking = false
	m.hasSilenceSince = false
	m.clearPendingDetectionLocked()
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
