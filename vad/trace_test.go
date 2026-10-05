package vad

import (
	"context"
	"sync"
	"testing"
	"time"

	"govoice/core"
)

type traceExecutor struct{}
type traceStream struct{}

func (traceExecutor) Register(uint64) Stream { return traceStream{} }
func (traceExecutor) Stats() Stats           { return Stats{} }
func (traceExecutor) Capacity() (int, int)   { return 1, 1 }
func (traceExecutor) IsReady() bool          { return true }
func (traceExecutor) Close() error           { return nil }
func (traceStream) Submit(j *Job) error {
	j.Prob = 0.9
	j.Finished = time.Now()
	go j.Done(j)
	return nil
}
func (traceStream) Close() {}

type vadTraceRecorder struct {
	mu     sync.Mutex
	events []core.TraceEvent
	ends   map[string][]error
}
type vadTraceSpan struct {
	r    *vadTraceRecorder
	name string
}

func (r *vadTraceRecorder) StartSpan(_ context.Context, e core.TraceEvent) core.Span {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
	return vadTraceSpan{r: r, name: e.Name}
}
func (vadTraceSpan) SetAttribute(string, any) {}
func (s vadTraceSpan) AddEvent(e core.TraceEvent) {
	s.r.mu.Lock()
	s.r.events = append(s.r.events, e)
	s.r.mu.Unlock()
}
func (s vadTraceSpan) End(err error) {
	s.r.mu.Lock()
	if s.r.ends == nil {
		s.r.ends = make(map[string][]error)
	}
	s.r.ends[s.name] = append(s.r.ends[s.name], err)
	s.r.mu.Unlock()
}
func (r *vadTraceRecorder) has(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		if e.Name == name {
			return true
		}
	}
	return false
}

type pendingTraceStream struct {
	mu        sync.Mutex
	job       *Job
	submitted chan struct{}
}

func (s *pendingTraceStream) Submit(j *Job) error {
	s.mu.Lock()
	s.job = j
	s.mu.Unlock()
	close(s.submitted)
	return nil
}
func (*pendingTraceStream) Close() {}

type pendingTraceExecutor struct{ stream *pendingTraceStream }

func (e pendingTraceExecutor) Register(uint64) Stream { return e.stream }
func (pendingTraceExecutor) Stats() Stats             { return Stats{} }
func (pendingTraceExecutor) Capacity() (int, int)     { return 1, 1 }
func (pendingTraceExecutor) IsReady() bool            { return true }
func (pendingTraceExecutor) Close() error             { return nil }

func TestVADCloseEndsPendingInferenceOnceBeforeLateCallback(t *testing.T) {
	stream := &pendingTraceStream{submitted: make(chan struct{})}
	p := NewProcessor(pendingTraceExecutor{stream}, DefaultProcessorConfig())
	pipe, _ := core.NewPipeline(p)
	s, _ := core.NewSession(502, pipe, core.DefaultResourceBudget())
	r := &vadTraceRecorder{}
	if err := s.SetTracer(r, core.TracePolicy{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	if err := s.Post(&core.InputAudioFrame{Samples: make([]byte, ChunkSamples*2), SampleRate: 16000, Channels: 1, Gen: 1, Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stream.submitted:
	case <-time.After(time.Second):
		t.Fatal("job not submitted")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Second)
	for {
		r.mu.Lock()
		ends := append([]error(nil), r.ends["vad.inference"]...)
		r.mu.Unlock()
		if len(ends) == 1 {
			if ends[0] == nil || ends[0].Error() != "canceled" {
				t.Fatalf("pending trace end=%v", ends[0])
			}
			break
		}
		if time.Now().After(until) {
			t.Fatalf("pending trace ends=%v", ends)
		}
		time.Sleep(time.Millisecond)
	}
	stream.mu.Lock()
	job := stream.job
	stream.mu.Unlock()
	job.Done(job)
	time.Sleep(10 * time.Millisecond)
	r.mu.Lock()
	count := len(r.ends["vad.inference"])
	r.mu.Unlock()
	if count != 1 {
		t.Fatalf("late callback ended span %d times", count)
	}
}

func TestVADTracesJobLatencyAndSpeechDecision(t *testing.T) {
	cfg := DefaultProcessorConfig()
	cfg.StartChunks = 1
	p := NewProcessor(traceExecutor{}, cfg)
	pipe, _ := core.NewPipeline(p)
	s, _ := core.NewSession(501, pipe, core.DefaultResourceBudget())
	r := &vadTraceRecorder{}
	if err := s.SetTracer(r, core.TracePolicy{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Post(&core.InputAudioFrame{Samples: make([]byte, ChunkSamples*2), SampleRate: 16000, Channels: 1, Gen: 1, Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(2 * time.Second)
	for !(r.has("vad.inference") && r.has("vad.result") && r.has("vad.speech_start")) {
		if time.Now().After(until) {
			t.Fatal("missing VAD trace lifecycle")
		}
		time.Sleep(time.Millisecond)
	}
}
