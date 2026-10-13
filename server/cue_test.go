package server

import (
	"sync"
	"testing"
	"time"

	"govoice/core"
	"govoice/server/flow"
	"govoice/transport/ws"
)

type fakePlayer struct {
	mu            sync.Mutex
	starts, stops int
	fades         []time.Duration
}

func (p *fakePlayer) Start(ws.Underlay) { p.mu.Lock(); p.starts++; p.mu.Unlock() }
func (p *fakePlayer) Stop(f time.Duration) {
	p.mu.Lock()
	p.stops++
	p.fades = append(p.fades, f)
	p.mu.Unlock()
}
func (p *fakePlayer) counts() (int, int) { p.mu.Lock(); defer p.mu.Unlock(); return p.starts, p.stops }

type cueRecorder struct {
	mu sync.Mutex
	ev []string
}

func (r *cueRecorder) record(t TimelineEventType, d string) {
	r.mu.Lock()
	r.ev = append(r.ev, string(t)+":"+d)
	r.mu.Unlock()
}
func (r *cueRecorder) events() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ev...)
}

func newTestCue(t *testing.T) (*cueController, *fakePlayer, *cueRecorder) {
	t.Helper()
	p, r := &fakePlayer{}, &cueRecorder{}
	c := newCueController(p, func() ws.Underlay { return nil }, 20*time.Millisecond, r.record)
	c.cap = 200 * time.Millisecond
	return c, p, r
}

func waitCue(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(2 * time.Second); !ok(); time.Sleep(time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func prep() core.HandoffEvent {
	return core.HandoffEvent{Kind: core.HandoffPreparing, From: "a", To: "b"}
}
func active() core.HandoffEvent {
	return core.HandoffEvent{Kind: core.HandoffActive, From: "a", To: "b"}
}

func TestCueFastRoutePlaysNothing(t *testing.T) {
	c, p, _ := newTestCue(t)
	c.onRoute(prep())
	c.onRoute(active())
	c.botAudio() // the new step answers before after_ms
	time.Sleep(60 * time.Millisecond)
	if s, _ := p.counts(); s != 0 {
		t.Fatal("a handoff faster than after_ms must stay silent")
	}
}

func TestCueSlowRoutePlaysUntilFirstAudio(t *testing.T) {
	c, p, r := newTestCue(t)
	c.onRoute(prep())
	c.botAudio() // the old step's farewell: must not stop anything
	waitCue(t, "cue start", func() bool { s, _ := p.counts(); return s == 1 })
	c.botAudio() // farewell tail before the flip: still not the new step
	if _, st := p.counts(); st != 0 {
		t.Fatal("audio before the handoff is active must not stop the cue")
	}
	c.onRoute(active())
	c.botAudio()
	if s, st := p.counts(); s != 1 || st != 1 {
		t.Fatalf("starts=%d stops=%d", s, st)
	}
	if ev := r.events(); len(ev) != 2 || ev[0] != "cue_start:" || ev[1] != "cue_stop:first_audio" {
		t.Fatalf("events = %v", ev)
	}
	c.botAudio() // later audio changes nothing
	if _, st := p.counts(); st != 1 {
		t.Fatal("stop must happen exactly once")
	}
}

func TestCueStopsOnEveryExit(t *testing.T) {
	cases := map[string]func(*cueController){
		"barge_in": func(c *cueController) { c.userSpeech(true) },
		"failed":   func(c *cueController) { c.onRoute(core.HandoffEvent{Kind: core.HandoffFailed}) },
		"cap":      func(c *cueController) { time.Sleep(260 * time.Millisecond) },
	}
	for reason, trigger := range cases {
		t.Run(reason, func(t *testing.T) {
			c, p, r := newTestCue(t)
			c.onRoute(prep())
			waitCue(t, "cue start", func() bool { s, _ := p.counts(); return s == 1 })
			trigger(c)
			waitCue(t, "cue stop", func() bool { _, st := p.counts(); return st == 1 })
			ev := r.events()
			if ev[len(ev)-1] != "cue_stop:"+reason {
				t.Fatalf("events = %v", ev)
			}
			if reason != "barge_in" {
				time.Sleep(60 * time.Millisecond)
				if s, _ := p.counts(); s != 1 {
					t.Fatalf("a finished handoff must not restart the cue (starts=%d)", s)
				}
			}
		})
	}
}

func TestCueDoesNotStartWhileCallerSpeaks(t *testing.T) {
	c, p, _ := newTestCue(t)
	c.userSpeech(true)
	c.onRoute(prep())
	time.Sleep(80 * time.Millisecond)
	if s, _ := p.counts(); s != 0 {
		t.Fatal("the cue must wait for the caller to stop speaking")
	}
	c.userSpeech(false)
	waitCue(t, "cue start after the caller stops", func() bool { s, _ := p.counts(); return s == 1 })
}

func TestNilCueControllerIsInert(t *testing.T) {
	var c *cueController
	c.botAudio()
	c.userSpeech(true)
	c.onRoute(prep())
}

func TestNewCallCueNeedsFlowCueSoundAndOutput(t *testing.T) {
	s := apiTestServer(t)
	tl := NewSessionTimeline(1, 10, time.Minute)
	out := ws.NewCueUnderlay(nil)
	sound := func() ws.Underlay { return nil }
	s.cues = map[string]func() ws.Underlay{"chime": sound}
	cue := &flow.RouteCue{Name: "chime"}
	for name, got := range map[string]*cueController{
		"no graph":         s.newCallCue(nil, out, tl),
		"no cue":           s.newCallCue(&CallFlow{}, out, tl),
		"unknown sound":    s.newCallCue(&CallFlow{Cue: &flow.RouteCue{Name: "nope"}}, out, tl),
		"no cue output":    s.newCallCue(&CallFlow{Cue: cue}, (*ws.CueUnderlay)(nil), tl),
		"no player at all": s.newCallCue(&CallFlow{Cue: cue}, nil, tl),
	} {
		if got != nil {
			t.Errorf("%s: want no controller", name)
		}
	}
	if s.newCallCue(&CallFlow{Cue: cue}, out, tl) == nil {
		t.Fatal("a configured cue with an output must yield a controller")
	}
}
