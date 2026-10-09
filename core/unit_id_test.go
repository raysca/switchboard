package core_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"govoice/core"
)

type unitRec struct {
	mu  sync.Mutex
	ids []uint64
}

func (r *unitRec) ProcessFrame(_ context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	if su, ok := f.(*core.SynthesisUnitFrame); ok {
		r.mu.Lock()
		r.ids = append(r.ids, su.UnitID)
		r.mu.Unlock()
	}
	return e.PushDirection(f, dir)
}

// Idle speech re-enters with unit id 0 and takes its id at delivery, so a reply
// unit allocated while it waited in the inbox cannot get a lower id (M1-02).
func TestOutOfBandUnitIDIsStampedAtDelivery(t *testing.T) {
	release := make(chan struct{})
	rec := &unitRec{}
	pipe, err := core.NewPipeline(&gateStage{hold: func() { <-release }}, rec)
	if err != nil {
		t.Fatal(err)
	}
	s, err := core.NewSession(1, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_ = s.Post(&gateFrame{})
	if err := s.Reenter(&core.SynthesisUnitFrame{Turn: "idle-say-1", Text: "still there?", IsFinal: true}, core.Downstream, 1, 0); err != nil {
		t.Fatal(err)
	}
	reply := s.NextUnitID() // a reply unit allocated meanwhile
	close(release)
	for end := time.Now().Add(2 * time.Second); ; time.Sleep(time.Millisecond) {
		rec.mu.Lock()
		ids := append([]uint64(nil), rec.ids...)
		rec.mu.Unlock()
		if len(ids) == 1 {
			if ids[0] <= reply {
				t.Fatalf("idle unit id %d, want above the reply's %d", ids[0], reply)
			}
			return
		}
		if time.Now().After(end) {
			t.Fatal("idle unit not delivered")
		}
	}
}
