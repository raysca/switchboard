package audio

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"govoice/core"
)

var (
	ErrFilterChainClosed        = errors.New("audio: filter chain is closed")
	ErrFilterIncompatibleFormat = errors.New("audio: filter input format does not match preceding filter output format")
	ErrFilterNotFound           = errors.New("audio: filter with specified ID not found")
	ErrDuplicateFilterID        = errors.New("audio: filter with specified ID already exists")
)

// FilterEntry wraps an AudioFilter with metadata and bypass control.
type FilterEntry struct {
	ID       string
	Filter   core.AudioFilter
	Bypassed bool
}

// FilterChain executes a sequential pipeline of AudioFilters with bypass support and format validation.
type FilterChain struct {
	mu      sync.RWMutex
	filters []*FilterEntry
	closed  bool
}

// NewFilterChain creates a new empty FilterChain.
func NewFilterChain() *FilterChain {
	return &FilterChain{
		filters: make([]*FilterEntry, 0),
	}
}

// AddFilter appends an AudioFilter to the chain, checking format compatibility with the current chain tail.
func (fc *FilterChain) AddFilter(id string, filter core.AudioFilter) error {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	if fc.closed {
		return ErrFilterChainClosed
	}

	for _, entry := range fc.filters {
		if entry.ID == id {
			return ErrDuplicateFilterID
		}
	}

	// Validate format compatibility with preceding filter if present
	if len(fc.filters) > 0 {
		last := fc.filters[len(fc.filters)-1]
		lastOut := last.Filter.OutputFormat()
		nextIn := filter.InputFormat()

		if lastOut != nextIn {
			return fmt.Errorf("%w: expected %v, got %v", ErrFilterIncompatibleFormat, lastOut, nextIn)
		}
	}

	fc.filters = append(fc.filters, &FilterEntry{
		ID:       id,
		Filter:   filter,
		Bypassed: false,
	})
	return nil
}

// SetBypass enables or disables bypass for a specific filter in the chain.
func (fc *FilterChain) SetBypass(id string, bypass bool) error {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	if fc.closed {
		return ErrFilterChainClosed
	}

	for _, entry := range fc.filters {
		if entry.ID == id {
			// Bypassing a filter that changes format is only safe if input and output formats match
			if bypass && entry.Filter.InputFormat() != entry.Filter.OutputFormat() {
				return errors.New("audio: cannot bypass filter with disparate input and output formats")
			}
			entry.Bypassed = bypass
			return nil
		}
	}
	return ErrFilterNotFound
}

// TotalLatency returns the cumulative latency of all non-bypassed filters in the chain.
func (fc *FilterChain) TotalLatency() time.Duration {
	fc.mu.RLock()
	defer fc.mu.RUnlock()

	var total time.Duration
	for _, entry := range fc.filters {
		if !entry.Bypassed {
			total += entry.Filter.Latency()
		}
	}
	return total
}

// Process passes audio sequentially through all active filters in the chain.
func (fc *FilterChain) Process(ctx context.Context, in []byte) ([]byte, error) {
	fc.mu.RLock()
	defer fc.mu.RUnlock()

	if fc.closed {
		return nil, ErrFilterChainClosed
	}

	current := in
	for _, entry := range fc.filters {
		if entry.Bypassed {
			continue
		}
		out, err := entry.Filter.Process(ctx, current)
		if err != nil {
			return nil, fmt.Errorf("audio: filter %q failed: %w", entry.ID, err)
		}
		current = out
	}
	return current, nil
}

// Flush flushes remaining samples through all active filters.
func (fc *FilterChain) Flush(ctx context.Context) ([]byte, error) {
	fc.mu.RLock()
	defer fc.mu.RUnlock()

	if fc.closed {
		return nil, ErrFilterChainClosed
	}

	var accumulated []byte
	for _, entry := range fc.filters {
		if entry.Bypassed {
			continue
		}
		out, err := entry.Filter.Flush(ctx)
		if err != nil {
			return nil, fmt.Errorf("audio: filter %q flush failed: %w", entry.ID, err)
		}
		if len(out) > 0 {
			accumulated = append(accumulated, out...)
		}
	}
	return accumulated, nil
}

// Reset resets state across all filters.
func (fc *FilterChain) Reset() error {
	fc.mu.RLock()
	defer fc.mu.RUnlock()

	if fc.closed {
		return ErrFilterChainClosed
	}

	for _, entry := range fc.filters {
		if err := entry.Filter.Reset(); err != nil {
			return fmt.Errorf("audio: filter %q reset failed: %w", entry.ID, err)
		}
	}
	return nil
}

// Close closes all filters in the chain and marks the chain as closed.
func (fc *FilterChain) Close() error {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	if fc.closed {
		return nil
	}
	fc.closed = true

	var firstErr error
	for _, entry := range fc.filters {
		if err := entry.Filter.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
