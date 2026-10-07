// Package resilience is the circuit breaker and retry loop shared by every
// provider. Providers supply only a Classify function for their errors.
package resilience

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"time"
)

var (
	ErrCircuitOpen    = errors.New("provider circuit breaker is open")
	ErrRetryCommitted = errors.New("retry prohibited after output was committed")
	// ErrTimeout is the cause of a request's own deadline (Policy.AttemptTimeout).
	// Unlike a caller's deadline (a tool round) or cancellation (barge-in), it
	// means the provider hung, so it counts against the breaker.
	ErrTimeout = errors.New("provider request timed out")
)

// State is a breaker state; the strings are what metrics and /ready report.
type State string

const (
	Closed   State = "CLOSED"
	Open     State = "OPEN"
	HalfOpen State = "HALF_OPEN"
)

type Config struct {
	FailureThreshold int           // failures in a row that open the breaker
	Cooldown         time.Duration // time open before a half-open probe
}

func DefaultConfig() Config { return Config{FailureThreshold: 5, Cooldown: 30 * time.Second} }

// Breaker opens after FailureThreshold failures and lets one probe through
// after Cooldown. One breaker may be shared by every call to a provider.
type Breaker struct {
	mu          sync.Mutex
	cfg         Config
	state       State
	failures    int
	lastFailure time.Time
	probing     bool
	probe       uint64 // token of the granted half-open probe; 0 when none
	onChange    func(State)
}

func NewBreaker(cfg Config) *Breaker {
	def := DefaultConfig()
	if cfg.FailureThreshold <= 0 {
		cfg.FailureThreshold = def.FailureThreshold
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = def.Cooldown
	}
	return &Breaker{cfg: cfg, state: Closed}
}

// Allow reports whether a request may proceed.
func (b *Breaker) Allow() bool {
	ok, _ := b.allow()
	return ok
}

// allow also returns the probe token when it grants the half-open probe (0
// otherwise); only that token can release the probe.
func (b *Breaker) allow() (bool, uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case Closed:
		return true, 0
	case Open:
		if time.Since(b.lastFailure) < b.cfg.Cooldown {
			return false, 0
		}
		b.state = HalfOpen
	}
	if b.probing {
		return false, 0
	}
	b.probing = true
	b.probe++
	return true, b.probe
}

// SetObserver receives state changes; it runs without the breaker lock.
func (b *Breaker) SetObserver(fn func(State)) {
	b.mu.Lock()
	b.onChange = fn
	b.mu.Unlock()
}

// State is the current state.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

func (b *Breaker) RecordSuccess() {
	b.mu.Lock()
	b.failures, b.probing, b.state = 0, false, Closed
	b.notifyLocked()
}

func (b *Breaker) RecordFailure() {
	b.mu.Lock()
	b.failures++
	b.lastFailure = time.Now()
	b.probing = false
	if b.state == HalfOpen || b.failures >= b.cfg.FailureThreshold {
		b.state = Open
	}
	b.notifyLocked()
}

// notifyLocked unlocks b and then reports the state.
func (b *Breaker) notifyLocked() {
	fn, st := b.onChange, b.state
	b.mu.Unlock()
	if fn != nil {
		fn(st)
	}
}

// release frees the half-open probe of a call that ended without a provider
// outcome (cancelled, or the caller's fault). A stale call cannot free another
// call's probe.
func (b *Breaker) release(probe uint64) {
	b.mu.Lock()
	if probe != 0 && probe == b.probe {
		b.probing = false
	}
	b.mu.Unlock()
}

// Report records a provider error seen outside Execute (e.g. on a stream):
// client errors are ignored, anything else counts.
func (b *Breaker) Report(err error, classify Classify) {
	if !classify(err).ClientError {
		b.RecordFailure()
	}
}

// Verdict is a provider's reading of one of its errors.
type Verdict struct {
	Retryable   bool          // worth another attempt (unless output was committed)
	ClientError bool          // the caller's fault (bad key, bad request): not a provider outage
	RetryAfter  time.Duration // provider-requested wait, if any
}

type Classify func(error) Verdict

type Policy struct {
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	Multiplier     float64
	MaxAttempts    int
	// AttemptTimeout bounds one request (for a streamed LLM reply, the whole
	// stream). Hitting it is ErrTimeout: a provider failure, retried unless
	// output was committed.
	AttemptTimeout time.Duration
}

func DefaultPolicy() Policy {
	return Policy{InitialBackoff: 100 * time.Millisecond, MaxBackoff: 2 * time.Second, Multiplier: 2, MaxAttempts: 3, AttemptTimeout: 30 * time.Second}
}

// sleep waits d or until ctx ends. Tests swap it.
var sleep = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Execute runs op (attempt counts from 0) under the breaker and retry policy:
//   - the breaker refuses the call while open (ErrCircuitOpen);
//   - cancellation and the caller's own deadline are not provider failures;
//   - an attempt that runs past AttemptTimeout fails with ErrTimeout and counts;
//   - other errors count unless classify says they are the caller's fault;
//   - nothing is retried once committed reports true (ErrRetryCommitted);
//   - retries use capped, fully jittered exponential backoff or RetryAfter.
//
// b and committed may be nil.
func Execute(ctx context.Context, b *Breaker, p Policy, classify Classify, committed func() bool, op func(ctx context.Context, attempt int) error) error {
	def := DefaultPolicy()
	if p.MaxAttempts <= 0 {
		p.MaxAttempts = 1
	}
	if p.InitialBackoff <= 0 {
		p.InitialBackoff = def.InitialBackoff
	}
	if p.MaxBackoff <= 0 {
		p.MaxBackoff = def.MaxBackoff
	}
	if p.Multiplier <= 1 {
		p.Multiplier = def.Multiplier
	}
	if p.AttemptTimeout <= 0 {
		p.AttemptTimeout = def.AttemptTimeout
	}
	backoff := p.InitialBackoff
	var err error
	for attempt := 0; attempt < p.MaxAttempts; attempt++ {
		var probe uint64
		if b != nil {
			var ok bool
			if ok, probe = b.allow(); !ok {
				return ErrCircuitOpen
			}
		}
		if ctx.Err() != nil {
			b.releaseIfSet(probe)
			return ctx.Err()
		}

		attemptCtx, cancel := context.WithTimeoutCause(ctx, p.AttemptTimeout, ErrTimeout)
		err = op(attemptCtx, attempt)
		timedOut := errors.Is(context.Cause(attemptCtx), ErrTimeout)
		cancel()
		if err == nil {
			if b != nil {
				b.RecordSuccess()
			}
			return nil
		}

		var v Verdict
		switch {
		case timedOut:
			err, v = ErrTimeout, Verdict{Retryable: true}
		case ctx.Err() != nil || errors.Is(err, context.Canceled):
			b.releaseIfSet(probe)
			return err
		default:
			v = classify(err)
		}
		if b != nil {
			if v.ClientError {
				b.release(probe)
			} else {
				b.RecordFailure()
			}
		}

		if committed != nil && committed() {
			return errors.Join(ErrRetryCommitted, err)
		}
		if !v.Retryable || attempt == p.MaxAttempts-1 {
			return err
		}
		delay := backoff
		if v.RetryAfter > 0 {
			delay = v.RetryAfter
		}
		delay = min(time.Duration(rand.Float64()*float64(delay)), p.MaxBackoff) // full jitter
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= delay {
			return err // no time left to retry
		}
		if serr := sleep(ctx, delay); serr != nil {
			return serr
		}
		backoff = min(time.Duration(float64(backoff)*p.Multiplier), p.MaxBackoff)
	}
	return err
}

func (b *Breaker) releaseIfSet(probe uint64) {
	if b != nil {
		b.release(probe)
	}
}
