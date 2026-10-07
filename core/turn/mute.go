package turn

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"govoice/core"
)

// UserMuteStrategy decides, from bot/tool state, whether user input is muted
// (Pipecat turns/user_mute). While muted the Processor drops VAD start/stop
// and transcripts: nothing starts a turn, barges in or reaches the context.
// Audio is NOT dropped: it keeps flowing to VAD, STT and recording, so no STT
// stream or VAD state is left half-open. Mute gates new turns only: a turn
// that is already admitted when the mute begins runs to completion; one that
// is still pending is abandoned (its transcripts invalidated).
//
// The interface is sealed; strategies are called from the session run loop and
// must not block. An instance belongs to one Processor.
type UserMuteStrategy interface {
	name() string
	bot(speaking, critical bool)
	muted() bool
}

// Mute strategy names (config values and the bounded debug "reason").
//
// ponytail: Pipecat also releases until_first_bot_complete on an ErrorFrame
// before first audio; LLM/TTS errors flow downstream of this processor, so a
// failed greeting is released by MaxMute instead. critical_output keys off
// SpeakingStarted only: an uninterruptible unit queued behind interruptible
// speech in the same speaking span does not mute (emit a disposition change
// from SpeakingTracker if such mixed spans become real).
const (
	MuteAlways           = "always"                   // while the bot speaks
	MuteFirstSpeech      = "first_speech"             // while the bot speaks its first turn
	MuteUntilFirstBot    = "until_first_bot_complete" // from session start until the bot's first speech ends
	MuteFunctionCall     = "function_call"            // while a tool call runs
	MuteCriticalOutput   = "critical_output"          // while uninterruptible output plays
	DebugUserMuted       = "user.muted"
	DebugUserUnmuted     = "user.unmuted"
	DefaultMaxMute       = 30 * time.Second
	muteReasonUnspecific = "none"
)

// ParseMuteStrategies builds strategies from config names. tools may be nil
// (validation only); function_call then never mutes.
func ParseMuteStrategies(names []string, tools *ToolActivity) ([]UserMuteStrategy, error) {
	var out []UserMuteStrategy
	for _, n := range names {
		switch n {
		case MuteAlways:
			out = append(out, &botMute{n: n, f: func(s *botMute) bool { return s.speaking }})
		case MuteFirstSpeech:
			out = append(out, &botMute{n: n, f: func(s *botMute) bool { return s.speaking && !s.done }})
		case MuteUntilFirstBot:
			out = append(out, &botMute{n: n, f: func(s *botMute) bool { return !s.done }})
		case MuteCriticalOutput:
			out = append(out, &botMute{n: n, f: func(s *botMute) bool { return s.speaking && s.critical }})
		case MuteFunctionCall:
			out = append(out, &toolMute{tools})
		default:
			return nil, fmt.Errorf("turn.mute: unknown strategy %q (%s, %s, %s, %s, %s)", n,
				MuteAlways, MuteFirstSpeech, MuteUntilFirstBot, MuteFunctionCall, MuteCriticalOutput)
		}
	}
	return out, nil
}

// botMute tracks bot speaking state; f maps it to the strategy's verdict.
type botMute struct {
	n                        string
	speaking, critical, done bool // done: the bot's first speech has ended
	f                        func(*botMute) bool
}

func (s *botMute) name() string { return s.n }
func (s *botMute) bot(speaking, critical bool) {
	if s.speaking && !speaking {
		s.done = true
	}
	s.speaking, s.critical = speaking, speaking && critical
}
func (s *botMute) muted() bool { return s.f(s) }

type toolMute struct{ t *ToolActivity }

func (s *toolMute) name() string { return MuteFunctionCall }
func (*toolMute) bot(_, _ bool)  {}
func (s *toolMute) muted() bool  { return s.t != nil && s.t.Active() }

// ToolActivity counts in-flight tool executions of one call. Wrap the call's
// ToolExecutor with it; the entry is removed when Execute returns (result,
// error, cancel or stale generation), so an ID cannot leave the mute stuck.
type ToolActivity struct {
	mu sync.Mutex
	n  map[string]int
}

func NewToolActivity() *ToolActivity { return &ToolActivity{n: map[string]int{}} }

// Active reports whether any tracked tool call is executing.
func (t *ToolActivity) Active() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.n) > 0
}

// Wrap tracks calls to next. Tools named in async (ToolPolicy.Async) are not
// tracked: the user may talk while they run in the background.
func (t *ToolActivity) Wrap(next core.ToolExecutor, defs []core.ToolDefinition) core.ToolExecutor {
	if next == nil {
		return nil
	}
	async := map[string]bool{}
	for _, d := range defs {
		if d.Policy.Async {
			async[d.Name] = true
		}
	}
	return core.ToolExecutorFunc(func(ctx context.Context, c core.ToolCall) (core.ToolResult, error) {
		if !async[c.Name] {
			t.mu.Lock()
			t.n[c.ID]++
			t.mu.Unlock()
			defer func() {
				t.mu.Lock()
				if t.n[c.ID]--; t.n[c.ID] <= 0 {
					delete(t.n, c.ID)
				}
				t.mu.Unlock()
			}()
		}
		return next.Execute(ctx, c)
	})
}

// muter combines strategies. Each strategy's mute is capped at max so a lost
// bot-stopped signal or a tool that never returns cannot silence the user
// forever; the cap re-arms once the strategy releases.
type muter struct {
	s       []UserMuteStrategy
	max     time.Duration
	since   []time.Time
	expired []bool
	on      bool
	reason  string // strategy that caused the current mute
}

func newMuter(s []UserMuteStrategy, max time.Duration) *muter {
	if len(s) == 0 {
		return nil
	}
	if max <= 0 {
		max = DefaultMaxMute
	}
	return &muter{s: s, max: max, since: make([]time.Time, len(s)), expired: make([]bool, len(s))}
}

func (m *muter) onBot(speaking, critical bool) {
	if m == nil {
		return
	}
	for _, s := range m.s {
		s.bot(speaking, critical)
	}
}

// eval returns the mute decision, the first muting strategy and whether it changed.
func (m *muter) eval(now time.Time) (on bool, reason string, changed bool) {
	reason = muteReasonUnspecific
	for i, s := range m.s {
		if !s.muted() {
			m.since[i], m.expired[i] = time.Time{}, false
			continue
		}
		if m.since[i].IsZero() {
			m.since[i] = now
		}
		if m.expired[i] {
			continue
		}
		if now.Sub(m.since[i]) >= m.max {
			m.expired[i] = true
			continue
		}
		if !on {
			on, reason = true, s.name()
		}
	}
	changed = on != m.on
	if on {
		m.reason = reason
	} else if changed {
		reason = m.reason
	}
	m.on = on
	return
}

func (p *Processor) muted() bool { return p.mute != nil && p.mute.on }

// Muted reports whether a user mute strategy is muting now (any goroutine).
func (p *Processor) Muted() bool { return p.mutedNow.Load() }

func (p *Processor) drop() {
	if p.onMuteDrop != nil {
		p.onMuteDrop()
	}
}

// markDropped retires a muted segment id so its late transcripts are dropped
// even after the mute ends. Counted by drop() at the start frame itself.
func (p *Processor) markDropped(seg string) {
	if len(p.closed) > 64 {
		p.closed = make(map[string]struct{})
	}
	p.closed[seg] = struct{}{}
	p.drop()
}

// checkMute re-evaluates the strategies (run-loop only) and applies a change.
// Mute while a turn is pending (open, never admitted) abandons it; an admitted
// turn is left to finish. The manager is paused only when no turn is admitted,
// which also drops its start candidate and shifts its no-speech clock.
func (p *Processor) checkMute(e core.Emitter) error {
	if p.mute == nil {
		return nil
	}
	now := p.now()
	on, reason, changed := p.mute.eval(now)
	if changed {
		p.mutedNow.Store(on)
	}
	if !changed {
		return nil
	}
	name := DebugUserUnmuted
	if on {
		name = DebugUserMuted
	}
	if d, ok := p.session.(interface{ EmitDebug(core.DebugEvent) }); ok {
		d.EmitDebug(core.NewDebugEvent(name, now, core.SessionID(strconv.FormatUint(p.session.ID(), 10)), p.id, 0, map[string]any{"reason": reason}))
	}
	if !on {
		if p.mgrMuted {
			p.mgrMuted = false
			_ = p.mgr.SetMuted(false, now)
		}
		return nil
	}
	if p.started {
		return nil
	}
	p.mgrMuted = p.mgr.SetMuted(true, now) == nil
	if !p.open {
		return nil
	}
	gen, id := core.Generation(p.candGen), p.id
	// The segment's start went downstream, so its stop must too (the client
	// would otherwise see the user speaking forever); with the turn closed the
	// stop just passes through.
	p.vadSpeaking = false
	p.finish()
	p.retireSegments()
	if gen != 0 {
		return e.Push(&core.TranscriptInvalidationFrame{Turn: id, Gen: gen})
	}
	return nil
}
