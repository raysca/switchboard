package mcp

import (
	"context"
	"log/slog"
	"maps"
	"math/rand/v2"
	"slices"
	"time"

	"govoice/server/storage"
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
	strikes  map[string]bool // server ids with one unwritten failure on a healthy state; the loop goroutine only
	cancel   context.CancelFunc
	done     chan struct{}
}

// NewSyncer returns a stopped syncer; interval must be > 0. onResult (may be
// nil) is told the outcome of each server sync.
func NewSyncer(s *Service, interval time.Duration, logger *slog.Logger, onResult func(ok bool)) *Syncer {
	return &Syncer{svc: s, interval: interval, logger: logger, onResult: onResult, strikes: map[string]bool{}, done: make(chan struct{})}
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
	for _, listed := range servers {
		// Work from a fresh read: an edit or manual Sync may have landed since the list.
		m, err := y.svc.store.MCP().Server(ctx, listed.ID)
		if err != nil || !m.Enabled || m.PendingDelete {
			delete(y.strikes, listed.ID)
			continue
		}
		// current reports whether m is still the stored server: same target, nobody wrote a newer result.
		current := func() bool {
			n, err := y.svc.store.MCP().Server(ctx, m.ID)
			return err == nil && n.Enabled && !n.PendingDelete && n.Endpoint == m.Endpoint && n.AuthEnv == m.AuthEnv && n.LastCheckedAt.Equal(m.LastCheckedAt)
		}
		sctx, cancel := y.svc.timed(ctx)
		r := y.svc.syncServer(sctx, m, current)
		cancel()
		if ctx.Err() != nil { // stopping: this result says nothing about the server
			return
		}
		// Stale, or the sync never started: write nothing and leave the strikes alone.
		// (A tiny race remains between this check and the write.)
		if r.State == "" || !current() {
			continue
		}
		ok := r.Err == nil
		if y.onResult != nil {
			y.onResult(ok)
		}
		// A failure on a stored-healthy server (also one a manual Sync healed
		// after a written failure) is a first strike: note it, write nothing.
		if !ok && m.ValidationState == StateHealthy && !y.strikes[m.ID] {
			y.strikes[m.ID] = true
			continue
		}
		y.svc.writeResult(ctx, m.ID, r)
		delete(y.strikes, m.ID)
		if r.State != m.ValidationState {
			y.logger.Info("MCP server state changed", slog.String("mcp_server_id", m.ID), slog.String("from", m.ValidationState), slog.String("to", r.State))
		}
	}
	maps.DeleteFunc(y.strikes, func(id string, _ bool) bool {
		return !slices.ContainsFunc(servers, func(l storage.MCPServer) bool { return l.ID == id })
	})
}
