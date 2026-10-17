// Package resilience is the circuit breaker and retry loop shared by every
// provider. Providers supply only a Classify function for their errors.
package resilience

import (
	"context"
	"errors"
	"fmt"
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
	half := false
	ok, probe := func() (bool, uint64) {
		switch b.state {
		case Closed:
			return true, 0
		case Open:
			if time.Since(b.lastFailure) < b.cfg.Cooldown {
				return false, 0
			}
			b.state, half = HalfOpen, true
		}
		if b.probing {
			return false, 0
		}
		b.probing = true
		b.probe++
		return true, b.probe
	}()
	fn := b.onChange
	b.mu.Unlock()
	if half && fn != nil {
		fn(HalfOpen) // observers see the probe window, not OPEN until it resolves
	}
	return ok, probe
}

// WouldRefuse reports, without changing state, whether Allow would refuse now
// because the breaker is open and still inside its cooldown. A cooled-down
// breaker is not refusing (its next call is the half-open probe).
func (b *Breaker) WouldRefuse() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state == Open && time.Since(b.lastFailure) < b.cfg.Cooldown
}

// SetObserver receives state changes; it runs without the breaker lock.
func (b *Breaker) SetObserver(fn func(State)) {
	b.mu.Lock()
	b.onChange = fn
	b.mu.Unlock()
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
	if v := classify(err); !v.ClientError && !v.NoBreaker {
		b.RecordFailure()
	}
}

// Verdict is a provider's reading of one of its errors.
type Verdict struct {
	Retryable   bool // worth another attempt (unless output was committed)
	ClientError bool // the caller's fault (bad key, bad request): not a provider outage
	// NoBreaker: retry and fail over as usual but do not count on the breaker.
	// For one credential's quota or rate limit (429): it says nothing about the
	// provider's health and breakers are shared by every tenant.
	NoBreaker  bool
	RetryAfter time.Duration // provider-requested wait, if any
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
	// OnRetry, if set, is told when a failed attempt has been judged
	// retryable, before the backoff wait, with the retry's number (1 for the
	// second attempt). A cancellation during the backoff means that retry
	// never starts. It runs on the caller's goroutine and must not block; a
	// panic in it is contained.
	OnRetry func(retry int)
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
			if v.ClientError || v.NoBreaker {
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
		// The provider's Retry-After is honoured as given (capped); our own
		// backoff is fully jittered.
		delay := min(time.Duration(rand.Float64()*float64(backoff)), p.MaxBackoff)
		if v.RetryAfter > 0 {
			delay = min(v.RetryAfter, p.MaxBackoff)
		}
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= delay {
			return err // no time left to retry
		}
		if p.OnRetry != nil {
			func() {
				defer func() { _ = recover() }() // an observer bug must not fail the request
				p.OnRetry(attempt + 1)
			}()
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

// OpenBounded runs open (a stream or connection setup whose ctx must outlive the
// call: the stream lives on it) under an open deadline and a bounded retry:
// at most p.MaxAttempts tries, each given p.AttemptTimeout to return. A try
// that does not return in time has its ctx cancelled and fails with an error
// matching both ErrTimeout and context.DeadlineExceeded; it, and any error
// classify calls retryable (and not the caller's fault), is retried after
// p.InitialBackoff. Breaker accounting stays with the caller (one report per
// call). The ctx open receives derives from parent, so cancelling parent ends
// the opened stream; on success it is not cancelled here.
func OpenBounded[T any](parent context.Context, p Policy, classify Classify, open func(ctx context.Context) (T, error)) (T, error) {
	var zero T
	def := DefaultPolicy()
	if p.MaxAttempts <= 0 {
		p.MaxAttempts = 1
	}
	if p.AttemptTimeout <= 0 {
		p.AttemptTimeout = def.AttemptTimeout
	}
	if p.InitialBackoff <= 0 {
		p.InitialBackoff = def.InitialBackoff
	}
	var err error
	for attempt := 0; attempt < p.MaxAttempts; attempt++ {
		ctx, cancel := context.WithCancelCause(parent)
		timer := time.AfterFunc(p.AttemptTimeout, func() { cancel(ErrTimeout) })
		var v T
		v, err = open(ctx)
		if !timer.Stop() { // fired: the ctx v may hang on is already cancelled
			if err == nil { // opened just as the deadline hit: release it
				switch c := any(v).(type) {
				case interface{ CloseNow() error }: // websocket conns
					_ = c.CloseNow()
				case interface{ Close() error }:
					_ = c.Close()
				}
			}
			err = fmt.Errorf("%w: open exceeded %v: %w", ErrTimeout, p.AttemptTimeout, context.DeadlineExceeded)
		} else if err == nil {
			return v, nil
		}
		cancel(err)
		if parent.Err() != nil {
			return zero, err
		}
		if !errors.Is(err, ErrTimeout) {
			if vd := classify(err); !vd.Retryable || vd.ClientError {
				return zero, err
			}
		}
		if attempt < p.MaxAttempts-1 {
			if serr := sleep(parent, p.InitialBackoff); serr != nil {
				return zero, serr
			}
		}
	}
	return zero, err
}
