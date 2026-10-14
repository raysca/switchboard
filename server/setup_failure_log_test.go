package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"govoice/core/redact"
)

// M2-02: a failed call setup leaves exactly one warn line naming the cause
// category, session and flow.
func TestSetupFailureLogsOneWarnWithCategory(t *testing.T) {
	rd, err := redact.New(redact.Config{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	s := &Server{redactor: rd, logger: slog.New(slog.NewTextHandler(&buf, nil))}

	s.logSetupFailure(7, "flow-1", "build_pipeline", errors.New(`build pipeline: nvidia.stt: parse "https://u:sk-live-SECRET@host": invalid port`))
	s.logSetupFailure(8, "flow-1", "resolve", fmt.Errorf("resolve: %w", context.DeadlineExceeded))

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("log lines = %d:\n%s", len(lines), buf.String())
	}
	for i, want := range []string{"category=build_pipeline", "category=timeout"} {
		l := lines[i]
		if !strings.Contains(l, "level=WARN") || !strings.Contains(l, "call setup failed") || !strings.Contains(l, want) || !strings.Contains(l, "flow=flow-1") {
			t.Errorf("line %d = %s", i, l)
		}
	}
	if !strings.Contains(lines[0], "stage=nvidia.stt") {
		t.Errorf("cause stage missing: %s", lines[0])
	}
	if strings.Contains(buf.String(), "SECRET") || strings.Contains(buf.String(), "https://") {
		t.Errorf("error text leaked into the log: %s", buf.String())
	}
}
