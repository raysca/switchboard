package server

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"govoice/core/redact"
	"govoice/server/credential"
)

// M2-02: a failed call setup leaves exactly one warn line naming the cause
// category, session and agent.
func TestSetupFailureLogsOneWarnWithCategory(t *testing.T) {
	rd, err := redact.New(redact.Config{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	s := &Server{redactor: rd, logger: slog.New(slog.NewTextHandler(&buf, nil))}

	s.logSetupFailure(7, "agent-1", "build_pipeline", errors.New("build pipeline: nvidia.stt: stored credential is not valid JSON"))
	s.logSetupFailure(8, "agent-1", "resolve", fmt.Errorf("resolve: %w", credential.ErrDecrypt))

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("log lines = %d:\n%s", len(lines), buf.String())
	}
	for i, want := range []string{"category=build_pipeline", "category=credential_decrypt"} {
		l := lines[i]
		if !strings.Contains(l, "level=WARN") || !strings.Contains(l, "call setup failed") || !strings.Contains(l, want) || !strings.Contains(l, "agent=agent-1") {
			t.Errorf("line %d = %s", i, l)
		}
	}
	if !strings.Contains(lines[0], "nvidia.stt") {
		t.Errorf("cause stage missing: %s", lines[0])
	}
}
