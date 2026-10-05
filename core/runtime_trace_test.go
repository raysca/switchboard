package core_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"govoice/core"
)

type traceRecorder struct {
	mu          sync.Mutex
	events      []core.TraceEvent
	parents     []bool
	ends        int
	endErrors   []error
	parentNames []string
}

func (r *traceRecorder) StartSpan(ctx context.Context, event core.TraceEvent) core.Span {
	r.mu.Lock()
	r.events = append(r.events, event)
	r.parents = append(r.parents, core.SpanFromContext(ctx) != nil)
	parentName := ""
	if parent, ok := core.SpanFromContext(ctx).(*traceRecordSpan); ok {
		parentName = parent.name
	}
	r.parentNames = append(r.parentNames, parentName)
	r.mu.Unlock()
	return &traceRecordSpan{recorder: r, name: event.Name}
}

type traceRecordSpan struct {
	recorder *traceRecorder
	name     string
}

func (s *traceRecordSpan) SetAttribute(string, any) {}
func (s *traceRecordSpan) AddEvent(event core.TraceEvent) {
	s.recorder.mu.Lock()
	s.recorder.events = append(s.recorder.events, event)
	s.recorder.mu.Unlock()
}
func (s *traceRecordSpan) End(err error) {
	s.recorder.mu.Lock()
	s.recorder.ends++
	s.recorder.endErrors = append(s.recorder.endErrors, err)
	s.recorder.mu.Unlock()
}

func TestSessionTraceTurnParentAndRawErrorSafety(t *testing.T) {
	const secret = "provider-credential-962"
	pipe, err := core.NewPipeline(traceFailProcessor{err: errors.New(secret)})
	if err != nil {
		t.Fatal(err)
	}
	session, err := core.NewSession(91, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	recorder := &traceRecorder{}
	if err := session.SetTracer(recorder, core.TracePolicy{}); err != nil {
		t.Fatal(err)
	}
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	if err := session.Post(&core.TurnEventFrame{Kind: core.TurnEventStarted, Turn: "turn-safe", Gen: 1}); err != nil {
		t.Fatal(err)
	}
	<-session.Done()
	deadline := time.Now().Add(time.Second)
	for {
		recorder.mu.Lock()
		ends := recorder.ends
		recorder.mu.Unlock()
		if ends >= 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	var hasTurn, hasTurnFrame, hasChild bool
	for i, event := range recorder.events {
		if event.Name == "turn.start" && recorder.parentNames[i] == "session.start" {
			hasTurn = true
		}
		if event.Name == "turn.event" && recorder.parentNames[i] == "turn.start" {
			hasTurnFrame = true
		}
		if event.Name == "processor.frame" && recorder.parentNames[i] == "turn.event" {
			hasChild = true
		}
	}
	if !hasTurn || !hasTurnFrame || !hasChild {
		t.Fatalf("turn hierarchy missing: events=%#v parents=%#v", recorder.events, recorder.parents)
	}
	for _, ended := range recorder.endErrors {
		if ended != nil && strings.Contains(ended.Error(), secret) {
			t.Fatalf("raw error leaked through span end: %v", ended)
		}
	}
}

func TestAsyncTracerKeepsEndedParentForLaterChild(t *testing.T) {
	recorder := &traceRecorder{}
	tracer := core.NewAsyncTracer(recorder, 8)
	parent := tracer.StartSpan(context.Background(), core.NewTraceEvent("parent", time.Now(), "s", "t", 1, core.TracePolicy{}, nil))
	parent.End(nil)
	deadline := time.Now().Add(time.Second)
	for {
		recorder.mu.Lock()
		ended := recorder.ends
		recorder.mu.Unlock()
		if ended == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	child := tracer.StartSpan(core.ContextWithSpan(context.Background(), parent), core.NewTraceEvent("child", time.Now(), "s", "t", 1, core.TracePolicy{}, nil))
	child.End(nil)
	tracer.Close()
	deadline = time.Now().Add(time.Second)
	for {
		recorder.mu.Lock()
		ended := recorder.ends
		parents := append([]string(nil), recorder.parentNames...)
		recorder.mu.Unlock()
		if ended == 2 || time.Now().After(deadline) {
			if len(parents) != 2 || parents[1] != "parent" {
				t.Fatalf("child parent after end = %v", parents)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
}

type traceFailProcessor struct{ err error }

func (p traceFailProcessor) ProcessFrame(context.Context, core.Frame, core.Direction, core.Emitter) error {
	return p.err
}

type tracePassProcessor struct{}

func (tracePassProcessor) ProcessFrame(_ context.Context, frame core.Frame, direction core.Direction, emitter core.Emitter) error {
	return emitter.PushDirection(frame, direction)
}

func TestSessionTraceCorrelatesFramesAndNeverCapturesAudioOrTextByDefault(t *testing.T) {
	pipe, err := core.NewPipeline(tracePassProcessor{})
	if err != nil {
		t.Fatal(err)
	}
	session, err := core.NewSession(74, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	recorder := &traceRecorder{}
	if err := session.SetTracer(recorder, core.TracePolicy{}); err != nil {
		t.Fatal(err)
	}
	processed := make(chan struct{}, 3)
	session.OnOutput = func(core.Frame) { processed <- struct{}{} }
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	const sentinel = "trace-private-1149"
	if err := session.Post(&core.InputAudioFrame{Samples: []byte(sentinel), Encoding: core.EncodingPCM16LE, SampleRate: 16000, Channels: 1, Gen: 1}); err != nil {
		t.Fatal(err)
	}
	if err := session.Post(&core.TranscriptCandidateFrame{Text: sentinel, Turn: "turn-a", Gen: 1, Provider: "mock", Final: true}); err != nil {
		t.Fatal(err)
	}
	if err := session.Post(&core.TurnEventFrame{Kind: core.TurnEventStarted, Turn: "turn-a", Gen: 1, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		select {
		case <-processed:
		case <-time.After(time.Second):
			t.Fatal("session did not process admitted frame")
		}
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		recorder.mu.Lock()
		foundEnd := false
		for _, event := range recorder.events {
			if event.Name == "session.end" {
				foundEnd = true
				break
			}
		}
		recorder.mu.Unlock()
		if foundEnd {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("asynchronous trace worker did not drain terminal events")
		}
		time.Sleep(time.Millisecond)
	}

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	var names []string
	var sessionStarts, terminalEnds int
	var sawProcessorChild bool
	for index, event := range recorder.events {
		names = append(names, event.Name)
		if strings.Contains(event.Name, sentinel) {
			t.Fatalf("trace event name leaked sentinel: %q", event.Name)
		}
		for key, value := range event.Attributes() {
			if strings.Contains(key, "audio") || strings.Contains(key, "transcript") || strings.Contains(key, "text") {
				t.Fatalf("sensitive attribute admitted: %s=%#v", key, value)
			}
			if text, ok := value.(string); ok && strings.Contains(text, sentinel) {
				t.Fatalf("trace attribute leaked sentinel: %s=%q", key, text)
			}
		}
		if event.Name == "session.start" {
			sessionStarts++
		}
		if event.Name == "turn.start" && (event.TurnID != "turn-a" || event.Generation != 1) {
			t.Fatalf("turn correlation lost: %+v", event)
		}
		if event.Name == "session.end" {
			terminalEnds++
		}
		if event.Name == "processor.frame" && recorder.parents[index] {
			sawProcessorChild = true
		}
	}
	if sessionStarts != 1 || terminalEnds != 1 || recorder.ends == 0 {
		t.Fatalf("session trace terminal lifecycle unbalanced: names=%v span ends=%d", names, recorder.ends)
	}
	if !sawProcessorChild {
		t.Fatal("processor spans must retain parent context")
	}
	if !containsTraceName(names, "frame.input_audio") || !containsTraceName(names, "turn.start") || !containsTraceName(names, "stt.final") {
		t.Fatalf("runtime events missing: %v", names)
	}
}

func TestSessionTracerPanicDoesNotFailSession(t *testing.T) {
	pipe, _ := core.NewPipeline(tracePassProcessor{})
	session, _ := core.NewSession(75, pipe, core.DefaultResourceBudget())
	if err := session.SetTracer(panickingTracer{}, core.TracePolicy{}); err != nil {
		t.Fatal(err)
	}
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	if err := session.Post(&core.TurnEventFrame{Kind: core.TurnEventStarted, Turn: "turn-p", Gen: 1}); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("tracer panic must not fail session: %v", err)
	}
}

func TestBlockingTracerCallbackDoesNotHoldSessionAdmissionLock(t *testing.T) {
	pipe, _ := core.NewPipeline(tracePassProcessor{})
	session, _ := core.NewSession(76, pipe, core.DefaultResourceBudget())
	tracer := &blockingTracer{entered: make(chan struct{}), release: make(chan struct{})}
	processed := make(chan struct{}, 2)
	session.OnOutput = func(core.Frame) { processed <- struct{}{} }
	if err := session.SetTracer(tracer, core.TracePolicy{}); err != nil {
		t.Fatal(err)
	}
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	if err := session.Post(&core.TurnEventFrame{Kind: core.TurnEventStarted, Turn: "turn-block", Gen: 1}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-tracer.entered:
	case <-time.After(time.Second):
		t.Fatal("trace callback did not start")
	}
	posted := make(chan error, 1)
	go func() { posted <- session.Post(&core.InputAudioFrame{Samples: []byte{1, 2}, Gen: 1}) }()
	select {
	case err := <-posted:
		if err != nil {
			t.Fatalf("post while tracer blocked: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("tracer callback held the session admission lock")
	}
	select {
	case <-processed:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("blocking tracer delayed the voice session run loop")
	}
	close(tracer.release)
	select {
	case <-processed:
	case <-time.After(time.Second):
		t.Fatal("queued frame did not resume after tracer returned")
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAsyncTracerDeliversAcceptedSpanTerminalWhenQueueIsFull(t *testing.T) {
	underlying := &terminalBlockingTracer{entered: make(chan struct{}), release: make(chan struct{}), ended: make(chan string, 2)}
	tracer := core.NewAsyncTracer(underlying, 1)
	defer tracer.Close()

	first := tracer.StartSpan(context.Background(), core.NewTraceEvent("block", time.Now(), "s", "", 1, core.TracePolicy{}, nil))
	select {
	case <-underlying.entered:
	case <-time.After(time.Second):
		t.Fatal("first trace callback did not block")
	}
	second := tracer.StartSpan(context.Background(), core.NewTraceEvent("accepted", time.Now(), "s", "", 1, core.TracePolicy{}, nil))
	second.End(nil)
	close(underlying.release)

	select {
	case name := <-underlying.ended:
		if name != "accepted" {
			t.Fatalf("ended span = %q, want accepted", name)
		}
	case <-time.After(time.Second):
		t.Fatal("accepted span terminal operation was dropped")
	}
	first.End(nil)
}

type terminalBlockingTracer struct {
	entered chan struct{}
	release chan struct{}
	ended   chan string
}

func (t *terminalBlockingTracer) StartSpan(_ context.Context, event core.TraceEvent) core.Span {
	if event.Name == "block" {
		close(t.entered)
		<-t.release
	}
	return terminalBlockingSpan{name: event.Name, ended: t.ended}
}

type terminalBlockingSpan struct {
	name  string
	ended chan string
}

func (terminalBlockingSpan) SetAttribute(string, any) {}
func (terminalBlockingSpan) AddEvent(core.TraceEvent) {}
func (s terminalBlockingSpan) End(error)              { s.ended <- s.name }

type blockingTracer struct {
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (t *blockingTracer) StartSpan(_ context.Context, event core.TraceEvent) core.Span {
	if event.Name == "turn.start" {
		t.once.Do(func() { close(t.entered) })
		<-t.release
	}
	return &traceRecordSpan{recorder: &traceRecorder{}}
}

type panickingTracer struct{}

func (panickingTracer) StartSpan(context.Context, core.TraceEvent) core.Span {
	panic("provider-secret")
}

func containsTraceName(names []string, expected string) bool {
	for _, name := range names {
		if name == expected {
			return true
		}
	}
	return false
}
