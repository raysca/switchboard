package server

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"govoice/core"
	"govoice/server/agent"
	"govoice/transport/ws"
)

// cuePlayer is the session's run-time cue output (*ws.CueUnderlay).
type cuePlayer interface {
	Start(ws.Underlay)
	Stop(fade time.Duration)
}

var cueBase = time.Now() // monotonic reference for lock-free "last activity" stamps

const (
	defaultCueAfter = 300 * time.Millisecond
	cueFade         = 30 * time.Millisecond
	cueHardCap      = 10 * time.Second
	cueMaxWait      = 90 * time.Second // outer bound on one route's wait for the new step's audio
)

// cueController plays the route cue over the gap between the old step's
// last audio and the new step's first audio. It starts the cue only if that
// gap outlasts `after`, so quick routes stay silent, and stops it (with a
// short fade) on the new step's first audio, on caller speech, on failure, or
// at a hard cap. All methods are safe on a nil receiver.
type cueController struct {
	player cuePlayer
	sound  func() ws.Underlay // a fresh cursor over the cue sound
	after  time.Duration
	cap    time.Duration
	record func(TimelineEventType, string)

	waiting   atomic.Bool  // from preparing until the new step speaks (or the route fails)
	activated atomic.Bool  // the route is active: the next bot audio is the new step's
	speaking  atomic.Bool  // the caller is speaking
	activity  atomic.Int64 // last bot audio or caller speech, monotonic offset from cueBase

	mu      sync.Mutex
	playing bool
	began   time.Time // when waiting started (monotonic)
	timer   *time.Timer
	capTmr  *time.Timer
}

func newCueController(p cuePlayer, sound func() ws.Underlay, after time.Duration, record func(TimelineEventType, string)) *cueController {
	if after <= 0 {
		after = defaultCueAfter
	}
	return &cueController{player: p, sound: sound, after: after, cap: cueHardCap, record: record}
}

// waitingNow reports whether the route cue is waiting for the new step's audio (nil-safe).
func (c *cueController) waitingNow() bool { return c != nil && c.waiting.Load() }

func (c *cueController) stamp() { c.activity.Store(int64(time.Since(cueBase)) + 1) }

// botAudio is called for every bot audio frame the call produces.
func (c *cueController) botAudio() {
	if c == nil || !c.waiting.Load() {
		return
	}
	c.stamp()
	if c.activated.Load() {
		c.finish("first_audio", true)
	}
}

// userSpeech is called when the caller starts or stops speaking.
func (c *cueController) userSpeech(speaking bool) {
	if c == nil {
		return
	}
	c.speaking.Store(speaking) // tracked always: a route may begin mid-speech
	if !c.waiting.Load() {
		return
	}
	c.stamp()
	if speaking {
		c.finish("barge_in", false)
	}
}

func (c *cueController) onRoute(e core.RouteEvent) {
	if c == nil {
		return
	}
	switch e.Kind {
	case core.RoutePreparing:
		c.mu.Lock()
		c.began = time.Now()
		c.activated.Store(false)
		c.waiting.Store(true)
		c.stamp()
		c.armLocked(c.after)
		c.mu.Unlock()
	case core.RouteActive:
		c.activated.Store(true)
	case core.RouteFailed:
		c.finish("failed", true)
	}
}

// afterFuncSafe is time.AfterFunc whose callback cannot take the process down:
// a panic (a cue sound or player bug) lands on the call timeline, without the
// panic value.
func afterFuncSafe(d time.Duration, record func(TimelineEventType, string), fn func()) *time.Timer {
	return time.AfterFunc(d, func() {
		defer func() {
			if recover() != nil {
				record(EventError, "cue timer panicked")
			}
		}()
		fn()
	})
}

func (c *cueController) armLocked(d time.Duration) {
	if c.timer != nil {
		c.timer.Stop()
	}
	c.timer = afterFuncSafe(max(d, 5*time.Millisecond), c.record, c.check)
}

// check starts the cue once the call has been quiet for `after`.
func (c *cueController) check() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.waiting.Load() || c.playing {
		return
	}
	if time.Since(c.began) > cueMaxWait {
		c.waiting.Store(false)
		return
	}
	quiet := time.Duration(int64(time.Since(cueBase)) + 1 - c.activity.Load())
	if c.speaking.Load() || quiet < c.after {
		wait := c.after - quiet
		if c.speaking.Load() {
			wait = c.after
		}
		c.armLocked(wait)
		return
	}
	c.playing = true
	c.player.Start(c.sound())
	c.record(EventCueStart, "")
	c.capTmr = afterFuncSafe(c.cap, c.record, func() { c.finish("cap", true) })
}

// finish stops a playing cue; end also closes the waiting window so the cue
// is not started again for this route.
func (c *cueController) finish(reason string, end bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if end {
		c.waiting.Store(false)
		c.activated.Store(false)
		if c.timer != nil {
			c.timer.Stop()
		}
	}
	if !c.playing {
		return
	}
	c.playing = false
	if c.capTmr != nil {
		c.capTmr.Stop()
	}
	c.player.Stop(cueFade)
	c.record(EventCueStop, reason)
	if !end && c.waiting.Load() {
		c.armLocked(c.after) // the caller stopped talking mid-route: the gap may still be open
	}
}

// newCallCue returns the call's cue controller, or nil when the flow has no
// route cue, the transport has no cue output, or the named sound is not
// configured (logged; the call runs without a cue).
func (s *Server) newCallCue(g *CallAgent, player cuePlayer, tl *SessionTimeline) *cueController {
	if g == nil || g.Cue == nil || g.Cue.Name == "" {
		return nil
	}
	if p, ok := player.(*ws.CueUnderlay); !ok || p == nil {
		return nil
	}
	sound, ok := s.cues[g.Cue.Name]
	if !ok {
		s.logger.Warn("route cue is not configured; the call runs without it", "cue", g.Cue.Name)
		return nil
	}
	return newCueController(player, sound, time.Duration(g.Cue.AfterMS)*time.Millisecond, func(t TimelineEventType, d string) {
		tl.Record(TimelineEvent{Type: t, Details: d})
	})
}

const (
	// defaultHoldAfter is the tool cue's wait when the flow gives none. It is deliberately longer than the
	// route cue's 300 ms: a route gap is always dead air while the next step is built, but most tool calls
	// return within a second and a sound on each of them would be noise.
	defaultHoldAfter = time.Second
	holdHardCap      = 30 * time.Second // the tool loop's own call timeout
)

// holdCue plays the flow's hold sound while a slow synchronous tool runs. It starts once a tool has run
// `after` with the call quiet, pauses for bot audio or caller speech (and re-arms while the tool still
// runs), and stops when the last running tool returns, at a hard cap, or when a route starts preparing:
// both sounds share one player, and stopping fades whatever cue is current, so a hold still playing when
// the route cue started would cut it. It never starts while the route cue waits. All methods are safe on a
// nil receiver.
type holdCue struct {
	player  cuePlayer
	sound   func() ws.Underlay
	after   time.Duration
	cap     time.Duration
	blocked func() bool // the route cue is waiting
	record  func(TimelineEventType, string)

	running  atomic.Int32 // tools executing now
	playing  atomic.Bool  // written under mu; read lock-free on the audio path
	speaking atomic.Bool  // the caller is speaking
	routing  atomic.Bool  // a route is preparing
	activity atomic.Int64 // last bot audio or caller speech, monotonic offset from cueBase

	mu     sync.Mutex
	capped bool // the cap fired during this busy period: no restart until every tool has returned
	epoch  uint64
	began  time.Time // when the first running tool started
	timer  *time.Timer
	capTmr *time.Timer
}

func newHoldCue(p cuePlayer, sound func() ws.Underlay, after time.Duration, blocked func() bool, record func(TimelineEventType, string)) *holdCue {
	if after <= 0 {
		after = defaultHoldAfter
	}
	return &holdCue{player: p, sound: sound, after: after, cap: holdHardCap, blocked: blocked, record: record}
}

func (h *holdCue) stamp() { h.activity.Store(int64(time.Since(cueBase)) + 1) }

// toolStarted and toolDone bracket one synchronous tool execution.
func (h *holdCue) toolStarted() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.running.Add(1) == 1 {
		h.began, h.capped = time.Now(), false
		h.armLocked(h.after)
	}
}

func (h *holdCue) toolDone() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.running.Add(-1) > 0 {
		return
	}
	h.disarmLocked()
	h.pauseLocked("tool_done")
}

// botAudio is called for every bot audio frame: lock-free unless a tool runs, and takes the lock only
// when the cue is playing.
func (h *holdCue) botAudio() {
	if h == nil || h.running.Load() == 0 {
		return
	}
	h.stamp()
	if h.playing.Load() {
		h.pauseAndRearm("bot_audio")
	}
}

func (h *holdCue) userSpeech(speaking bool) {
	if h == nil {
		return
	}
	h.speaking.Store(speaking)
	if h.running.Load() == 0 {
		return
	}
	h.stamp()
	if speaking && h.playing.Load() {
		h.pauseAndRearm("barge_in")
	}
}

func (h *holdCue) onRoute(e core.RouteEvent) {
	if h == nil {
		return
	}
	switch e.Kind {
	case core.RoutePreparing:
		h.routing.Store(true)
		h.mu.Lock()
		h.disarmLocked()
		h.pauseLocked("route")
		h.mu.Unlock()
	case core.RouteActive, core.RouteFailed:
		h.routing.Store(false)
		if h.running.Load() > 0 {
			h.mu.Lock()
			h.armLocked(h.after)
			h.mu.Unlock()
		}
	}
}

func (h *holdCue) pauseAndRearm(reason string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pauseLocked(reason)
	if h.running.Load() > 0 {
		h.armLocked(h.after)
	}
}

func (h *holdCue) armLocked(d time.Duration) {
	if h.timer != nil {
		h.timer.Stop()
	}
	h.timer = afterFuncSafe(max(d, 5*time.Millisecond), h.record, h.check)
}

func (h *holdCue) disarmLocked() {
	if h.timer != nil {
		h.timer.Stop()
		h.timer = nil
	}
}

// check starts the cue once a tool has run `after` and the call has been quiet for `after`.
func (h *holdCue) check() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.running.Load() == 0 || h.playing.Load() || h.capped {
		return
	}
	quiet := time.Duration(int64(time.Since(cueBase)) + 1 - h.activity.Load())
	ran := time.Since(h.began)
	if h.routing.Load() || h.blocked() || h.speaking.Load() {
		h.armLocked(h.after)
		return
	}
	if wait := h.after - min(quiet, ran); wait > 0 {
		h.armLocked(wait)
		return
	}
	h.playing.Store(true)
	h.player.Start(h.sound())
	h.record(EventCueStart, "tool")
	h.epoch++
	epoch := h.epoch
	h.capTmr = afterFuncSafe(h.cap, h.record, func() { h.capHit(epoch) })
}

func (h *holdCue) capHit(epoch uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.epoch != epoch || !h.playing.Load() {
		return
	}
	h.capped = true
	h.pauseLocked("cap")
}

// pauseLocked stops a playing cue (a short fade) and records why.
func (h *holdCue) pauseLocked(reason string) {
	if !h.playing.Load() {
		return
	}
	h.playing.Store(false)
	if h.capTmr != nil {
		h.capTmr.Stop()
	}
	h.player.Stop(cueFade)
	h.record(EventCueStop, reason)
}

// holdTools arms h around every synchronous MCP tool of defs. Async tools (the model acknowledges them),
// transfer tools, end_call and send_dtmf are not MCP tools and never arm it. The deferred toolDone also
// runs when the tool is cancelled (barge-in, stale generation) or panics, so the sound cannot leak.
func holdTools(next core.ToolExecutor, defs []core.ToolDefinition, h *holdCue) core.ToolExecutor {
	if h == nil || next == nil {
		return next
	}
	slow := map[string]bool{}
	for _, d := range defs {
		if d.RemoteName != "" && !d.Policy.Async {
			slow[d.Name] = true
		}
	}
	if len(slow) == 0 {
		return next
	}
	return core.ToolExecutorFunc(func(ctx context.Context, c core.ToolCall) (core.ToolResult, error) {
		if slow[c.Name] {
			// Done once, by whichever comes first: the executor returning, or its context ending (the
			// loop abandons an executor that ignores the context, and that must not leave the sound on).
			var once sync.Once
			done := func() { once.Do(h.toolDone) }
			h.toolStarted()
			defer done()
			defer context.AfterFunc(ctx, done)()
		}
		return next.Execute(ctx, c)
	})
}

// newCallHold returns the call's hold sound controller: nil when the flow has no tool cue, the transport has no cue output, or the named
// sound is not configured (logged; the call runs without it).
func (s *Server) newCallHold(a *agent.AudioSettings, player cuePlayer, tl *SessionTimeline, route *cueController) *holdCue {
	if a == nil || a.ToolCue == nil || a.ToolCue.Name == "" {
		return nil
	}
	if p, ok := player.(*ws.CueUnderlay); !ok || p == nil {
		return nil
	}
	sound, ok := s.cues[a.ToolCue.Name]
	if !ok {
		s.logger.Warn("tool cue is not configured; the call runs without it", "cue", a.ToolCue.Name)
		return nil
	}
	return newHoldCue(player, sound, time.Duration(a.ToolCue.AfterMS)*time.Millisecond, route.waitingNow, func(t TimelineEventType, d string) {
		tl.Record(TimelineEvent{Type: t, Details: d})
	})
}
