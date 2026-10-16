package transcript

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/coretest"
)

type manualPolicyClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *manualPolicyClock) Now() time.Time                     { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *manualPolicyClock) NewTimer(time.Duration) PolicyTimer { panic("timer not used") }
func (c *manualPolicyClock) Set(at time.Time)                   { c.mu.Lock(); c.now = at; c.mu.Unlock() }

type recordingEmitter struct{ frames []core.Frame }

func (e *recordingEmitter) Push(f core.Frame) error {
	e.frames = append(e.frames, f)
	return nil
}

func (e *recordingEmitter) PushDirection(f core.Frame, _ core.Direction) error {
	e.frames = append(e.frames, f)
	return nil
}

func TestProcessorSeparatesRawCandidatesFromCommittedFinals(t *testing.T) {
	cfg := testConfig()
	cfg.GracePeriod = 0
	p, err := NewProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	emitter := &recordingEmitter{}
	at := time.Unix(10, 0)
	err = p.ProcessFrame(context.Background(), &core.TranscriptCandidateFrame{
		Text:           " committed once ",
		Turn:           "A",
		Gen:            1,
		Timestamp:      at,
		Final:          true,
		EndOfUtterance: true,
		Provider:       "test",
	}, core.Downstream, emitter)
	if err != nil {
		t.Fatal(err)
	}
	if len(emitter.frames) != 1 {
		t.Fatalf("expected one output, got %d: %T", len(emitter.frames), emitter.frames)
	}
	final, ok := emitter.frames[0].(*core.FinalTranscriptionFrame)
	if !ok || !final.Committed || final.Text != "committed once" || final.Turn != "A" || final.Timestamp != at {
		t.Fatalf("raw candidate did not become one marked commit: %#v", emitter.frames[0])
	}

	// A committed output is ordinary downstream data. The processor must never
	// reinterpret it as a raw candidate and recursively commit it.
	if err := p.ProcessFrame(context.Background(), final, core.Downstream, emitter); err != nil {
		t.Fatal(err)
	}
	if len(emitter.frames) != 2 || emitter.frames[1] != final {
		t.Fatalf("committed output was not forwarded exactly once: %#v", emitter.frames)
	}
}

func TestProcessorConsumesTurnDecisionAndExplicitAdvance(t *testing.T) {
	cfg := testConfig()
	clock := &manualPolicyClock{now: time.Unix(100, 0)}
	cfg.PolicyClock = clock
	p, _ := NewProcessor(cfg)
	emitter := &recordingEmitter{}
	t0 := time.Unix(11, 0)
	if err := p.ProcessFrame(context.Background(), &core.TranscriptCandidateFrame{
		Text: "fallback", Turn: "A", Gen: 1, Timestamp: t0,
	}, core.Downstream, emitter); err != nil {
		t.Fatal(err)
	}
	if err := p.ProcessFrame(context.Background(), &core.TurnEventFrame{
		Kind: core.TurnEventCompleted, Turn: "A", Gen: 1, At: t0.Add(time.Millisecond),
	}, core.Downstream, emitter); err != nil {
		t.Fatal(err)
	}
	clock.Set(time.Unix(102, 0))
	if err := p.ProcessFrame(context.Background(), &core.TranscriptAdvanceFrame{
		At: t0.Add(time.Second + time.Millisecond),
	}, core.Downstream, emitter); err != nil {
		t.Fatal(err)
	}
	var commits int
	for _, frame := range emitter.frames {
		if final, ok := frame.(*core.FinalTranscriptionFrame); ok && final.Committed {
			commits++
		}
	}
	if commits != 1 {
		t.Fatalf("expected one timeout commit, got %d in %#v", commits, emitter.frames)
	}
}

func TestProcessorPolicyOrderingIgnoresDelayedSourceTimestamp(t *testing.T) {
	cfg := testConfig()
	cfg.GracePeriod = 0
	clock := &manualPolicyClock{now: time.Unix(110, 0)}
	cfg.PolicyClock = clock
	p, _ := NewProcessor(cfg)
	emitter := &recordingEmitter{}
	sourceCandidate := time.Unix(110, 0)
	if err := p.ProcessFrame(context.Background(), &core.TranscriptCandidateFrame{
		Text: "on time", Turn: "A", Gen: 1, Timestamp: sourceCandidate, Final: true,
	}, core.Downstream, emitter); err != nil {
		t.Fatal(err)
	}
	clock.Set(time.Unix(111, 0))
	if err := p.ProcessFrame(context.Background(), &core.TurnEventFrame{
		Kind: core.TurnEventCompleted, Turn: "A", Gen: 1, At: time.Unix(100, 0),
	}, core.Downstream, emitter); err != nil {
		t.Fatalf("delayed source event regressed policy time: %v", err)
	}
	final, ok := emitter.frames[len(emitter.frames)-1].(*core.FinalTranscriptionFrame)
	if !ok || final.Timestamp != sourceCandidate || final.Text != "on time" {
		t.Fatalf("source timestamp was not retained separately: %#v", emitter.frames)
	}
}

func TestProcessorDeadlineDriverCommitsWithoutExplicitAdvance(t *testing.T) {
	cfg := testConfig()
	cfg.GracePeriod = 5 * time.Millisecond
	p, _ := NewProcessor(cfg)
	sink := &coretest.PassthroughProcessor{}
	pipe, _ := core.NewPipeline(p, sink)
	session, _ := core.NewSession(991, pipe, core.DefaultResourceBudget())
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	source := time.Unix(50, 0)
	_ = session.Post(&core.TranscriptCandidateFrame{Text: "driven", Turn: "A", Gen: 1, Timestamp: source, Final: true})
	_ = session.Post(&core.TurnEventFrame{Kind: core.TurnEventCompleted, Turn: "A", Gen: 1, At: time.Unix(40, 0)})
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		for _, frame := range sink.GetFrames() {
			if final, ok := frame.(*core.FinalTranscriptionFrame); ok && final.Committed {
				if final.Text != "driven" || final.Timestamp != source {
					t.Fatalf("wrong commit: %#v", final)
				}
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("deadline driver did not commit")
}

func TestProcessorDropsInterruptedTurn(t *testing.T) {
	p, _ := NewProcessor(testConfig())
	emitter := &recordingEmitter{}
	t0 := time.Unix(12, 0)
	_ = p.ProcessFrame(context.Background(), &core.TranscriptCandidateFrame{
		Text: "discard me", Turn: "A", Gen: 1, Timestamp: t0,
	}, core.Downstream, emitter)
	if err := p.ProcessFrame(context.Background(), &core.TurnEventFrame{
		Kind: core.TurnEventInterrupted, Turn: "A", Gen: 1, At: t0.Add(time.Millisecond),
	}, core.Downstream, emitter); err != nil {
		t.Fatal(err)
	}
	if err := p.ProcessFrame(context.Background(), &core.TranscriptAdvanceFrame{
		At: t0.Add(2 * time.Second),
	}, core.Downstream, emitter); err != nil {
		t.Fatal(err)
	}
	for _, frame := range emitter.frames {
		if final, ok := frame.(*core.FinalTranscriptionFrame); ok && final.Committed {
			t.Fatalf("interrupted turn committed: %#v", final)
		}
	}
}

func TestProcessor_PendingTurnLimit_ResetsAndContinues(t *testing.T) {
	cfg := testConfig()
	cfg.MaxRetiredTurns = 128
	cfg.GracePeriod = 0
	cfg.PolicyClock = &manualPolicyClock{now: time.Unix(100, 0)}
	p, err := NewProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	emitter := &recordingEmitter{}
	ctx := context.Background()
	at := time.Unix(10, 0)
	for i := 0; i < 129; i++ {
		err := p.ProcessFrame(ctx, &core.TranscriptCandidateFrame{
			Text: "never completes", Turn: core.TurnID(fmt.Sprintf("orphan-%d", i)), Gen: 1, Timestamp: at,
		}, core.Downstream, emitter)
		if err != nil {
			t.Fatalf("orphan %d: ProcessFrame returned %v", i, err)
		}
	}
	err = p.ProcessFrame(ctx, &core.TranscriptCandidateFrame{
		Text: "hello", Turn: "normal", Gen: 1, Timestamp: at, Final: true, EndOfUtterance: true,
	}, core.Downstream, emitter)
	if err != nil {
		t.Fatalf("normal turn: %v", err)
	}
	var finals []*core.FinalTranscriptionFrame
	for _, f := range emitter.frames {
		if ft, ok := f.(*core.FinalTranscriptionFrame); ok {
			finals = append(finals, ft)
		}
	}
	if len(finals) != 1 || finals[0].Turn != "normal" || finals[0].Text != "hello" {
		t.Fatalf("expected only the normal turn final, got %#v", finals)
	}
}
