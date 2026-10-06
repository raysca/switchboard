package core

import (
	"context"
	"testing"
	"time"
)

type describableProcessor struct {
	desc string
}

func (d *describableProcessor) ProcessFrame(ctx context.Context, f Frame, dir Direction, e Emitter) error {
	return e.Push(f)
}

func (d *describableProcessor) Description() string {
	return d.desc
}

func TestSession_SnapshotIntrospection(t *testing.T) {
	proc1 := &describableProcessor{desc: "Inbound VAD Gate"}
	proc2 := &testFnProcessor{fn: func(_ context.Context, f Frame, d Direction, e Emitter) error {
		return e.PushDirection(f, d)
	}}

	pipe, err := NewPipeline(proc1, proc2)
	if err != nil {
		t.Fatalf("NewPipeline failed: %v", err)
	}

	session, err := NewSession(1001, pipe, DefaultResourceBudget())
	if err != nil {
		t.Fatalf("NewSession failed: %v", err)
	}

	if err := session.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer session.Close()

	// Apply an initial runtime settings update
	lang := "en-US"
	silence := 450 * time.Millisecond
	u := &RuntimeSettingsUpdate{
		Revision:       1,
		Language:       &lang,
		SilenceTimeout: &silence,
	}
	if err := session.Post(u); err != nil {
		t.Fatalf("Post settings update failed: %v", err)
	}

	// Allow settings event to process
	time.Sleep(20 * time.Millisecond)

	snap := session.Snapshot()

	if snap.SessionID != 1001 {
		t.Errorf("expected session ID 1001, got %d", snap.SessionID)
	}
	if snap.State != "running" {
		t.Errorf("expected running state, got %s", snap.State)
	}
	if snap.Settings.Language != "en-US" || snap.Settings.SilenceTimeout != 450*time.Millisecond {
		t.Errorf("unexpected snapshot settings: %+v", snap.Settings)
	}
	if len(snap.Processors) != 2 {
		t.Fatalf("expected 2 processors in snapshot, got %d", len(snap.Processors))
	}
	if snap.Processors[0].Description != "Inbound VAD Gate" {
		t.Errorf("expected processor description, got %q", snap.Processors[0].Description)
	}
	if snap.Processors[0].Index != 0 || snap.Processors[1].Index != 1 {
		t.Errorf("unexpected processor indices: %+v", snap.Processors)
	}
}
