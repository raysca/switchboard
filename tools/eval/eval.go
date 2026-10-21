// Package eval provides deterministic, credential-free P0 behavior probes.
package eval

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"govoice/core"
	"govoice/core/coretest"
	coretools "govoice/core/tools"
	"govoice/core/transcript"
	"govoice/core/turn"
	"govoice/tools/compat/whisker"
	"govoice/transport/ws"
)

type Result struct {
	Scenario    string   `json:"scenario"`
	Passed      bool     `json:"passed"`
	DurationNS  int64    `json:"duration_ns"`
	Steps       []string `json:"steps"`
	Diagnostics []string `json:"diagnostics,omitempty"`
}
type SuiteResult struct {
	Name     string   `json:"name"`
	Status   string   `json:"status"`
	Evidence []string `json:"evidence"`
}
type Compatibility struct {
	RTVI          SuiteResult `json:"rtvi_client"`
	Whisker       SuiteResult `json:"whisker_semantic"`
	Pipecat       SuiteResult `json:"pipecat_parity"`
	PlaybackClear string      `json:"playback_clear_probe"`
}
type Report struct {
	Schema        string        `json:"schema"`
	Results       []Result      `json:"results"`
	Compatibility Compatibility `json:"compatibility"`
}

type scenario struct {
	name     string
	duration time.Duration
	steps    []string
	run      func() error
}

func RunAll() (Report, error) {
	base := time.Unix(100, 0)
	scenarios := []scenario{
		{"barge_in", 40 * time.Millisecond, []string{"observe runtime interruption", "advance output generation", "discard old output"}, checkBargeIn},
		{"flush", 100 * time.Millisecond, []string{"enqueue queued and in-flight output", "wait for flush barrier", "verify new generation resumes"}, checkFlush},
		{"turn_completion", 700 * time.Millisecond, []string{"start speech turn", "advance silence deadline", "observe completion"}, func() error { return checkTurn(base) }},
		{"silence_timeout", 700 * time.Millisecond, []string{"observe activity", "advance silence deadline", "observe completion"}, func() error { return checkTurn(base) }},
		{"transcript_stabilization", 200 * time.Millisecond, []string{"observe final candidate", "complete turn", "advance grace", "verify one commit"}, checkTranscript},
		{"late_final_and_invalidation", 200 * time.Millisecond, []string{"retire expired candidate", "reject late final", "invalidate incomplete turn"}, checkLateTranscript},
		{"output_pacing_and_speaking", 60 * time.Millisecond, []string{"write three paced audio frames", "verify order and media time", "verify speaking start and drain"}, checkPacingSpeaking},
		{"tool_interruption_cancellation", time.Millisecond, []string{"interrupt active tool call", "verify executor cancellation"}, checkToolCancellation},
		{"teardown_leak", time.Millisecond, []string{"start session runtime", "close and await teardown", "verify no supervised tasks leaked"}, checkSessionTeardown},
		{"input_mute", time.Millisecond, []string{"mute turn manager and runtime admission", "verify input is dropped before processors", "unmute and resume audio"}, func() error { return checkMute(base) }},
	}
	report := Report{Schema: "govoice.eval/v1", Results: make([]Result, 0, len(scenarios))}
	for _, sc := range scenarios {
		err := sc.run()
		result := Result{Scenario: sc.name, Passed: err == nil, DurationNS: int64(sc.duration), Steps: append([]string(nil), sc.steps...)}
		if err != nil {
			result.Diagnostics = []string{safeDiagnostic(err)}
		}
		report.Results = append(report.Results, result)
	}
	for _, sc := range whisker.RunScenarios() {
		result := Result{Scenario: "whisker_" + sc.Name, Passed: sc.Err == nil, DurationNS: int64(time.Millisecond), Steps: []string{"observe runtime frames", "adapt ordered semantic events", "verify correlation and privacy"}}
		if sc.Err != nil {
			result.Diagnostics = []string{safeDiagnostic(sc.Err)}
		}
		report.Results = append(report.Results, result)
	}
	report.Compatibility = Compatibility{
		RTVI:          SuiteResult{Name: "pinned_rtvi_client", Status: "separate_suite", Evidence: []string{"node tools/compat/test_rtvi_client.js", "node tools/compat/verify_fixtures.js"}},
		Whisker:       SuiteResult{Name: "whisker_semantic", Status: whiskerStatus(report.Results), Evidence: []string{"tools/compat/whisker runtime frame scenarios"}},
		Pipecat:       SuiteResult{Name: "pinned_pipecat_parity", Status: "separate_suite", Evidence: []string{"paired cases are maintained in docs/pipecat-compatibility.md"}},
		PlaybackClear: "UNMODIFIED_CLIENT_CANNOT_CLEAR_BUFFERED_PLAYBACK_ON_WIRE_EVENT",
	}
	for _, result := range report.Results {
		if !result.Passed {
			return report, fmt.Errorf("eval scenario %s failed: %s", result.Scenario, result.Diagnostics[0])
		}
	}
	return report, nil
}

func whiskerStatus(results []Result) string {
	seen := false
	for _, result := range results {
		if len(result.Scenario) < len("whisker_") || result.Scenario[:len("whisker_")] != "whisker_" {
			continue
		}
		seen = true
		if !result.Passed {
			return "fail"
		}
	}
	if seen {
		return "pass"
	}
	return "not_run"
}

func ExitCode(report Report) int {
	for _, result := range report.Results {
		if !result.Passed {
			return 1
		}
	}
	return 0
}
func safeDiagnostic(err error) string {
	if errors.Is(err, context.Canceled) {
		return "operation cancelled as requested"
	}
	return fmt.Sprintf("scenario check failed (%T)", err)
}

func checkFlush() error {
	p := ws.NewPacedWriter(ws.PacerConfig{}, func(context.Context, []byte) error { return nil })
	defer p.Close()
	p.SetActiveGeneration(1)
	if err := p.Enqueue(&core.OutputAudioFrame{Samples: []byte{1, 2}, SampleRate: 24000, Channels: 1, Duration: time.Millisecond, Gen: 1, Turn: "eval"}); err != nil {
		return err
	}
	if p.QueuedFrames() != 1 {
		return fmt.Errorf("expected one queued frame, got %d", p.QueuedFrames())
	}
	if err := p.Flush(context.Background(), 1); err != nil {
		return err
	}
	if p.QueuedFrames() != 0 {
		return fmt.Errorf("flush retained %d queued frames", p.QueuedFrames())
	}
	return checkInflightFlush()
}

func checkInflightFlush() error {
	entered := make(chan struct{})
	release := make(chan struct{})
	var writes atomic.Uint64
	p := ws.NewPacedWriter(ws.DefaultPacerConfig(), func(context.Context, []byte) error {
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
	frame := func(seq uint64, gen uint64) *core.OutputAudioFrame {
		return &core.OutputAudioFrame{Samples: make([]byte, 960), SampleRate: 24000, Channels: 1, Duration: 20 * time.Millisecond, Turn: "eval", Gen: gen, Seq: seq}
	}
	if err := p.Enqueue(frame(1, 1)); err != nil {
		return err
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		return errors.New("in-flight output never entered writer")
	}
	if err := p.Enqueue(frame(2, 1)); err != nil {
		close(release)
		return err
	}
	flushDone := make(chan error, 1)
	go func() { flushDone <- p.Flush(context.Background(), 1) }()
	select {
	case err := <-flushDone:
		close(release)
		return fmt.Errorf("flush acknowledged before in-flight write completed: %v", err)
	case <-time.After(10 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-flushDone:
		if err != nil {
			return err
		}
	case <-time.After(time.Second):
		return errors.New("flush did not acknowledge completed write")
	}
	if p.QueuedFrames() != 0 || p.FramesDropped.Load() == 0 {
		return errors.New("flush retained queued or in-flight generation-one output")
	}
	if err := p.Enqueue(frame(3, 2)); err != nil {
		return err
	}
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for writes.Load() < 2 {
		select {
		case <-deadline.C:
			return errors.New("new-generation output did not resume after flush")
		case <-time.After(time.Millisecond):
		}
	}
	if writes.Load() != 2 {
		return fmt.Errorf("stale queued output was written: %d writes", writes.Load())
	}
	return nil
}

func checkTurn(at time.Time) error {
	cfg := turn.DefaultConfig()
	cfg.SilenceTimeout = 100 * time.Millisecond
	cfg.StartDebounce = 0
	cfg.MinimumSpeechDuration = 0
	cfg.StopDebounce = 0
	m, err := turn.NewManager(cfg, turn.SilenceDetector{})
	if err != nil {
		return err
	}
	if err = m.Start(at); err != nil {
		return err
	}
	frames, err := m.Observe(turn.Observation{At: at.Add(time.Millisecond), TurnID: "t", Generation: 1, Speaking: true, Activity: true})
	if err != nil {
		return err
	}
	if hasTurnKind(frames, core.TurnEventStarted) != nil {
		frames, err = m.Observe(turn.Observation{At: at.Add(2 * time.Millisecond), TurnID: "t", Generation: 1, Speaking: true, Activity: true})
		if err != nil {
			return err
		}
		if hasTurnKind(frames, core.TurnEventStarted) != nil {
			return errors.New("speech start was not accepted")
		}
	}
	if _, err = m.Observe(turn.Observation{At: at.Add(3 * time.Millisecond), TurnID: "t", Generation: 1, Speaking: false}); err != nil {
		return err
	}
	frames, err = m.Advance(at.Add(200 * time.Millisecond))
	if err != nil {
		return err
	}
	return hasTurnKind(frames, core.TurnEventCompleted)
}
func hasTurnKind(frames []core.Frame, kind core.TurnEventKind) error {
	for _, f := range frames {
		if ev, ok := f.(*core.TurnEventFrame); ok && ev.Kind == kind {
			return nil
		}
	}
	return fmt.Errorf("expected turn event %v", kind)
}

func checkMute(at time.Time) error {
	cfg := turn.DefaultConfig()
	m, err := turn.NewManager(cfg, turn.SilenceDetector{})
	if err != nil {
		return err
	}
	if err = m.Start(at); err != nil {
		return err
	}
	if err = m.SetMuted(true, at.Add(time.Millisecond)); err != nil {
		return err
	}
	frames, err := m.Observe(turn.Observation{At: at.Add(2 * time.Millisecond), TurnID: "t", Generation: 1, Speaking: true, Activity: true})
	if err != nil {
		return err
	}
	if len(frames) != 0 {
		return errors.New("muted observation emitted turn events")
	}
	if err := m.SetMuted(false, at.Add(3*time.Millisecond)); err != nil {
		return err
	}
	return checkRuntimeMuteGate()
}

type muteInputProbe struct{ audio atomic.Uint64 }

func (p *muteInputProbe) ProcessFrame(_ context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	if f.Type() == core.FrameInputAudio {
		p.audio.Add(1)
	}
	return e.PushDirection(f, dir)
}

func checkRuntimeMuteGate() error {
	probe := &muteInputProbe{}
	pipe, err := core.NewPipeline(probe)
	if err != nil {
		return err
	}
	session, err := core.NewSession(9902, pipe, core.DefaultResourceBudget())
	if err != nil {
		return err
	}
	if err := session.Start(); err != nil {
		return err
	}
	defer session.Close()
	select {
	case <-session.Ready():
	case <-time.After(time.Second):
		return errors.New("mute probe session not ready")
	}
	states := make(chan bool, 2)
	audioDelivered := make(chan struct{}, 1)
	session.OnOutput = func(f core.Frame) {
		if state, ok := f.(*core.InputMutedStateFrame); ok {
			states <- state.Muted
		}
		if f.Type() == core.FrameInputAudio {
			audioDelivered <- struct{}{}
		}
	}
	if err := session.Post(&core.InputMuteFrame{}); err != nil {
		return err
	}
	select {
	case muted := <-states:
		if !muted || !session.InputMuted() {
			return errors.New("runtime mute state not admitted")
		}
	case <-time.After(time.Second):
		return errors.New("runtime mute state not observed")
	}
	audio := &core.InputAudioFrame{Samples: make([]byte, 640), Encoding: core.EncodingPCM16LE, SampleRate: 16000, Channels: 1, Duration: 20 * time.Millisecond, Gen: 1}
	if err := session.Post(audio); err != nil {
		return err
	}
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for session.AudioDropped.Load() == 0 {
		select {
		case <-deadline.C:
			return errors.New("muted audio reached VAD/STT path")
		case <-time.After(time.Millisecond):
		}
	}
	if probe.audio.Load() != 0 {
		return errors.New("muted audio reached processor")
	}
	if err := session.Post(&core.InputUnmuteFrame{}); err != nil {
		return err
	}
	select {
	case muted := <-states:
		if muted || session.InputMuted() {
			return errors.New("runtime unmute state not admitted")
		}
	case <-time.After(time.Second):
		return errors.New("runtime unmute state not observed")
	}
	if err := session.Post(audio); err != nil {
		return err
	}
	select {
	case <-audioDelivered:
	case <-time.After(time.Second):
		return errors.New("audio did not resume after unmute")
	}
	if probe.audio.Load() != 1 {
		return fmt.Errorf("unexpected VAD/STT input count %d", probe.audio.Load())
	}
	return nil
}

func checkTranscript() error {
	f, err := transcript.NewFinalizer(transcript.DefaultConfig())
	if err != nil {
		return err
	}
	at := time.Unix(100, 0)
	out, err := f.Observe(transcript.Candidate{Turn: "t", Generation: 1, Text: "hello", At: at, Final: true})
	if err != nil {
		return err
	}
	if len(out) != 0 {
		return errors.New("provider final committed before turn completion")
	}
	out, err = f.Complete("t", 1, at.Add(time.Millisecond))
	if err != nil {
		return err
	}
	if len(out) != 0 {
		return errors.New("turn committed before grace")
	}
	out, err = f.Advance(at.Add(time.Second))
	if err != nil {
		return err
	}
	commits := 0
	for _, o := range out {
		if o.Kind == transcript.Committed {
			commits++
		}
	}
	if commits != 1 {
		return fmt.Errorf("expected one stabilized commit, got %d", commits)
	}
	return nil
}

func checkLateTranscript() error {
	f, err := transcript.NewFinalizer(transcript.DefaultConfig())
	if err != nil {
		return err
	}
	at := time.Unix(100, 0)
	if _, err := f.Observe(transcript.Candidate{Turn: "t", Generation: 1, Text: "first", At: at, Final: true}); err != nil {
		return err
	}
	if _, err := f.Complete("t", 1, at.Add(time.Millisecond)); err != nil {
		return err
	}
	if _, err := f.Advance(at.Add(time.Second)); err != nil {
		return err
	}
	late, err := f.Observe(transcript.Candidate{Turn: "t", Generation: 1, Text: "late correction", At: at.Add(time.Second + time.Millisecond), Final: true})
	if err != nil {
		return err
	}
	if len(late) != 0 {
		return errors.New("late final changed a retired transcript")
	}
	invalid, err := f.Observe(transcript.Candidate{Turn: "invalid", Generation: 1, Text: "partial", At: at.Add(time.Second + 3*time.Millisecond), Final: true})
	if err != nil {
		return err
	}
	if err := f.Interrupt("invalid", 1, at.Add(time.Second+4*time.Millisecond)); err != nil {
		return err
	}
	if len(invalid) != 0 {
		return errors.New("interrupted incomplete transcript emitted output")
	}
	lateInvalid, err := f.Observe(transcript.Candidate{Turn: "invalid", Generation: 1, Text: "late after interruption", At: at.Add(time.Second + 5*time.Millisecond), Final: true})
	if err != nil {
		return err
	}
	completed, err := f.Complete("invalid", 1, at.Add(time.Second+6*time.Millisecond))
	if err != nil {
		return err
	}
	advanced, err := f.Advance(at.Add(2 * time.Second))
	if err != nil {
		return err
	}
	if len(lateInvalid) != 0 || len(completed) != 0 || len(advanced) != 0 {
		return errors.New("late interrupted transcript committed after invalidation")
	}
	return nil
}

func checkBargeIn() error {
	p := ws.NewPacedWriter(ws.PacerConfig{}, func(context.Context, []byte) error { return nil })
	defer p.Close()
	p.SetActiveGeneration(2)
	if err := p.Enqueue(&core.OutputAudioFrame{Samples: []byte{1, 2}, SampleRate: 24000, Channels: 1, Duration: time.Millisecond, Turn: "old", Gen: 1}); err != nil {
		return err
	}
	if p.QueuedFrames() != 0 {
		return fmt.Errorf("stale generation survived barge-in fence: queued=%d", p.QueuedFrames())
	}
	return checkRuntimeBargeIn()
}

func checkRuntimeBargeIn() error {
	contextPool := core.NewConversationContext(core.ContextConfig{})
	assistant := core.NewAssistantContextProcessor(contextPool, core.DefaultAssistantContextConfig())
	transport := coretest.NewTransportOutput(coretest.TransportConfig{})
	pipe, err := core.NewPipeline(assistant, transport)
	if err != nil {
		return err
	}
	session, err := core.NewSession(9903, pipe, core.DefaultResourceBudget())
	if err != nil {
		return err
	}
	observed := make(chan core.FrameType, 16)
	session.OnFrame = func(f core.Frame, dir core.Direction) {
		if dir == core.Downstream {
			select {
			case observed <- f.Type():
			default:
			}
		}
	}
	if err := session.Start(); err != nil {
		return err
	}
	defer session.Close()
	select {
	case <-session.Ready():
	case <-time.After(time.Second):
		return errors.New("barge-in session not ready")
	}
	if err := session.Post(&core.UserMessageFrame{Turn: "eval", Gen: 1}); err != nil {
		return err
	}
	if err := session.Post(&core.UserStartedSpeakingFrame{Gen: 1}); err != nil {
		return err
	}
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	seenInterrupt := false
	for !seenInterrupt {
		select {
		case kind := <-observed:
			if kind == core.FrameInterruption {
				seenInterrupt = true
			}
		case <-deadline.C:
			return errors.New("runtime interruption was not observed")
		}
	}
	if session.CurrentGeneration() != 2 || assistant.InterruptionCount.Load() != 1 {
		return errors.New("interruption did not advance active generation")
	}
	for transport.FlushAcksSent.Load() == 0 {
		select {
		case <-deadline.C:
			return errors.New("runtime flush acknowledgement absent")
		case <-time.After(time.Millisecond):
		}
	}
	old := &core.OutputAudioFrame{Samples: []byte{1, 2}, SampleRate: 24000, Channels: 1, Turn: "eval", Gen: 1}
	if err := session.Post(old); err != nil {
		return err
	}
	for session.StaleDiscarded.Load() == 0 {
		select {
		case <-deadline.C:
			return errors.New("stale output was not discarded after interruption")
		case <-time.After(time.Millisecond):
		}
	}
	if len(transport.GetWrittenFrames()) != 0 {
		return errors.New("stale output was delivered after interruption")
	}
	return nil
}

func checkToolCancellation() error {
	assistant := core.NewAssistantContextProcessor(core.NewConversationContext(core.ContextConfig{}), core.DefaultAssistantContextConfig())
	transport := coretest.NewTransportOutput(coretest.TransportConfig{})
	pipe, err := core.NewPipeline(assistant, transport)
	if err != nil {
		return err
	}
	session, err := core.NewSession(9904, pipe, core.DefaultResourceBudget())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	interrupted := make(chan struct{}, 1)
	session.OnFrame = func(f core.Frame, dir core.Direction) {
		if f.Type() == core.FrameInterruption && dir == core.Downstream {
			cancel()
			select {
			case interrupted <- struct{}{}:
			default:
			}
		}
	}
	delivered := make(chan core.FrameType, 8)
	session.OnOutput = func(f core.Frame) { delivered <- f.Type() }
	if err := session.Start(); err != nil {
		return err
	}
	defer session.Close()
	if err := session.Post(&core.UserMessageFrame{Turn: "t", Gen: 1}); err != nil {
		return err
	}
	select {
	case kind := <-delivered:
		if kind != core.FrameUserMessage {
			return fmt.Errorf("tool session observed %v before user turn", kind)
		}
	case <-time.After(time.Second):
		return errors.New("tool session did not activate turn")
	}
	started := make(chan struct{})
	cfg := coretools.DefaultToolLoopConfig()
	cfg.IsCurrent = func(call core.ToolCall) bool { return session.CurrentGeneration() == uint64(call.Generation) }
	loop, err := coretools.NewToolLoop([]core.ToolDefinition{{Name: "wait", InputSchema: []byte(`{"type":"object"}`)}}, core.ToolExecutorFunc(func(ctx context.Context, call core.ToolCall) (core.ToolResult, error) {
		close(started)
		<-ctx.Done()
		return core.ToolResult{CallID: call.ID, Status: core.ToolCancelled}, ctx.Err()
	}), core.NewConversationContext(core.ContextConfig{}), cfg)
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() {
		_, runErr := loop.Run(ctx, []core.ToolCall{{ID: "call", Name: "wait", Arguments: []byte(`{}`), SessionID: "s", TurnID: "t", Generation: 1}}, nil)
		done <- runErr
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		return errors.New("tool executor did not start")
	}
	if err := session.Post(&core.UserStartedSpeakingFrame{Gen: 1}); err != nil {
		return err
	}
	select {
	case <-interrupted:
	case <-time.After(time.Second):
		return errors.New("active tool was not interrupted by session")
	}
	if session.CurrentGeneration() != 2 || assistant.InterruptionCount.Load() != 1 {
		return errors.New("tool interruption did not advance session generation")
	}
	select {
	case runErr := <-done:
		if !errors.Is(runErr, context.Canceled) && !errors.Is(runErr, coretools.ErrStaleToolCall) {
			return fmt.Errorf("tool interruption returned %v", runErr)
		}
		return nil
	case <-time.After(time.Second):
		return errors.New("tool executor survived interruption")
	}
}

func checkPacingSpeaking() error {
	writes := make(chan time.Time, 4)
	completions := make(chan uint64, 4)
	states := make(chan core.SpeakingStateFrame, 4)
	p := ws.NewPacedWriter(ws.DefaultPacerConfig(), func(_ context.Context, samples []byte) error {
		writes <- time.Now()
		return nil
	})
	defer p.Close()
	p.OnSpeakingState = func(event core.SpeakingStateFrame) { states <- event }
	p.OnWriteComplete = func(frame *core.OutputAudioFrame, err error) {
		if err == nil {
			completions <- frame.Seq
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- p.Run(ctx) }()
	p.SetActiveGeneration(1)
	for i := 0; i < 3; i++ {
		samples := make([]byte, 960)
		samples[0] = byte(i + 1)
		if err := p.Enqueue(&core.OutputAudioFrame{Samples: samples, SampleRate: 24000, Channels: 1, Duration: 20 * time.Millisecond, Turn: "eval", Gen: 1, UnitID: 1, Seq: uint64(i + 1), IsFinal: i == 2, TurnFinal: i == 2}); err != nil {
			return err
		}
	}
	seenStart, seenStop := false, false
	observed := make([]time.Time, 0, 3)
	completed := make([]uint64, 0, 3)
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for !seenStop {
		select {
		case at := <-writes:
			observed = append(observed, at)
		case seq := <-completions:
			completed = append(completed, seq)
		case event := <-states:
			if event.State == core.SpeakingStarted {
				if seenStart {
					return errors.New("speaking started more than once")
				}
				seenStart = true
			}
			if event.State == core.SpeakingStopped {
				if !seenStart {
					return errors.New("speaking stopped before start")
				}
				seenStop = true
			}
		case <-deadline.C:
			p.Close()
			cancel()
			return errors.New("paced speaking lifecycle did not drain")
		case err := <-runDone:
			return fmt.Errorf("paced writer exited before drain: %v", err)
		}
	}
	if !seenStart {
		return errors.New("speaking stop observed without output-derived start")
	}
	// Channel delivery can select a state before a write that already happened.
	for len(observed) < 3 {
		select {
		case at := <-writes:
			observed = append(observed, at)
		default:
			return fmt.Errorf("speaking drained after %d/3 writes", len(observed))
		}
	}
	for len(completed) < 3 {
		select {
		case seq := <-completions:
			completed = append(completed, seq)
		default:
			return fmt.Errorf("only %d/3 frames completed", len(completed))
		}
	}
	for i, seq := range completed {
		if seq != uint64(i+1) {
			return fmt.Errorf("paced write %d out of order: %d", i, seq)
		}
	}
	if observed[2].Sub(observed[0]) < 30*time.Millisecond {
		return errors.New("output frames were not paced against media time")
	}
	if p.FramesWritten.Load() != 3 {
		return fmt.Errorf("expected three writes, got %d", p.FramesWritten.Load())
	}
	p.Close()
	cancel()
	return nil
}

func checkSessionTeardown() error {
	pipe, err := core.NewPipeline(coretest.NewLLMProcessor(coretest.LLMConfig{ResponseChunks: []string{"eval"}}))
	if err != nil {
		return err
	}
	session, err := core.NewSession(9901, pipe, core.DefaultResourceBudget())
	if err != nil {
		return err
	}
	if err = session.Start(); err != nil {
		return err
	}
	select {
	case <-session.Ready():
	case <-time.After(time.Second):
		return errors.New("session did not become ready")
	}
	if err = session.Close(); err != nil {
		return err
	}
	select {
	case <-session.Done():
	case <-time.After(time.Second):
		return errors.New("session teardown did not finish")
	}
	if session.LeakedTasks() != 0 {
		return fmt.Errorf("session reported %d leaked tasks", session.LeakedTasks())
	}
	return nil
}
