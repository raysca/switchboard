package server

import (
	"context"
	"sync"
	"sync/atomic"
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

func prep() core.RouteEvent {
	return core.RouteEvent{Kind: core.RoutePreparing, From: "a", To: "b"}
}
func active() core.RouteEvent {
	return core.RouteEvent{Kind: core.RouteActive, From: "a", To: "b"}
}

func TestCueFastRoutePlaysNothing(t *testing.T) {
	c, p, _ := newTestCue(t)
	c.onRoute(prep())
	c.onRoute(active())
	c.botAudio() // the new step answers before after_ms
	time.Sleep(60 * time.Millisecond)
	if s, _ := p.counts(); s != 0 {
		t.Fatal("a route faster than after_ms must stay silent")
	}
}

func TestCueSlowRoutePlaysUntilFirstAudio(t *testing.T) {
	c, p, r := newTestCue(t)
	c.onRoute(prep())
	c.botAudio() // the old step's farewell: must not stop anything
	waitCue(t, "cue start", func() bool { s, _ := p.counts(); return s == 1 })
	c.botAudio() // farewell tail before the flip: still not the new step
	if _, st := p.counts(); st != 0 {
		t.Fatal("audio before the route is active must not stop the cue")
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
		"failed":   func(c *cueController) { c.onRoute(core.RouteEvent{Kind: core.RouteFailed}) },
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
					t.Fatalf("a finished route must not restart the cue (starts=%d)", s)
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
	out := ws.NewCueUnderlay()
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

func newTestHold(t *testing.T, blocked func() bool) (*holdCue, *fakePlayer, *cueRecorder) {
	t.Helper()
	p, r := &fakePlayer{}, &cueRecorder{}
	if blocked == nil {
		blocked = func() bool { return false }
	}
	h := newHoldCue(p, func() ws.Underlay { return nil }, 20*time.Millisecond, blocked, r.record)
	h.cap = 200 * time.Millisecond
	return h, p, r
}

func TestHoldPlaysOnlyOnceAToolHasRunQuietlyAndStopsWhenItReturns(t *testing.T) {
	h, p, r := newTestHold(t, nil)
	h.toolStarted()
	h.toolDone() // a quick tool stays silent
	time.Sleep(60 * time.Millisecond)
	if s, _ := p.counts(); s != 0 {
		t.Fatal("a tool faster than after must stay silent")
	}
	h.toolStarted()
	waitCue(t, "hold start", func() bool { s, _ := p.counts(); return s == 1 })
	h.toolDone()
	if ev := r.events(); len(ev) != 2 || ev[0] != "cue_start:tool" || ev[1] != "cue_stop:tool_done" {
		t.Fatalf("events = %v", ev)
	}
	time.Sleep(60 * time.Millisecond)
	if s, st := p.counts(); s != 1 || st != 1 {
		t.Fatalf("starts=%d stops=%d", s, st)
	}
}

func TestHoldPausesForBotAudioAndCallerSpeechAndRearms(t *testing.T) {
	h, p, r := newTestHold(t, nil)
	h.toolStarted()
	defer h.toolDone()
	waitCue(t, "hold start", func() bool { s, _ := p.counts(); return s == 1 })
	h.botAudio()
	if ev := r.events(); ev[len(ev)-1] != "cue_stop:bot_audio" {
		t.Fatalf("events = %v", ev)
	}
	waitCue(t, "re-arm after the bot is quiet", func() bool { s, _ := p.counts(); return s == 2 })
	h.userSpeech(true)
	if ev := r.events(); ev[len(ev)-1] != "cue_stop:barge_in" {
		t.Fatalf("events = %v", ev)
	}
	time.Sleep(80 * time.Millisecond)
	if s, _ := p.counts(); s != 2 {
		t.Fatal("the cue must wait for the caller to stop speaking")
	}
	h.userSpeech(false)
	waitCue(t, "restart after the caller stops", func() bool { s, _ := p.counts(); return s == 3 })
}

func TestHoldStopsOnlyWhenTheLastOfTwoToolsReturns(t *testing.T) {
	h, p, _ := newTestHold(t, nil)
	h.toolStarted()
	h.toolStarted()
	waitCue(t, "hold start", func() bool { s, _ := p.counts(); return s == 1 })
	h.toolDone()
	if _, st := p.counts(); st != 0 {
		t.Fatal("one tool still runs: the sound must keep playing")
	}
	h.toolDone()
	if s, st := p.counts(); s != 1 || st != 1 {
		t.Fatalf("starts=%d stops=%d", s, st)
	}
}

func TestHoldStopsWhenARouteStartsPreparingAndNeverStartsWhileTheRouteCueWaits(t *testing.T) {
	var route atomic.Bool
	h, p, r := newTestHold(t, route.Load)
	h.toolStarted()
	defer h.toolDone()
	waitCue(t, "hold start", func() bool { s, _ := p.counts(); return s == 1 })
	h.onRoute(prep())
	if ev := r.events(); ev[len(ev)-1] != "cue_stop:route" {
		t.Fatalf("a route preparing must finish the hold first: %v", r.events())
	}
	time.Sleep(80 * time.Millisecond)
	if s, _ := p.counts(); s != 1 {
		t.Fatal("the hold must not restart while a route prepares")
	}
	route.Store(true) // the route cue waits
	h.onRoute(core.RouteEvent{Kind: core.RouteFailed})
	time.Sleep(80 * time.Millisecond)
	if s, _ := p.counts(); s != 1 {
		t.Fatal("the hold must not start while the route cue waits")
	}
	route.Store(false)
	waitCue(t, "start once the route cue is done", func() bool { s, _ := p.counts(); return s == 2 })
}

func TestHoldStopsAtTheCapAndStaysOffUntilAllToolsReturn(t *testing.T) {
	h, p, r := newTestHold(t, nil)
	h.toolStarted()
	waitCue(t, "hold start", func() bool { s, _ := p.counts(); return s == 1 })
	// The player's Stop is counted before the hold records the event, so wait for the event itself.
	waitCue(t, "cap stop", func() bool { ev := r.events(); return len(ev) > 0 && ev[len(ev)-1] == "cue_stop:cap" })
	if _, st := p.counts(); st != 1 {
		t.Fatalf("cap stops = %d, want 1", st)
	}
	h.botAudio()
	time.Sleep(80 * time.Millisecond)
	if s, _ := p.counts(); s != 1 {
		t.Fatal("a capped hold must not restart for the same tool")
	}
	h.toolDone()
	h.toolStarted() // a new busy period may play again
	defer h.toolDone()
	waitCue(t, "second period", func() bool { s, _ := p.counts(); return s == 2 })
}

func TestHoldIsNilSafe(t *testing.T) {
	var h *holdCue
	h.toolStarted()
	h.botAudio()
	h.userSpeech(true)
	h.onRoute(prep())
	h.toolDone()
	if holdTools(nil, nil, nil) != nil {
		t.Fatal("no hold: the executor is returned as is")
	}
}

type holdExec struct{ gate chan struct{} }

func (e holdExec) Execute(ctx context.Context, c core.ToolCall) (core.ToolResult, error) {
	select {
	case <-e.gate:
	case <-ctx.Done():
		return core.ToolResult{}, ctx.Err()
	}
	if c.Name == "panics" {
		panic("tool exploded")
	}
	return core.ToolResult{CallID: c.ID}, nil
}

func TestHoldToolsArmsOnlyForSynchronousMCPTools(t *testing.T) {
	defs := []core.ToolDefinition{
		{Name: "mcp_sync", RemoteName: "sync"},
		{Name: "mcp_async", RemoteName: "async", Policy: core.ToolPolicy{Async: true}},
		{Name: "panics", RemoteName: "panics"},
		{Name: "transfer_to_sales"},
		{Name: "end_call"},
		{Name: "send_dtmf"},
	}
	for _, c := range []struct {
		tool string
		arms bool
	}{{"mcp_sync", true}, {"mcp_async", false}, {"transfer_to_sales", false}, {"end_call", false}, {"send_dtmf", false}} {
		t.Run(c.tool, func(t *testing.T) {
			h, p, _ := newTestHold(t, nil)
			gate := make(chan struct{})
			exec := holdTools(holdExec{gate}, defs, h)
			done := make(chan struct{})
			go func() { _, _ = exec.Execute(t.Context(), core.ToolCall{ID: "1", Name: c.tool}); close(done) }()
			if c.arms {
				waitCue(t, "hold start", func() bool { s, _ := p.counts(); return s == 1 })
			} else {
				time.Sleep(80 * time.Millisecond)
				if s, _ := p.counts(); s != 0 {
					t.Fatalf("%s must not arm the hold", c.tool)
				}
			}
			close(gate)
			<-done
			if h.running.Load() != 0 {
				t.Fatal("the in-flight count must return to zero")
			}
		})
	}
}

func TestHoldToolsStopsTheSoundWhenTheToolIsCancelledOrPanics(t *testing.T) {
	defs := []core.ToolDefinition{{Name: "slow", RemoteName: "slow"}, {Name: "panics", RemoteName: "panics"}}
	t.Run("cancelled", func(t *testing.T) {
		h, p, r := newTestHold(t, nil)
		ctx, cancel := context.WithCancel(t.Context())
		exec := holdTools(holdExec{make(chan struct{})}, defs, h)
		done := make(chan struct{})
		go func() { _, _ = exec.Execute(ctx, core.ToolCall{ID: "1", Name: "slow"}); close(done) }()
		waitCue(t, "hold start", func() bool { s, _ := p.counts(); return s == 1 })
		cancel()
		<-done
		if ev := r.events(); ev[len(ev)-1] != "cue_stop:tool_done" {
			t.Fatalf("events = %v", ev)
		}
	})
	t.Run("panics", func(t *testing.T) {
		h, p, _ := newTestHold(t, nil)
		gate := make(chan struct{})
		exec := holdTools(holdExec{gate}, defs, h)
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer func() { _ = recover() }()
			_, _ = exec.Execute(t.Context(), core.ToolCall{ID: "1", Name: "panics"})
		}()
		waitCue(t, "hold start", func() bool { s, _ := p.counts(); return s == 1 })
		close(gate)
		<-done
		if _, st := p.counts(); st != 1 || h.running.Load() != 0 {
			t.Fatalf("a panicking tool must stop the sound: stops=%d running=%d", st, h.running.Load())
		}
	})
}

// M1: the hold ends with the loop's outcome, not only when the executor really returns: an executor that
// ignores its context and is abandoned must not leave the sound playing.
func TestHoldToolsStopsWhenTheContextEndsEvenIfTheExecutorIgnoresIt(t *testing.T) {
	h, p, r := newTestHold(t, nil)
	gate := make(chan struct{})
	defer close(gate)
	exec := holdTools(core.ToolExecutorFunc(func(_ context.Context, c core.ToolCall) (core.ToolResult, error) {
		<-gate // never looks at ctx
		return core.ToolResult{CallID: c.ID}, nil
	}), []core.ToolDefinition{{Name: "slow", RemoteName: "slow"}}, h)
	ctx, cancel := context.WithCancel(t.Context())
	go func() { _, _ = exec.Execute(ctx, core.ToolCall{ID: "1", Name: "slow"}) }()
	waitCue(t, "hold start", func() bool { s, _ := p.counts(); return s == 1 })
	cancel()
	waitCue(t, "hold stop although the executor still runs", func() bool { _, st := p.counts(); return st == 1 })
	if h.running.Load() != 0 {
		t.Fatalf("running = %d, want 0", h.running.Load())
	}
	if ev := r.events(); ev[len(ev)-1] != "cue_stop:tool_done" {
		t.Fatalf("events = %v", ev)
	}
}

type panicPlayer struct{ fakePlayer }

func (*panicPlayer) Start(ws.Underlay) { panic("player") }

func TestCueTimerPanicIsRecordedNotFatal(t *testing.T) {
	r := &cueRecorder{}
	c := newCueController(&panicPlayer{}, func() ws.Underlay { return nil }, 20*time.Millisecond, r.record)
	c.onRoute(prep())
	waitCue(t, "panic recorded", func() bool { return len(r.events()) > 0 })
	if ev := r.events(); ev[0] != "error:cue timer panicked" {
		t.Fatalf("events = %v", ev)
	}
}
