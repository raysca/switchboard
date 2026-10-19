package google

import (
	"context"
	"strings"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/resilience"
)

// The attempt timeout bounds one synthesis request (one piece), not the whole
// unit: three pieces of 40 ms each pass under a 100 ms timeout.
func TestTTS_AttemptTimeoutIsPerPiece(t *testing.T) {
	client := &mockTTSClient{synthesizeFn: func(ctx context.Context, _, _ string, _ int) ([]byte, error) {
		select {
		case <-time.After(40 * time.Millisecond):
			return make([]byte, 960), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	cfg := charCfg(client)
	cfg.MaxSynthesisUnitChars = 6
	cfg.RetryPolicy = resilience.Policy{MaxAttempts: 1, AttemptTimeout: 100 * time.Millisecond}
	_, session, log := charStart(t, cfg)
	_ = session.Post(&core.SynthesisUnitFrame{Turn: "t", Gen: 1, UnitID: 1, Text: "aaaaa bbbbb ccccc", IsFinal: true})
	if !log.waitFor(func(fs []core.Frame) bool { return strings.HasSuffix(charMarks(fs, 1), "TP") }) {
		t.Fatalf("marks = %q, want a played unit", charMarks(log.snapshot(), 1))
	}
	if m := charMarks(log.snapshot(), 1); strings.Contains(m, "E") || len(client.calls) != 3 {
		t.Fatalf("marks = %q, calls = %d, want no error and 3 pieces", m, len(client.calls))
	}
}
