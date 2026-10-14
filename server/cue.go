package server

import (
	"sync"
	"sync/atomic"
	"time"

	"govoice/core"
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

func (c *cueController) armLocked(d time.Duration) {
	if c.timer != nil {
		c.timer.Stop()
	}
	c.timer = time.AfterFunc(max(d, 5*time.Millisecond), c.check)
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
	c.capTmr = time.AfterFunc(c.cap, func() { c.finish("cap", true) })
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
func (s *Server) newCallCue(g *CallFlow, player cuePlayer, tl *SessionTimeline) *cueController {
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
