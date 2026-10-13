package auth

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// maxThrottleEntries bounds the per-address table.
const maxThrottleEntries = 10000

// overflowKey is the shared bucket new addresses fall into when the table is full of active ones.
const overflowKey = "*"

// Throttle limits failed authentication attempts per client address: each address may fail
// burst times, then one more attempt every `every`. A blocked address is refused before its
// credential is looked at, so the correct key from a blocked address is not an oracle.
// Only addresses that have failed hold an entry.
type Throttle struct {
	burst int
	every time.Duration
	now   func() time.Time // monotonic clock readings come from time.Now

	mu     sync.Mutex
	limits map[string]*rate.Limiter
}

func NewThrottle(burst int, every time.Duration) *Throttle {
	return &Throttle{burst: burst, every: every, now: time.Now, limits: make(map[string]*rate.Limiter)}
}

func (t *Throttle) limiter(key string, now time.Time) *rate.Limiter {
	if l, ok := t.limits[key]; ok {
		return l
	}
	if len(t.limits) >= maxThrottleEntries {
		for k, l := range t.limits { // sweep addresses whose allowance is whole again
			if l.TokensAt(now) >= float64(t.burst) {
				delete(t.limits, k)
			}
		}
		if len(t.limits) >= maxThrottleEntries {
			key = overflowKey // a flood from many addresses shares one bucket
			if l, ok := t.limits[key]; ok {
				return l
			}
		}
	}
	l := rate.NewLimiter(rate.Every(t.every), t.burst)
	t.limits[key] = l
	return l
}

// Blocked reports whether key has used up its failures.
func (t *Throttle) Blocked(key string) bool {
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	l, ok := t.limits[key]
	return ok && l.TokensAt(now) < 1
}

// Fail records one failed attempt by key.
func (t *Throttle) Fail(key string) {
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.limiter(key, now).AllowN(now, 1)
}
