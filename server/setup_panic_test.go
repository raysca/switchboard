package server

import (
	"testing"
	"time"

	"govoice/core"
)

// A panic in a provider constructor during call setup must not leak the admission slot or the stored session row.
func TestSetupPanicReleasesAdmissionAndRow(t *testing.T) {
	probe := newProbe()
	r := newFlowRigE2E(t, probe)
	r.s.registry.Register("echo.tts", func(cfg *Config, spec StageSpec) (core.Processor, error) { panic("tts ctor bug") })
	c := r.dial(r.createRoutedFlow("salesvoice"))
	select {
	case <-c.done:
	case <-time.After(5 * time.Second):
		t.Fatal("connection not closed after setup panic")
	}
	time.Sleep(300 * time.Millisecond)
	r.s.admission.mu.Lock()
	active := r.s.admission.active
	r.s.admission.mu.Unlock()
	t.Logf("admission active=%d storedActiveRows=%d", active, r.activeSessions())
	if active != 0 {
		t.Errorf("admission slot leaked: active=%d", active)
	}
	if n := r.activeSessions(); n != 0 {
		t.Errorf("stored session row left active: %d", n)
	}
}
