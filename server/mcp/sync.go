package mcp

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"time"
)

// Syncer re-syncs every enabled server in the background, one round at a time.
// A failure on a healthy server is written only when the next round fails too
// (two strikes); a success, or a failure on a server already failing, is
// written at once. A manual Refresh always writes its own result.
type Syncer struct {
	svc      *Service
	interval time.Duration
	logger   *slog.Logger
	onResult func(ok bool)
	strikes  map[string]int // server id -> consecutive failures; the loop goroutine only
	cancel   context.CancelFunc
	done     chan struct{}
}

// NewSyncer returns a stopped syncer; interval must be > 0. onResult (may be
// nil) is told the outcome of each server sync.
func NewSyncer(s *Service, interval time.Duration, logger *slog.Logger, onResult func(ok bool)) *Syncer {
	return &Syncer{svc: s, interval: interval, logger: logger, onResult: onResult, strikes: map[string]int{}, done: make(chan struct{})}
}

// Start runs the loop until Stop. Call it once.
func (y *Syncer) Start() {
	ctx, cancel := context.WithCancel(context.Background()) // owner lifetime: cancelled by Stop
	y.cancel = cancel
	go func() {
		defer close(y.done)
		// A random first delay spreads instances started together.
		timer := time.NewTimer(rand.N(y.interval))
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			y.round(ctx)
			timer.Reset(y.interval) // after the round: rounds never overlap
		}
	}()
}

// Stop cancels the round in progress and waits for the loop, at most until ctx ends.
func (y *Syncer) Stop(ctx context.Context) error {
	if y.cancel == nil {
		return nil
	}
	y.cancel()
	select {
	case <-y.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// round syncs each enabled server that is not pending deletion, one after another.
func (y *Syncer) round(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			y.logger.Error("MCP sync round panicked", slog.Any("panic", r))
		}
	}()
	servers, err := y.svc.store.MCP().ListServers(ctx)
	if err != nil {
		if ctx.Err() == nil {
			y.logger.Warn("MCP sync could not list servers", slog.String("err", err.Error()))
		}
		return
	}
	present := make(map[string]bool, len(servers))
	for _, m := range servers {
		present[m.ID] = true
		if !m.Enabled || m.PendingDelete {
			delete(y.strikes, m.ID)
			continue
		}
		sctx, cancel := y.svc.timed(ctx)
		r := y.svc.syncServer(sctx, m)
		cancel()
		if ctx.Err() != nil { // stopping: this result says nothing about the server
			return
		}
		ok := r.Err == nil
		if y.onResult != nil {
			y.onResult(ok)
		}
		// Stored healthy with a count of 2 means something else (a manual Sync)
		// healed it since: that is a fresh first strike too.
		if !ok && m.ValidationState == StateHealthy && y.strikes[m.ID] != 1 {
			y.strikes[m.ID] = 1
			continue
		}
		y.svc.writeResult(ctx, m.ID, r)
		if ok {
			delete(y.strikes, m.ID)
		} else {
			y.strikes[m.ID] = 2
		}
		if r.State != m.ValidationState {
			y.logger.Info("MCP server state changed", slog.String("mcp_server_id", m.ID), slog.String("from", m.ValidationState), slog.String("to", r.State))
		}
	}
	for id := range y.strikes {
		if !present[id] {
			delete(y.strikes, id)
		}
	}
}
