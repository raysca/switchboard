package turn

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/coretest"
)

type muteHarness struct {
	*harness
	drops  atomic.Int64
	debugs chan core.DebugEvent
}

func newMuteHarness(t *testing.T, cfg Config, strategies []UserMuteStrategy, max time.Duration) *muteHarness {
	t.Helper()
	m := &muteHarness{debugs: make(chan core.DebugEvent, 32)}
	m.harness = &harness{t: t, clk: &fakeClock{now: time.Unix(1000, 0)}, stt: &fakeSTT{}, sink: &coretest.PassthroughProcessor{}}
	proc, err := NewProcessor(cfg, ProcessorOptions{Clock: m.clk, Mute: strategies, MaxMute: max, OnMuteDrop: func() { m.drops.Add(1) }})
	if err != nil {
		t.Fatal(err)
	}
	m.proc = proc
	acfg := core.DefaultAssistantContextConfig()
	acfg.BargeInOnTurnEvent = true
	m.asst = core.NewAssistantContextProcessor(core.NewConversationContext(core.ContextConfig{MaxMessages: 10, MaxTokens: 1000}), acfg)
	pipe, err := core.NewPipeline(&m.stt.PassthroughProcessor, proc, m.asst, m.sink)
	if err != nil {
		t.Fatal(err)
	}
	if m.sess, err = core.NewSession(1, pipe, core.DefaultResourceBudget()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.sess.Subscribe(32, func(e core.DebugEvent) {
		if e.Name == DebugUserMuted || e.Name == DebugUserUnmuted {
			m.debugs <- e
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.sess.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.sess.Close() })
	return m
}

func (m *muteHarness) botStops() {
	_ = m.sess.Reenter(&core.SpeakingStateFrame{State: core.SpeakingStopped, Turn: "bot-1", Gen: 1}, core.Upstream, m.sess.TailIndex(), 0)
	m.settle()
}

func (m *muteHarness) candidates() (n int) {
	for _, f := range m.sink.GetFrames() {
		if f.Type() == core.FrameTranscriptCandidate {
			n++
		}
	}
	return
}

// speak sends one VAD segment with a final transcript; seq ids are seg/seg+1.
func (m *muteHarness) speak(seg uint64, id, text string) {
	_ = m.sess.Post(&core.UserStartedSpeakingFrame{Seq: seg, Gen: 1})
	_ = m.sess.Post(&core.TranscriptCandidateFrame{Turn: core.TurnID(id), Gen: 1, Text: text, Final: true})
	m.run(300*time.Millisecond, func() bool { return false }) // long enough to pass the start debounce
	_ = m.sess.Post(&core.UserStoppedSpeakingFrame{Seq: seg + 1, Gen: 1})
	m.settle()
}

func (m *muteHarness) debugNames() (out []string) {
	for {
		select {
		case e := <-m.debugs:
			out = append(out, e.Name+":"+e.Attributes()["reason"].(string))
		default:
			return
		}
	}
}

func mustMute(t *testing.T, names []string, ta *ToolActivity) []UserMuteStrategy {
	t.Helper()
	s, err := ParseMuteStrategies(names, ta)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestMuteGreetingNotInterruptibleUntilComplete(t *testing.T) {
	m := newMuteHarness(t, DefaultConfig(), mustMute(t, []string{MuteUntilFirstBot}, nil), 0)
	_ = m.sess.Reenter(&core.SpeakingStateFrame{State: core.SpeakingStarted, Turn: "bot-1", Gen: 1}, core.Upstream, m.sess.TailIndex(), 0)
	m.settle() // greeting audible

	m.speak(1, "turn-1-1", "hey stop")
	m.run(time.Second, func() bool { return false })
	if m.asst.InterruptionCount.Load() != 0 || len(m.events(core.TurnEventStarted)) != 0 || m.candidates() != 0 {
		t.Fatal("user barged in on the greeting")
	}
	if m.count(core.FrameUserStartedSpeaking) != 1 {
		t.Fatal("raw VAD must still reach STT upstream of the processor")
	}
	if m.drops.Load() != 3 {
		t.Fatalf("dropped %d, want start+candidate+stop", m.drops.Load())
	}

	m.botStops()
	m.speak(3, "turn-1-3", "now I talk")
	if !m.run(3*time.Second, func() bool { return len(m.events(core.TurnEventCompleted)) == 1 }) {
		t.Fatal("user turn after the greeting did not complete")
	}
	if m.candidates() == 0 {
		t.Fatal("post-greeting transcript was dropped")
	}
	got := m.debugNames()
	if len(got) != 2 || got[0] != "user.muted:"+MuteUntilFirstBot || got[1] != "user.unmuted:"+MuteUntilFirstBot {
		t.Fatalf("debug events %v", got)
	}
	if err := m.sess.Close(); err != nil || m.sess.LeakedTasks() != 0 {
		t.Fatalf("teardown err=%v leaked=%d", err, m.sess.LeakedTasks())
	}
}

func TestMuteLateTranscriptAfterUnmuteStaysDropped(t *testing.T) {
	m := newMuteHarness(t, DefaultConfig(), mustMute(t, []string{MuteUntilFirstBot}, nil), 0)
	_ = m.sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	m.settle()
	_ = m.sess.Reenter(&core.SpeakingStateFrame{State: core.SpeakingStarted, Turn: "bot-1", Gen: 1}, core.Upstream, m.sess.TailIndex(), 0)
	m.botStops() // unmute mid-segment
	_ = m.sess.Post(&core.TranscriptCandidateFrame{Turn: "turn-1-1", Gen: 1, Text: "muted words", Final: true})
	_ = m.sess.Post(&core.UserStoppedSpeakingFrame{Seq: 2, Gen: 1})
	m.run(time.Second, func() bool { return false })
	if m.candidates() != 0 || len(m.events(core.TurnEventStarted)) != 0 || m.count(core.FrameUserStoppedSpeaking) != 1 {
		t.Fatal("muted segment leaked after unmute")
	}
}

func TestMuteToolCallMutesUntilResult(t *testing.T) {
	ta := NewToolActivity()
	release := make(chan struct{})
	ex := ta.Wrap(core.ToolExecutorFunc(func(ctx context.Context, c core.ToolCall) (core.ToolResult, error) {
		select {
		case <-release:
			return core.ToolResult{}, nil
		case <-ctx.Done():
			return core.ToolResult{}, ctx.Err()
		}
	}), nil)
	m := newMuteHarness(t, DefaultConfig(), mustMute(t, []string{MuteFunctionCall}, ta), 0)
	done := make(chan struct{})
	go func() { _, _ = ex.Execute(context.Background(), core.ToolCall{ID: "c1", Name: "x"}); close(done) }()
	for !ta.Active() {
		time.Sleep(time.Millisecond)
	}
	m.speak(1, "turn-1-1", "during tool")
	m.run(time.Second, func() bool { return false })
	if len(m.events(core.TurnEventStarted)) != 0 || m.candidates() != 0 {
		t.Fatal("user input accepted while a tool was in flight")
	}
	close(release)
	<-done
	if ta.Active() {
		t.Fatal("result did not clear the tool")
	}
	m.speak(3, "turn-1-3", "after tool")
	if !m.run(3*time.Second, func() bool { return len(m.events(core.TurnEventCompleted)) == 1 }) {
		t.Fatal("no turn after the tool result")
	}
}

func TestToolActivityCancelErrorAndAsyncCannotStick(t *testing.T) {
	ta := NewToolActivity()
	ctx, cancel := context.WithCancel(context.Background())
	ex := ta.Wrap(core.ToolExecutorFunc(func(ctx context.Context, c core.ToolCall) (core.ToolResult, error) {
		<-ctx.Done()
		return core.ToolResult{}, ctx.Err() // stale-generation cancel
	}), []core.ToolDefinition{{Name: "bg", Policy: core.ToolPolicy{Async: true}}})
	go func() { _, _ = ex.Execute(ctx, core.ToolCall{ID: "a", Name: "fg"}) }()
	go func() { _, _ = ex.Execute(ctx, core.ToolCall{ID: "b", Name: "bg"}) }()
	for deadline := time.Now().Add(time.Second); !ta.Active() && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	ta.mu.Lock()
	n := len(ta.n)
	ta.mu.Unlock()
	if n != 1 {
		t.Fatalf("tracked %d calls; the async one must not mute", n)
	}
	cancel()
	for deadline := time.Now().Add(time.Second); ta.Active() && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	if ta.Active() {
		t.Fatal("cancelled call left the mute stuck")
	}
}

func TestMuteMaxSafetyTimeout(t *testing.T) {
	ta := NewToolActivity()
	ta.mu.Lock()
	ta.n["stuck"] = 1 // a call that never returns
	ta.mu.Unlock()
	m := newMuteHarness(t, DefaultConfig(), mustMute(t, []string{MuteFunctionCall}, ta), 5*time.Second)
	m.speak(1, "turn-1-1", "too early")
	if m.candidates() != 0 {
		t.Fatal("not muted")
	}
	m.clk.Advance(6 * time.Second)
	m.speak(3, "turn-1-3", "after cap")
	if !m.run(3*time.Second, func() bool { return len(m.events(core.TurnEventCompleted)) == 1 }) {
		t.Fatal("safety timeout did not release the mute")
	}
	got := m.debugNames()
	if len(got) != 2 || got[1] != "user.unmuted:"+MuteFunctionCall {
		t.Fatalf("debug events %v", got)
	}
}

func TestMuteAdmittedTurnRunsToCompletionPendingIsAbandoned(t *testing.T) {
	// always: bot speaking mutes. An admitted turn must finish when the bot
	// starts mid-turn.
	m := newMuteHarness(t, DefaultConfig(), mustMute(t, []string{MuteAlways}, nil), 0)
	_ = m.sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	_ = m.sess.Post(&core.TranscriptCandidateFrame{Turn: "turn-1-1", Gen: 1, Text: "hello there", Final: true})
	if !m.run(time.Second, func() bool { return len(m.events(core.TurnEventStarted)) == 1 }) {
		t.Fatal("turn not admitted")
	}
	m.botSpeaks() // mute begins mid-turn
	_ = m.sess.Post(&core.UserStoppedSpeakingFrame{Seq: 2, Gen: 1})
	if !m.run(3*time.Second, func() bool { return len(m.events(core.TurnEventCompleted)) == 1 }) {
		t.Fatal("admitted turn was cut by the mute")
	}

	// A pending (never admitted) turn is abandoned and its transcript retired.
	cfg := DefaultConfig()
	cfg.StartStrategies = []UserTurnStartStrategy{MinWordsStart(3, true)}
	ta := NewToolActivity()
	p := newMuteHarness(t, cfg, mustMute(t, []string{MuteFunctionCall}, ta), 0)
	p.botSpeaks() // bot audible: 3 words needed to barge in
	_ = p.sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	_ = p.sess.Post(&core.TranscriptCandidateFrame{Turn: "turn-1-1", Gen: 1, Text: "uh"})
	p.settle()
	ta.mu.Lock()
	ta.n["c"] = 1 // a tool call starts mid-pending-turn
	ta.mu.Unlock()
	_ = p.sess.Post(&core.TranscriptCandidateFrame{Turn: "turn-1-1", Gen: 1, Text: "uh huh yes sure"})
	_ = p.sess.Post(&core.UserStoppedSpeakingFrame{Seq: 2, Gen: 1})
	p.run(time.Second, func() bool { return false })
	if len(p.events(core.TurnEventStarted)) != 0 || p.asst.InterruptionCount.Load() != 0 {
		t.Fatal("pending turn started under mute")
	}
	inval := false
	for _, f := range p.sink.GetFrames() {
		inval = inval || f.Type() == core.FrameTranscriptInvalidation
	}
	if !inval || p.proc.open || p.proc.vadSpeaking {
		t.Fatalf("pending turn not retired: inval=%v open=%v", inval, p.proc.open)
	}
	stops := 0
	for _, f := range p.sink.GetFrames() {
		if f.Type() == core.FrameUserStoppedSpeaking {
			stops++
		}
	}
	if stops != 1 {
		t.Fatal("the abandoned segment's start went downstream, so its stop must too")
	}
	n := p.candidates()
	ta.mu.Lock()
	delete(ta.n, "c") // unmute; a late final of the abandoned segment must stay dead
	ta.mu.Unlock()
	_ = p.sess.Post(&core.TranscriptCandidateFrame{Turn: "turn-1-1", Gen: 1, Text: "uh huh yes sure thing", Final: true})
	p.run(time.Second, func() bool { return false })
	if p.candidates() != n {
		t.Fatal("abandoned segment's late transcript leaked")
	}
}

func TestParseMuteStrategiesRejectsUnknown(t *testing.T) {
	if _, err := ParseMuteStrategies([]string{"nope"}, nil); err == nil {
		t.Fatal("unknown name accepted")
	}
	if s, err := ParseMuteStrategies([]string{MuteFunctionCall, MuteAlways}, nil); err != nil || len(s) != 2 || s[0].muted() {
		t.Fatal("nil tools must never mute")
	}
}

func TestParseMuteStrategiesRejectsCriticalOutput(t *testing.T) {
	_, err := ParseMuteStrategies([]string{"critical_output"}, nil)
	want := "turn.mute: critical_output was removed (nothing produces uninterruptible speech); remove it"
	if err == nil || err.Error() != want {
		t.Fatalf("critical_output: %v, want %q", err, want)
	}
	if _, err := ParseMuteStrategies([]string{"nope"}, nil); err == nil || strings.Contains(err.Error(), "removed") ||
		!strings.Contains(err.Error(), "always, first_speech, until_first_bot_complete, function_call") {
		t.Fatalf("other unknown names keep the generic message listing the four: %v", err)
	}
}

type flagMute struct{ on bool }

func (*flagMute) name() string  { return "test" }
func (*flagMute) bot(bool)      {}
func (f *flagMute) muted() bool { return f.on }

type recEmitter struct{ out []core.Frame }

func (r *recEmitter) Push(f core.Frame) error { r.out = append(r.out, f); return nil }
func (r *recEmitter) PushDirection(f core.Frame, d core.Direction) error {
	if d == core.Downstream {
		r.out = append(r.out, f)
	}
	return nil
}

// Random interleavings of VAD, transcripts, mute toggles, ticks and the safety
// cap: downstream VAD stays paired, no turn starts while muted, and no
// transcript of a segment whose start was dropped ever leaves the processor.
func TestMuteRandomInterleavingsKeepInvariants(t *testing.T) {
	for seed := int64(0); seed < 300; seed++ {
		rng := rand.New(rand.NewSource(seed))
		clk := &fakeClock{now: time.Unix(1000, 0)}
		fm := &flagMute{}
		p, err := NewProcessor(DefaultConfig(), ProcessorOptions{Clock: clk, Mute: []UserMuteStrategy{fm}, MaxMute: time.Duration(1+rng.Intn(4)) * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		e := &recEmitter{}
		var seq uint64
		speaking, inDown := false, false
		dropped := map[string]bool{}
		send := func(f core.Frame) {
			n := len(e.out)
			if err := p.ProcessFrame(context.Background(), f, core.Downstream, e); err != nil {
				t.Fatal(err)
			}
			for _, o := range e.out[n:] {
				switch v := o.(type) {
				case *core.UserStartedSpeakingFrame:
					if inDown {
						t.Fatalf("seed %d: two downstream starts", seed)
					}
					inDown = true
				case *core.UserStoppedSpeakingFrame:
					if !inDown {
						t.Fatalf("seed %d: unpaired downstream stop", seed)
					}
					inDown = false
				case *core.TurnEventFrame:
					if v.Kind == core.TurnEventStarted && p.muted() {
						t.Fatalf("seed %d: turn started while muted", seed)
					}
				case *core.TranscriptCandidateFrame:
					for w := range dropped {
						if strings.Contains(v.Text, w) {
							t.Fatalf("seed %d: muted segment %s leaked: %q", seed, w, v.Text)
						}
					}
				}
			}
		}
		for i := 0; i < 200; i++ {
			switch rng.Intn(6) {
			case 0:
				if !speaking {
					seq += 2
					speaking = true
					n := len(e.out)
					send(&core.UserStartedSpeakingFrame{Seq: seq, Gen: 1})
					if len(e.out) == n {
						dropped[fmt.Sprintf("<%d>", seq)] = true
					}
				} else {
					speaking = false
					send(&core.UserStoppedSpeakingFrame{Seq: seq + 1, Gen: 1})
				}
			case 1, 2:
				if seq > 0 {
					s := seq - uint64(2*rng.Intn(2)) // current or a late one for the previous segment
					send(&core.TranscriptCandidateFrame{Turn: core.TurnID(fmt.Sprintf("turn-1-%d", s)), Gen: 1,
						Text: fmt.Sprintf("<%d> words here", s), Final: rng.Intn(2) == 0})
				}
			case 3:
				fm.on = !fm.on
				send(tickFrame{})
			case 4:
				clk.Advance(time.Duration(rng.Intn(1500)) * time.Millisecond)
				send(tickFrame{})
			case 5:
				send(tickFrame{})
			}
		}
	}
}
