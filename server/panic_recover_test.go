package server

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/redact"
	"govoice/server/storage"
)

// A panic during call setup refuses the call; it does not end the process.
func TestOnSessionReadyPanicRefusesTheCall(t *testing.T) {
	s := &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	sess, err := s.onSessionReady(nil) // a nil connection panics inside setup
	if sess != nil || err == nil {
		t.Fatalf("session %v err %v, want a refused call", sess, err)
	}
}

func TestRedactErrMasksStoreErrors(t *testing.T) {
	rd, err := redact.New(redact.Config{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{redactor: rd}
	got := s.redactErr(io.ErrUnexpectedEOF).Error() + s.redactErr(errString("dup key 4111 1111 1111 1111")).Error()
	if strings.Contains(got, "4111 1111") {
		t.Fatalf("store error not redacted: %q", got)
	}
	if s.redactErr(nil) != nil {
		t.Fatal("nil error must stay nil")
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// A store panic loses that row, not the process or the rows behind it.
func TestTurnRecorderSurvivesAStorePanic(t *testing.T) {
	var calls, stored atomic.Int32
	r := newTurnRecorder(func(context.Context, storage.TurnRecord) error {
		if calls.Add(1) == 1 {
			panic("store")
		}
		stored.Add(1)
		return nil
	}, nil, nil, "sess", slog.New(slog.NewTextHandler(io.Discard, nil)), newTurnLatencies())
	pipe, _ := core.NewPipeline(r)
	session, _ := core.NewSession(1, pipe, core.DefaultResourceBudget())
	_ = session.Start()
	for _, turn := range []string{"t1", "t2"} {
		_ = session.Post(&core.UserMessageFrame{Turn: turn, Content: "hi", Timestamp: time.Now()})
		_ = session.Post(&core.AssistantMessageFrame{Turn: turn, Content: "ok", Timestamp: time.Now()})
	}
	time.Sleep(50 * time.Millisecond)
	_ = session.Close()
	r.close(t.Context())
	if stored.Load() != 1 {
		t.Fatalf("stored %d rows after the panic, want 1", stored.Load())
	}
}
