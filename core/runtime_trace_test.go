package core_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"govoice/core"
)

type traceRecorder struct {
	mu          sync.Mutex
	events      []core.TraceEvent
	starts      []core.TraceEvent
	parents     []bool
	ends        int
	endErrors   []error
	endedNames  []string
	parentNames []string
}

func (r *traceRecorder) StartSpan(ctx context.Context, event core.TraceEvent) core.Span {
	r.mu.Lock()
	r.events = append(r.events, event)
	r.starts = append(r.starts, event)
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
	s.recorder.endedNames = append(s.recorder.endedNames, s.name)
	s.recorder.mu.Unlock()
}

func waitTraceTerminal(t *testing.T, r *traceRecorder, terminal string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		r.mu.Lock()
		found := false
		for _, name := range r.endedNames {
			if name == terminal {
				found = true
				break
			}
		}
		r.mu.Unlock()
		if found {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("trace terminal %s not exported", terminal)
		}
		time.Sleep(time.Millisecond)
	}
}

type traceSliceFrame struct{ data []byte }

func (traceSliceFrame) Type() core.FrameType { return core.FrameType(999) }

type traceSliceProducer struct{}

func (traceSliceProducer) ProcessFrame(_ context.Context, f core.Frame, d core.Direction, e core.Emitter) error {
	if _, ok := f.(traceSliceFrame); ok {
		if err := e.PushDirection(traceSliceFrame{data: []byte("fresh")}, d); err != nil {
			return err
		}
	}
	return e.PushDirection(f, d)
}

func TestSessionTraceNonComparableFrameQueuedAndInline(t *testing.T) {
	pipe, _ := core.NewPipeline(traceSliceProducer{}, tracePassProcessor{}, tracePassProcessor{})
	s, _ := core.NewSession(701, pipe, core.DefaultResourceBudget())
	r := &traceRecorder{}
	if err := s.SetTracer(r, core.TracePolicy{}); err != nil {
		t.Fatal(err)
	}
	seen := make(chan struct{}, 2)
	s.OnOutput = func(core.Frame) { seen <- struct{}{} }
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	if err := s.Post(traceSliceFrame{data: []byte("queued")}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-seen:
		case <-time.After(time.Second):
			t.Fatal("noncomparable frame delivery failed")
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	waitTraceTerminal(t, r, "session.end")
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, ev := range r.events {
		if ev.Name == "frame.999" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("semantic events=%d, want one per queued and inline origin", n)
	}
}

type traceProviderChild struct{}

func (traceProviderChild) ProcessFrame(ctx context.Context, f core.Frame, d core.Direction, e core.Emitter) error {
	span := core.StartContextSpan(ctx, "provider.child", "702", "", 1, nil)
	span.End(nil)
	return e.PushDirection(f, d)
}
func TestSessionTraceForwardedFrameRetainsProcessorChain(t *testing.T) {
	pipe, _ := core.NewPipeline(tracePassProcessor{}, tracePassProcessor{}, traceProviderChild{})
	s, _ := core.NewSession(702, pipe, core.DefaultResourceBudget())
	r := &traceRecorder{}
	if err := s.SetTracer(r, core.TracePolicy{}); err != nil {
		t.Fatal(err)
	}
	seen := make(chan struct{}, 1)
	s.OnOutput = func(core.Frame) { seen <- struct{}{} }
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	if err := s.Post(&core.InputAudioFrame{Samples: make([]byte, 2), SampleRate: 16000}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-seen:
	case <-time.After(time.Second):
		t.Fatal("missing output")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	waitTraceTerminal(t, r, "session.end")
	r.mu.Lock()
	defer r.mu.Unlock()
	var parents []string
	semantic := 0
	for i, ev := range r.starts {
		if ev.Name == "processor.frame" || ev.Name == "provider.child" {
			parents = append(parents, r.parentNames[i])
		}
		if ev.Name == "frame.input_audio" {
			semantic++
		}
	}
	want := []string{"frame.input_audio", "processor.frame", "processor.frame", "processor.frame"}
	if !reflect.DeepEqual(parents, want) || semantic != 1 {
		t.Fatalf("parents=%v semantic=%d", parents, semantic)
	}
}

type tracePanicProcessor struct{}

func (tracePanicProcessor) ProcessFrame(context.Context, core.Frame, core.Direction, core.Emitter) error {
	panic("private processor panic")
}

type traceInlinePanicProducer struct{}

func (traceInlinePanicProducer) ProcessFrame(_ context.Context, _ core.Frame, _ core.Direction, e core.Emitter) error {
	return e.Push(&core.OutputAudioFrame{})
}
func TestSessionTracePanicEndsProcessorAndInlineSpansAsFailure(t *testing.T) {
	for _, processors := range [][]core.Processor{{tracePanicProcessor{}}, {traceInlinePanicProducer{}, tracePanicProcessor{}}} {
		pipe, _ := core.NewPipeline(processors...)
		s, _ := core.NewSession(703, pipe, core.DefaultResourceBudget())
		r := &traceRecorder{}
		if err := s.SetTracer(r, core.TracePolicy{}); err != nil {
			t.Fatal(err)
		}
		if err := s.Start(); err != nil {
			t.Fatal(err)
		}
		if err := s.Post(&core.InputAudioFrame{Samples: make([]byte, 2), SampleRate: 16000}); err != nil {
			t.Fatal(err)
		}
		<-s.Done()
		_ = s.Close()
		waitTraceTerminal(t, r, "session.failure")
		r.mu.Lock()
		for i, name := range r.endedNames {
			if name == "processor.frame" || name == "transport.output_audio" {
				if r.endErrors[i] == nil || r.endErrors[i].Error() != "processor_error" {
					r.mu.Unlock()
					t.Fatalf("%s panic end=%v", name, r.endErrors[i])
				}
			}
		}
		r.mu.Unlock()
	}
}

func TestSessionTraceGenericFramesDoNotClaimProviderFirstOrCompletion(t *testing.T) {
	pipe, _ := core.NewPipeline(tracePassProcessor{})
	s, _ := core.NewSession(704, pipe, core.DefaultResourceBudget())
	r := &traceRecorder{}
	if err := s.SetTracer(r, core.TracePolicy{}); err != nil {
		t.Fatal(err)
	}
	seen := make(chan struct{}, 6)
	s.OnOutput = func(core.Frame) { seen <- struct{}{} }
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	frames := []core.Frame{&core.LLMTextDeltaFrame{Delta: "a"}, &core.LLMTextDeltaFrame{Delta: "b"}, &core.TTSStartedFrame{}, &core.TTSAudioFrame{}, &core.TTSAudioFrame{}, &core.TTSStoppedFrame{}}
	for _, f := range frames {
		if err := s.Post(f); err != nil {
			t.Fatal(err)
		}
	}
	for range frames {
		select {
		case <-seen:
		case <-time.After(time.Second):
			t.Fatal("missing frame")
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	waitTraceTerminal(t, r, "session.end")
	r.mu.Lock()
	defer r.mu.Unlock()
	counts := map[string]int{}
	for _, ev := range r.events {
		counts[ev.Name]++
		switch ev.Name {
		case "llm.first_output", "tts.request", "tts.first_audio", "tts.complete":
			t.Fatalf("generic frame claimed provider lifecycle: %s", ev.Name)
		}
	}
	for name, want := range map[string]int{"llm.delta": 2, "tts.started": 1, "tts.audio_chunk": 2, "tts.stopped": 1} {
		if counts[name] != want {
			t.Fatalf("generic %s count=%d, want %d", name, counts[name], want)
		}
	}
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

type gatedParentTracer struct {
	recorder *traceRecorder
	gate     chan struct{}
}

func (g gatedParentTracer) StartSpan(ctx context.Context, event core.TraceEvent) core.Span {
	if event.Name == "parent" {
		<-g.gate
	}
	return g.recorder.StartSpan(ctx, event)
}
func TestAsyncTracerCrossDispatcherParentResolution(t *testing.T) {
	r := &traceRecorder{}
	gate := make(chan struct{})
	underlying := gatedParentTracer{recorder: r, gate: gate}
	a, b := core.NewAsyncTracer(underlying, 8), core.NewAsyncTracer(underlying, 8)
	parent := a.StartSpan(context.Background(), core.NewTraceEvent("parent", time.Now(), "s", "t", 1, core.TracePolicy{}, nil))
	child := b.StartSpan(core.ContextWithSpan(context.Background(), parent), core.NewTraceEvent("child", time.Now(), "s", "t", 1, core.TracePolicy{}, nil))
	time.Sleep(10 * time.Millisecond)
	close(gate)
	child.End(nil)
	parent.End(nil)
	a.Close()
	b.Close()
	deadline := time.Now().Add(time.Second)
	for {
		r.mu.Lock()
		events := append([]core.TraceEvent(nil), r.events...)
		parents := append([]string(nil), r.parentNames...)
		r.mu.Unlock()
		if len(events) >= 2 || time.Now().After(deadline) {
			for i, e := range events {
				if e.Name == "child" {
					if parents[i] != "parent" {
						t.Fatalf("cross dispatcher child parent=%q", parents[i])
					}
					return
				}
			}
			t.Fatal("child span missing")
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
	waitTraceTerminal(t, recorder, "session.end")

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
