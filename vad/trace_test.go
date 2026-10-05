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
}
type vadTraceSpan struct{ r *vadTraceRecorder }

func (r *vadTraceRecorder) StartSpan(_ context.Context, e core.TraceEvent) core.Span {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
	return vadTraceSpan{r}
}
func (vadTraceSpan) SetAttribute(string, any) {}
func (s vadTraceSpan) AddEvent(e core.TraceEvent) {
	s.r.mu.Lock()
	s.r.events = append(s.r.events, e)
	s.r.mu.Unlock()
}
func (vadTraceSpan) End(error) {}
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
