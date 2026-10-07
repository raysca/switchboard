package resilience

import (
	"context"
	"errors"
	"testing"
	"time"
)

var errOutage = errors.New("503 unavailable")
var errClient = errors.New("401 bad key")

func classify(err error) Verdict {
	if errors.Is(err, errClient) {
		return Verdict{ClientError: true}
	}
	return Verdict{Retryable: true}
}

func fail(err error) func(context.Context, int) error {
	return func(context.Context, int) error { return err }
}

func noSleep(t *testing.T) *[]time.Duration {
	var slept []time.Duration
	orig := sleep
	sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	t.Cleanup(func() { sleep = orig })
	return &slept
}

func TestBreakerOpensProbesAndCloses(t *testing.T) {
	b := NewBreaker(Config{FailureThreshold: 2, Cooldown: 20 * time.Millisecond})
	var seen []State
	b.SetObserver(func(s State) { seen = append(seen, s) })
	b.RecordFailure()
	if !b.Allow() {
		t.Fatal("opened before the threshold")
	}
	b.RecordFailure()
	if b.Allow() || b.State() != Open {
		t.Fatal("did not open at the threshold")
	}
	time.Sleep(25 * time.Millisecond)
	if !b.Allow() || b.State() != HalfOpen {
		t.Fatal("no probe after cooldown")
	}
	if b.Allow() {
		t.Fatal("a second probe got through")
	}
	b.RecordFailure() // failed probe reopens
	if b.State() != Open {
		t.Fatal("failed probe did not reopen")
	}
	time.Sleep(25 * time.Millisecond)
	b.Allow()
	b.RecordSuccess()
	if b.State() != Closed || seen[len(seen)-1] != Closed {
		t.Fatalf("state %s, observed %v", b.State(), seen)
	}
}

func TestOnlyTheProbeHolderReleasesIt(t *testing.T) {
	b := NewBreaker(Config{FailureThreshold: 1, Cooldown: time.Millisecond})
	_, stale := b.allow() // admitted while closed: token 0
	b.RecordFailure()
	time.Sleep(2 * time.Millisecond)
	ok, probe := b.allow()
	if !ok {
		t.Fatal("no probe after cooldown")
	}
	b.release(stale)
	if b.Allow() {
		t.Fatal("a stale call freed the live probe")
	}
	b.release(probe)
	if !b.Allow() {
		t.Fatal("the holder could not release its probe")
	}
}

func TestClientErrorsDoNotCountOutagesDo(t *testing.T) {
	noSleep(t)
	b := NewBreaker(Config{FailureThreshold: 1, Cooldown: time.Hour})
	_ = Execute(context.Background(), b, Policy{MaxAttempts: 1}, classify, nil, fail(errClient))
	b.Report(errClient, classify)
	if !b.Allow() {
		t.Fatal("a tenant's bad request opened the shared breaker")
	}
	_ = Execute(context.Background(), b, Policy{MaxAttempts: 1}, classify, nil, fail(errOutage))
	if b.Allow() {
		t.Fatal("an outage did not count")
	}
	b = NewBreaker(Config{FailureThreshold: 1, Cooldown: time.Hour})
	b.Report(errOutage, classify)
	if b.Allow() {
		t.Fatal("a reported stream outage did not count")
	}
}

func TestHungAttemptTimesOutRetriesAndCounts(t *testing.T) {
	noSleep(t)
	b := NewBreaker(Config{FailureThreshold: 2, Cooldown: time.Hour})
	attempts := 0
	err := Execute(context.Background(), b, Policy{MaxAttempts: 2, AttemptTimeout: 10 * time.Millisecond}, classify, nil,
		func(ctx context.Context, _ int) error { attempts++; <-ctx.Done(); return ctx.Err() })
	if attempts != 2 || !errors.Is(err, ErrTimeout) || b.Allow() {
		t.Fatalf("attempts=%d err=%v open=%v; want 2 timed-out attempts that open the breaker", attempts, err, !b.Allow())
	}
}

// A caller's deadline (e.g. a slow tool round) and cancellation (barge-in) are
// not provider failures, and they free a half-open probe.
func TestCallerDeadlineAndCancellationDoNotCount(t *testing.T) {
	b := NewBreaker(Config{FailureThreshold: 1, Cooldown: time.Millisecond})
	hang := func(ctx context.Context, _ int) error { <-ctx.Done(); return ctx.Err() }
	deadline, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	_ = Execute(deadline, b, Policy{MaxAttempts: 1}, classify, nil, hang)
	_ = Execute(context.Background(), b, Policy{MaxAttempts: 1}, classify, nil, fail(context.Canceled))
	if !b.Allow() {
		t.Fatal("a caller deadline or cancellation counted")
	}

	b.RecordFailure()
	time.Sleep(2 * time.Millisecond)
	_ = Execute(context.Background(), b, Policy{MaxAttempts: 1}, classify, nil, fail(context.Canceled))
	if !b.Allow() {
		t.Fatal("a cancelled half-open probe was never released")
	}
}

func TestNoRetryAfterCommittedOutput(t *testing.T) {
	noSleep(t)
	attempts := 0
	err := Execute(context.Background(), nil, Policy{MaxAttempts: 3}, classify, func() bool { return true },
		func(context.Context, int) error { attempts++; return errOutage })
	if attempts != 1 || !errors.Is(err, ErrRetryCommitted) || !errors.Is(err, errOutage) {
		t.Fatalf("attempts=%d err=%v", attempts, err)
	}
}

func TestRetriesWithBackoffRetryAfterAndStopsOnNonRetryable(t *testing.T) {
	slept := noSleep(t)
	var attempts []int
	err := Execute(context.Background(), nil, Policy{MaxAttempts: 3, InitialBackoff: 10 * time.Millisecond, MaxBackoff: time.Second}, classify, nil,
		func(_ context.Context, attempt int) error {
			attempts = append(attempts, attempt)
			if attempt < 2 {
				return errOutage
			}
			return nil
		})
	if err != nil || len(attempts) != 3 || attempts[0] != 0 || len(*slept) != 2 {
		t.Fatalf("err=%v attempts=%v slept=%v", err, attempts, *slept)
	}
	for _, d := range *slept {
		if d > 20*time.Millisecond {
			t.Fatalf("backoff %v exceeds the jitter cap", d)
		}
	}

	*slept = nil
	retryAfter := func(error) Verdict { return Verdict{Retryable: true, RetryAfter: 500 * time.Millisecond} }
	_ = Execute(context.Background(), nil, Policy{MaxAttempts: 2, MaxBackoff: time.Second}, retryAfter, nil, fail(errOutage))
	if len(*slept) != 1 || (*slept)[0] != 500*time.Millisecond {
		t.Fatalf("RetryAfter not honoured exactly: %v", *slept)
	}

	calls := 0
	_ = Execute(context.Background(), nil, Policy{MaxAttempts: 3}, classify, nil, func(context.Context, int) error { calls++; return errClient })
	if calls != 1 {
		t.Fatalf("a non-retryable error was retried %d times", calls-1)
	}
}

func TestOpenBreakerRefusesCalls(t *testing.T) {
	b := NewBreaker(Config{FailureThreshold: 1, Cooldown: time.Hour})
	b.RecordFailure()
	called := false
	if err := Execute(context.Background(), b, Policy{MaxAttempts: 1}, classify, nil, func(context.Context, int) error { called = true; return nil }); !errors.Is(err, ErrCircuitOpen) || called {
		t.Fatalf("err=%v called=%v", err, called)
	}
}

func TestWouldRefuseOnlyWhileOpenInsideCooldown(t *testing.T) {
	b := NewBreaker(Config{FailureThreshold: 1, Cooldown: 20 * time.Millisecond})
	if b.WouldRefuse() {
		t.Fatal("closed breaker refuses")
	}
	b.RecordFailure()
	if !b.WouldRefuse() || b.State() != Open {
		t.Fatal("open breaker inside cooldown must refuse")
	}
	time.Sleep(30 * time.Millisecond)
	if b.WouldRefuse() || b.State() != Open {
		t.Fatal("cooled-down breaker must not refuse, and the check must not change state")
	}
}

func TestOpenBoundedTimesOutHungOpenAndRetries(t *testing.T) {
	p := Policy{MaxAttempts: 2, AttemptTimeout: 30 * time.Millisecond, InitialBackoff: time.Millisecond}
	calls := 0
	start := time.Now()
	_, err := OpenBounded(context.Background(), p, classify, func(ctx context.Context) (int, error) {
		calls++
		<-ctx.Done() // hangs until the open deadline
		return 0, ctx.Err()
	})
	if !errors.Is(err, ErrTimeout) || !errors.Is(err, context.DeadlineExceeded) || calls != 2 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("took %v", d)
	}
}

func TestOpenBoundedClientErrorNotRetriedAndSuccessCtxSurvives(t *testing.T) {
	p := Policy{MaxAttempts: 3, AttemptTimeout: 20 * time.Millisecond, InitialBackoff: time.Millisecond}
	calls := 0
	if _, err := OpenBounded(context.Background(), p, classify, func(context.Context) (int, error) { calls++; return 0, errClient }); !errors.Is(err, errClient) || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
	var opened context.Context
	if _, err := OpenBounded(context.Background(), p, classify, func(ctx context.Context) (int, error) { opened = ctx; return 1, nil }); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond) // past the open deadline: the stream's ctx must be alive
	if opened.Err() != nil {
		t.Fatalf("stream ctx cancelled after a successful open: %v", opened.Err())
	}
}
