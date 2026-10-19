package core_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
)

type fakeIdleClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeIdleTimer
}

type fakeIdleTimer struct {
	clk  *fakeIdleClock
	c    chan time.Time
	at   time.Time
	done bool // fired or stopped
}

func (t *fakeIdleTimer) C() <-chan time.Time { return t.c }
func (t *fakeIdleTimer) Stop() bool {
	t.clk.mu.Lock()
	defer t.clk.mu.Unlock()
	was := !t.done
	t.done = true
	return was
}

func (c *fakeIdleClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }

func (c *fakeIdleClock) TimerAt(at time.Time) core.IdleTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeIdleTimer{clk: c, c: make(chan time.Time, 1), at: at}
	if !at.After(c.now) {
		t.done = true
		t.c <- c.now
	}
	c.timers = append(c.timers, t)
	return t
}

func (c *fakeIdleClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	for _, t := range c.timers {
		if !t.done && !t.at.After(c.now) {
			t.done = true
			t.c <- c.now
		}
	}
}

// parkedAt waits until the controller sleeps on a timer due at exactly at.
func (c *fakeIdleClock) parkedAt(t *testing.T, at time.Time) {
	t.Helper()
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(time.Millisecond) {
		c.mu.Lock()
		for _, tm := range c.timers {
			if !tm.done && tm.at.Equal(at) {
				c.mu.Unlock()
				return
			}
		}
		c.mu.Unlock()
	}
	t.Fatalf("controller never parked a timer at %v", at)
}

// unparked waits until the controller sleeps with no timer (no deadline left).
func (c *fakeIdleClock) unparked(t *testing.T) {
	t.Helper()
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(time.Millisecond) {
		c.mu.Lock()
		live := 0
		for _, tm := range c.timers {
			if !tm.done {
				live++
			}
		}
		c.mu.Unlock()
		if live == 0 {
			return
		}
	}
	t.Fatal("controller still holds a timer")
}

// stageRec records what reaches its position.
type stageRec struct {
	mu     sync.Mutex
	frames []core.Frame
	keep   func(core.Frame) bool
}

func (r *stageRec) ProcessFrame(_ context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	if r.keep(f) {
		r.mu.Lock()
		r.frames = append(r.frames, f)
		r.mu.Unlock()
	}
	return e.PushDirection(f, dir)
}

func only[T core.Frame](extra ...func(core.Frame) bool) func(core.Frame) bool {
	return func(f core.Frame) bool {
		if _, ok := f.(T); ok {
			return true
		}
		for _, x := range extra {
			if x(f) {
				return true
			}
		}
		return false
	}
}

func isInterrupt(f core.Frame) bool { _, ok := f.(*core.InterruptionFrame); return ok }

func (r *stageRec) texts() (out []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, f := range r.frames {
		switch v := f.(type) {
		case *core.SynthesisUnitFrame:
			out = append(out, "say:"+v.Text)
		case *core.UserMessageFrame:
			out = append(out, "user:"+v.Turn)
		case *core.InterruptionFrame:
			out = append(out, "interrupt")
		case *core.ErrorFrame:
			out = append(out, "error")
		}
	}
	return out
}

func (r *stageRec) has(want string) bool {
	for _, s := range r.texts() {
		if s == want {
			return true
		}
	}
	return false
}

type idleRig struct {
	t         *testing.T
	ctl       *core.IdleController
	clk       *fakeIdleClock
	llm, tts  *stageRec
	tail      *stageRec
	sess      *core.Session
	hist      *core.ConversationContext
	ended     chan string
	events    chan core.DebugEvent
	toolsBusy atomic.Bool
	muted     atomic.Bool
}

func newIdleRig(t *testing.T, mod func(*core.IdleConfig)) *idleRig {
	t.Helper()
	r := &idleRig{t: t, clk: &fakeIdleClock{now: time.Unix(1000, 0)}, llm: &stageRec{keep: only[*core.UserMessageFrame](isInterrupt)}, tts: &stageRec{keep: only[*core.SynthesisUnitFrame]()}, tail: &stageRec{keep: only[*core.ErrorFrame]()},
		ended: make(chan string, 4), events: make(chan core.DebugEvent, 64), hist: core.NewConversationContext(core.ContextConfig{})}
	cfg := core.IdleConfig{
		UserTimeout: 15 * time.Second, MaxPrompts: 2, Say: true, UserPrompt: "are you still there?",
		History: r.hist, Clock: r.clk,
		ToolsBusy: r.toolsBusy.Load, Muted: r.muted.Load,
		OnEnd: func(reason string) { r.ended <- reason },
		// the stand-in for AssistantContextProcessor.Say: the line enters at the TTS recorder
		Speak: func(text string) error {
			return r.sess.Reenter(&core.SynthesisUnitFrame{Turn: "say-x", Text: text, IsFinal: true}, core.Downstream, 2, 0)
		},
	}
	if mod != nil {
		mod(&cfg)
	}
	var err error
	if r.ctl, err = core.NewIdleController(cfg); err != nil {
		t.Fatal(err)
	}
	pipe, err := core.NewPipeline(r.ctl, r.llm, r.tts, r.tail)
	if err != nil {
		t.Fatal(err)
	}
	if r.sess, err = core.NewSession(1, pipe, core.DefaultResourceBudget()); err != nil {
		t.Fatal(err)
	}
	if _, _, err = r.sess.Subscribe(64, func(e core.DebugEvent) { r.events <- e }); err != nil {
		t.Fatal(err)
	}
	if err = r.sess.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = r.sess.Close()
		if n := r.sess.LeakedTasks(); n != 0 {
			t.Errorf("leaked %d tasks", n)
		}
	})
	return r
}

func (r *idleRig) down(f core.Frame) {
	r.t.Helper()
	if err := r.sess.Reenter(f, core.Downstream, 0, 0); err != nil {
		r.t.Fatal(err)
	}
}

func (r *idleRig) up(f core.Frame) {
	r.t.Helper()
	if err := r.sess.Reenter(f, core.Upstream, r.sess.TailIndex(), 0); err != nil {
		r.t.Fatal(err)
	}
}

func (r *idleRig) botSpoke() {
	r.up(&core.SpeakingStateFrame{State: core.SpeakingStarted})
	r.up(&core.SpeakingStateFrame{State: core.SpeakingStopped, Reason: core.SpeakingStopDrained})
}

func (r *idleRig) at(d time.Duration) time.Time { return time.Unix(1000, 0).Add(d) }

func (r *idleRig) wait(want string, rec *stageRec) {
	r.t.Helper()
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(time.Millisecond) {
		if rec.has(want) {
			return
		}
	}
	r.t.Fatalf("never saw %q; got %v", want, rec.texts())
}

// quiet waits for exactly n recorded frames and checks no more follow.
func (r *idleRig) quiet(rec *stageRec, n int) {
	r.t.Helper()
	for end := time.Now().Add(3 * time.Second); len(rec.texts()) < n && time.Now().Before(end); time.Sleep(time.Millisecond) {
	}
	time.Sleep(30 * time.Millisecond)
	if got := len(rec.texts()); got != n {
		r.t.Fatalf("frames = %v, want %d", rec.texts(), n)
	}
}

func (r *idleRig) endedWith(want string) {
	r.t.Helper()
	select {
	case got := <-r.ended:
		if got != want {
			r.t.Fatalf("ended with %q, want %q", got, want)
		}
	case <-time.After(3 * time.Second):
		r.t.Fatalf("call did not end (want %q)", want)
	}
}

func (r *idleRig) noEnd() {
	r.t.Helper()
	select {
	case got := <-r.ended:
		r.t.Fatalf("call ended: %q", got)
	case <-time.After(30 * time.Millisecond):
	}
}

func (r *idleRig) eventNames() (out []string) {
	for {
		select {
		case e := <-r.events:
			if strings.HasPrefix(e.Name, "idle.") {
				out = append(out, e.Name+"#"+strconv.Itoa(e.Attributes()["count"].(int)))
			}
		default:
			return
		}
	}
}

// prompt -> prompt -> goodbye -> hang-up, with exact timings.
func TestIdleUserPromptPromptHangup(t *testing.T) {
	r := newIdleRig(t, func(c *core.IdleConfig) { c.Goodbye = "bye" })
	r.clk.parkedAt(t, r.at(15*time.Second)) // armed at call start

	r.clk.Advance(14*time.Second + 900*time.Millisecond)
	r.quiet(r.tts, 0)
	r.clk.Advance(100 * time.Millisecond) // t=15s
	r.wait("say:are you still there?", r.tts)
	r.clk.parkedAt(t, r.at(30*time.Second))

	r.clk.Advance(15*time.Second - time.Nanosecond)
	r.quiet(r.tts, 1)
	r.clk.Advance(time.Nanosecond) // t=30s
	r.quiet(r.tts, 2)
	r.clk.parkedAt(t, r.at(45*time.Second))

	r.clk.Advance(15 * time.Second) // t=45s: out of prompts
	r.wait("say:bye", r.tts)
	r.noEnd() // waits for the goodbye to play
	r.up(&core.SpeakingStateFrame{State: core.SpeakingStarted})
	r.noEnd()
	r.up(&core.SpeakingStateFrame{State: core.SpeakingStopped, Reason: core.SpeakingStopDrained})
	r.endedWith(core.IdleReasonUser)

	if got, want := r.eventNames(), []string{"idle.user_prompt#1", "idle.user_prompt#2", "idle.user_timeout#2"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if r.ctl.UserPrompts.Load() != 2 || r.ctl.UserHangups.Load() != 1 {
		t.Fatalf("counters: prompts=%d hangups=%d", r.ctl.UserPrompts.Load(), r.ctl.UserHangups.Load())
	}
}

func TestIdleHangupWithoutGoodbyeIsImmediate(t *testing.T) {
	r := newIdleRig(t, nil)
	for i := 1; i <= 2; i++ {
		r.clk.parkedAt(t, r.at(time.Duration(i)*15*time.Second))
		r.clk.Advance(15 * time.Second)
		r.quiet(r.tts, i)
	}
	r.clk.parkedAt(t, r.at(45*time.Second))
	r.clk.Advance(15 * time.Second)
	r.endedWith("user_idle")
	r.quiet(r.tts, 2) // no goodbye configured
}

func TestIdleGoodbyeWaitIsBounded(t *testing.T) {
	r := newIdleRig(t, func(c *core.IdleConfig) { c.Goodbye = "bye"; c.MaxPrompts = 1; c.GoodbyeWait = 5 * time.Second })
	r.clk.parkedAt(t, r.at(15*time.Second)) // armed at call start
	r.clk.Advance(15 * time.Second)
	r.clk.parkedAt(t, r.at(30*time.Second))
	r.clk.Advance(15 * time.Second)
	r.wait("say:bye", r.tts)
	r.clk.parkedAt(t, r.at(35*time.Second)) // the speech never starts: cap
	r.clk.Advance(5*time.Second - time.Nanosecond)
	r.noEnd()
	r.clk.Advance(time.Nanosecond)
	r.endedWith("user_idle")
}

func TestIdleUserActivityResets(t *testing.T) {
	for name, input := range map[string]func(*idleRig){
		"vad": func(r *idleRig) {
			r.down(&core.UserStartedSpeakingFrame{})
			r.down(&core.UserStoppedSpeakingFrame{})
		},
		"dtmf": func(r *idleRig) { r.down(&core.InputDTMFFrame{Digit: "5"}) },
		"turn": func(r *idleRig) {
			r.down(&core.TurnEventFrame{Kind: core.TurnEventStarted})
			r.down(&core.TurnEventFrame{Kind: core.TurnEventCompleted})
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newIdleRig(t, nil)
			r.clk.parkedAt(t, r.at(15*time.Second))
			r.clk.Advance(15 * time.Second)
			r.wait("say:are you still there?", r.tts)
			r.clk.parkedAt(t, r.at(30*time.Second))

			r.clk.Advance(5 * time.Second) // t=20s: the user does something
			input(r)
			r.clk.parkedAt(t, r.at(35*time.Second)) // clock restarted from the input, count back to 0
			r.clk.Advance(14 * time.Second)
			r.quiet(r.tts, 1)
			r.clk.Advance(time.Second)
			r.quiet(r.tts, 2)
			r.clk.parkedAt(t, r.at(50*time.Second))
			r.clk.Advance(15 * time.Second)
			r.quiet(r.tts, 3) // second prompt of the new episode, so not hung up yet
			r.noEnd()
			if r.ctl.UserHangups.Load() != 0 {
				t.Fatal("hung up after activity reset the count")
			}
		})
	}
}

func TestIdleUserTurnAndBotSpeechPauseTheClock(t *testing.T) {
	r := newIdleRig(t, func(c *core.IdleConfig) { c.BotTimeout = time.Minute })
	r.clk.parkedAt(t, r.at(15*time.Second)) // armed at call start
	// A user turn completes: the bot is now the one expected to speak.
	r.down(&core.UserMessageFrame{Turn: "t1", Content: "hi"})
	r.clk.parkedAt(t, r.at(time.Minute)) // only the bot watchdog remains
	r.up(&core.SpeakingStateFrame{State: core.SpeakingStarted})
	r.clk.unparked(t)              // seen: no deadline is left
	r.clk.Advance(2 * time.Minute) // bot speaking: never a prompt or a stuck report
	r.quiet(r.tts, 0)
	r.quiet(r.tail, 0)
	r.up(&core.SpeakingStateFrame{State: core.SpeakingStopped})
	r.clk.parkedAt(t, r.at(2*time.Minute+15*time.Second))
}

func TestIdleToolInFlightAndMutePauseTheClock(t *testing.T) {
	r := newIdleRig(t, nil)
	r.toolsBusy.Store(true)
	r.clk.parkedAt(t, r.at(15*time.Second)) // armed at call start
	r.clk.Advance(15 * time.Second)
	r.clk.parkedAt(t, r.at(30*time.Second)) // pushed out, no prompt
	r.quiet(r.tts, 0)
	r.toolsBusy.Store(false)
	r.muted.Store(true)
	r.clk.Advance(15 * time.Second)
	r.clk.parkedAt(t, r.at(45*time.Second))
	r.quiet(r.tts, 0)
	r.muted.Store(false)
	r.clk.Advance(15 * time.Second)
	r.wait("say:are you still there?", r.tts)
	if r.ctl.UserPrompts.Load() != 1 {
		t.Fatalf("prompts = %d", r.ctl.UserPrompts.Load())
	}
}

func TestIdleLLMNudgeIsLabelledAndStartsATurn(t *testing.T) {
	r := newIdleRig(t, func(c *core.IdleConfig) { c.Say = false })
	r.clk.parkedAt(t, r.at(15*time.Second)) // armed at call start
	r.clk.Advance(15 * time.Second)
	r.wait("user:idle-1", r.llm)
	msgs := r.hist.Messages()
	if len(msgs) != 1 || !strings.Contains(msgs[0].Content, "not spoken by the user") || !strings.Contains(msgs[0].Content, "are you still there?") {
		t.Fatalf("history = %+v", msgs)
	}
	if len(r.tts.texts()) != 0 {
		t.Fatalf("spoke directly in nudge mode: %v", r.tts.texts())
	}
}

func TestIdleBotStuckCancelsAndNextTurnWorks(t *testing.T) {
	r := newIdleRig(t, func(c *core.IdleConfig) { c.BotTimeout = 30 * time.Second; c.BotFallback = "sorry" })
	r.down(&core.UserMessageFrame{Turn: "t1", Content: "hi"})
	r.clk.parkedAt(t, r.at(30*time.Second))
	r.clk.Advance(30*time.Second - time.Nanosecond)
	r.quiet(r.tail, 0)
	r.clk.Advance(time.Nanosecond)
	r.wait("error", r.tail)
	r.wait("interrupt", r.llm)
	r.quiet(r.tts, 0) // held until the assistant's own interruption comes back (its generation bump would fence it)
	r.up(&core.InterruptionFrame{})
	r.wait("say:sorry", r.tts)
	if r.ctl.BotStuck.Load() != 1 || r.ctl.BotHangups.Load() != 0 {
		t.Fatalf("counters: stuck=%d hangups=%d", r.ctl.BotStuck.Load(), r.ctl.BotHangups.Load())
	}
	r.noEnd()
	// The user clock took over (after cancel), and the next turn is watched again.
	r.clk.parkedAt(t, r.at(45*time.Second))
	r.down(&core.UserMessageFrame{Turn: "t2", Content: "hello?"})
	r.clk.parkedAt(t, r.at(60*time.Second))
	r.botSpoke() // answered this time
	if got := r.eventNames(); len(got) != 1 || got[0] != "idle.bot_stuck#1" {
		t.Fatalf("events = %v", got)
	}
	r.noEnd()
}

func TestIdleBotStuckRepeatedEndsTheCall(t *testing.T) {
	r := newIdleRig(t, func(c *core.IdleConfig) {
		c.BotTimeout = 10 * time.Second
		c.BotMaxStuck = 2
		c.UserTimeout = 0
		c.MaxPrompts = 0
	})
	for i := 1; i <= 2; i++ {
		r.down(&core.UserMessageFrame{Turn: "t", Content: "hi"})
		r.clk.parkedAt(t, r.clk.Now().Add(10*time.Second))
		if i == 2 {
			r.noEnd()
		}
		r.clk.Advance(10 * time.Second)
		for end := time.Now().Add(3 * time.Second); r.ctl.BotStuck.Load() != uint64(i) && time.Now().Before(end); time.Sleep(time.Millisecond) {
		}
	}
	r.endedWith(core.IdleReasonBot)
	if r.ctl.BotHangups.Load() != 1 {
		t.Fatal("hangup not counted")
	}
}

func TestIdleBotWatchdogWaitsForTools(t *testing.T) {
	r := newIdleRig(t, func(c *core.IdleConfig) { c.BotTimeout = 10 * time.Second; c.UserTimeout = 0; c.MaxPrompts = 0 })
	r.toolsBusy.Store(true)
	r.down(&core.UserMessageFrame{Turn: "t1", Content: "hi"})
	r.clk.parkedAt(t, r.at(10*time.Second))
	r.clk.Advance(10 * time.Second)
	r.clk.parkedAt(t, r.at(20*time.Second))
	r.quiet(r.tail, 0)
	r.toolsBusy.Store(false)
	r.clk.Advance(10 * time.Second)
	r.wait("error", r.tail)
}

func TestIdleStrikesResetWhenBotSpeaks(t *testing.T) {
	r := newIdleRig(t, func(c *core.IdleConfig) {
		c.BotTimeout = 10 * time.Second
		c.BotMaxStuck = 2
		c.UserTimeout = 0
		c.MaxPrompts = 0
	})
	for i := 0; i < 3; i++ { // stuck, answered, stuck, answered ...: never two in a row
		r.down(&core.UserMessageFrame{Turn: "t", Content: "hi"})
		r.clk.parkedAt(t, r.clk.Now().Add(10*time.Second))
		r.clk.Advance(10 * time.Second)
		for end := time.Now().Add(3 * time.Second); r.ctl.BotStuck.Load() != uint64(i+1) && time.Now().Before(end); time.Sleep(time.Millisecond) {
		}
		if r.ctl.BotStuck.Load() != uint64(i+1) {
			t.Fatalf("stuck count = %d, want %d", r.ctl.BotStuck.Load(), i+1)
		}
		r.down(&core.UserMessageFrame{Turn: "t", Content: "hi"})
		r.clk.parkedAt(t, r.clk.Now().Add(10*time.Second)) // watched
		r.botSpoke()
		r.clk.unparked(t) // answered: no deadline left
		r.clk.Advance(time.Second)
	}
	r.noEnd()
}

func TestIdleConfigValidation(t *testing.T) {
	for name, cfg := range map[string]core.IdleConfig{
		"neg user":  {UserTimeout: -1},
		"neg bot":   {BotTimeout: -1},
		"neg stuck": {BotMaxStuck: -1},
		"no prompt": {UserTimeout: time.Second, MaxPrompts: 0},
	} {
		if _, err := core.NewIdleController(cfg); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := core.NewIdleController(core.IdleConfig{BotTimeout: time.Second}); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(core.ErrBotIdle, context.DeadlineExceeded) {
		t.Fatal("bot idle error should classify as a timeout for the client")
	}
}

// Teardown with a running clock leaves no task behind, whatever state it is in.
func TestIdleTeardownWithPendingDeadlines(t *testing.T) {
	r := newIdleRig(t, func(c *core.IdleConfig) { c.BotTimeout = time.Minute })
	r.clk.parkedAt(t, r.at(15*time.Second)) // armed at call start
	r.down(&core.UserMessageFrame{Turn: "t1", Content: "hi"})
	r.clk.parkedAt(t, r.at(time.Minute))
	if err := r.sess.Close(); err != nil {
		t.Fatal(err)
	}
	if n := r.sess.LeakedTasks(); n != 0 {
		t.Fatalf("leaked %d tasks", n)
	}
}

// No frame at all (a caller who connects and stays silent): the clock runs
// from the start of the call.
func TestIdleClockRunsFromCallStart(t *testing.T) {
	r := newIdleRig(t, func(c *core.IdleConfig) { c.MaxPrompts = 1 })
	r.clk.parkedAt(t, r.at(15*time.Second))
	r.clk.Advance(15 * time.Second)
	r.wait("say:are you still there?", r.tts)
	r.clk.parkedAt(t, r.at(30*time.Second))
	r.clk.Advance(15 * time.Second)
	r.endedWith(core.IdleReasonUser)
}

// A user turn that commits no message (filtered backchannel) must not stop
// the clock for good.
func TestIdleTurnWithoutMessageKeepsTheClock(t *testing.T) {
	r := newIdleRig(t, nil)
	r.clk.Advance(5 * time.Second)
	r.down(&core.TurnEventFrame{Kind: core.TurnEventStarted})
	r.down(&core.UserStartedSpeakingFrame{})
	r.down(&core.UserStoppedSpeakingFrame{})
	r.down(&core.TurnEventFrame{Kind: core.TurnEventCompleted})
	r.clk.parkedAt(t, r.at(20*time.Second))
	r.clk.Advance(15 * time.Second)
	r.wait("say:are you still there?", r.tts)
}

// A reply that finishes without speech (tool-only) is not a stuck bot; while a
// reply is in progress the user is not prompted.
func TestIdleSilentReplyIsNotStuck(t *testing.T) {
	var answering atomic.Bool
	r := newIdleRig(t, func(c *core.IdleConfig) { c.BotTimeout = 10 * time.Second; c.Answering = answering.Load })
	answering.Store(true)
	r.down(&core.UserMessageFrame{Turn: "t1", Content: "hi"})
	r.clk.parkedAt(t, r.at(10*time.Second))
	answering.Store(false) // the model answered with a tool call only
	r.clk.Advance(10 * time.Second)
	r.clk.parkedAt(t, r.at(25*time.Second)) // the user clock took over
	r.quiet(r.tail, 0)
	if r.ctl.BotStuck.Load() != 0 {
		t.Fatal("silent reply counted as stuck")
	}
	answering.Store(true) // a reply is generating: no prompt yet
	r.clk.Advance(15 * time.Second)
	r.clk.parkedAt(t, r.at(40*time.Second))
	r.quiet(r.tts, 0)
	answering.Store(false)
	r.clk.Advance(15 * time.Second)
	r.wait("say:are you still there?", r.tts)
}

// RequestEnd with the controller otherwise idle (no user or bot timers): the call
// ends when the bot's next speech finishes, and not before.
func TestIdleRequestEndWaitsForTheSpeech(t *testing.T) {
	r := newIdleRig(t, func(c *core.IdleConfig) { c.UserTimeout, c.MaxPrompts = 0, 0 })
	if !r.ctl.RequestEnd("ended_by_flow", 10*time.Second) {
		t.Fatal("first RequestEnd must be accepted")
	}
	r.noEnd()
	r.up(&core.SpeakingStateFrame{State: core.SpeakingStarted})
	r.noEnd()
	r.up(&core.SpeakingStateFrame{State: core.SpeakingStopped, Reason: core.SpeakingStopDrained})
	r.endedWith("ended_by_flow")
}

// Speech already in progress when end is requested: its stop ends the call.
func TestIdleRequestEndDuringSpeech(t *testing.T) {
	r := newIdleRig(t, func(c *core.IdleConfig) { c.UserTimeout, c.MaxPrompts = 0, 0 })
	r.up(&core.SpeakingStateFrame{State: core.SpeakingStarted})
	r.ctl.RequestEnd("ended_by_flow", 10*time.Second)
	r.noEnd()
	r.up(&core.SpeakingStateFrame{State: core.SpeakingStopped, Reason: core.SpeakingStopFlushed})
	r.endedWith("ended_by_flow") // an interrupted goodbye still ends the call
}

// The bot never speaks: the wait is the cap.
func TestIdleRequestEndWaitIsBounded(t *testing.T) {
	r := newIdleRig(t, func(c *core.IdleConfig) { c.UserTimeout, c.MaxPrompts = 0, 0 })
	r.ctl.RequestEnd("ended_by_flow", 4*time.Second)
	r.clk.parkedAt(t, r.at(4*time.Second))
	r.clk.Advance(4*time.Second - time.Nanosecond)
	r.noEnd()
	r.clk.Advance(time.Nanosecond)
	r.endedWith("ended_by_flow")
}

// A second request (a model calling end_call twice) neither re-arms nor ends twice.
func TestIdleRequestEndOnce(t *testing.T) {
	r := newIdleRig(t, func(c *core.IdleConfig) { c.UserTimeout, c.MaxPrompts = 0, 0 })
	if !r.ctl.RequestEnd("ended_by_flow", 10*time.Second) || r.ctl.RequestEnd("ended_by_flow", 0) {
		t.Fatal("only the first RequestEnd is accepted")
	}
	r.botSpoke()
	r.endedWith("ended_by_flow")
	r.noEnd() // exactly one hang-up
	if r.ctl.RequestEnd("x", 10*time.Second) {
		t.Fatal("no request after the call ended")
	}
}

// The model's pre-tool sentence plays, the reply is still being generated when it
// stops, and the post-tool goodbye is a second span: the call ends after the second.
func TestIdleRequestEndWaitsForTheReplyAfterTheFirstSpan(t *testing.T) {
	var answering atomic.Bool
	r := newIdleRig(t, func(c *core.IdleConfig) { c.UserTimeout, c.MaxPrompts = 0, 0; c.Answering = answering.Load })
	r.up(&core.SpeakingStateFrame{State: core.SpeakingStarted})
	answering.Store(true)
	r.ctl.RequestEnd("ended_by_flow", 10*time.Second)
	r.up(&core.SpeakingStateFrame{State: core.SpeakingStopped, Reason: core.SpeakingStopDrained})
	r.noEnd()
	r.up(&core.SpeakingStateFrame{State: core.SpeakingStarted})
	answering.Store(false)
	r.noEnd()
	r.up(&core.SpeakingStateFrame{State: core.SpeakingStopped, Reason: core.SpeakingStopDrained})
	r.endedWith("ended_by_flow")
}

// The first span stopped while a reply was generating and the reply is silent: the
// call ends once the reply is done, well before the cap.
func TestIdleRequestEndSilentReplyDoesNotWaitForTheCap(t *testing.T) {
	var answering atomic.Bool
	r := newIdleRig(t, func(c *core.IdleConfig) { c.UserTimeout, c.MaxPrompts = 0, 0; c.Answering = answering.Load })
	r.up(&core.SpeakingStateFrame{State: core.SpeakingStarted})
	answering.Store(true)
	r.ctl.RequestEnd("ended_by_flow", 10*time.Second)
	r.up(&core.SpeakingStateFrame{State: core.SpeakingStopped, Reason: core.SpeakingStopDrained})
	r.noEnd()
	answering.Store(false)
	r.clk.parkedAt(t, r.at(100*time.Millisecond))
	r.clk.Advance(100 * time.Millisecond)
	r.endedWith("ended_by_flow")
}

// A reply that never finishes: the cap still ends the call.
func TestIdleRequestEndStuckReplyHitsTheCap(t *testing.T) {
	r := newIdleRig(t, func(c *core.IdleConfig) {
		c.UserTimeout, c.MaxPrompts = 0, 0
		c.Answering = func() bool { return true }
	})
	r.up(&core.SpeakingStateFrame{State: core.SpeakingStarted})
	r.ctl.RequestEnd("ended_by_flow", 2*time.Second)
	r.up(&core.SpeakingStateFrame{State: core.SpeakingStopped, Reason: core.SpeakingStopDrained})
	r.clk.Advance(2 * time.Second)
	r.endedWith("ended_by_flow")
}

// Pre-tool speech already finished and the reply after the tool is silent: nothing
// ever starts speaking, so the call must end on the second quiet poll, not the cap.
func TestIdleRequestEndSilentReplyAfterFinishedSpeechEndsWithinTwoPolls(t *testing.T) {
	var answering atomic.Bool
	r := newIdleRig(t, func(c *core.IdleConfig) { c.UserTimeout, c.MaxPrompts = 0, 0; c.Answering = answering.Load })
	answering.Store(true) // the model is still finishing the turn that called the tool
	r.ctl.RequestEnd("ended_by_flow", 15*time.Second)
	r.clk.parkedAt(t, r.at(100*time.Millisecond))
	r.clk.Advance(100 * time.Millisecond)
	r.noEnd() // still answering
	answering.Store(false)
	r.clk.parkedAt(t, r.at(200*time.Millisecond))
	r.clk.Advance(100 * time.Millisecond)
	r.noEnd() // one quiet poll is not enough: the follow-up reply may be about to start
	r.clk.parkedAt(t, r.at(300*time.Millisecond))
	r.clk.Advance(100 * time.Millisecond)
	r.endedWith("ended_by_flow")
}

// A reply that begins speaking after the poll window opened is not cut: the goodbye
// plays and the call ends after it.
func TestIdleRequestEndDoesNotCutAReplyThatStartsLater(t *testing.T) {
	var answering atomic.Bool
	r := newIdleRig(t, func(c *core.IdleConfig) { c.UserTimeout, c.MaxPrompts = 0, 0; c.Answering = answering.Load })
	r.ctl.RequestEnd("ended_by_flow", 15*time.Second)
	r.clk.parkedAt(t, r.at(100*time.Millisecond))
	r.clk.Advance(100 * time.Millisecond) // quiet once
	answering.Store(true)                 // the follow-up reply starts generating
	r.clk.parkedAt(t, r.at(200*time.Millisecond))
	r.clk.Advance(100 * time.Millisecond)
	r.noEnd()
	r.up(&core.SpeakingStateFrame{State: core.SpeakingStarted})
	r.clk.Advance(time.Second)
	r.noEnd()
	answering.Store(false)
	r.up(&core.SpeakingStateFrame{State: core.SpeakingStopped, Reason: core.SpeakingStopDrained})
	r.endedWith("ended_by_flow")
}

func TestIdleRequestEndRefusesANonPositiveWait(t *testing.T) {
	r := newIdleRig(t, func(c *core.IdleConfig) { c.UserTimeout, c.MaxPrompts = 0, 0 })
	if r.ctl.RequestEnd("ended_by_flow", 0) || r.ctl.RequestEnd("ended_by_flow", -time.Second) {
		t.Fatal("a non-positive wait must be refused")
	}
	if !r.ctl.RequestEnd("ended_by_flow", time.Second) {
		t.Fatal("a refused request must not consume the end")
	}
}
