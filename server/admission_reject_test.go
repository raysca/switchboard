package server

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"govoice/core"
	"govoice/core/redact"
)

// readClose reads until the server closes and returns the close status and every message seen.
func readClose(t *testing.T, conn *websocket.Conn) (websocket.StatusCode, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var all []byte
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return websocket.CloseStatus(err), all
		}
		all = append(all, data...)
	}
}

// Every admission reject reaches the client as an RTVI error with a stable
// reason code and close 1013, never the generic session_setup_failed / 1011.
func TestAdmissionRejectSignalsReason(t *testing.T) {
	cases := []struct {
		name   string
		reason string
		setup  func(s *Server, dial func(bool) (*websocket.Conn, error))
	}{
		{"global", "capacity_exceeded", func(s *Server, _ func(bool) (*websocket.Conn, error)) { s.cfg.Limits.MaxSessions = 0 }},
		{"principal", "principal_limit", func(s *Server, _ func(bool) (*websocket.Conn, error)) { s.cfg.Limits.MaxSessionsPerPrincipal = 0 }},
		{"vad", "capacity_exceeded", func(s *Server, _ func(bool) (*websocket.Conn, error)) {
			s.SetVADExecutor(&mockVADExecutor{ready: false})
		}},
		{"agent", "flow_limit", func(s *Server, dial func(bool) (*websocket.Conn, error)) {
			s.cfg.Limits.MaxSessionsPerFlow = 1
			c, _ := dial(true) // occupies the only slot
			waitFor(t, func() bool { return s.metrics.ActiveSessions.Load() == 1 })
			_ = c
		}},
		{"rate", "rate_limited", func(s *Server, dial func(bool) (*websocket.Conn, error)) {
			s.cfg.Limits.StartRatePerPrincipal, s.cfg.Limits.StartBurstPerPrincipal = 0.001, 1
			c, _ := dial(true)
			waitFor(t, func() bool { return s.metrics.ActiveSessions.Load() == 1 })
			_ = c
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _, dial := drainFixture(t, time.Second)
			tc.setup(s, dial)
			conn, err := dial(true)
			if err != nil {
				t.Fatal(err)
			}
			status, msgs := readClose(t, conn)
			if status != websocket.StatusTryAgainLater {
				t.Fatalf("close status %d, want 1013", status)
			}
			if !bytes.Contains(msgs, []byte(tc.reason)) {
				t.Fatalf("no %q RTVI error in %q", tc.reason, msgs)
			}
			if bytes.Contains(msgs, []byte("session_setup_failed")) {
				t.Fatalf("generic setup error sent: %q", msgs)
			}
		})
	}
}

// A genuine setup failure keeps session_setup_failed / 1011, and its timeline
// and log text pass through the redactor.
func TestSetupFailureStaysGenericAndTimelineRedacted(t *testing.T) {
	s, _, dial := drainFixture(t, time.Second)
	rd, err := redact.New(redact.Config{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	s.redactor = rd
	s.registry.Register("google.stt", func(*Config, StageSpec) (core.Processor, error) {
		return nil, errors.New("provider echoed card 4111 1111 1111 1111")
	})
	conn, err := dial(true)
	if err != nil {
		t.Fatal(err)
	}
	status, msgs := readClose(t, conn)
	if status != websocket.StatusInternalError || !bytes.Contains(msgs, []byte("session_setup_failed")) {
		t.Fatalf("status %d msgs %q, want 1011 session_setup_failed", status, msgs)
	}
	tl := s.timelines.Get(1)
	if tl == nil {
		t.Fatal("no timeline")
	}
	found := false
	for _, e := range tl.Events() {
		if e.Type == EventError {
			found = true
			if strings.Contains(e.Details, "4111") {
				t.Fatalf("timeline leaked: %q", e.Details)
			}
		}
	}
	if !found {
		t.Fatal("no error event recorded")
	}
}

func TestHandshakeRateConfig(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	if cfg.Limits.HandshakeRatePerIP != 20 {
		t.Fatalf("default %v", cfg.Limits.HandshakeRatePerIP)
	}
	cfg.Limits.HandshakeRatePerIP = -1
	if err := ValidateConfig(cfg); err == nil {
		t.Fatal("negative rate accepted")
	}
}
