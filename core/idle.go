package core

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Idle DebugEvent names. Attributes use the trace allowlist keys: count (the
// prompt number, prompts spent, or stuck turns in a row), duration_ms and reason.
const (
	DebugIdleUserPrompt  = "idle.user_prompt"  // count: 1-based prompt number
	DebugIdleUserTimeout = "idle.user_timeout" // count: prompts spent; the call is ending
	DebugIdleBotStuck    = "idle.bot_stuck"    // count: stuck turns in a row; duration_ms: the watchdog
)

// Reason codes passed to IdleConfig.OnEnd.
const (
	IdleReasonUser = "user_idle"
	IdleReasonBot  = "bot_idle"
)

// IdleTimer and IdleClock make the controller deterministic in tests.
type IdleTimer interface {
	C() <-chan time.Time
	Stop() bool
}

// TimerAt takes an absolute deadline so a fake clock arms it atomically
// against its own now (a duration computed from an earlier Now would drift).
type IdleClock interface {
	Now() time.Time
	TimerAt(time.Time) IdleTimer
}

type realIdleClock struct{}
type realIdleTimer struct{ t *time.Timer }

func (realIdleClock) Now() time.Time { return time.Now() }
func (realIdleClock) TimerAt(t time.Time) IdleTimer {
	return realIdleTimer{time.NewTimer(time.Until(t))}
}
func (t realIdleTimer) C() <-chan time.Time { return t.t.C }
func (t realIdleTimer) Stop() bool          { return t.t.Stop() }

// IdleConfig tunes IdleController. A zero UserTimeout disables user idle and a
// zero BotTimeout disables the bot watchdog; with both zero do not build the
// controller (a disabled call has no extra processor).
type IdleConfig struct {
	// UserTimeout: silence after the bot finishes speaking (or the call starts) before a prompt.
	// Prompts are spaced by it, and the call ends one UserTimeout after the last.
	UserTimeout time.Duration
	// MaxPrompts: prompts before hanging up (>= 1 when UserTimeout > 0).
	MaxPrompts int
	// UserPrompt is the nudge text. By default it is handed to the model as a
	// labelled non-user instruction; with Say it is spoken verbatim through TTS.
	UserPrompt string
	Say        bool
	// Goodbye is spoken (TTS, no model) before hanging up for user idle.
	Goodbye string
	// GoodbyeWait caps the wait for the goodbye to finish playing (default 10s).
	GoodbyeWait time.Duration

	// BotTimeout: a completed user turn with no bot output (speech) after this
	// long is a stuck model/TTS: the generation is cancelled and an error sent.
	BotTimeout time.Duration
	// BotMaxStuck consecutive stuck turns (no bot speech between) end the call
	// (default 3).
	BotMaxStuck int
	// BotFallback is spoken (TTS) after cancelling a stuck generation.
	BotFallback string

	// History receives the nudge message (nil, or Say, speaks UserPrompt instead).
	History *ConversationContext
	// ToolsBusy reports a tool call in flight: neither idle clock runs while it does.
	ToolsBusy func() bool
	// Muted reports user input muted (turn mute strategies): no prompts then.
	Muted func() bool
	// Answering reports a reply generating or playing (assistant context
	// Active). Nil = always. No prompt while it does; a watchdog expiry while
	// it does not is a reply that finished without speech (tool-only), not a stuck bot.
	Answering func() bool
	// OnEnd hangs the call up. It runs on a supervised task and must not wait
	// for Session.Close (start it on its own goroutine).
	OnEnd func(reason string)
	// Clock nil = wall clock.
	Clock IdleClock
}

// Validate rejects negative values and an enabled user idle without prompts.
func (c IdleConfig) Validate() error {
	for name, d := range map[string]time.Duration{"user_timeout": c.UserTimeout, "bot_timeout": c.BotTimeout, "goodbye_wait": c.GoodbyeWait} {
		if d < 0 {
			return fmt.Errorf("idle.%s must be >= 0", name)
		}
	}
	if c.BotMaxStuck < 0 {
		return errors.New("idle.bot_max_stuck must be >= 0")
	}
	if c.UserTimeout > 0 && c.MaxPrompts < 1 {
		return errors.New("idle.max_prompts must be >= 1 when idle.user_timeout is set")
	}
	return nil
}

// ErrBotIdle is the error sent to the client when the bot never answered.
var ErrBotIdle = fmt.Errorf("core: bot produced no output after the user turn: %w", context.DeadlineExceeded)

// IdleController is a pass-through stage placed directly before the LLM
// (before the async trigger). It watches the call for two failures:
//
//   - user idle: the bot finished speaking and the user says nothing. Prompt up
//     to MaxPrompts times, then end the call (Pipecat UserIdleController).
//   - bot idle: a user turn completed and the bot never produced speech (stuck
//     LLM or TTS). Cancel the generation and tell the client.
//
// Frames tell it the call state: VAD/turn/DTMF/committed user turns downstream,
// bot speaking and interruptions upstream. Deadlines live under one mutex; a
// single supervised task (started at Attach) sleeps on the earliest of them.
// Prompts and hang-up are skipped while a tool runs, the user is muted, a
// reply is in progress, or either side speaks.
type IdleController struct {
	cfg     IdleConfig
	clk     IdleClock
	session SessionController
	index   int
	speak   int // pipeline index of TTS for direct speech; -1 = unavailable

	wake chan struct{}

	mu                                  sync.Mutex
	botSpeaking, userSpeaking, turnUser bool
	waitingBot                          bool // a user turn is complete and the bot has not produced speech
	userAt, botAt, endAt                time.Time
	prompts, strikes                    int
	ending, sawStart, done              bool
	sayAfterCancel, sayNow              string // fallback phrase: held until the cancel's interruption is back, then due
	endReason                           string
	sayN, nudgeN                        atomic.Uint64

	UserPrompts atomic.Uint64 // prompts issued
	UserHangups atomic.Uint64 // calls ended for user idle
	BotStuck    atomic.Uint64 // stuck-generation events
	BotHangups  atomic.Uint64 // calls ended for repeated stuck turns
}

// NewIdleController validates cfg and applies defaults.
func NewIdleController(cfg IdleConfig) (*IdleController, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.BotMaxStuck == 0 {
		cfg.BotMaxStuck = 3
	}
	if cfg.GoodbyeWait == 0 {
		cfg.GoodbyeWait = 10 * time.Second
	}
	if cfg.UserPrompt == "" {
		cfg.UserPrompt = "Are you still there?"
	}
	c := &IdleController{cfg: cfg, clk: cfg.Clock, speak: -1, wake: make(chan struct{}, 1)}
	if c.clk == nil {
		c.clk = realIdleClock{}
	}
	return c, nil
}

// SetSpeakIndex sets the pipeline index of the TTS stage; without it prompts
// fall back to the model and the goodbye/fallback phrases are skipped.
func (c *IdleController) SetSpeakIndex(i int) { c.speak = i }

// SetOnEnd installs the hang-up (before the session starts); see IdleConfig.OnEnd.
func (c *IdleController) SetOnEnd(fn func(reason string)) { c.cfg.OnEnd = fn }

// Attach starts the clock: the user clock runs from the start of the call, so
// a caller who connects and never speaks is prompted and hung up too.
func (c *IdleController) Attach(s SessionController, index int) error {
	c.session, c.index = s, index
	c.mu.Lock()
	c.arm(c.clk.Now())
	c.mu.Unlock()
	return s.Go(c.run)
}

func (c *IdleController) ProcessFrame(_ context.Context, f Frame, dir Direction, e Emitter) error {
	switch f.(type) {
	case *SpeakingStateFrame, *UserStartedSpeakingFrame, *UserStoppedSpeakingFrame, *TurnEventFrame,
		*UserMessageFrame, *InputDTMFFrame, *InterruptionFrame:
		c.observe(f)
	}
	return e.PushDirection(f, dir)
}

func (c *IdleController) poke() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// arm starts the user clock when nobody is speaking or being answered.
func (c *IdleController) arm(now time.Time) {
	if c.cfg.UserTimeout <= 0 || c.ending || c.done || c.botSpeaking || c.userSpeaking || c.turnUser || c.waitingBot {
		return
	}
	c.userAt = now.Add(c.cfg.UserTimeout)
}

// userInput records any sign of the user: the prompt count restarts and the
// user is no longer waiting for an answer to an earlier turn.
func (c *IdleController) userInput() {
	c.prompts, c.userAt = 0, time.Time{}
	c.waitingBot, c.botAt = false, time.Time{}
	c.sayAfterCancel = "" // never talk over the user with a stale fallback
}

func (c *IdleController) observe(f Frame) {
	now := c.clk.Now()
	c.mu.Lock()
	if c.done {
		c.mu.Unlock()
		return
	}
	switch v := f.(type) {
	case *SpeakingStateFrame:
		switch v.State {
		case SpeakingStarted:
			c.botSpeaking, c.waitingBot, c.sawStart = true, false, true
			c.userAt, c.botAt, c.strikes = time.Time{}, time.Time{}, 0
		case SpeakingStopped:
			c.botSpeaking = false
			if c.ending {
				if c.sawStart {
					c.endAt = now
				}
			} else {
				c.arm(now)
			}
		}
	case *UserStartedSpeakingFrame:
		c.userInput()
		c.userSpeaking = true
	case *UserStoppedSpeakingFrame:
		c.userSpeaking = false
		c.arm(now)
	case *TurnEventFrame:
		switch v.Kind {
		case TurnEventStarted:
			c.userInput()
			c.turnUser = true
		case TurnEventCompleted, TurnEventInterrupted, TurnEventNoSpeechTimeout:
			// The committed user message usually follows and stops the clock; a
			// turn that commits nothing (filtered backchannel) must not stop it for good.
			c.turnUser = false
			c.arm(now)
		}
	case *InputDTMFFrame:
		c.userInput()
		c.arm(now) // restarts the clock; the aggregated message follows within the aggregate timeout
	case *UserMessageFrame:
		c.userAt = time.Time{}
		c.prompts = 0 // synthetic turns never pass this stage (they re-enter after it)
		if c.cfg.BotTimeout > 0 {
			c.waitingBot, c.botAt = true, now.Add(c.cfg.BotTimeout)
		} else {
			c.arm(now)
		}
	case *InterruptionFrame:
		c.waitingBot, c.botAt = false, time.Time{}
		if c.sayAfterCancel != "" {
			c.sayNow, c.sayAfterCancel = c.sayAfterCancel, ""
		}
	}
	c.mu.Unlock()
	c.poke()
}

// run is the single supervised task: it sleeps until the earliest deadline and
// acts on the ones that have passed.
func (c *IdleController) run(ctx context.Context) {
	for {
		c.mu.Lock()
		if c.done {
			c.mu.Unlock()
			return
		}
		next := earliest(c.userAt, c.botAt, c.endAt)
		c.mu.Unlock()
		var tm IdleTimer
		var tc <-chan time.Time
		if !next.IsZero() {
			tm = c.clk.TimerAt(next)
			tc = tm.C()
		}
		select {
		case <-ctx.Done():
			if tm != nil {
				tm.Stop()
			}
			return
		case <-c.wake:
			c.mu.Lock()
			text := c.sayNow
			c.sayNow = ""
			c.mu.Unlock()
			if text != "" {
				c.say(text)
			}
		case <-tc:
			c.fire()
		}
		if tm != nil {
			tm.Stop()
		}
	}
}

func earliest(ts ...time.Time) (out time.Time) {
	for _, t := range ts {
		if !t.IsZero() && (out.IsZero() || t.Before(out)) {
			out = t
		}
	}
	return out
}

func (c *IdleController) busy() bool { return c.cfg.ToolsBusy != nil && c.cfg.ToolsBusy() }

func (c *IdleController) answering() bool { return c.cfg.Answering == nil || c.cfg.Answering() }

func (c *IdleController) fire() {
	now := c.clk.Now()
	var acts []func()
	c.mu.Lock()
	due := func(t time.Time) bool { return !t.IsZero() && !t.After(now) }
	switch {
	case c.done:
	case due(c.endAt):
		c.done = true
		reason := c.endReason
		acts = append(acts, func() {
			if c.cfg.OnEnd != nil {
				c.cfg.OnEnd(reason)
			}
		})
	default:
		if due(c.botAt) {
			acts = append(acts, c.botStuckLocked(now)...)
		}
		if due(c.userAt) {
			acts = append(acts, c.userIdleLocked(now)...)
		}
	}
	c.mu.Unlock()
	for _, a := range acts {
		a()
	}
}

// botStuckLocked handles an expired bot watchdog.
func (c *IdleController) botStuckLocked(now time.Time) []func() {
	c.botAt = time.Time{}
	if c.busy() { // a tool is not a stuck model: look again later
		c.botAt = now.Add(c.cfg.BotTimeout)
		return nil
	}
	c.waitingBot = false
	if !c.answering() { // the reply finished without speech: not stuck
		c.arm(now)
		return nil
	}
	c.strikes++
	c.BotStuck.Add(1)
	strikes := c.strikes
	attrs := map[string]any{"count": strikes, "duration_ms": c.cfg.BotTimeout.Milliseconds(), "reason": IdleReasonBot}
	acts := []func(){
		func() { c.debug(DebugIdleBotStuck, attrs) },
		func() { c.toClient(&ErrorFrame{Err: ErrBotIdle}) },
		// Cancels the model/TTS work and bumps the generation so late output is dropped.
		func() { _ = c.session.Reenter(&InterruptionFrame{}, Downstream, c.index+1, 0) },
	}
	if strikes >= c.cfg.BotMaxStuck {
		c.BotHangups.Add(1)
		c.beginEndLocked(now, IdleReasonBot, "")
		return acts
	}
	if c.cfg.BotFallback != "" && c.speak >= 0 {
		// Spoken once the interruption comes back: the generation bump it makes
		// would otherwise fence this speech (the envelope is stamped at admission).
		c.sayAfterCancel = c.cfg.BotFallback
	}
	c.arm(now)
	return acts
}

// userIdleLocked handles an expired user clock: prompt, or hang up after the last.
func (c *IdleController) userIdleLocked(now time.Time) []func() {
	c.userAt = time.Time{}
	if c.botSpeaking || c.userSpeaking || c.turnUser {
		return nil // the next transition re-arms the clock
	}
	if c.busy() || (c.cfg.Muted != nil && c.cfg.Muted()) || (c.cfg.Answering != nil && c.cfg.Answering()) {
		c.userAt = now.Add(c.cfg.UserTimeout)
		return nil
	}
	if c.prompts >= c.cfg.MaxPrompts {
		c.UserHangups.Add(1)
		prompts := c.prompts
		acts := []func(){func() { c.debug(DebugIdleUserTimeout, map[string]any{"count": prompts, "reason": IdleReasonUser}) }}
		if c.beginEndLocked(now, IdleReasonUser, c.cfg.Goodbye) {
			acts = append(acts, func() { c.say(c.cfg.Goodbye) })
		}
		return acts
	}
	c.prompts++
	n := c.prompts
	c.UserPrompts.Add(1)
	c.userAt = now.Add(c.cfg.UserTimeout)
	nudge := !c.cfg.Say && c.cfg.History != nil
	if nudge && c.cfg.BotTimeout > 0 {
		c.waitingBot, c.botAt = true, now.Add(c.cfg.BotTimeout)
	}
	return []func(){
		func() { c.debug(DebugIdleUserPrompt, map[string]any{"count": n}) },
		func() { c.prompt(n, nudge) },
	}
}

// beginEndLocked starts the hang-up: at once, or (true) once the goodbye the
// caller must now speak has played.
func (c *IdleController) beginEndLocked(now time.Time, reason, goodbye string) bool {
	c.ending, c.endReason = true, reason
	c.userAt, c.botAt = time.Time{}, time.Time{}
	c.endAt = now
	if goodbye == "" || c.speak < 0 {
		return false
	}
	c.endAt, c.sawStart = now.Add(c.cfg.GoodbyeWait), false
	return true
}

// toClient delivers f at the transport (the last stage) only.
func (c *IdleController) toClient(f Frame) {
	if t, ok := c.session.(interface{ TailIndex() int }); ok {
		_ = c.session.Reenter(f, Downstream, t.TailIndex(), 0)
	}
}

func (c *IdleController) prompt(n int, nudge bool) {
	if !nudge {
		c.say(c.cfg.UserPrompt)
		return
	}
	turn := "idle-" + strconv.FormatUint(c.nudgeN.Add(1), 10)
	at := time.Now()
	msg := Message{
		Role: RoleUser, TurnID: turn, CreatedAt: at, Metadata: map[string]any{"idle_prompt": n},
		Content: fmt.Sprintf("[idle notice; system notice, not spoken by the user] The user has been silent for %d seconds. Briefly check that they are still there, in the spirit of: %q. Do not mention this notice.",
			int(c.cfg.UserTimeout/time.Second), c.cfg.UserPrompt),
	}
	if c.cfg.History.AddMessage(msg) != nil {
		return
	}
	_ = c.session.Reenter(&UserMessageFrame{Turn: turn, Content: msg.Content, Gen: c.session.CurrentGeneration(), Timestamp: at}, Downstream, c.index+1, 0)
}

// say speaks text through TTS without the model. Generation 0 keeps it from
// being fenced by the interruption that may have just cancelled a stuck turn.
func (c *IdleController) say(text string) {
	if c.speak < 0 {
		return
	}
	n := c.sayN.Add(1)
	// Unit ids must increase across the whole session (transport pacer): a
	// Session stamps id 0 when it delivers the frame (allocating here could
	// lose the race to a reply unit and reach the pacer out of order).
	unit := n
	if _, ok := c.session.(interface{ NextUnitID() uint64 }); ok {
		unit = 0
	}
	_ = c.session.Reenter(&SynthesisUnitFrame{Turn: "idle-say-" + strconv.FormatUint(n, 10), UnitID: unit, Text: text, IsFinal: true}, Downstream, c.speak, 0)
}

func (c *IdleController) debug(name string, attrs map[string]any) {
	if d, ok := c.session.(interface{ EmitDebug(DebugEvent) }); ok {
		d.EmitDebug(NewDebugEvent(name, time.Now(), SessionID(strconv.FormatUint(c.session.ID(), 10)), "", Generation(c.session.CurrentGeneration()), attrs))
	}
}
