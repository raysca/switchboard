package vad

import (
	"context"
	"sync"
	"testing"
	"time"

	"govoice/core"
)

// queueStream records submitted jobs so the test completes them by hand.
type queueStream struct {
	mu   sync.Mutex
	jobs []*Job
	n    int
}

func (s *queueStream) Submit(j *Job) error {
	s.mu.Lock()
	s.jobs = append(s.jobs, j)
	s.n++
	s.mu.Unlock()
	return nil
}
func (s *queueStream) Close() {}
func (s *queueStream) submitted() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}
func (s *queueStream) take() *Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.jobs) == 0 {
		return nil
	}
	j := s.jobs[0]
	s.jobs = s.jobs[1:]
	return j
}

type queueExec struct{ st *queueStream }

func (e *queueExec) Register(uint64) Stream { return e.st }
func (e *queueExec) Stats() Stats           { return Stats{} }
func (e *queueExec) Capacity() (int, int)   { return 1, 1 }
func (e *queueExec) IsReady() bool          { return true }
func (e *queueExec) Close() error           { return nil }

// loopBlocker holds the run loop on a DTMF frame until release is closed.
type loopBlocker struct{ entered, release chan struct{} }

func (b *loopBlocker) ProcessFrame(_ context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	if _, ok := f.(*core.InputDTMFFrame); ok {
		select {
		case <-b.entered:
		default:
			close(b.entered)
		}
		<-b.release
	}
	return e.PushDirection(f, dir)
}

func speechChunk() *core.InputAudioFrame {
	return &core.InputAudioFrame{Samples: make([]byte, ChunkSamples*2), SampleRate: 16000, Channels: 1, Timestamp: time.Now()}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(3 * time.Second); !cond(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// startBlocked runs a session with the VAD, holds the run loop, and returns
// with one job in flight and the loop blocked.
func startBlocked(t *testing.T) (*core.Session, *queueStream, *loopBlocker) {
	t.Helper()
	st := &queueStream{}
	b := &loopBlocker{entered: make(chan struct{}), release: make(chan struct{})}
	pipe, err := core.NewPipeline(NewProcessor(&queueExec{st}, DefaultProcessorConfig()), b)
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
	t.Cleanup(func() { s.Close() })
	if err := s.Post(speechChunk()); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "first submit", func() bool { return st.submitted() == 1 })
	if err := s.Post(&core.InputDTMFFrame{}); err != nil {
		t.Fatal(err)
	}
	<-b.entered
	return s, st, b
}

// requireResubmits keeps feeding audio (some is shed while the inbox is full)
// until the processor submits another job.
func requireResubmits(t *testing.T, s *core.Session, st *queueStream) {
	t.Helper()
	waitUntil(t, "a job submitted after the result was handled", func() bool {
		_ = s.Post(speechChunk())
		return st.submitted() > 1
	})
}

// A result queued when the generation advances (barge-in, DTMF, ...) must still
// be processed; otherwise busy stays set and VAD never runs again (M1-01).
func TestResultSurvivesGenerationBump(t *testing.T) {
	s, st, b := startBlocked(t)
	j := st.take()
	j.Done(j) // worker finished: result queued behind the blocked loop
	s.IncrementGeneration()
	close(b.release)
	requireResubmits(t, s, st)
}

// A full inbox refuses internal frames; the result must be retried, not lost.
func TestResultRetriedWhenInboxFull(t *testing.T) {
	s, st, b := startBlocked(t)
	for i := 0; i < core.DefaultResourceBudget().MaxQueuedFrames; i++ {
		if err := s.Post(&core.InputDTMFFrame{}); err != nil { // not audio: never shed
			t.Fatal(err)
		}
	}
	j := st.take()
	j.Done(j) // inbox full: first Reenter returns ErrQueueFull
	close(b.release)
	requireResubmits(t, s, st)
}
