package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"govoice/core"
	"govoice/server/agent"
)

// waitSetupReleased waits for a panicked setup's connection to close, then polls until the admission
// slots held beyond baseline and the stored active rows are gone.
func waitSetupReleased(t *testing.T, r *agentRigE2E, c *e2eCall, baseline int) {
	t.Helper()
	select {
	case <-c.done:
	case <-time.After(5 * time.Second):
		t.Fatal("connection not closed after setup panic")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		r.s.admission.mu.Lock()
		active := r.s.admission.active
		r.s.admission.mu.Unlock()
		rows := r.activeSessions()
		if active == baseline && rows == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("setup panic left admission active=%d (want %d), stored active rows=%d", active, baseline, rows)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A panic in a provider constructor during call setup must not leak the admission slot or the stored session row.
func TestSetupPanicReleasesAdmissionAndRow(t *testing.T) {
	r := newAgentRigE2E(t, newProbe())
	r.s.registry.Register("echo.tts", func(cfg *Config, spec StageSpec) (core.Processor, error) { panic("tts ctor bug") })
	waitSetupReleased(t, r, r.dial(r.createRoutedAgent("salesvoice")), 0)
}

// A panic after the flow is resolved but before the build's fail path exists (here a nil step background
// in the call's flow) must still end the stored row and release the admission slot.
func TestSetupPanicAfterResolveEndsRow(t *testing.T) {
	r := newAgentRigE2E(t, newProbe())
	r.s.adjustCall = func(call *CallSpec) {
		call.Agent = &CallAgent{Start: "start", Backgrounds: map[string]*agent.BackgroundSettings{"other": nil}}
	}
	waitSetupReleased(t, r, r.dial(r.createRoutedAgent("salesvoice")), 0)
}

// A panic while the resolved call is being finished (after the row and pins exist) ends the row.
func TestResolveCallPanicEndsRow(t *testing.T) {
	r := newAgentRigE2E(t, newProbe())
	r.s.adjustCall = func(*CallSpec) { panic("adjust bug") }
	waitSetupReleased(t, r, r.dial(r.createRoutedAgent("salesvoice")), 0)
}

// A panic inside the fail path, after it released the slot, must not release it a second time when the
// setup recover runs fail again (a second call's slot, held here, would be taken).
func TestFailPathPanicReleasesAdmissionOnce(t *testing.T) {
	r := newAgentRigE2E(t, newProbe())
	r.s.registry.Register("echo.tts", func(cfg *Config, spec StageSpec) (core.Processor, error) { return nil, errors.New("tts down") })
	if err := r.s.admission.AcquireSession("other", "other-flow", true); err != nil {
		t.Fatal(err)
	}
	r.s.logger = slog.New(panicOnSetupFailure{slog.NewTextHandler(io.Discard, nil)}) // fail's last step panics
	waitSetupReleased(t, r, r.dial(r.createRoutedAgent("salesvoice")), 1)
}

// panicOnSetupFailure panics when the call-setup failure is logged, the last step of the fail path.
type panicOnSetupFailure struct{ slog.Handler }

func (h panicOnSetupFailure) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == "call setup failed" {
		panic("log sink bug")
	}
	return h.Handler.Handle(ctx, r)
}
