package transcript

import (
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"govoice/core"
)

func testConfig() Config {
	return Config{
		InterimStability:    0.8,
		InterimRepeats:      2,
		GracePeriod:         200 * time.Millisecond,
		FinalizationTimeout: time.Second,
		MaxTextBytes:        64,
		MaxRetiredTurns:     2,
	}
}

func candidate(turn string, gen uint64, text string, at time.Time) Candidate {
	return Candidate{Turn: core.TurnID(turn), Generation: core.Generation(gen), Text: text, At: at}
}

func TestFinalizerStableInterimFiltering(t *testing.T) {
	f, err := NewFinalizer(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Unix(1, 0)

	outs, err := f.Observe(candidate("A", 1, "hel", t0))
	if err != nil || len(outs) != 0 {
		t.Fatalf("unstable first interim: outputs=%v err=%v", outs, err)
	}
	outs, err = f.Observe(candidate("A", 1, "hel", t0.Add(time.Millisecond)))
	if err != nil || len(outs) != 1 || outs[0].Kind != Interim || outs[0].Text != "hel" {
		t.Fatalf("repeated interim was not released: outputs=%v err=%v", outs, err)
	}

	stable := candidate("A", 1, "hello", t0.Add(2*time.Millisecond))
	stable.Stability, stable.HasStability = 0.9, true
	outs, err = f.Observe(stable)
	if err != nil || len(outs) != 1 || outs[0].Kind != Interim || outs[0].Text != "hello" {
		t.Fatalf("stable provider interim was not released: outputs=%v err=%v", outs, err)
	}
	outs, err = f.Observe(stable)
	if err != nil || len(outs) != 0 {
		t.Fatalf("duplicate interim escaped: outputs=%v err=%v", outs, err)
	}
}

func TestFinalizerGraceAcceptsBetterLateFinalAndCommitsOnce(t *testing.T) {
	f, _ := NewFinalizer(testConfig())
	t0 := time.Unix(2, 0)
	first := candidate("A", 1, "book a flight", t0)
	first.Final = true
	if outs, err := f.Observe(first); err != nil || len(outs) != 0 {
		t.Fatalf("first final committed before grace: %v %v", outs, err)
	}
	if outs, err := f.Complete("A", 1, t0.Add(10*time.Millisecond)); err != nil || len(outs) != 0 {
		t.Fatalf("turn completion committed before grace: %v %v", outs, err)
	}
	better := candidate("A", 1, "book a flight to Rome", t0.Add(150*time.Millisecond))
	better.Final = true
	if outs, err := f.Observe(better); err != nil || len(outs) != 0 {
		t.Fatalf("late final committed before grace: %v %v", outs, err)
	}
	outs, err := f.Advance(t0.Add(210 * time.Millisecond))
	if err != nil || len(outs) != 1 || outs[0].Kind != Committed || outs[0].Text != better.Text {
		t.Fatalf("wrong committed late final: outputs=%v err=%v", outs, err)
	}
	if outs, err = f.Observe(candidate("A", 1, "duplicate", t0.Add(220*time.Millisecond))); err != nil || len(outs) != 0 {
		t.Fatalf("post-commit candidate escaped: outputs=%v err=%v", outs, err)
	}
	if outs, err = f.Advance(t0.Add(time.Second)); err != nil || len(outs) != 0 {
		t.Fatalf("turn committed twice: outputs=%v err=%v", outs, err)
	}
}

func TestFinalizerTimeoutCommitsBestInterim(t *testing.T) {
	f, _ := NewFinalizer(testConfig())
	t0 := time.Unix(3, 0)
	c := candidate("A", 1, "timeout fallback", t0)
	if _, err := f.Observe(c); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Complete("A", 1, t0.Add(10*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if outs, err := f.Advance(t0.Add(1009 * time.Millisecond)); err != nil || len(outs) != 0 {
		t.Fatalf("committed before timeout: outputs=%v err=%v", outs, err)
	}
	outs, err := f.Advance(t0.Add(1010 * time.Millisecond))
	if err != nil || len(outs) != 1 || outs[0].Text != "timeout fallback" {
		t.Fatalf("timeout did not commit best candidate: outputs=%v err=%v", outs, err)
	}
}

func TestFinalizerInterruptAndStaleGenerationFence(t *testing.T) {
	f, _ := NewFinalizer(testConfig())
	t0 := time.Unix(4, 0)
	if _, err := f.Observe(candidate("A", 1, "old", t0)); err != nil {
		t.Fatal(err)
	}
	if err := f.Interrupt("A", 1, t0.Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	late := candidate("A", 1, "late", t0.Add(2*time.Millisecond))
	late.Final = true
	if outs, err := f.Observe(late); err != nil || len(outs) != 0 {
		t.Fatalf("interrupted candidate escaped: outputs=%v err=%v", outs, err)
	}
	newer := candidate("B", 2, "current", t0.Add(3*time.Millisecond))
	newer.Final = true
	if _, err := f.Observe(newer); err != nil {
		t.Fatal(err)
	}
	if outs, err := f.Observe(late); err != nil || len(outs) != 0 {
		t.Fatalf("stale generation escaped: outputs=%v err=%v", outs, err)
	}
}

func TestFinalizerSameGenerationDistinctTurnsAndBoundedRetirement(t *testing.T) {
	cfg := testConfig()
	cfg.GracePeriod = 0
	f, _ := NewFinalizer(cfg)
	t0 := time.Unix(5, 0)
	for i, turn := range []string{"A", "B"} {
		c := candidate(turn, 1, "same text", t0.Add(time.Duration(i)*time.Millisecond))
		c.Final = true
		c.EndOfUtterance = true
		outs, err := f.Observe(c)
		if err != nil || len(outs) != 1 || outs[0].Turn != core.TurnID(turn) {
			t.Fatalf("turn %s: outputs=%v err=%v", turn, outs, err)
		}
	}
	c := candidate("C", 1, "third", t0.Add(3*time.Millisecond))
	c.Final = true
	c.EndOfUtterance = true
	if _, err := f.Observe(c); !errors.Is(err, ErrRetiredTurnLimit) {
		t.Fatalf("expected fail-closed retirement limit, got %v", err)
	}
	c.Generation, c.Turn, c.At = 2, "C", t0.Add(4*time.Millisecond)
	if outs, err := f.Observe(c); err != nil || len(outs) != 1 {
		t.Fatalf("generation advance did not reset bounded retirement: %v %v", outs, err)
	}
}

func TestFinalizerRejectsInvalidOrUnboundedTextAndCopiesAcceptedText(t *testing.T) {
	f, _ := NewFinalizer(testConfig())
	t0 := time.Unix(6, 0)
	bad := candidate("A", 1, string([]byte{0xff}), t0)
	if _, err := f.Observe(bad); !errors.Is(err, ErrInvalidText) {
		t.Fatalf("invalid UTF-8 accepted: %v", err)
	}
	tooLong := candidate("A", 1, strings.Repeat("x", 65), t0.Add(time.Millisecond))
	if _, err := f.Observe(tooLong); !errors.Is(err, ErrTextTooLong) {
		t.Fatalf("oversized text accepted: %v", err)
	}
	backing := []byte(" copied text ")
	good := candidate("A", 1, string(backing), t0.Add(2*time.Millisecond))
	good.Final = true
	good.EndOfUtterance = true
	good.Text = strings.Clone(good.Text)
	outs, err := f.Observe(good)
	if err != nil {
		t.Fatal(err)
	}
	if len(outs) != 0 {
		t.Fatalf("unexpected early output: %v", outs)
	}
	backing[1] = 'X'
	outs, err = f.Advance(t0.Add(202 * time.Millisecond))
	if err != nil || len(outs) != 1 || outs[0].Text != "copied text" {
		t.Fatalf("accepted text was not normalized/copied: outputs=%v err=%v", outs, err)
	}
}

func TestFinalizerRejectsTimestampRegression(t *testing.T) {
	f, _ := NewFinalizer(testConfig())
	t0 := time.Unix(7, 0)
	if _, err := f.Observe(candidate("A", 1, "new", t0)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Observe(candidate("A", 1, "old", t0.Add(-time.Nanosecond))); !errors.Is(err, ErrTimestampRegression) {
		t.Fatalf("timestamp regression accepted: %v", err)
	}
}

func TestFinalizerRejectsInvalidStability(t *testing.T) {
	f, _ := NewFinalizer(testConfig())
	t0 := time.Unix(8, 0)
	for _, value := range []float64{math.NaN(), math.Inf(1), -0.1, 1.1} {
		c := candidate("A", 1, "text", t0)
		c.HasStability, c.Stability = true, value
		if _, err := f.Observe(c); !errors.Is(err, ErrInvalidStability) {
			t.Fatalf("invalid stability %v accepted: %v", value, err)
		}
	}
}

func TestFinalizerDuplicateCompletionDoesNotExtendGrace(t *testing.T) {
	f, _ := NewFinalizer(testConfig())
	t0 := time.Unix(9, 0)
	c := candidate("A", 1, "final", t0)
	c.Final = true
	_, _ = f.Observe(c)
	_, _ = f.Complete("A", 1, t0.Add(10*time.Millisecond))
	_, _ = f.Complete("A", 1, t0.Add(100*time.Millisecond))
	outs, err := f.Advance(t0.Add(210 * time.Millisecond))
	if err != nil || len(outs) != 1 {
		t.Fatalf("duplicate completion extended commit deadline: outputs=%v err=%v", outs, err)
	}
}

func TestFinalizerConcurrentFinalReplayCommitsOnce(t *testing.T) {
	cfg := testConfig()
	cfg.GracePeriod = 0
	f, _ := NewFinalizer(cfg)
	t0 := time.Unix(10, 0)
	c := candidate("A", 1, "once", t0)
	c.Final, c.EndOfUtterance = true, true
	var wg sync.WaitGroup
	var mu sync.Mutex
	commits := 0
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outs, err := f.Observe(c)
			if err != nil {
				t.Errorf("observe: %v", err)
				return
			}
			mu.Lock()
			for _, out := range outs {
				if out.Kind == Committed {
					commits++
				}
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if commits != 1 {
		t.Fatalf("expected one concurrent commit, got %d", commits)
	}
}

func TestFinalizerRejectedInputDoesNotAdvancePolicyTime(t *testing.T) {
	f, _ := NewFinalizer(testConfig())
	t0 := time.Unix(11, 0)
	bad := candidate("A", 1, string([]byte{0xff}), t0.Add(20*time.Millisecond))
	if _, err := f.Observe(bad); !errors.Is(err, ErrInvalidText) {
		t.Fatalf("expected invalid text, got %v", err)
	}
	good := candidate("A", 1, "valid", t0.Add(10*time.Millisecond))
	if _, err := f.Observe(good); err != nil {
		t.Fatalf("rejected input poisoned policy time: %v", err)
	}
}
