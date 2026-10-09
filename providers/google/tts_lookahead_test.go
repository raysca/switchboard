package google

import (
	"context"
	"sync"
	"testing"
	"time"

	"govoice/core"
)

// M3-01: unit N+1 is synthesized while unit N is still queued, up to a bound,
// and audio is still emitted in unit order.
func TestTTS_SynthesizesAheadInOrderWithBound(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	started := map[string]bool{}
	cfg := DefaultTTSConfig()
	cfg.Client = &mockTTSClient{synthesizeFn: func(ctx context.Context, text, voice string, rate int) ([]byte, error) {
		mu.Lock()
		started[text] = true
		mu.Unlock()
		if text == "u1" {
			<-release
		}
		return make([]byte, 960), nil
	}}
	proc, _ := NewTTSProcessor(cfg)
	pipe, _ := core.NewPipeline(proc)
	session, _ := core.NewSession(1, pipe, core.DefaultResourceBudget())
	var order []uint64
	session.OnOutput = func(f core.Frame) {
		if a, ok := f.(*core.OutputAudioFrame); ok {
			mu.Lock()
			order = append(order, a.UnitID)
			mu.Unlock()
		}
	}
	_ = proc.Attach(session, 0)
	_ = session.Start()
	defer session.Close()
	for i, txt := range []string{"u1", "u2", "u3", "u4"} {
		_ = session.Post(&core.SynthesisUnitFrame{Turn: "t", Gen: 1, UnitID: uint64(i + 1), Text: txt})
	}
	time.Sleep(150 * time.Millisecond)
	mu.Lock()
	if !started["u2"] || !started["u3"] || started["u4"] || len(order) != 0 {
		t.Fatalf("while u1 is pending: started=%v emitted=%v; want u2,u3 ahead, u4 held, nothing emitted", started, order)
	}
	mu.Unlock()
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(order)
		mu.Unlock()
		if n == 4 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("emitted %v", order)
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	for i, id := range order {
		if id != uint64(i+1) {
			t.Fatalf("emission order = %v", order)
		}
	}
}
