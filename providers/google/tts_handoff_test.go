package google

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"govoice/core"
)

// A refused background task must not leave a hole in the play order: the lost
// unit gets one ErrorFrame and the next unit is still spoken.
func TestTTS_RefusedHandoffDoesNotWedgeLaterUnits(t *testing.T) {
	release := make(chan struct{})
	cfg := DefaultTTSConfig()
	cfg.Client = &mockTTSClient{synthesizeFn: func(ctx context.Context, text, voice string, rate int) ([]byte, error) {
		if text == "one" {
			<-release
		}
		return make([]byte, 4800), nil
	}}
	proc, _ := NewTTSProcessor(cfg)
	pipe, _ := core.NewPipeline(proc)
	b := core.DefaultResourceBudget()
	b.MaxAsyncTasks = 1
	session, _ := core.NewSession(1, pipe, b)
	var mu sync.Mutex
	audio, errs := map[uint64]int{}, map[uint64]int{}
	session.OnOutput = func(f core.Frame) {
		mu.Lock()
		defer mu.Unlock()
		switch v := f.(type) {
		case *core.OutputAudioFrame:
			audio[v.UnitID]++
		case *core.ErrorFrame:
			errs[v.UnitID]++
		}
	}
	_ = proc.Attach(session, 0)
	_ = session.Start()
	defer session.Close()

	say := func(id uint64, text string) {
		if err := session.Post(&core.SynthesisUnitFrame{Turn: "t", Gen: 1, UnitID: id, Text: text}); err != nil {
			t.Fatal(err)
		}
	}
	say(1, "one")
	say(2, "two") // budget full: refused
	time.Sleep(50 * time.Millisecond)
	close(release)
	time.Sleep(50 * time.Millisecond)
	say(3, "three")
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		ok := audio[3] > 0
		mu.Unlock()
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("unit 3 never spoken: play order wedged")
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if audio[1] == 0 || audio[2] != 0 || errs[2] != 1 || len(errs) != 1 {
		t.Fatalf("audio=%v errors=%v", audio, errs)
	}
}

// The in-task error carries the unit's Turn and Gen like ttsunit's, so the
// failover wrapper and tracing attribute it to the right turn.
func TestTTS_SynthesisErrorFrameCarriesTurnAndGen(t *testing.T) {
	cfg := DefaultTTSConfig()
	cfg.Client = &mockTTSClient{synthesizeFn: func(ctx context.Context, text, voice string, rate int) ([]byte, error) {
		return nil, errors.New("boom")
	}}
	proc, _ := NewTTSProcessor(cfg)
	pipe, _ := core.NewPipeline(proc)
	session, _ := core.NewSession(1, pipe, core.DefaultResourceBudget())
	got := make(chan *core.ErrorFrame, 1)
	session.OnOutput = func(f core.Frame) {
		if v, ok := f.(*core.ErrorFrame); ok {
			select {
			case got <- v:
			default:
			}
		}
	}
	_ = proc.Attach(session, 0)
	_ = session.Start()
	defer session.Close()
	if err := session.Post(&core.SynthesisUnitFrame{Turn: "t9", Gen: 1, UnitID: 7, Text: "x"}); err != nil {
		t.Fatal(err)
	}
	select {
	case ef := <-got:
		if ef.UnitID != 7 || ef.Turn != "t9" || ef.Gen != 1 {
			t.Fatalf("error frame %+v", ef)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no error frame")
	}
}
