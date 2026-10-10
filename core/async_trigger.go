package core

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// AsyncTriggerConfig tunes when a finished async tool call may start a model turn.
type AsyncTriggerConfig struct {
	// Busy reports that an assistant reply is generating or playing
	// (AssistantContextProcessor.Active). May be nil.
	Busy func() bool
	// MaxQueue bounds pending results (default 8). On overflow the oldest is
	// injected into history at once without its own turn (the next triggered
	// turn's model sees it), so a result is never silently lost.
	MaxQueue int
	// Settle is how long the call must stay idle before a turn starts (default 300ms).
	Settle time.Duration
	// Poll is the idle re-check interval (default 50ms).
	Poll time.Duration
	// MaxWait caps how long one busy signal (user speaking, bot speaking, reply
	// active) can defer delivery, so a lost "stopped" event cannot starve it (default 60s).
	MaxWait time.Duration
	// OnTriggered / OnDropped, if set, mirror TriggeredCount / DroppedCount (metrics).
	OnTriggered, OnDropped func()
}

// AsyncTrigger is a pass-through stage placed directly before the LLM. It
// receives finished async tool results (Deliver), waits until the user is not
// speaking and the bot is neither speaking nor replying, then injects their
// envelopes into the conversation and starts one model turn for all of them.
// Frames it sees tell it the call's state: VAD/turn events and committed user
// turns downstream, bot speaking upstream.
type AsyncTrigger struct {
	cfg     AsyncTriggerConfig
	session SessionController
	index   int

	mu      sync.Mutex
	queue   []AsyncToolResult
	running bool
	seq     int

	// monotonic offset from monoBase; 0 means off.
	vadUser, turnUser, bot, reply atomic.Int64

	DroppedCount   atomic.Uint64 // results whose injection failed (loop closed)
	TriggeredCount atomic.Uint64 // model turns started
}

func NewAsyncTrigger(cfg AsyncTriggerConfig) *AsyncTrigger {
	if cfg.MaxQueue <= 0 {
		cfg.MaxQueue = 8
	}
	if cfg.Settle <= 0 {
		cfg.Settle = 300 * time.Millisecond
	}
	if cfg.Poll <= 0 {
		cfg.Poll = 50 * time.Millisecond
	}
	if cfg.MaxWait <= 0 {
		cfg.MaxWait = time.Minute
	}
	return &AsyncTrigger{cfg: cfg}
}

// monoBase carries a monotonic reading. Offsets from it ignore wall-clock steps.
var monoBase = time.Now()

func (t *AsyncTrigger) Attach(s SessionController, index int) error {
	t.session, t.index = s, index
	return nil
}

func set(a *atomic.Int64, on bool) {
	if !on {
		a.Store(0)
	} else if a.Load() == 0 {
		a.Store(int64(time.Since(monoBase)))
	}
}

func (t *AsyncTrigger) ProcessFrame(_ context.Context, f Frame, dir Direction, e Emitter) error {
	if dir == Upstream {
		if v, ok := f.(*SpeakingStateFrame); ok {
			set(&t.bot, v.State == SpeakingStarted)
		}
		return e.PushDirection(f, dir)
	}
	switch v := f.(type) {
	case *UserStartedSpeakingFrame:
		set(&t.vadUser, true)
	case *UserStoppedSpeakingFrame:
		set(&t.vadUser, false)
	case *TurnEventFrame:
		switch v.Kind {
		case TurnEventStarted:
			set(&t.turnUser, true)
		case TurnEventCompleted, TurnEventInterrupted, TurnEventNoSpeechTimeout:
			set(&t.turnUser, false)
		}
	case *UserMessageFrame:
		set(&t.turnUser, false)
		t.reply.Store(int64(time.Since(monoBase)))
	}
	return e.PushDirection(f, dir)
}

// Deliver queues a finished async call. It blocks only on queue overflow,
// while the oldest result is injected (at most one tool round).
func (t *AsyncTrigger) Deliver(r AsyncToolResult) {
	if t.session == nil {
		return
	}
	t.mu.Lock()
	var early func() error
	if len(t.queue) >= t.cfg.MaxQueue {
		early = t.queue[0].Inject
		t.queue = t.queue[1:]
	}
	t.queue = append(t.queue, r)
	start := !t.running
	t.running = true
	t.mu.Unlock()
	if early != nil && early() != nil { // may wait for a tool round (the loop's lock)
		t.dropped()
	}
	if start && t.session.Go(t.run) != nil { // session closed
		t.mu.Lock()
		t.running, t.queue = false, nil
		t.mu.Unlock()
	}
}

func (t *AsyncTrigger) on(a *atomic.Int64) bool {
	since := a.Load()
	if since == 0 {
		return false
	}
	return time.Duration(int64(time.Since(monoBase))-since) < t.cfg.MaxWait
}

func (t *AsyncTrigger) idle() bool {
	if t.on(&t.vadUser) || t.on(&t.turnUser) || t.on(&t.bot) {
		return false
	}
	return !(t.cfg.Busy != nil && t.cfg.Busy() && t.on(&t.reply))
}

func (t *AsyncTrigger) run(ctx context.Context) {
	tick := time.NewTicker(t.cfg.Poll)
	defer tick.Stop()
	var idleSince time.Time
	for {
		t.mu.Lock()
		if len(t.queue) == 0 {
			t.running = false
			t.mu.Unlock()
			return
		}
		t.mu.Unlock()
		select {
		case <-ctx.Done():
			t.mu.Lock()
			t.running, t.queue = false, nil
			t.mu.Unlock()
			return
		case <-tick.C:
		}
		if !t.idle() {
			idleSince = time.Time{}
			continue
		}
		if idleSince.IsZero() {
			idleSince = time.Now()
		}
		if time.Since(idleSince) < t.cfg.Settle {
			continue
		}
		idleSince = time.Time{}
		t.fire()
	}
}

// fire injects every queued envelope, then starts one model turn at the stage after this one.
func (t *AsyncTrigger) fire() {
	t.mu.Lock()
	batch := t.queue
	t.queue = nil
	t.seq++
	turn := fmt.Sprintf("async-%d", t.seq)
	t.mu.Unlock()
	var names string
	injected := 0
	for _, r := range batch {
		if r.Inject() != nil {
			t.dropped()
			continue
		}
		if injected++; injected <= 3 {
			names += " " + r.Call.Name
		}
	}
	if injected == 0 {
		return
	}
	t.reply.Store(int64(time.Since(monoBase)))
	um := &UserMessageFrame{Turn: turn, Content: "[async tool result:" + names + "]", Gen: t.session.CurrentGeneration(), Timestamp: time.Now()}
	if t.session.Reenter(um, Downstream, t.index+1, 0) == nil {
		t.TriggeredCount.Add(1)
		if t.cfg.OnTriggered != nil {
			t.cfg.OnTriggered()
		}
	}
}

func (t *AsyncTrigger) dropped() {
	t.DroppedCount.Add(1)
	if t.cfg.OnDropped != nil {
		t.cfg.OnDropped()
	}
}
