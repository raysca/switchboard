package whisker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"govoice/core"
	"govoice/core/coretest"
	coretools "govoice/core/tools"
	"govoice/transport/ws"
)

// ScenarioResult is based on frames observed inside a running session, then
// adapted at the observer boundary. It does not claim a Whisker wire format.
type ScenarioResult struct {
	Name   string
	Events []Event
	Err    error
}

type frameProbe struct {
	mu       sync.Mutex
	sequence uint64
	turn     core.TurnID
	events   []Event
}

func (p *frameProbe) toolStarted(call core.ToolCall) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sequence++
	e := core.NewDebugEvent("tool.started", time.Unix(1, int64(p.sequence)), "compat-session", call.TurnID, call.Generation, nil)
	e.PipelineID, e.Processor, e.ProcessorIndex = "compat-pipeline", "frame-probe", 0
	e.Direction, e.FrameKind, e.Sequence, e.ToolCallID = core.Downstream, core.FrameToolCall, p.sequence, call.ID
	if out, err := (Adapter{}).Adapt(e); err == nil {
		p.events = append(p.events, out)
	}
}

func (p *frameProbe) runtimeCompleted(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sequence++
	e := core.NewDebugEvent(name, time.Unix(1, int64(p.sequence)), "compat-session", p.turn, 0, nil)
	e.PipelineID, e.Processor, e.ProcessorIndex = "compat-pipeline", "frame-probe", 0
	e.Direction, e.FrameKind, e.Sequence = core.Downstream, core.FrameEnd, p.sequence
	if out, err := (Adapter{}).Adapt(e); err == nil {
		p.events = append(p.events, out)
	}
}

func (p *frameProbe) ProcessFrame(_ context.Context, frame core.Frame, direction core.Direction, emitter core.Emitter) error {
	p.mu.Lock()
	p.sequence++
	if frame.Type() == core.FrameUserStartedSpeaking && p.turn == "" {
		p.turn = "turn-1"
	}
	name, attrs, turn, gen, call := observedFrameDetails(frame)
	if turn == "" {
		turn = p.turn
	}
	e := core.NewDebugEvent(name, time.Unix(1, int64(p.sequence)), "compat-session", turn, gen, attrs)
	e.PipelineID, e.Processor, e.ProcessorIndex = "compat-pipeline", "frame-probe", 0
	e.Direction, e.FrameKind, e.Sequence, e.ToolCallID = direction, frame.Type(), p.sequence, call
	out, err := (Adapter{}).Adapt(e)
	if err == nil {
		p.events = append(p.events, out)
	}
	p.mu.Unlock()
	if err != nil {
		return err
	}
	return emitter.PushDirection(frame, direction)
}

func (p *frameProbe) snapshot() []Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Event(nil), p.events...)
}

func observedFrameDetails(f core.Frame) (string, map[string]any, core.TurnID, core.Generation, string) {
	switch x := f.(type) {
	case *core.StartFrame:
		return "session.started", nil, "", 0, ""
	case *core.EndFrame:
		return "session.ended", nil, "", 0, ""
	case *core.CancelFrame:
		return "session.cancelled", nil, "", core.Generation(x.Gen), ""
	case *core.InputAudioFrame:
		return "audio.input", map[string]any{"count": len(x.Samples), "sample_rate": x.SampleRate, "channels": x.Channels, "format": string(x.Encoding)}, "", core.Generation(x.Gen), ""
	case *core.OutputAudioFrame:
		return "audio.output", map[string]any{"count": len(x.Samples), "sample_rate": x.SampleRate, "channels": x.Channels, "format": string(x.Encoding)}, core.TurnID(x.Turn), core.Generation(x.Gen), ""
	case *core.UserStartedSpeakingFrame:
		return "user.speech.started", nil, "", core.Generation(x.Gen), ""
	case *core.UserStoppedSpeakingFrame:
		return "user.speech.stopped", nil, "", core.Generation(x.Gen), ""
	case *core.InterimTranscriptionFrame:
		return "stt.interim", map[string]any{"status": "interim"}, core.TurnID(x.Turn), core.Generation(x.Gen), ""
	case *core.FinalTranscriptionFrame:
		return "stt.final", map[string]any{"status": "final"}, core.TurnID(x.Turn), core.Generation(x.Gen), ""
	case *core.UserMessageFrame:
		return "llm.requested", nil, core.TurnID(x.Turn), core.Generation(x.Gen), ""
	case *core.LLMTextDeltaFrame:
		return "llm.output", nil, core.TurnID(x.Turn), core.Generation(x.Gen), ""
	case *core.LLMTextFrame:
		return "llm.completed", nil, core.TurnID(x.Turn), core.Generation(x.Gen), ""
	case *core.TTSStartedFrame:
		return "tts.started", nil, core.TurnID(x.Turn), core.Generation(x.Gen), ""
	case *core.TTSAudioFrame:
		return "tts.audio", map[string]any{"count": len(x.Audio), "sample_rate": x.SampleRate, "channels": x.Channels}, core.TurnID(x.Turn), core.Generation(x.Gen), ""
	case *core.TTSStoppedFrame:
		return "tts.completed", nil, core.TurnID(x.Turn), core.Generation(x.Gen), ""
	case *core.SpeakingStateFrame:
		if x.State == core.SpeakingStarted {
			return "assistant.speaking.started", nil, core.TurnID(x.Turn), core.Generation(x.Gen), ""
		}
		return "assistant.speaking.stopped", nil, core.TurnID(x.Turn), core.Generation(x.Gen), ""
	case *core.InterruptionFrame:
		return "turn.interrupted", nil, core.TurnID(x.Turn), core.Generation(x.Gen), ""
	case *core.FlushFrame:
		return "output.flush.started", nil, "", core.Generation(x.Gen), ""
	case *core.FlushProcessedFrame:
		return "output.flush.completed", nil, "", core.Generation(x.Gen), ""
	case *core.ToolCallFrame:
		return "tool.requested", nil, x.Call.TurnID, x.Call.Generation, x.Call.ID
	case *core.ToolResultFrame:
		if x.Result.Status != core.ToolSucceeded {
			return "tool.failed", map[string]any{"status": string(x.Result.Status)}, x.Turn, x.Gen, x.Result.CallID
		}
		return "tool.completed", map[string]any{"status": string(x.Result.Status)}, x.Turn, x.Gen, x.Result.CallID
	case *core.ErrorFrame:
		if x.Fatal {
			return "session.error", map[string]any{"error.category": "internal_error"}, x.Turn, x.Gen, ""
		}
		return "provider.error", map[string]any{"error.category": "provider_error"}, x.Turn, x.Gen, ""
	default:
		return "frame.observed", nil, "", 0, ""
	}
}

type scenarioSpec struct {
	name     string
	frames   []core.Frame
	expected []string
}

// RunScenarios executes the eleven semantic observer cases independently.
func RunScenarios() []ScenarioResult {
	audio := []byte{1, 2}
	specs := []scenarioSpec{
		{"session_pipeline_start", nil, []string{"session.started", "session.ended"}},
		{"audio_frame_flow", []core.Frame{&core.InputAudioFrame{Samples: audio, Encoding: core.EncodingPCM16LE, SampleRate: 16000, Channels: 1}, &core.OutputAudioFrame{Samples: audio, Encoding: core.EncodingPCM16LE, SampleRate: 24000, Channels: 1, Turn: "turn-1", Gen: 1}}, []string{"audio.input", "audio.output"}},
		{"user_speech", []core.Frame{&core.UserStartedSpeakingFrame{Gen: 1}, &core.UserStoppedSpeakingFrame{Gen: 1}}, []string{"user.speech.started", "user.speech.stopped"}},
		{"stt_interim_final", []core.Frame{&core.InterimTranscriptionFrame{Text: "private interim", Turn: "turn-1", Gen: 1}, &core.FinalTranscriptionFrame{Text: "private final", Turn: "turn-1", Gen: 1}}, []string{"stt.interim", "stt.final"}},
		{"llm_output", []core.Frame{&core.UserMessageFrame{Content: "private prompt", Turn: "turn-1", Gen: 1}, &core.LLMTextDeltaFrame{Delta: "private", Turn: "turn-1", Gen: 1}, &core.LLMTextFrame{Text: "private", Turn: "turn-1", Gen: 1}, &core.ErrorFrame{Err: errors.New("credential secret"), Fatal: false, Turn: "turn-1", Gen: 1}}, []string{"llm.requested", "llm.output", "llm.completed", "provider.error"}},
		{"tts_lifecycle_audio", []core.Frame{&core.TTSStartedFrame{Turn: "turn-1", Gen: 1}, &core.TTSAudioFrame{Audio: audio, Turn: "turn-1", Gen: 1, SampleRate: 24000, Channels: 1}, &core.SpeakingStateFrame{State: core.SpeakingStarted, Turn: "turn-1", Gen: 1}, &core.TTSStoppedFrame{Turn: "turn-1", Gen: 1}, &core.SpeakingStateFrame{State: core.SpeakingStopped, Turn: "turn-1", Gen: 1}}, []string{"tts.started", "tts.audio", "assistant.speaking.started", "tts.completed", "assistant.speaking.stopped"}},
		{"barge_in", nil, []string{"turn.interrupted", "output.flush.started", "output.flush.completed"}},
		{"flush", nil, []string{"output.flush.started", "output.flush.completed"}},
		{"tool_call_result", []core.Frame{&core.ToolCallFrame{Call: core.ToolCall{ID: "call-1", Name: "probe", Arguments: []byte(`{}`), SessionID: "compat-session", TurnID: "turn-1", Generation: 1}}}, []string{"tool.requested", "tool.started", "tool.completed", "tool.requested", "tool.started", "tool.failed"}},
		{"provider_pipeline_errors", []core.Frame{&core.ErrorFrame{Err: errors.New("credential secret"), Fatal: false}, &core.ErrorFrame{Err: errors.New("credential secret"), Fatal: true}}, []string{"provider.error", "session.error"}},
		{"session_teardown", []core.Frame{&core.CancelFrame{Gen: 1}}, []string{"session.cancelled", "session.ended", "processor.shutdown", "session.resources.completed"}},
	}
	results := make([]ScenarioResult, 0, len(specs))
	for _, spec := range specs {
		events, err := runRuntimeScenario(spec)
		results = append(results, ScenarioResult{Name: spec.name, Events: events, Err: err})
	}
	return results
}

func runRuntimeScenario(spec scenarioSpec) ([]Event, error) {
	if spec.name == "barge_in" {
		return runBargeScenario(spec)
	}
	if spec.name == "flush" {
		return runPacerFlushScenario(spec)
	}
	if spec.name == "session_teardown" {
		return runTeardownScenario(false)
	}
	probe := &frameProbe{}
	pipe, err := core.NewPipeline(probe)
	if err != nil {
		return nil, err
	}
	session, err := core.NewSession(1400, pipe, core.DefaultResourceBudget())
	if err != nil {
		return nil, err
	}
	outputs := make(chan core.FrameType, len(spec.frames)+3)
	session.OnOutput = func(f core.Frame) { outputs <- f.Type() }
	if err := session.Start(); err != nil {
		return nil, err
	}
	defer session.Close()
	select {
	case <-session.Ready():
	case <-time.After(time.Second):
		return nil, errors.New("session did not become ready")
	}
	if err := session.Post(&core.StartFrame{}); err != nil {
		return nil, err
	}
	select {
	case <-outputs:
	case <-time.After(time.Second):
		return nil, errors.New("runtime did not deliver start frame")
	}
	for _, frame := range spec.frames {
		if err := session.Post(frame); err != nil {
			return probe.snapshot(), err
		}
		select {
		case <-outputs:
		case <-time.After(time.Second):
			return probe.snapshot(), fmt.Errorf("runtime did not deliver %T", frame)
		}
	}
	if spec.name == "tool_call_result" {
		call := spec.frames[0].(*core.ToolCallFrame).Call
		loop, err := coretools.NewToolLoop([]core.ToolDefinition{{Name: "probe", InputSchema: []byte(`{"type":"object"}`)}}, core.ToolExecutorFunc(func(_ context.Context, c core.ToolCall) (core.ToolResult, error) {
			probe.toolStarted(c)
			if c.ID == "call-2" {
				return core.ToolResult{}, errors.New("tool backend unavailable")
			}
			return core.ToolResult{CallID: c.ID, Status: core.ToolSucceeded, Result: []byte(`{"ok":true}`)}, nil
		}), core.NewConversationContext(core.ContextConfig{}), coretools.DefaultToolLoopConfig())
		if err != nil {
			return probe.snapshot(), err
		}
		out, err := loop.Run(context.Background(), []core.ToolCall{call}, nil)
		if err != nil || len(out.Results) != 1 {
			return probe.snapshot(), fmt.Errorf("tool loop did not complete: %v", err)
		}
		if err := session.Post(&core.ToolResultFrame{Turn: call.TurnID, Gen: call.Generation, Result: out.Results[0]}); err != nil {
			return probe.snapshot(), err
		}
		select {
		case <-outputs:
		case <-time.After(time.Second):
			return probe.snapshot(), errors.New("tool result not delivered")
		}
		failed := call
		failed.ID = "call-2"
		if err := session.Post(&core.ToolCallFrame{Call: failed}); err != nil {
			return probe.snapshot(), err
		}
		select {
		case <-outputs:
		case <-time.After(time.Second):
			return probe.snapshot(), errors.New("failed tool request not delivered")
		}
		failure, err := loop.Run(context.Background(), []core.ToolCall{failed}, nil)
		if err != nil || len(failure.Results) != 1 || failure.Results[0].Status != core.ToolFailed {
			return probe.snapshot(), fmt.Errorf("tool failure not observed: %v", err)
		}
		if err := session.Post(&core.ToolResultFrame{Turn: failed.TurnID, Gen: failed.Generation, Result: failure.Results[0]}); err != nil {
			return probe.snapshot(), err
		}
		select {
		case <-outputs:
		case <-time.After(time.Second):
			return probe.snapshot(), errors.New("failed tool result not delivered")
		}
	}
	if err := session.Post(&core.EndFrame{}); err != nil {
		return probe.snapshot(), err
	}
	select {
	case <-session.Done():
	case <-time.After(time.Second):
		return probe.snapshot(), errors.New("session teardown timed out")
	}
	events := probe.snapshot()
	matched := 0
	for _, event := range events {
		if matched < len(spec.expected) && event.Name == spec.expected[matched] {
			matched++
		}
		if attrs := event.Attributes(); attrs["text"] != nil || attrs["transcript"] != nil {
			return events, errors.New("private content entered debug event")
		}
	}
	if matched != len(spec.expected) {
		return events, fmt.Errorf("observed %d/%d required transitions", matched, len(spec.expected))
	}
	if session.LeakedTasks() != 0 {
		return events, errors.New("runtime leaked supervised tasks")
	}
	return events, nil
}

// teardownProbe records only callbacks the session actually invokes. Its
// worker is registered with the session supervisor through the attached
// controller's Go capability, then exits after Close releases it.
type teardownProbe struct {
	*frameProbe
	closed               chan struct{}
	workerStarted        chan struct{}
	closeOnce            sync.Once
	callbackCalled       atomic.Bool
	closeObserved        atomic.Bool
	workerExited         atomic.Bool
	omitCloseObservation bool
}

func (p *teardownProbe) Attach(controller core.SessionController, _ int) error {
	supervisor, ok := controller.(interface {
		Go(func(context.Context)) error
	})
	if !ok {
		return errors.New("session controller lacks supervised Go")
	}
	return supervisor.Go(func(ctx context.Context) {
		close(p.workerStarted)
		<-ctx.Done()
		<-p.closed
		p.runtimeCompleted("session.resources.completed")
		p.workerExited.Store(true)
	})
}

func (p *teardownProbe) Close() error {
	p.closeOnce.Do(func() {
		p.callbackCalled.Store(true)
		if !p.omitCloseObservation {
			p.runtimeCompleted("processor.shutdown")
			p.closeObserved.Store(true)
		}
		close(p.closed)
	})
	return nil
}

func runTeardownScenario(omitCloseObservation bool) ([]Event, error) {
	probe := &teardownProbe{frameProbe: &frameProbe{}, closed: make(chan struct{}), workerStarted: make(chan struct{}), omitCloseObservation: omitCloseObservation}
	pipe, err := core.NewPipeline(probe)
	if err != nil {
		return nil, err
	}
	session, err := core.NewSession(1402, pipe, core.DefaultResourceBudget())
	if err != nil {
		return nil, err
	}
	outputs := make(chan core.FrameType, 4)
	session.OnOutput = func(f core.Frame) { outputs <- f.Type() }
	if err := session.Start(); err != nil {
		return nil, err
	}
	defer session.Close()
	select {
	case <-probe.workerStarted:
	case <-time.After(time.Second):
		return probe.snapshot(), errors.New("supervised teardown worker did not start")
	}
	for _, frame := range []core.Frame{&core.StartFrame{}, &core.CancelFrame{Gen: 1}, &core.EndFrame{}} {
		if err := session.Post(frame); err != nil {
			return probe.snapshot(), err
		}
		select {
		case kind := <-outputs:
			if kind != frame.Type() {
				return probe.snapshot(), fmt.Errorf("teardown delivered %v before %v", kind, frame.Type())
			}
		case <-time.After(time.Second):
			return probe.snapshot(), fmt.Errorf("teardown did not deliver %T", frame)
		}
	}
	select {
	case <-session.Done():
	case <-time.After(time.Second):
		return probe.snapshot(), errors.New("session teardown did not finish")
	}
	events := probe.snapshot()
	if !probe.callbackCalled.Load() || !probe.closeObserved.Load() {
		return events, errors.New("processor Close observation missing")
	}
	if !probe.workerExited.Load() || session.LeakedTasks() != 0 {
		return events, errors.New("supervised resource exit missing")
	}
	if err := requireEventOrder(events, []string{"session.cancelled", "session.ended", "processor.shutdown", "session.resources.completed"}); err != nil {
		return events, err
	}
	return events, nil
}

func runBargeScenario(spec scenarioSpec) ([]Event, error) {
	assistant := core.NewAssistantContextProcessor(core.NewConversationContext(core.ContextConfig{}), core.DefaultAssistantContextConfig())
	probe := &frameProbe{}
	transport := coretest.NewTransportOutput(coretest.TransportConfig{})
	pipe, err := core.NewPipeline(assistant, probe, transport)
	if err != nil {
		return nil, err
	}
	session, err := core.NewSession(1401, pipe, core.DefaultResourceBudget())
	if err != nil {
		return nil, err
	}
	delivered := make(chan core.FrameType, 16)
	session.OnOutput = func(f core.Frame) { delivered <- f.Type() }
	if err := session.Start(); err != nil {
		return nil, err
	}
	defer session.Close()
	postAndWait := func(f core.Frame) error {
		if err := session.Post(f); err != nil {
			return err
		}
		deadline := time.NewTimer(time.Second)
		defer deadline.Stop()
		for {
			select {
			case kind := <-delivered:
				if kind == f.Type() {
					return nil
				}
			case <-deadline.C:
				return fmt.Errorf("runtime did not deliver %T", f)
			}
		}
	}
	if err := postAndWait(&core.StartFrame{}); err != nil {
		return probe.snapshot(), err
	}
	if err := postAndWait(&core.UserMessageFrame{Turn: "turn-1", Gen: 1}); err != nil {
		return probe.snapshot(), err
	}
	if err := session.Post(&core.UserStartedSpeakingFrame{Gen: 1}); err != nil {
		return probe.snapshot(), err
	}
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		events := probe.snapshot()
		found := false
		for _, e := range events {
			if e.Name == "output.flush.completed" && e.Direction == core.Upstream.String() {
				found = true
			}
		}
		if found {
			break
		}
		select {
		case <-deadline.C:
			return events, errors.New("runtime transport flush acknowledgement absent")
		case <-time.After(time.Millisecond):
		}
	}
	if session.CurrentGeneration() != 2 || assistant.InterruptionCount.Load() != 1 || transport.FlushAcksSent.Load() != 1 {
		return probe.snapshot(), errors.New("runtime interruption/flush state mismatch")
	}
	if err := session.Post(&core.OutputAudioFrame{Samples: []byte{1, 2}, SampleRate: 24000, Channels: 1, Turn: "turn-1", Gen: 1}); err != nil {
		return probe.snapshot(), err
	}
	for session.StaleDiscarded.Load() == 0 {
		select {
		case <-deadline.C:
			return probe.snapshot(), errors.New("stale output was not rejected")
		case <-time.After(time.Millisecond):
		}
	}
	if len(transport.GetWrittenFrames()) != 0 {
		return probe.snapshot(), errors.New("stale output reached transport")
	}
	if err := session.Post(&core.EndFrame{}); err != nil {
		return probe.snapshot(), err
	}
	select {
	case <-session.Done():
	case <-deadline.C:
		return probe.snapshot(), errors.New("barge scenario did not tear down")
	}
	events := probe.snapshot()
	return events, requireEventOrder(events, spec.expected)
}

func runPacerFlushScenario(spec scenarioSpec) ([]Event, error) {
	entered, release := make(chan struct{}), make(chan struct{})
	var writes atomic.Uint64
	p := ws.NewPacedWriter(ws.DefaultPacerConfig(), func(_ context.Context, _ []byte) error {
		if writes.Add(1) == 1 {
			close(entered)
			<-release
		}
		return nil
	})
	defer p.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)
	p.SetActiveGeneration(1)
	frame := func(seq, gen uint64) *core.OutputAudioFrame {
		return &core.OutputAudioFrame{Samples: make([]byte, 960), SampleRate: 24000, Channels: 1, Duration: 20 * time.Millisecond, Turn: "turn-1", Gen: gen, Seq: seq}
	}
	if err := p.Enqueue(frame(1, 1)); err != nil {
		return nil, err
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		return nil, errors.New("pacer did not enter write")
	}
	if err := p.Enqueue(frame(2, 1)); err != nil {
		close(release)
		return nil, err
	}
	events := []Event{runtimePacerEvent("output.flush.started", 1, 1, nil)}
	flushed := make(chan error, 1)
	go func() { flushed <- p.Flush(context.Background(), 1) }()
	select {
	case err := <-flushed:
		close(release)
		return events, fmt.Errorf("flush acknowledged in-flight write early: %v", err)
	case <-time.After(10 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-flushed:
		if err != nil {
			return events, err
		}
	case <-time.After(time.Second):
		return events, errors.New("pacer flush barrier timed out")
	}
	if p.QueuedFrames() != 0 || p.FramesDropped.Load() == 0 {
		return events, errors.New("pacer failed to flush queued output")
	}
	events = append(events, runtimePacerEvent("output.flush.completed", 2, 1, map[string]any{"count": int(p.FramesDropped.Load())}))
	if err := p.Enqueue(frame(3, 2)); err != nil {
		return events, err
	}
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for writes.Load() < 2 {
		select {
		case <-deadline.C:
			return events, errors.New("new generation did not resume")
		case <-time.After(time.Millisecond):
		}
	}
	if writes.Load() != 2 {
		return events, errors.New("stale queued output was written")
	}
	return events, requireEventOrder(events, spec.expected)
}

func runtimePacerEvent(name string, sequence, gen uint64, attrs map[string]any) Event {
	e := core.NewDebugEvent(name, time.Unix(1, int64(sequence)), "compat-session", "turn-1", core.Generation(gen), attrs)
	e.PipelineID, e.Processor, e.ProcessorIndex = "compat-pipeline", "paced-writer", 0
	e.FrameKind, e.Direction, e.Sequence = core.FrameFlush, core.Downstream, sequence
	out, _ := (Adapter{}).Adapt(e)
	return out
}

func requireEventOrder(events []Event, expected []string) error {
	matched := 0
	for _, e := range events {
		if matched < len(expected) && e.Name == expected[matched] {
			matched++
		}
	}
	if matched != len(expected) {
		return fmt.Errorf("observed %d/%d required runtime transitions", matched, len(expected))
	}
	return nil
}
