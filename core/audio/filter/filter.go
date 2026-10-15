// Package filter is the inbound audio filter chain: pluggable PCM16 mono
// filters (denoise, and later RNNoise/Krisp/AIC) that run between the DTMF
// detector and VAD.
//
// Pipeline order: transport -> DTMF detector -> filter chain -> VAD -> STT.
// DTMF sees unfiltered audio because noise suppression attenuates keypad
// tones. Recording taps inbound audio at the transport, so recordings hold the
// raw audio, not the filtered audio (raw by default; a "record filtered"
// option would need a tap after the chain).
package filter

import (
	"errors"
	"fmt"
	"time"
)

// ErrUnsupportedRate is returned by a filter that cannot run at the given rate.
var ErrUnsupportedRate = errors.New("filter: unsupported sample rate")

// Filter transforms mono PCM16 audio. An instance is stateful (it carries
// noise estimates and overlap buffers across calls) and must belong to exactly
// one session; it is not safe for concurrent use.
//
// Contract: Process returns a NEW slice of len(pcm) and never mutates pcm.
// Any algorithmic delay is constant and fixed (see Delayer); the output is the
// input delayed by that amount, so chunking never changes the result.
// Implementations that wrap native code (RNNoise, Krisp, AIC) or a remote
// worker satisfy the same interface; the chain does not care.
type Filter interface {
	// Process must not retain pcm after returning (the caller reuses it) and
	// returns a new slice.
	Process(pcm []int16, rate int) ([]int16, error)
	// Reset drops all adaptive state (call between sessions or streams).
	Reset()
	Close() error
}

// Delayer is optionally implemented by filters that add algorithmic latency.
type Delayer interface{ Delay() time.Duration }

// Namer is optionally implemented by filters; the name labels metrics.
type Namer interface{ Name() string }

// Observer is called once per filter per Process call with the wall time spent.
type Observer func(name string, d time.Duration)

// Chain applies filters in order. It is itself a Filter.
type Chain struct {
	filters []Filter
	obs     Observer
}

func NewChain(filters ...Filter) *Chain { return &Chain{filters: filters} }

// SetObserver installs a timing hook; nil disables it. Call before use.
func (c *Chain) SetObserver(o Observer) { c.obs = o }

// Len is the number of filters.
func (c *Chain) Len() int { return len(c.filters) }

func (c *Chain) Process(pcm []int16, rate int) ([]int16, error) {
	cur := pcm
	for i, f := range c.filters {
		var t0 time.Time
		if c.obs != nil {
			t0 = time.Now()
		}
		out, err := f.Process(cur, rate)
		if err != nil {
			return nil, fmt.Errorf("filter %d (%s): %w", i, nameOf(f), err)
		}
		if len(out) != len(cur) {
			return nil, fmt.Errorf("filter %d (%s): returned %d samples for %d", i, nameOf(f), len(out), len(cur))
		}
		if c.obs != nil {
			c.obs(nameOf(f), time.Since(t0))
		}
		cur = out
	}
	if len(c.filters) == 0 { // keep the "new slice" contract
		cur = append([]int16(nil), pcm...)
	}
	return cur, nil
}

func (c *Chain) Reset() {
	for _, f := range c.filters {
		f.Reset()
	}
}

func (c *Chain) Close() error {
	var errs []error
	for _, f := range c.filters {
		errs = append(errs, f.Close())
	}
	return errors.Join(errs...)
}

// Delay is the summed algorithmic latency of the chain.
func (c *Chain) Delay() time.Duration {
	var d time.Duration
	for _, f := range c.filters {
		if x, ok := f.(Delayer); ok {
			d += x.Delay()
		}
	}
	return d
}

func nameOf(f Filter) string {
	if n, ok := f.(Namer); ok {
		return n.Name()
	}
	return "other"
}
