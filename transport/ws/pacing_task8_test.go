package ws_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"govoice/core"
	"govoice/transport/rtvi"
	"govoice/transport/ws"
)

func outputFrame(marker byte, gen uint64, disposition core.OutputDisposition) *core.OutputAudioFrame {
	samples := make([]byte, 960)
	samples[0] = marker
	return &core.OutputAudioFrame{
		Samples: samples, Encoding: core.EncodingPCM16LE, SampleRate: 24000, Channels: 1,
		Gen: gen, Turn: "turn", UnitID: 1, Disposition: disposition,
	}
}

func TestPacedWriterNormalizesAccountingCopiesInputAndRollsBackOverflow(t *testing.T) {
	var written byte
	pacer := ws.NewPacedWriter(ws.PacerConfig{
		MaxQueueDuration: 20 * time.Millisecond,
		MaxQueueBytes:    960,
		WriteTimeout:     time.Second,
	}, func(_ context.Context, data []byte) error {
		decoded, err := ws.DecodeBinaryFrame(data, 1, time.Now())
		if err != nil {
			return err
		}
		written = decoded.(*core.InputAudioFrame).Samples[0]
		return nil
	})

	frame := outputFrame(7, 1, core.OutputInterruptible)
	if err := pacer.Enqueue(frame); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	frame.Samples[0] = 99
	if got := pacer.QueuedDuration(); got != 20*time.Millisecond {
		t.Fatalf("derived duration = %v, want 20ms", got)
	}
	if err := pacer.Enqueue(outputFrame(8, 1, core.OutputInterruptible)); !errors.Is(err, ws.ErrQueueBufferFull) {
		t.Fatalf("overflow = %v, want ErrQueueBufferFull", err)
	}
	if pacer.QueuedBytes() != 960 || pacer.QueuedDuration() != 20*time.Millisecond {
		t.Fatalf("overflow changed accounting: bytes=%d duration=%v", pacer.QueuedBytes(), pacer.QueuedDuration())
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- pacer.Run(ctx) }()
	deadline := time.After(time.Second)
	for pacer.FramesWritten.Load() != 1 {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for write")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	pacer.Close()
	<-done
	if written != 7 {
		t.Fatalf("writer observed caller mutation: got %d, want 7", written)
	}
	if pacer.QueuedBytes() != 0 || pacer.QueuedDuration() != 0 {
		t.Fatalf("drain left accounting: bytes=%d duration=%v", pacer.QueuedBytes(), pacer.QueuedDuration())
	}
}

func TestPacedWriterSpeakingUsesRealWritesAndWholeTurnFinal(t *testing.T) {
	pacer := ws.NewPacedWriter(ws.PacerConfig{
		MaxQueueDuration: time.Second, MaxQueueBytes: 8192, WriteTimeout: time.Second,
	}, func(context.Context, []byte) error { return nil })

	var mu sync.Mutex
	var transitions []core.SpeakingStateFrame
	pacer.OnSpeakingState = func(event core.SpeakingStateFrame) {
		mu.Lock()
		transitions = append(transitions, event)
		mu.Unlock()
	}
	completions := make(chan *core.OutputAudioFrame, 4)
	pacer.OnWriteComplete = func(frame *core.OutputAudioFrame, _ error) { completions <- frame }

	unit1 := outputFrame(1, 1, core.OutputInterruptible)
	unit1.IsFinal = true
	unit2 := outputFrame(2, 1, core.OutputInterruptible)
	unit2.UnitID = 2
	unit2.IsFinal = true
	if err := pacer.MarkTurnFinal(unit2.Turn, unit2.Gen, unit2.UnitID); err != nil {
		t.Fatal(err)
	}
	if err := pacer.Enqueue(unit1); err != nil {
		t.Fatal(err)
	}
	if err := pacer.Enqueue(unit2); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- pacer.Run(ctx) }()
	for i := 0; i < 2; i++ {
		select {
		case <-completions:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for completion")
		}
	}
	cancel()
	pacer.Close()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(transitions) != 2 || transitions[0].State != core.SpeakingStarted || transitions[1].State != core.SpeakingStopped {
		t.Fatalf("multi-unit turn transitions = %#v, want one start and one stop", transitions)
	}
	if transitions[1].Reason != core.SpeakingStopDrained {
		t.Fatalf("stop reason = %q, want drained", transitions[1].Reason)
	}
}

func TestPacedWriterEmptyFinalAndEncoderErrorDoNotStartSpeaking(t *testing.T) {
	pacer := ws.NewPacedWriter(ws.PacerConfig{
		MaxQueueDuration: time.Second, MaxQueueBytes: 8192, WriteTimeout: time.Second,
	}, func(context.Context, []byte) error { return nil })
	var transitions []core.SpeakingStateFrame
	pacer.OnSpeakingState = func(event core.SpeakingStateFrame) { transitions = append(transitions, event) }
	completions := make(chan error, 2)
	pacer.OnWriteComplete = func(_ *core.OutputAudioFrame, err error) { completions <- err }

	empty := outputFrame(0, 1, core.OutputInterruptible)
	empty.Samples = nil
	empty.IsFinal = true
	empty.TurnFinal = true
	bad := outputFrame(1, 1, core.OutputInterruptible)
	bad.Channels = 2
	if err := pacer.Enqueue(empty); err != nil {
		t.Fatal(err)
	}
	if err := pacer.Enqueue(bad); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- pacer.Run(ctx) }()
	if err := <-completions; err != nil {
		t.Fatalf("empty final completion = %v, want nil", err)
	}
	if err := <-completions; err == nil {
		t.Fatal("expected encoder error completion")
	}
	if err := <-errCh; err == nil {
		t.Fatal("expected encoder error from Run")
	}
	if len(transitions) != 0 {
		t.Fatalf("empty/error output started speaking: %#v", transitions)
	}
}

func TestPacedWriterFlushPreservesOldUninterruptibleOrderAndFencesLateCompletion(t *testing.T) {
	firstWrite := make(chan struct{})
	releaseLate := make(chan struct{})
	var mu sync.Mutex
	var markers []byte
	writeCalls := 0
	pacer := ws.NewPacedWriter(ws.PacerConfig{
		MaxQueueDuration: time.Second, MaxQueueBytes: 16384, WriteTimeout: time.Second,
	}, func(_ context.Context, data []byte) error {
		decoded, err := ws.DecodeBinaryFrame(data, 1, time.Now())
		if err != nil {
			return err
		}
		marker := decoded.(*core.InputAudioFrame).Samples[0]
		mu.Lock()
		writeCalls++
		call := writeCalls
		mu.Unlock()
		if call == 1 {
			close(firstWrite)
			<-releaseLate // deliberately ignore cancellation, then report success
		}
		mu.Lock()
		markers = append(markers, marker)
		mu.Unlock()
		return nil
	})
	var completionCount int
	pacer.OnWriteComplete = func(*core.OutputAudioFrame, error) { completionCount++ }

	oldInterruptible := outputFrame(1, 1, core.OutputInterruptible)
	oldCritical := outputFrame(2, 1, core.OutputUninterruptible)
	if err := pacer.Enqueue(oldInterruptible); err != nil {
		t.Fatal(err)
	}
	if err := pacer.Enqueue(oldCritical); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- pacer.Run(ctx) }()
	select {
	case <-firstWrite:
	case <-time.After(time.Second):
		t.Fatal("write did not start")
	}

	flushDone := make(chan error, 1)
	pacer.BeginFlush(1, func(err error) { flushDone <- err })
	newFrame := outputFrame(3, 2, core.OutputInterruptible)
	newFrame.TurnFinal = true
	if err := pacer.Enqueue(newFrame); err != nil {
		t.Fatal(err)
	}
	select {
	case <-flushDone:
		t.Fatal("flush acknowledged before targeted in-flight write stopped")
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseLate)
	select {
	case err := <-flushDone:
		if err != nil {
			t.Fatalf("flush: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("flush did not acknowledge")
	}
	deadline := time.After(time.Second)
	for pacer.FramesWritten.Load() != 2 {
		select {
		case <-deadline:
			t.Fatal("surviving writes did not drain")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	pacer.Close()
	<-done

	mu.Lock()
	got := append([]byte(nil), markers...)
	mu.Unlock()
	if len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 {
		// The physical writer may have accepted marker 1 before cancellation; it must
		// not be counted/completed, while critical marker 2 remains before generation 2.
		t.Fatalf("physical write order = %v, want [1 2 3]", got)
	}
	if completionCount != 2 {
		t.Fatalf("late stale write produced completion: got %d, want 2", completionCount)
	}
}

func TestPacedWriterRepeatedOverlappingFlushesAckOwnGenerationAndRecover(t *testing.T) {
	writeStarted := make(chan struct{})
	pacer := ws.NewPacedWriter(ws.PacerConfig{
		MaxQueueDuration: time.Second, MaxQueueBytes: 8192, WriteTimeout: time.Second, AbortInProgressWrite: true,
	}, func(ctx context.Context, _ []byte) error {
		select {
		case <-writeStarted:
		default:
			close(writeStarted)
		}
		<-ctx.Done()
		return ctx.Err()
	})
	if err := pacer.Enqueue(outputFrame(1, 1, core.OutputInterruptible)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = pacer.Run(ctx) }()
	select {
	case <-writeStarted:
	case <-time.After(time.Second):
		t.Fatal("write did not start")
	}

	type ack struct {
		gen uint64
		err error
	}
	acks := make(chan ack, 3)
	for _, gen := range []uint64{1, 1, 2} {
		g := gen
		pacer.BeginFlush(g, func(err error) { acks <- ack{g, err} })
	}
	seen := map[uint64]int{}
	for i := 0; i < 3; i++ {
		select {
		case a := <-acks:
			if a.err != nil {
				t.Fatalf("flush %d: %v", a.gen, a.err)
			}
			seen[a.gen]++
		case <-time.After(time.Second):
			t.Fatal("flush acknowledgement timed out")
		}
	}
	if seen[1] != 2 || seen[2] != 1 {
		t.Fatalf("flush callbacks acknowledged wrong generations: %v", seen)
	}

	// Recovery is exercised with a fresh writer function because this fixture's
	// writer intentionally blocks every write until cancellation.
	recovered := ws.NewPacedWriter(ws.PacerConfig{MaxQueueDuration: time.Second, MaxQueueBytes: 8192, WriteTimeout: time.Second}, func(context.Context, []byte) error { return nil })
	recovered.SetActiveGeneration(3)
	if err := recovered.Enqueue(outputFrame(3, 3, core.OutputInterruptible)); err != nil {
		t.Fatal(err)
	}
	rctx, rcancel := context.WithCancel(context.Background())
	go func() { _ = recovered.Run(rctx) }()
	deadline := time.After(time.Second)
	for recovered.FramesWritten.Load() != 1 {
		select {
		case <-deadline:
			t.Fatal("new generation did not recover")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	rcancel()
	recovered.Close()
}

func TestPacedWriterCallbacksAllowReentryAndContainPanic(t *testing.T) {
	pacer := ws.NewPacedWriter(ws.PacerConfig{MaxQueueDuration: time.Second, MaxQueueBytes: 8192, WriteTimeout: time.Second}, func(context.Context, []byte) error { return nil })
	callbackReturned := make(chan struct{}, 1)
	pacer.OnSpeakingState = func(event core.SpeakingStateFrame) {
		if event.State == core.SpeakingStarted {
			pacer.BeginFlush(event.Gen, func(error) { callbackReturned <- struct{}{} })
			panic("callback panic must be isolated")
		}
	}
	if err := pacer.Enqueue(outputFrame(1, 1, core.OutputInterruptible)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = pacer.Run(ctx) }()
	select {
	case <-callbackReturned:
	case <-time.After(time.Second):
		t.Fatal("reentrant callback deadlocked")
	}
	pacer.Close()
}

func TestRTVISpeakingTransitionAgreement(t *testing.T) {
	h := rtvi.NewHandler()
	started, err := h.FormatSpeakingState(core.SpeakingStateFrame{State: core.SpeakingStarted})
	if err != nil {
		t.Fatal(err)
	}
	stopped, err := h.FormatSpeakingState(core.SpeakingStateFrame{State: core.SpeakingStopped, Reason: core.SpeakingStopFlushed})
	if err != nil {
		t.Fatal(err)
	}
	if started == stopped || !containsType(started, rtvi.TypeBotStartedSpeaking) || !containsType(stopped, rtvi.TypeBotStoppedSpeaking) {
		t.Fatalf("RTVI speaking mapping disagrees: start=%s stop=%s", started, stopped)
	}
	if _, err := h.FormatSpeakingState(core.SpeakingStateFrame{}); err == nil {
		t.Fatal("invalid speaking transition must be rejected")
	}
}

func TestPacedWriterFinalMarkerSurvivesOverflowUntilAdmission(t *testing.T) {
	pacer := ws.NewPacedWriter(ws.PacerConfig{
		MaxQueueDuration: time.Second, MaxQueueBytes: 960, MaxQueueFrames: 2, WriteTimeout: time.Second,
	}, func(context.Context, []byte) error { return nil })
	if err := pacer.MarkTurnFinal("turn", 1, 9); err != nil {
		t.Fatal(err)
	}
	filler := outputFrame(1, 1, core.OutputUninterruptible)
	if err := pacer.Enqueue(filler); err != nil {
		t.Fatal(err)
	}
	target := outputFrame(2, 1, core.OutputInterruptible)
	target.UnitID = 9
	target.IsFinal = true
	if err := pacer.Enqueue(target); !errors.Is(err, ws.ErrQueueBufferFull) {
		t.Fatalf("overflow = %v, want queue full", err)
	}
	if pacer.PendingFinalMarkers() != 1 {
		t.Fatalf("overflow consumed final marker: pending=%d", pacer.PendingFinalMarkers())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = pacer.Run(ctx) }()
	deadline := time.Now().Add(time.Second)
	for pacer.QueuedBytes() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if err := pacer.Enqueue(target); err != nil {
		t.Fatalf("retry admission: %v", err)
	}
	if pacer.PendingFinalMarkers() != 0 {
		t.Fatalf("successful retry did not consume marker: pending=%d", pacer.PendingFinalMarkers())
	}
	pacer.Close()
}

func TestPacedWriterFinalMarkerReconcilesQueuedSelectedInflightAndCompleted(t *testing.T) {
	tests := []struct {
		name string
		mark func(t *testing.T, pacer *ws.PacedWriter, selected, registered <-chan struct{})
	}{
		{name: "before enqueue", mark: func(t *testing.T, pacer *ws.PacedWriter, _, _ <-chan struct{}) {
			if err := pacer.MarkTurnFinal("turn", 1, 5); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "while queued", mark: func(t *testing.T, pacer *ws.PacedWriter, _, _ <-chan struct{}) {
			if err := pacer.MarkTurnFinal("turn", 1, 5); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "selected prewrite", mark: func(t *testing.T, pacer *ws.PacedWriter, selected, _ <-chan struct{}) {
			select {
			case <-selected:
			case <-time.After(time.Second):
				t.Fatal("not selected")
			}
			if err := pacer.MarkTurnFinal("turn", 1, 5); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "registered write", mark: func(t *testing.T, pacer *ws.PacedWriter, _, registered <-chan struct{}) {
			select {
			case <-registered:
			case <-time.After(time.Second):
				t.Fatal("write not registered")
			}
			if err := pacer.MarkTurnFinal("turn", 1, 5); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			selected := make(chan struct{})
			registered := make(chan struct{})
			releaseSelected := make(chan struct{})
			releaseWrite := make(chan struct{})
			pacer := ws.NewPacedWriter(ws.PacerConfig{MaxQueueDuration: time.Second, MaxQueueBytes: 4096, MaxQueueFrames: 8, WriteTimeout: time.Second}, func(context.Context, []byte) error {
				select {
				case <-registered:
				default:
					close(registered)
				}
				<-releaseWrite
				return nil
			})
			pacer.AfterSelect = func() {
				select {
				case <-selected:
				default:
					close(selected)
				}
				<-releaseSelected
			}
			if tc.name == "before enqueue" {
				tc.mark(t, pacer, selected, registered)
			}
			frame := outputFrame(1, 1, core.OutputInterruptible)
			frame.UnitID = 5
			frame.IsFinal = true
			if err := pacer.Enqueue(frame); err != nil {
				t.Fatal(err)
			}
			if tc.name == "while queued" {
				tc.mark(t, pacer, selected, registered)
			}
			var transitionsMu sync.Mutex
			var transitions []core.SpeakingStateFrame
			pacer.OnSpeakingState = func(event core.SpeakingStateFrame) {
				transitionsMu.Lock()
				transitions = append(transitions, event)
				transitionsMu.Unlock()
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- pacer.Run(ctx) }()
			if tc.name == "selected prewrite" {
				tc.mark(t, pacer, selected, registered)
			}
			close(releaseSelected)
			if tc.name == "registered write" {
				tc.mark(t, pacer, selected, registered)
			}
			close(releaseWrite)
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				transitionsMu.Lock()
				n := len(transitions)
				transitionsMu.Unlock()
				if n == 2 {
					break
				}
				time.Sleep(time.Millisecond)
			}
			pacer.Close()
			<-done
			transitionsMu.Lock()
			defer transitionsMu.Unlock()
			if len(transitions) != 2 || transitions[0].State != core.SpeakingStarted || transitions[1].State != core.SpeakingStopped {
				t.Fatalf("transitions = %#v", transitions)
			}
			if pacer.PendingFinalMarkers() != 0 {
				t.Fatalf("leaked final marker: %d", pacer.PendingFinalMarkers())
			}
		})
	}
}

func TestPacedWriterLateFinalMarkerStopsCompletedUnitOnce(t *testing.T) {
	pacer := ws.NewPacedWriter(ws.PacerConfig{MaxQueueDuration: time.Second, MaxQueueBytes: 4096, MaxQueueFrames: 8, WriteTimeout: time.Second}, func(context.Context, []byte) error { return nil })
	frame := outputFrame(1, 1, core.OutputInterruptible)
	frame.UnitID = 7
	frame.IsFinal = true
	var mu sync.Mutex
	var transitions []core.SpeakingStateFrame
	pacer.OnSpeakingState = func(event core.SpeakingStateFrame) { mu.Lock(); transitions = append(transitions, event); mu.Unlock() }
	if err := pacer.Enqueue(frame); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = pacer.Run(ctx) }()
	deadline := time.Now().Add(time.Second)
	for pacer.FramesWritten.Load() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if err := pacer.MarkTurnFinal("turn", 1, 7); err != nil {
		t.Fatal(err)
	}
	if err := pacer.MarkTurnFinal("turn", 1, 7); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(transitions)
		mu.Unlock()
		if n == 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	pacer.Close()
	mu.Lock()
	defer mu.Unlock()
	if len(transitions) != 2 {
		t.Fatalf("late marker transitions = %#v", transitions)
	}
	if pacer.PendingFinalMarkers() != 0 {
		t.Fatalf("late marker leaked: %d", pacer.PendingFinalMarkers())
	}
}

func TestPacedWriterFlushWinsBeforeWriteAdmission(t *testing.T) {
	admissionReached := make(chan struct{})
	releaseAdmission := make(chan struct{})
	var writes atomic.Int32
	pacer := ws.NewPacedWriter(ws.PacerConfig{MaxQueueDuration: time.Second, MaxQueueBytes: 4096, MaxQueueFrames: 8, WriteTimeout: time.Second}, func(context.Context, []byte) error {
		writes.Add(1)
		return nil
	})
	pacer.BeforeWriteAdmission = func() { close(admissionReached); <-releaseAdmission }
	if err := pacer.Enqueue(outputFrame(1, 1, core.OutputInterruptible)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = pacer.Run(ctx) }()
	<-admissionReached
	flushDone := make(chan error, 1)
	go func() { flushDone <- pacer.Flush(context.Background(), 1) }()
	deadline := time.Now().Add(time.Second)
	for pacer.ActiveGeneration() != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if pacer.ActiveGeneration() != 2 {
		t.Fatal("flush did not commit generation fence")
	}
	close(releaseAdmission)
	select {
	case err := <-flushDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("flush blocked")
	}
	if got := writes.Load(); got != 0 {
		t.Fatalf("flush lost admission race: writes=%d", got)
	}
}

func TestPacedWriterSynchronousCallbacksCanFlush(t *testing.T) {
	t.Run("started callback removes subsequent queued write", func(t *testing.T) {
		var writes atomic.Int32
		pacer := ws.NewPacedWriter(ws.PacerConfig{MaxQueueDuration: time.Second, MaxQueueBytes: 4096, MaxQueueFrames: 8, WriteTimeout: time.Second}, func(context.Context, []byte) error { writes.Add(1); return nil })
		flushErr := make(chan error, 1)
		pacer.OnSpeakingState = func(event core.SpeakingStateFrame) {
			if event.State == core.SpeakingStarted {
				flushErr <- pacer.Flush(context.Background(), event.Gen)
			}
		}
		if err := pacer.Enqueue(outputFrame(1, 1, core.OutputInterruptible)); err != nil {
			t.Fatal(err)
		}
		if err := pacer.Enqueue(outputFrame(2, 1, core.OutputInterruptible)); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() { _ = pacer.Run(ctx) }()
		select {
		case err := <-flushErr:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("started callback flush deadlocked")
		}
		time.Sleep(20 * time.Millisecond)
		if got := writes.Load(); got != 1 {
			t.Fatalf("writes=%d, want 1", got)
		}
		pacer.Close()
	})
	t.Run("turn-final completion callback flushes without timeout", func(t *testing.T) {
		pacer := ws.NewPacedWriter(ws.PacerConfig{MaxQueueDuration: time.Second, MaxQueueBytes: 4096, MaxQueueFrames: 8, WriteTimeout: 50 * time.Millisecond}, func(context.Context, []byte) error { return nil })
		result := make(chan error, 1)
		pacer.OnWriteComplete = func(frame *core.OutputAudioFrame, _ error) { result <- pacer.Flush(context.Background(), frame.Gen) }
		frame := outputFrame(1, 1, core.OutputInterruptible)
		frame.TurnFinal = true
		if err := pacer.Enqueue(frame); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() { _ = pacer.Run(ctx) }()
		select {
		case err := <-result:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("completion callback flush deadlocked")
		}
		pacer.Close()
	})
}

func TestPacedWriterNoopFlushPreservesMediaClock(t *testing.T) {
	var mu sync.Mutex
	var times []time.Time
	pacer := ws.NewPacedWriter(ws.PacerConfig{MaxQueueDuration: time.Second, MaxQueueBytes: 4096, MaxQueueFrames: 8, WriteTimeout: time.Second, EnablePacing: true}, func(context.Context, []byte) error {
		mu.Lock()
		times = append(times, time.Now())
		mu.Unlock()
		return nil
	})
	pacer.OnWriteComplete = func(frame *core.OutputAudioFrame, _ error) {
		if frame.Seq == 1 {
			_ = pacer.Flush(context.Background(), 1)
		}
	}
	first := outputFrame(1, 2, core.OutputUninterruptible)
	first.Seq = 1
	second := outputFrame(2, 2, core.OutputUninterruptible)
	second.Seq = 2
	if err := pacer.Enqueue(first); err != nil {
		t.Fatal(err)
	}
	if err := pacer.Enqueue(second); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = pacer.Run(ctx) }()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(times)
		mu.Unlock()
		if n == 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	pacer.Close()
	mu.Lock()
	defer mu.Unlock()
	if len(times) != 2 {
		t.Fatalf("writes=%d", len(times))
	}
	if spacing := times[1].Sub(times[0]); spacing < 15*time.Millisecond {
		t.Fatalf("no-op flush collapsed pacing: %v", spacing)
	}
}

func TestPacedWriterCancellationClearsQueueAndBalancesSpeaking(t *testing.T) {
	tests := []struct {
		name   string
		pacing bool
	}{
		{name: "idle after speaking start"},
		{name: "selected pacing with queued output", pacing: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pacer := ws.NewPacedWriter(ws.PacerConfig{MaxQueueDuration: time.Second, MaxQueueBytes: 8192, MaxQueueFrames: 8, WriteTimeout: time.Second, EnablePacing: tc.pacing}, func(context.Context, []byte) error { return nil })
			var mu sync.Mutex
			var transitions []core.SpeakingStateFrame
			pacer.OnSpeakingState = func(event core.SpeakingStateFrame) { mu.Lock(); transitions = append(transitions, event); mu.Unlock() }
			first := outputFrame(1, 1, core.OutputInterruptible)
			first.Duration = 200 * time.Millisecond
			if err := pacer.Enqueue(first); err != nil {
				t.Fatal(err)
			}
			secondSelected := make(chan struct{})
			if tc.pacing {
				var selections atomic.Int32
				pacer.AfterSelect = func() {
					if selections.Add(1) == 2 {
						close(secondSelected)
					}
				}
				second := outputFrame(2, 1, core.OutputInterruptible)
				second.Duration = 200 * time.Millisecond
				if err := pacer.Enqueue(second); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- pacer.Run(ctx) }()
			if tc.pacing {
				select {
				case <-secondSelected:
				case <-time.After(time.Second):
					t.Fatal("second frame was not selected for pacing")
				}
			} else {
				deadline := time.Now().Add(time.Second)
				for pacer.FramesWritten.Load() == 0 && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
			}
			cancel()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("Run leaked after cancellation")
			}
			if pacer.QueuedBytes() != 0 || pacer.QueuedDuration() != 0 || pacer.QueuedFrames() != 0 {
				t.Fatalf("cancellation left queue accounting")
			}
			mu.Lock()
			defer mu.Unlock()
			if len(transitions) != 2 || transitions[0].State != core.SpeakingStarted || transitions[1].State != core.SpeakingStopped {
				t.Fatalf("unbalanced transitions: %#v", transitions)
			}
		})
	}
}

func TestPacedWriterDefaultsInvalidBudgetsAndBoundsEmptyFrames(t *testing.T) {
	pacer := ws.NewPacedWriter(ws.PacerConfig{MaxQueueDuration: -1, MaxQueueBytes: -1, MaxQueueFrames: 2, WriteTimeout: -1}, func(context.Context, []byte) error { return nil })
	for i := 0; i < 2; i++ {
		frame := outputFrame(byte(i), 1, core.OutputInterruptible)
		frame.Samples = nil
		if err := pacer.Enqueue(frame); err != nil {
			t.Fatalf("empty enqueue %d: %v", i, err)
		}
	}
	frame := outputFrame(3, 1, core.OutputInterruptible)
	frame.Samples = nil
	if err := pacer.Enqueue(frame); !errors.Is(err, ws.ErrQueueBufferFull) {
		t.Fatalf("empty-frame overflow=%v", err)
	}
	if pacer.QueuedFrames() != 2 || pacer.QueuedBytes() != 0 || pacer.QueuedDuration() != 0 {
		t.Fatalf("frame overflow changed accounting")
	}
	pacer.Close()
}

func TestPacedWriterRealWebSocketWaitsForStartedWriteAndRecovers(t *testing.T) {
	serverConnCh := make(chan *websocket.Conn, 1)
	handlerDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		serverConnCh <- conn
		<-handlerDone
	}))
	defer server.Close()

	client, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close(websocket.StatusNormalClosure, "")
	serverConn := <-serverConnCh
	defer func() { close(handlerDone); serverConn.Close(websocket.StatusNormalClosure, "") }()

	firstAccepted := make(chan struct{})
	releaseReturn := make(chan struct{})
	var writesMu sync.Mutex
	writes := 0
	pacer := ws.NewPacedWriter(ws.PacerConfig{
		MaxQueueDuration: time.Second, MaxQueueBytes: 4096, MaxQueueFrames: 8, WriteTimeout: time.Second,
		// AbortInProgressWrite deliberately remains false for coder/websocket.
	}, func(ctx context.Context, data []byte) error {
		if err := serverConn.Write(ctx, websocket.MessageBinary, data); err != nil {
			return err
		}
		writesMu.Lock()
		writes++
		call := writes
		writesMu.Unlock()
		if call == 1 {
			close(firstAccepted)
			<-releaseReturn
		}
		return nil
	})
	if err := pacer.Enqueue(outputFrame(1, 1, core.OutputInterruptible)); err != nil {
		t.Fatal(err)
	}
	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()
	go func() { _ = pacer.Run(runCtx) }()
	select {
	case <-firstAccepted:
	case <-time.After(time.Second):
		t.Fatal("real websocket write was not accepted")
	}
	flushDone := make(chan error, 1)
	pacer.BeginFlush(1, func(err error) { flushDone <- err })
	select {
	case err := <-flushDone:
		t.Fatalf("flush acknowledged before started websocket write returned: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseReturn)
	select {
	case err := <-flushDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("flush did not acknowledge completed websocket write")
	}

	second := outputFrame(2, 2, core.OutputInterruptible)
	second.TurnFinal = true
	if err := pacer.Enqueue(second); err != nil {
		t.Fatal(err)
	}
	var markers []byte
	for len(markers) < 2 {
		readCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		kind, data, err := client.Read(readCtx)
		cancel()
		if err != nil {
			t.Fatalf("same websocket did not recover: %v", err)
		}
		if kind != websocket.MessageBinary {
			continue
		}
		decoded, err := ws.DecodeBinaryFrame(data, 1, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		markers = append(markers, decoded.(*core.InputAudioFrame).Samples[0])
	}
	if markers[0] != 1 || markers[1] != 2 {
		t.Fatalf("real websocket markers=%v", markers)
	}
	pacer.Close()
}

func containsType(message, typ string) bool {
	return len(message) > 0 && len(typ) > 0 && stringContains(message, `"type":"`+typ+`"`)
}

func stringContains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
