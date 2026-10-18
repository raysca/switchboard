//go:build xaispeech

package xai

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"govoice/core"
)

type funcTTSClient func(context.Context, string, TTSConfig) ([]byte, error)

func (f funcTTSClient) Synthesize(ctx context.Context, text string, cfg TTSConfig) ([]byte, error) {
	return f(ctx, text, cfg)
}

// A refused background task must not leave a hole in the play order: the lost
// unit gets one ErrorFrame and the next unit is still spoken.
func TestTTS_RefusedHandoffDoesNotWedgeLaterUnits(t *testing.T) {
	cfg := DefaultTTSConfig()
	cfg.Client = funcTTSClient(func(ctx context.Context, text string, _ TTSConfig) ([]byte, error) {
		return make([]byte, 4800), nil
	})
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
	hold := make(chan struct{})
	if err := session.Go(func(context.Context) { <-hold }); err != nil { // takes the only slot
		t.Fatal(err)
	}
	say(1, "one") // budget full: refused
	time.Sleep(50 * time.Millisecond)
	close(hold)
	time.Sleep(50 * time.Millisecond)
	say(2, "two")
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		ok := audio[2] > 0
		mu.Unlock()
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("unit 2 never spoken: play order wedged")
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if audio[1] != 0 || errs[1] != 1 || len(errs) != 1 {
		t.Fatalf("audio=%v errors=%v", audio, errs)
	}
	if n := proc.ErrorsTotal.Load(); n != 0 {
		t.Fatalf("a refused local task counted as %d provider errors", n)
	}
}

// A unit that fails while an earlier one is still synthesizing must still
// release its play slot: unit 3 still plays, after unit 1. (Errors are
// control frames, so e2's position relative to audio is not asserted.)
func TestTTS_FailedUnitReleasesSlotInOrder(t *testing.T) {
	release := make(chan struct{})
	cfg := DefaultTTSConfig()
	cfg.Client = funcTTSClient(func(ctx context.Context, text string, _ TTSConfig) ([]byte, error) {
		switch text {
		case "one":
			<-release
		case "two":
			return nil, &ProviderError{Capability: "tts", Message: "boom", Class: ClassificationNonRetryable, Cause: errors.New("boom")}
		}
		return make([]byte, 960), nil
	})
	proc, _ := NewTTSProcessor(cfg)
	pipe, _ := core.NewPipeline(proc)
	session, _ := core.NewSession(1, pipe, core.DefaultResourceBudget())
	var mu sync.Mutex
	var order []string
	session.OnOutput = func(f core.Frame) {
		mu.Lock()
		defer mu.Unlock()
		switch v := f.(type) {
		case *core.OutputAudioFrame:
			order = append(order, fmt.Sprintf("a%d", v.UnitID))
		case *core.ErrorFrame:
			order = append(order, fmt.Sprintf("e%d", v.UnitID))
		}
	}
	_ = proc.Attach(session, 0)
	_ = session.Start()
	defer session.Close()
	for i, text := range []string{"one", "two", "three"} {
		if err := session.Post(&core.SynthesisUnitFrame{Turn: "t", Gen: 1, UnitID: uint64(i + 1), Text: text}); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(50 * time.Millisecond) // units 2 and 3 are done synthesizing
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		got := strings.Join(order, ",")
		mu.Unlock()
		if got == "a1,e2,a3" || got == "e2,a1,a3" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("order = %q, want a1 then a3 plus one e2", got)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The in-task error carries the unit's Turn and Gen like ttsunit's, so the
// failover wrapper and tracing attribute it to the right turn.
func TestTTS_SynthesisErrorFrameCarriesTurnAndGen(t *testing.T) {
	cfg := DefaultTTSConfig()
	cfg.Client = funcTTSClient(func(ctx context.Context, text string, _ TTSConfig) ([]byte, error) { return nil, errors.New("boom") })
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
