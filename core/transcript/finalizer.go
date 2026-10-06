// Package transcript provides provider-neutral transcript stabilization and
// exactly-once commit policy. All policy time is supplied by the caller.
package transcript

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"govoice/core"
)

var (
	ErrInvalidConfig       = errors.New("transcript: invalid config")
	ErrInvalidIdentity     = errors.New("transcript: turn and generation are required")
	ErrInvalidTimestamp    = errors.New("transcript: timestamp is required")
	ErrTimestampRegression = errors.New("transcript: timestamp regression")
	ErrInvalidText         = errors.New("transcript: text must be valid UTF-8 and non-empty")
	ErrTextTooLong         = errors.New("transcript: text exceeds configured byte limit")
	ErrInvalidStability    = errors.New("transcript: stability must be finite and in [0,1]")
	ErrRetiredTurnLimit    = errors.New("transcript: pending turn limit reached")
)

// Config bounds transcript state and defines deterministic release/commit policy.
type Config struct {
	InterimStability    float64
	InterimRepeats      int
	GracePeriod         time.Duration
	FinalizationTimeout time.Duration
	MaxTextBytes        int
	MaxRetiredTurns     int
	// PolicyClock supplies receipt-order time and the single deadline driver.
	// Nil selects the monotonic component of the process clock.
	PolicyClock PolicyClock
}

func DefaultConfig() Config {
	return Config{
		InterimStability:    0.8,
		InterimRepeats:      2,
		GracePeriod:         200 * time.Millisecond,
		FinalizationTimeout: 2 * time.Second,
		MaxTextBytes:        32 * 1024,
		MaxRetiredTurns:     128,
	}
}

// Candidate is a raw provider hypothesis. Final means the provider will not
// revise this hypothesis; EndOfUtterance is the stronger speech boundary.
type Candidate struct {
	Turn       core.TurnID
	Generation core.Generation
	Text       string
	At         time.Time
	// SourceAt is the provider/media timestamp retained for emitted frames.
	// At is receipt-ordered policy time and alone controls ordering/deadlines.
	SourceAt       time.Time
	Final          bool
	EndOfUtterance bool
	Stability      float64
	HasStability   bool
}

type OutputKind uint8

const (
	Interim OutputKind = iota + 1
	Committed
)

// Output is an immutable, normalized result released by the policy.
type Output struct {
	Kind       OutputKind
	Turn       core.TurnID
	Generation core.Generation
	Text       string
	At         time.Time
	SourceAt   time.Time
}

type candidateStrength uint8

const (
	strengthInterim candidateStrength = iota + 1
	strengthProviderFinal
	strengthUtteranceFinal
)

type turnKey struct {
	turn core.TurnID
	gen  core.Generation
}

type turnState struct {
	text            string
	hasCandidate    bool
	hasFinal        bool
	lastRawText     string
	repeatCount     int
	lastInterimText string
	completed       bool
	completedAt     time.Time
	deadline        time.Time
	strength        candidateStrength
	settledPrefix   string
	sourceAt        time.Time
}

// Finalizer is safe for concurrent callers. It never invokes external code,
// starts a goroutine, reads wall time, or retains unbounded turn/text state.
type Finalizer struct {
	mu         sync.Mutex
	cfg        Config
	generation core.Generation
	lastAt     time.Time
	turns      map[turnKey]*turnState
	retired    map[turnKey]struct{}
	// retiredOrder is a FIFO of retired keys; only the newest MaxRetiredTurns
	// are remembered, so a long generation never hits a turn limit.
	retiredOrder []turnKey
}

func NewFinalizer(cfg Config) (*Finalizer, error) {
	if math.IsNaN(cfg.InterimStability) || math.IsInf(cfg.InterimStability, 0) ||
		cfg.InterimStability < 0 || cfg.InterimStability > 1 || cfg.InterimRepeats <= 0 ||
		cfg.GracePeriod < 0 || cfg.FinalizationTimeout <= 0 ||
		cfg.GracePeriod > cfg.FinalizationTimeout || cfg.MaxTextBytes <= 0 || cfg.MaxRetiredTurns <= 0 {
		return nil, ErrInvalidConfig
	}
	return &Finalizer{
		cfg:     cfg,
		turns:   make(map[turnKey]*turnState),
		retired: make(map[turnKey]struct{}),
	}, nil
}

func (f *Finalizer) Observe(candidate Candidate) ([]Output, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if candidate.Turn == "" || candidate.Generation == 0 {
		return nil, ErrInvalidIdentity
	}
	if candidate.At.IsZero() {
		return nil, ErrInvalidTimestamp
	}
	if candidate.HasStability && (math.IsNaN(candidate.Stability) || math.IsInf(candidate.Stability, 0) || candidate.Stability < 0 || candidate.Stability > 1) {
		return nil, ErrInvalidStability
	}
	text, err := f.normalizeText(candidate.Text)
	if err != nil {
		return nil, err
	}
	if f.generation != 0 && candidate.Generation < f.generation {
		return nil, nil
	}
	if err := f.validateTimeLocked(candidate.At); err != nil {
		return nil, err
	}
	key := turnKey{turn: candidate.Turn, gen: candidate.Generation}
	advancing := candidate.Generation > f.generation
	if !advancing {
		if _, ok := f.retired[key]; ok {
			f.lastAt = candidate.At
			return f.expireDueLocked(candidate.At), nil
		}
		if f.turns[key] == nil && len(f.turns) >= f.cfg.MaxRetiredTurns {
			return nil, ErrRetiredTurnLimit
		}
	}
	f.commitIdentityTimeLocked(candidate.Generation, candidate.At)
	outputs := f.expireDueLocked(candidate.At)
	if _, ok := f.retired[key]; ok {
		return outputs, nil
	}
	state := f.turns[key]
	if state == nil {
		state = &turnState{}
		f.turns[key] = state
	}

	strength := strengthInterim
	if candidate.EndOfUtterance {
		strength = strengthUtteranceFinal
	} else if candidate.Final {
		strength = strengthProviderFinal
	}
	// A provider final can settle one audio portion while a later interim
	// extends the whole-turn snapshot. Keep the settled prefix authoritative,
	// but permit its mutable tail to grow or change before retirement.
	extendsSettledPrefix := strength == strengthInterim && state.strength == strengthProviderFinal &&
		state.settledPrefix != "" && (text == state.settledPrefix || strings.HasPrefix(text, state.settledPrefix+" "))
	if strength >= state.strength || extendsSettledPrefix {
		state.text = text
		state.hasCandidate = true
		if !extendsSettledPrefix {
			state.strength = strength
			state.hasFinal = strength >= strengthProviderFinal
			if strength >= strengthProviderFinal {
				state.settledPrefix = text
			}
		}
		state.sourceAt = candidate.SourceAt
		if state.sourceAt.IsZero() {
			state.sourceAt = candidate.At
		}
	}
	if strength >= strengthProviderFinal {
		if state.completed || strength == strengthUtteranceFinal {
			f.setDeadlineLocked(state, candidate.At.Add(f.cfg.GracePeriod))
		}
	} else if state.strength == strengthInterim {
		if text == state.lastRawText {
			state.repeatCount++
		} else {
			state.lastRawText = text
			state.repeatCount = 1
		}
		stable := candidate.HasStability && candidate.Stability >= f.cfg.InterimStability
		if (stable || state.repeatCount >= f.cfg.InterimRepeats) && text != state.lastInterimText {
			state.lastInterimText = text
			outputs = append(outputs, f.output(Interim, key, text, candidate.At, candidate.SourceAt))
		}
	}
	outputs = append(outputs, f.expireDueLocked(candidate.At)...)
	return outputs, nil
}

func (f *Finalizer) Complete(turn core.TurnID, generation core.Generation, at time.Time) ([]Output, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if turn == "" || generation == 0 {
		return nil, ErrInvalidIdentity
	}
	if at.IsZero() {
		return nil, ErrInvalidTimestamp
	}
	if f.generation != 0 && generation < f.generation {
		return nil, nil
	}
	if err := f.validateTimeLocked(at); err != nil {
		return nil, err
	}
	key := turnKey{turn: turn, gen: generation}
	advancing := generation > f.generation
	if !advancing {
		if _, ok := f.retired[key]; ok {
			f.lastAt = at
			return f.expireDueLocked(at), nil
		}
		if f.turns[key] == nil && len(f.turns) >= f.cfg.MaxRetiredTurns {
			return nil, ErrRetiredTurnLimit
		}
	}
	f.commitIdentityTimeLocked(generation, at)
	outputs := f.expireDueLocked(at)
	if _, ok := f.retired[key]; ok {
		return outputs, nil
	}
	state := f.turns[key]
	if state == nil {
		state = &turnState{}
		f.turns[key] = state
	}
	if state.completed {
		return outputs, nil
	}
	state.completed = true
	state.completedAt = at
	if state.hasFinal {
		f.setDeadlineLocked(state, at.Add(f.cfg.GracePeriod))
	} else {
		f.setDeadlineLocked(state, at.Add(f.cfg.FinalizationTimeout))
	}
	outputs = append(outputs, f.expireDueLocked(at)...)
	return outputs, nil
}

// Advance applies caller-supplied time and returns commits in stable generation/turn order.
func (f *Finalizer) Advance(at time.Time) ([]Output, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.validateTimeLocked(at); err != nil {
		return nil, err
	}
	f.lastAt = at
	return f.expireDueLocked(at), nil
}

// NextDeadline returns the earliest pending terminal deadline. It invokes no
// external code and is used by the processor's single lifecycle-owned driver.
func (f *Finalizer) NextDeadline() (time.Time, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var next time.Time
	for _, state := range f.turns {
		if !state.deadline.IsZero() && (next.IsZero() || state.deadline.Before(next)) {
			next = state.deadline
		}
	}
	return next, !next.IsZero()
}

func (f *Finalizer) expireDueLocked(at time.Time) []Output {
	keys := make([]turnKey, 0, len(f.turns))
	for key, state := range f.turns {
		if !state.deadline.IsZero() && !at.Before(state.deadline) {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].gen != keys[j].gen {
			return keys[i].gen < keys[j].gen
		}
		return keys[i].turn < keys[j].turn
	})
	var outputs []Output
	for _, key := range keys {
		outputs = append(outputs, f.retireLocked(key, f.turns[key], at)...)
	}
	return outputs
}

func (f *Finalizer) Interrupt(turn core.TurnID, generation core.Generation, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if turn == "" || generation == 0 {
		return ErrInvalidIdentity
	}
	if at.IsZero() {
		return ErrInvalidTimestamp
	}
	if f.generation != 0 && generation < f.generation {
		return nil
	}
	if err := f.validateTimeLocked(at); err != nil {
		return err
	}
	key := turnKey{turn: turn, gen: generation}
	advancing := generation > f.generation
	if !advancing {
		if _, ok := f.retired[key]; ok {
			f.lastAt = at
			return nil
		}
		if f.turns[key] == nil && len(f.turns) >= f.cfg.MaxRetiredTurns {
			return ErrRetiredTurnLimit
		}
	}
	f.commitIdentityTimeLocked(generation, at)
	f.retireKeyLocked(key)
	return nil
}

// InvalidateAll retires every pending candidate in the active generation
// without committing text. It is the fail-closed fallback for uncorrelated
// provider assembly failures; correlated invalidation should use Interrupt.
func (f *Finalizer) InvalidateAll(at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.validateTimeLocked(at); err != nil {
		return err
	}
	f.lastAt = at
	for key := range f.turns {
		f.retireKeyLocked(key)
	}
	return nil
}

func (f *Finalizer) commitIdentityTimeLocked(generation core.Generation, at time.Time) {
	if generation > f.generation {
		f.generation = generation
		clear(f.turns)
		clear(f.retired)
		f.retiredOrder = f.retiredOrder[:0]
	}
	f.lastAt = at
}

func (f *Finalizer) validateTimeLocked(at time.Time) error {
	if at.IsZero() {
		return ErrInvalidTimestamp
	}
	if !f.lastAt.IsZero() && at.Before(f.lastAt) {
		return ErrTimestampRegression
	}
	return nil
}

func (f *Finalizer) normalizeText(text string) (string, error) {
	if !utf8.ValidString(text) {
		return "", ErrInvalidText
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", ErrInvalidText
	}
	if len(text) > f.cfg.MaxTextBytes {
		return "", fmt.Errorf("%w: %d > %d", ErrTextTooLong, len(text), f.cfg.MaxTextBytes)
	}
	return strings.Clone(text), nil
}

// retireKeyLocked drops a pending turn and remembers it as retired, evicting the
// oldest remembered key once the window is full.
// ponytail: a re-delivery older than the window would start a fresh turn; the
// stale-generation check drops it once the call has moved on.
func (f *Finalizer) retireKeyLocked(key turnKey) {
	delete(f.turns, key)
	if _, ok := f.retired[key]; ok {
		return
	}
	if len(f.retiredOrder) >= f.cfg.MaxRetiredTurns {
		delete(f.retired, f.retiredOrder[0])
		f.retiredOrder = f.retiredOrder[1:]
	}
	f.retired[key] = struct{}{}
	f.retiredOrder = append(f.retiredOrder, key)
}

func (f *Finalizer) retireLocked(key turnKey, state *turnState, at time.Time) []Output {
	f.retireKeyLocked(key)
	if state == nil || !state.hasCandidate {
		return nil
	}
	return []Output{f.output(Committed, key, state.text, at, state.sourceAt)}
}

func (f *Finalizer) output(kind OutputKind, key turnKey, text string, at, sourceAt time.Time) Output {
	if sourceAt.IsZero() {
		sourceAt = at
	}
	return Output{Kind: kind, Turn: key.turn, Generation: key.gen, Text: strings.Clone(text), At: at, SourceAt: sourceAt}
}

func (f *Finalizer) setDeadlineLocked(state *turnState, deadline time.Time) {
	if state.deadline.IsZero() || deadline.Before(state.deadline) {
		state.deadline = deadline
	}
}
