package retention

import "time"

// WithClock sets a custom clock function for deterministic time in tests.
func WithClock(fn func() time.Time) Option {
	return func(j *Job) {
		if fn != nil {
			j.clock = fn
		}
	}
}
