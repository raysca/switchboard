package google

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
)

// Queued units must not each hold a session task slot (budget 16): every
// sentence of a long reply is spoken, none dropped with ErrTooManyTasks.
func TestTTS_ManyQueuedUnitsAreAllSpoken(t *testing.T) {
	release := make(chan struct{})
	cfg := DefaultTTSConfig()
	cfg.Client = &mockTTSClient{synthesizeFn: func(ctx context.Context, text, voice string, rate int) ([]byte, error) {
		<-release
		return make([]byte, 4800), nil
	}}
	proc, _ := NewTTSProcessor(cfg)
	pipe, _ := core.NewPipeline(proc)
	session, _ := core.NewSession(1, pipe, core.DefaultResourceBudget())
	var finals, errs atomic.Int32
	session.OnOutput = func(f core.Frame) {
		switch v := f.(type) {
		case *core.OutputAudioFrame:
			if v.IsFinal {
				finals.Add(1)
			}
		case *core.ErrorFrame:
			errs.Add(1)
		}
	}
	_ = proc.Attach(session, 0)
	_ = session.Start()
	defer session.Close()

	const n = 40
	for i := 1; i <= n; i++ {
		if err := session.Post(&core.SynthesisUnitFrame{Turn: "t", Gen: 1, UnitID: uint64(i), Text: "A sentence.", IsFinal: i == n}); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	for end := time.Now().Add(3 * time.Second); finals.Load() < n && time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
	}
	if finals.Load() != n || errs.Load() != 0 {
		t.Fatalf("finished units = %d of %d, errors = %d", finals.Load(), n, errs.Load())
	}
}
