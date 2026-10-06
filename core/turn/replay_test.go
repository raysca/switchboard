package turn

import (
	"context"
	"testing"
	"time"

	"govoice/core"
)

func TestTurnRecorderAndReplayer(t *testing.T) {
	recorder := NewTurnRecorder("turn-101", 1)

	// Record several events
	f1 := &core.UserStartedSpeakingFrame{Gen: 1}
	recorder.Record(f1, core.Downstream)

	time.Sleep(10 * time.Millisecond)
	f2 := &core.UserStoppedSpeakingFrame{Gen: 1}
	recorder.Record(f2, core.Downstream)

	rec := recorder.Stop()
	if len(rec.Entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(rec.Entries))
	}
	if rec.Entries[0].TurnID != "turn-101" || rec.Entries[0].Gen != 1 {
		t.Errorf("unexpected entry metadata: %+v", rec.Entries[0])
	}

	// JSON Round-trip
	data, err := rec.SerializeJSON()
	if err != nil {
		t.Fatalf("SerializeJSON failed: %v", err)
	}

	deserialized, err := DeserializeJSON(data)
	if err != nil {
		t.Fatalf("DeserializeJSON failed: %v", err)
	}
	if deserialized.TurnID != "turn-101" || len(deserialized.Entries) != 2 {
		t.Errorf("deserialization mismatch: %+v", deserialized)
	}

	// Replay accelerated (speed=0 for instant execution)
	replayer, err := NewTurnReplayer(deserialized, 0)
	if err != nil {
		t.Fatalf("NewTurnReplayer failed: %v", err)
	}

	replayedCount := 0
	ctx := context.Background()
	err = replayer.Replay(ctx, func(entry RecordedTurnEntry) error {
		replayedCount++
		return nil
	})
	if err != nil {
		t.Fatalf("Replay failed: %v", err)
	}
	if replayedCount != 2 {
		t.Errorf("expected 2 replayed entries, got %d", replayedCount)
	}
}
