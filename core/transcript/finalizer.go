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
	ErrRetiredTurnLimit    = errors.New("transcript: bounded turn retirement limit reached")
)

// Config bounds transcript state and defines deterministic release/commit policy.
type Config struct {
	InterimStability    float64
	InterimRepeats      int
	GracePeriod         time.Duration
	FinalizationTimeout time.Duration
	MaxTextBytes        int
	MaxRetiredTurns     int
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
	Turn           core.TurnID
	Generation     core.Generation
	Text           string
	At             time.Time
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
}

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
	key, stale, err := f.acceptIdentityLocked(candidate.Turn, candidate.Generation)
	if err != nil || stale {
		return nil, err
	}
	if _, ok := f.retired[key]; ok {
		return nil, nil
	}
	state := f.turns[key]
	if state == nil && len(f.turns)+len(f.retired) >= f.cfg.MaxRetiredTurns {
		return nil, ErrRetiredTurnLimit
	}
	if err := f.acceptTimeLocked(candidate.At); err != nil {
		return nil, err
	}
	if state == nil {
		state = &turnState{}
		f.turns[key] = state
	}

	var outputs []Output
	if candidate.Final {
		state.text = text
		state.hasCandidate = true
		state.hasFinal = true
		if state.completed {
			state.deadline = state.completedAt.Add(f.cfg.GracePeriod)
		} else if candidate.EndOfUtterance && state.deadline.IsZero() {
			state.deadline = candidate.At.Add(f.cfg.GracePeriod)
		}
	} else if !state.hasFinal {
		state.text = text
		state.hasCandidate = true
		if text == state.lastRawText {
			state.repeatCount++
		} else {
			state.lastRawText = text
			state.repeatCount = 1
		}
		stable := candidate.HasStability && candidate.Stability >= f.cfg.InterimStability
		if (stable || state.repeatCount >= f.cfg.InterimRepeats) && text != state.lastInterimText {
			state.lastInterimText = text
			outputs = append(outputs, f.output(Interim, key, text, candidate.At))
		}
	}

	if !state.deadline.IsZero() && !candidate.At.Before(state.deadline) {
		outputs = append(outputs, f.retireLocked(key, state, candidate.At)...)
	}
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
	key, stale, err := f.acceptIdentityLocked(turn, generation)
	if err != nil || stale {
		return nil, err
	}
	if _, ok := f.retired[key]; ok {
		return nil, nil
	}
	state := f.turns[key]
	if state == nil && len(f.turns)+len(f.retired) >= f.cfg.MaxRetiredTurns {
		return nil, ErrRetiredTurnLimit
	}
	if err := f.acceptTimeLocked(at); err != nil {
		return nil, err
	}
	if state == nil {
		state = &turnState{}
		f.turns[key] = state
	}
	if state.completed {
		return nil, nil
	}
	state.completed = true
	state.completedAt = at
	if state.hasFinal {
		state.deadline = at.Add(f.cfg.GracePeriod)
	} else {
		state.deadline = at.Add(f.cfg.FinalizationTimeout)
	}
	if !at.Before(state.deadline) {
		return f.retireLocked(key, state, at), nil
	}
	return nil, nil
}

// Advance applies caller-supplied time and returns commits in stable generation/turn order.
func (f *Finalizer) Advance(at time.Time) ([]Output, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.acceptTimeLocked(at); err != nil {
		return nil, err
	}
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
	return outputs, nil
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
	key, stale, err := f.acceptIdentityLocked(turn, generation)
	if err != nil || stale {
		return err
	}
	if _, ok := f.retired[key]; ok {
		return nil
	}
	if f.turns[key] == nil && len(f.turns)+len(f.retired) >= f.cfg.MaxRetiredTurns {
		return ErrRetiredTurnLimit
	}
	if err := f.acceptTimeLocked(at); err != nil {
		return err
	}
	delete(f.turns, key)
	f.retired[key] = struct{}{}
	return nil
}

func (f *Finalizer) acceptIdentityLocked(turn core.TurnID, generation core.Generation) (turnKey, bool, error) {
	key := turnKey{turn: turn, gen: generation}
	if turn == "" || generation == 0 {
		return key, false, ErrInvalidIdentity
	}
	if f.generation != 0 && generation < f.generation {
		return key, true, nil
	}
	if generation > f.generation {
		f.generation = generation
		f.lastAt = time.Time{}
		clear(f.turns)
		clear(f.retired)
	}
	return key, false, nil
}

func (f *Finalizer) acceptTimeLocked(at time.Time) error {
	if at.IsZero() {
		return ErrInvalidTimestamp
	}
	if !f.lastAt.IsZero() && at.Before(f.lastAt) {
		return ErrTimestampRegression
	}
	f.lastAt = at
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

func (f *Finalizer) retireLocked(key turnKey, state *turnState, at time.Time) []Output {
	delete(f.turns, key)
	f.retired[key] = struct{}{}
	if state == nil || !state.hasCandidate {
		return nil
	}
	return []Output{f.output(Committed, key, state.text, at)}
}

func (f *Finalizer) output(kind OutputKind, key turnKey, text string, at time.Time) Output {
	return Output{Kind: kind, Turn: key.turn, Generation: key.gen, Text: strings.Clone(text), At: at}
}
