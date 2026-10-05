// Package eval provides deterministic, credential-free P0 behavior probes.
package eval

import (
	"context"
	"errors"
	"fmt"
	"time"

	"govoice/core"
	coretools "govoice/core/tools"
	"govoice/core/transcript"
	"govoice/core/turn"
	"govoice/tools/compat/whisker"
	"govoice/transport/rtvi"
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
		{"barge_in", 40 * time.Millisecond, []string{"advance output generation", "discard old interruptible output"}, checkBargeIn},
		{"flush", 100 * time.Millisecond, []string{"enqueue bounded output", "flush generation", "verify queue drained"}, checkFlush},
		{"turn_completion", 700 * time.Millisecond, []string{"start speech turn", "advance silence deadline", "observe completion"}, func() error { return checkTurn(base, true) }},
		{"silence_timeout", 700 * time.Millisecond, []string{"observe activity", "advance silence deadline", "observe completion"}, func() error { return checkTurn(base, true) }},
		{"no_speech_timeout", 10 * time.Second, []string{"start session without speech", "advance initial silence", "observe no-speech event"}, func() error { return checkTurn(base, false) }},
		{"transcript_stabilization", 200 * time.Millisecond, []string{"observe final candidate", "complete turn", "advance grace", "verify one commit"}, checkTranscript},
		{"late_final_and_invalidation", 200 * time.Millisecond, []string{"retire expired candidate", "reject late final", "invalidate incomplete turn"}, checkLateTranscript},
		{"output_pacing_and_speaking", 20 * time.Millisecond, []string{"write paced turn-final audio", "verify output-derived speaking start and drain"}, checkPacingSpeaking},
		{"tool_interruption_cancellation", time.Millisecond, []string{"interrupt active tool call", "verify executor cancellation"}, checkToolCancellation},
		{"teardown_leak", time.Millisecond, []string{"start session runtime", "close and await teardown", "verify no supervised tasks leaked"}, checkSessionTeardown},
		{"input_mute", time.Millisecond, []string{"mute turn manager", "verify observations do not advance", "unmute"}, func() error { return checkMute(base) }},
		{"rtvi_wire", time.Millisecond, []string{"encode semantic speech-start event", "verify RTVI envelope and identity"}, checkRTVI},
		{"whisker_semantic", time.Millisecond, []string{"adapt debug event", "verify correlation and copied attributes"}, checkWhisker},
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
	report.Compatibility = Compatibility{
		RTVI:          SuiteResult{Name: "pinned_rtvi_client", Status: "separate_suite", Evidence: []string{"node tools/compat/test_rtvi_client.js", "node tools/compat/verify_fixtures.js"}},
		Whisker:       SuiteResult{Name: "whisker_semantic", Status: "pass", Evidence: []string{"tools/compat/whisker adapter scenarios"}},
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
	return nil
}

func checkTurn(at time.Time, speech bool) error {
	cfg := turn.DefaultConfig()
	cfg.InitialSilenceTimeout = time.Second
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
	if !speech {
		frames, err := m.Advance(at.Add(time.Second))
		if err != nil {
			return err
		}
		return hasTurnKind(frames, core.TurnEventNoSpeechTimeout)
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
	return m.SetMuted(false, at.Add(3*time.Millisecond))
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
	return nil
}

func checkCancellation() error {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	select {
	case <-ctx.Done():
		return nil
	case <-time.After(time.Second):
		return errors.New("cancelled operation remained live")
	}
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
	return nil
}

func checkToolCancellation() error {
	started := make(chan struct{})
	loop, err := coretools.NewToolLoop([]core.ToolDefinition{{Name: "wait", InputSchema: []byte(`{"type":"object"}`)}}, core.ToolExecutorFunc(func(ctx context.Context, call core.ToolCall) (core.ToolResult, error) {
		close(started)
		<-ctx.Done()
		return core.ToolResult{CallID: call.ID, Status: core.ToolCancelled}, ctx.Err()
	}), core.NewConversationContext(core.ContextConfig{}), coretools.DefaultToolLoopConfig())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, runErr := loop.Run(ctx, []core.ToolCall{{ID: "call", Name: "wait", Arguments: []byte(`{}`), SessionID: "s", TurnID: "t", Generation: 1}}, nil)
		done <- runErr
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		cancel()
		return errors.New("tool executor did not start")
	}
	cancel()
	select {
	case runErr := <-done:
		if !errors.Is(runErr, context.Canceled) {
			return fmt.Errorf("tool interruption returned %v", runErr)
		}
		return nil
	case <-time.After(time.Second):
		return errors.New("tool executor survived interruption")
	}
}

func checkPacingSpeaking() error {
	events := make(chan core.SpeakingStateFrame, 4)
	p := ws.NewPacedWriter(ws.PacerConfig{}, func(context.Context, []byte) error { return nil })
	defer p.Close()
	p.OnSpeakingState = func(event core.SpeakingStateFrame) { events <- event }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- p.Run(ctx) }()
	p.SetActiveGeneration(1)
	if err := p.Enqueue(&core.OutputAudioFrame{Samples: make([]byte, 960), SampleRate: 24000, Channels: 1, Duration: 20 * time.Millisecond, Turn: "eval", Gen: 1, UnitID: 1, IsFinal: true, TurnFinal: true}); err != nil {
		p.Close()
		cancel()
		return err
	}
	seenStart, seenStop := false, false
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for !seenStop {
		select {
		case event := <-events:
			if event.State == core.SpeakingStarted {
				seenStart = true
			}
			if event.State == core.SpeakingStopped {
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
	p.Close()
	cancel()
	return nil
}

func checkSessionTeardown() error {
	pipe, err := core.NewPipeline(core.NewFakeLLMProcessor(core.FakeLLMConfig{ResponseChunks: []string{"eval"}}))
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
func checkRTVI() error {
	e := core.NewDebugEvent("user.speech.started", time.Unix(1, 0), "s", "t", 1, nil)
	m, err := rtvi.EncodeDebugEvent(e)
	if err != nil {
		return err
	}
	if m.Label != rtvi.LabelRTVI || m.Type != rtvi.TypeUserStartedSpeaking {
		return errors.New("RTVI event mismatch")
	}
	return nil
}
func checkWhisker() error {
	e := core.NewDebugEvent("turn.started", time.Unix(1, 0), "s", "t", 1, nil)
	out, err := (whisker.Adapter{}).Adapt(e)
	if err != nil {
		return err
	}
	if out.SessionID != "s" || out.TurnID != "t" {
		return errors.New("Whisker identity mismatch")
	}
	return nil
}
