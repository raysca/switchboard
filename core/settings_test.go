package core

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestRuntimeSettings_Validation(t *testing.T) {
	// Zero revision is invalid (revision must be > 0 and monotonic)
	uZero := &RuntimeSettingsUpdate{Revision: 0}
	if err := uZero.Validate(); err == nil {
		t.Fatal("expected error for revision 0")
	}

	// Negative durations are invalid
	negDur := -10 * time.Millisecond
	uNegSilence := &RuntimeSettingsUpdate{Revision: 1, SilenceTimeout: &negDur}
	if err := uNegSilence.Validate(); err == nil {
		t.Fatal("expected error for negative SilenceTimeout")
	}

	uNegTurn := &RuntimeSettingsUpdate{Revision: 1, TurnTimeout: &negDur}
	if err := uNegTurn.Validate(); err == nil {
		t.Fatal("expected error for negative TurnTimeout")
	}

	// Empty string language is invalid if provided
	emptyLang := ""
	uEmptyLang := &RuntimeSettingsUpdate{Revision: 1, Language: &emptyLang}
	if err := uEmptyLang.Validate(); err == nil {
		t.Fatal("expected error for empty string language")
	}

	// Valid update with all fields
	validMute := true
	validInter := true
	validSilence := 500 * time.Millisecond
	validTurn := 30 * time.Second
	validLang := "en-US"
	uValid := &RuntimeSettingsUpdate{
		Revision:            1,
		InputMuted:          &validMute,
		InterruptionEnabled: &validInter,
		SilenceTimeout:      &validSilence,
		TurnTimeout:         &validTurn,
		Language:            &validLang,
	}
	if err := uValid.Validate(); err != nil {
		t.Fatalf("unexpected error for valid update: %v", err)
	}
}

func TestRuntimeSettings_SupportedUpdateAndNoSessionRebuild(t *testing.T) {
	var observedFrames []Frame
	var mu sync.Mutex

	pipe, _ := NewPipeline(&testFnProcessor{fn: func(_ context.Context, f Frame, d Direction, e Emitter) error {
		mu.Lock()
		observedFrames = append(observedFrames, f)
		mu.Unlock()
		return e.PushDirection(f, d)
	}})

	s, err := NewSession(100, pipe, DefaultResourceBudget())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	appliedEvents := make(chan *RuntimeSettingsAppliedEvent, 10)
	s.OnOutput = func(f Frame) {
		if ev, ok := f.(*RuntimeSettingsAppliedEvent); ok {
			appliedEvents <- ev
		}
	}

	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	// Initial settings snapshot
	initial := s.RuntimeSettings()
	if initial.Revision != 0 {
		t.Fatalf("expected initial revision 0, got %d", initial.Revision)
	}
	if initial.InputMuted {
		t.Fatal("expected input not muted initially")
	}
	if !initial.InterruptionEnabled {
		t.Fatal("expected interruption enabled initially")
	}

	// Apply revision 1: mute input and set language to en-GB
	muted := true
	lang := "en-GB"
	silence := 400 * time.Millisecond
	u1 := &RuntimeSettingsUpdate{
		Revision:       1,
		InputMuted:     &muted,
		Language:       &lang,
		SilenceTimeout: &silence,
	}
	if err := s.Post(u1); err != nil {
		t.Fatalf("Post u1: %v", err)
	}

	select {
	case ev := <-appliedEvents:
		if ev.Revision != 1 {
			t.Fatalf("expected applied event revision 1, got %d", ev.Revision)
		}
		if !ev.Settings.InputMuted {
			t.Fatal("expected InputMuted true in applied event")
		}
		if ev.Settings.Language != "en-GB" {
			t.Fatalf("expected Language en-GB, got %s", ev.Settings.Language)
		}
		if ev.Settings.SilenceTimeout != 400*time.Millisecond {
			t.Fatalf("expected SilenceTimeout 400ms, got %v", ev.Settings.SilenceTimeout)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for RuntimeSettingsAppliedEvent revision 1")
	}

	// Verify session query returns updated settings
	current := s.RuntimeSettings()
	if current.Revision != 1 {
		t.Fatalf("expected current revision 1, got %d", current.Revision)
	}
	if !current.InputMuted || !s.InputMuted() {
		t.Fatal("expected session input muted")
	}
	if current.Language != "en-GB" {
		t.Fatalf("expected language en-GB, got %s", current.Language)
	}
	if current.SilenceTimeout != 400*time.Millisecond {
		t.Fatalf("expected silence timeout 400ms, got %v", current.SilenceTimeout)
	}
	if !current.InterruptionEnabled {
		t.Fatal("expected interruptionEnabled preserved as true")
	}

	// Session is NOT rebuilt: pipeline, ID, and context are intact and running
	if s.ID() != 100 {
		t.Fatalf("session ID changed: got %d, want 100", s.ID())
	}
	if s.State() != StateRunning {
		t.Fatalf("session state changed: %v", s.State())
	}
}

func TestRuntimeSettings_StaleRevisionRejected(t *testing.T) {
	pipe, _ := NewPipeline(&PassthroughProcessor{})
	s, _ := NewSession(101, pipe, DefaultResourceBudget())

	errorEvents := make(chan *RuntimeSettingsErrorEvent, 10)
	appliedEvents := make(chan *RuntimeSettingsAppliedEvent, 10)
	s.OnOutput = func(f Frame) {
		if ev, ok := f.(*RuntimeSettingsAppliedEvent); ok {
			appliedEvents <- ev
		} else if errEv, ok := f.(*RuntimeSettingsErrorEvent); ok {
			errorEvents <- errEv
		}
	}

	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	// Apply revision 5
	lang := "fr-FR"
	u5 := &RuntimeSettingsUpdate{
		Revision: 5,
		Language: &lang,
	}
	if err := s.Post(u5); err != nil {
		t.Fatalf("Post u5: %v", err)
	}

	select {
	case ev := <-appliedEvents:
		if ev.Revision != 5 {
			t.Fatalf("expected revision 5, got %d", ev.Revision)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for revision 5 applied")
	}

	// Attempt to apply revision 3 (stale < 5)
	langStale := "es-ES"
	u3 := &RuntimeSettingsUpdate{
		Revision: 3,
		Language: &langStale,
	}
	if err := s.Post(u3); err != nil {
		t.Fatalf("Post u3: %v", err)
	}

	select {
	case errEv := <-errorEvents:
		if errEv.Revision != 3 {
			t.Fatalf("expected error event for revision 3, got %d", errEv.Revision)
		}
		if !errors.Is(errEv.Err, ErrStaleSettingsRevision) {
			t.Fatalf("expected ErrStaleSettingsRevision, got %v", errEv.Err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for stale revision error event")
	}

	// Verify settings were NOT mutated
	curr := s.RuntimeSettings()
	if curr.Revision != 5 {
		t.Fatalf("expected revision to remain 5, got %d", curr.Revision)
	}
	if curr.Language != "fr-FR" {
		t.Fatalf("expected language to remain fr-FR, got %s", curr.Language)
	}

	// Attempt to apply duplicate revision 5 (stale <= 5)
	u5Dup := &RuntimeSettingsUpdate{
		Revision: 5,
		Language: &langStale,
	}
	if err := s.Post(u5Dup); err != nil {
		t.Fatalf("Post u5Dup: %v", err)
	}

	select {
	case errEv := <-errorEvents:
		if errEv.Revision != 5 {
			t.Fatalf("expected error event for revision 5, got %d", errEv.Revision)
		}
		if !errors.Is(errEv.Err, ErrStaleSettingsRevision) {
			t.Fatalf("expected ErrStaleSettingsRevision, got %v", errEv.Err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for duplicate revision error event")
	}
}

func TestRuntimeSettings_InvalidPartialUpdateRejectedAllOrNothing(t *testing.T) {
	pipe, _ := NewPipeline(&PassthroughProcessor{})
	s, _ := NewSession(102, pipe, DefaultResourceBudget())

	errorEvents := make(chan *RuntimeSettingsErrorEvent, 10)
	s.OnOutput = func(f Frame) {
		if errEv, ok := f.(*RuntimeSettingsErrorEvent); ok {
			errorEvents <- errEv
		}
	}

	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	// Initial state: unmuted, language empty
	initSettings := s.RuntimeSettings()

	// Construct an update that has a valid mute flag but an invalid negative SilenceTimeout
	mute := true
	negSilence := -500 * time.Millisecond
	uBad := &RuntimeSettingsUpdate{
		Revision:       1,
		InputMuted:     &mute,
		SilenceTimeout: &negSilence,
	}

	if err := s.Post(uBad); err != nil {
		t.Fatalf("Post uBad: %v", err)
	}

	select {
	case errEv := <-errorEvents:
		if errEv.Revision != 1 {
			t.Fatalf("expected error event for revision 1, got %d", errEv.Revision)
		}
		if !errors.Is(errEv.Err, ErrInvalidRuntimeSettings) {
			t.Fatalf("expected ErrInvalidRuntimeSettings, got %v", errEv.Err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for invalid settings error event")
	}

	// Verify all-or-nothing: InputMuted was NOT partially applied!
	afterSettings := s.RuntimeSettings()
	if afterSettings.Revision != initSettings.Revision {
		t.Fatalf("revision changed on invalid update: %d", afterSettings.Revision)
	}
	if afterSettings.InputMuted != initSettings.InputMuted {
		t.Fatal("InputMuted was partially applied despite validation failure")
	}
	if s.InputMuted() != false {
		t.Fatal("Session InputMuted() changed on invalid update")
	}
}
