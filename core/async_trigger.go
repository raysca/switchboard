package core

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// AsyncTriggerConfig tunes when a finished async tool call may start a model turn.
type AsyncTriggerConfig struct {
	// Busy reports that an assistant reply is generating or playing
	// (AssistantContextProcessor.Active). May be nil.
	Busy func() bool
	// MaxQueue bounds pending results (default 8). On overflow the oldest tool result or
	// route flip is injected into history at once without its own turn (the next triggered
	// turn's model sees it), so a result is never silently lost. Operator items count
	// against a separate cap of the same size and are refused when it is full.
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
	closed  bool // the session ended: nothing more is accepted
	seq     int

	// monotonic offset from monoBase; 0 means off.
	vadUser, turnUser, bot, reply atomic.Int64
	ready                         atomic.Bool

	DroppedCount   atomic.Uint64 // results whose injection failed (loop closed)
	TriggeredCount atomic.Uint64 // model turns started
}

var (
	// ErrAsyncQueueFull: an operator delivery would exceed the operator cap (MaxQueue).
	ErrAsyncQueueFull = errors.New("async trigger: too many pending operator items")
	// ErrAsyncClosed: the call has ended; nothing was queued.
	ErrAsyncClosed = errors.New("async trigger: the call has ended")
	// ErrAsyncBusy: the call is live but could not start delivery right now (its task budget
	// is full); nothing was queued, and a retry may succeed.
	ErrAsyncBusy = errors.New("async trigger: the call is busy; retry")
)

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
	t.ready.Store(true)
	return nil
}

// Ready reports that the trigger is attached to a started session, so Deliver can run.
func (t *AsyncTrigger) Ready() bool { return t.ready.Load() }

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
		t.rideTurn()
	}
	return e.PushDirection(f, dir)
}

// rideTurn hands pending operator items to a model turn that is starting anyway (a caller
// turn reaching the model): notes go into the history now, before the model reads it, and a
// requested reply is this turn, so it does not start another one. The trigger's own turns
// enter after this stage and never come through here.
func (t *AsyncTrigger) rideTurn() {
	t.mu.Lock()
	var ride []AsyncToolResult
	keep := t.queue[:0]
	for _, r := range t.queue {
		if r.Operator {
			ride = append(ride, r)
		} else {
			keep = append(keep, r)
		}
	}
	t.queue = keep
	t.mu.Unlock()
	for _, r := range ride {
		if r.Inject() != nil {
			t.dropped()
		}
	}
}

// Deliver queues a finished async call or a route flip. It blocks only on queue overflow,
// while the oldest such item is injected (at most one tool round). Operator items never count
// toward that overflow and are never evicted by it.
func (t *AsyncTrigger) Deliver(r AsyncToolResult) {
	if t.session == nil {
		return
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		t.dropped()
		return
	}
	var early func() error
	evictedWantedTurn := false
	if count(t.queue, false) >= t.cfg.MaxQueue {
		i := slices.IndexFunc(t.queue, func(q AsyncToolResult) bool { return !q.Operator })
		early = t.queue[i].Inject
		evictedWantedTurn = !t.queue[i].NoTurn
		t.queue = slices.Delete(t.queue, i, i+1)
	}
	t.queue = append(t.queue, r)
	if evictedWantedTurn { // the evicted item is injected now, so its reply rides on the next one
		t.queue[0].NoTurn = false
	}
	start := !t.running
	t.running = true
	t.mu.Unlock()
	if early != nil && early() != nil { // may wait for a tool round (the loop's lock)
		t.dropped()
	}
	if start {
		if err := t.session.Go(t.run); err != nil {
			_ = t.startFailed(err, true)
		}
	}
}

// DeliverOperator queues operator notes and run requests, all or none: ErrAsyncQueueFull when
// they would exceed the operator cap, ErrAsyncClosed when the call has ended.
func (t *AsyncTrigger) DeliverOperator(items ...AsyncToolResult) error {
	if t.session == nil {
		return ErrAsyncClosed
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return ErrAsyncClosed
	}
	if count(t.queue, true)+len(items) > t.cfg.MaxQueue {
		t.mu.Unlock()
		return ErrAsyncQueueFull
	}
	for _, r := range items {
		r.Operator = true
		t.queue = append(t.queue, r)
	}
	start := !t.running
	t.running = true
	t.mu.Unlock()
	if start {
		if err := t.session.Go(t.run); err != nil {
			return t.startFailed(err, false)
		}
	}
	return nil
}

// startFailed handles a delivery runner that could not start. A closed session ends the
// trigger. Anything else (a full task budget) is transient: the queue is emptied (counted
// as dropped for tool results and flips; an operator request is refused instead, so it can be
// retried) and the trigger stays usable for later deliveries.
func (t *AsyncTrigger) startFailed(err error, countDrops bool) error {
	if errors.Is(err, ErrSessionClosed) {
		t.close()
		return ErrAsyncClosed
	}
	t.mu.Lock()
	n := len(t.queue)
	t.running, t.queue = false, nil
	t.mu.Unlock()
	if countDrops {
		for range n {
			t.dropped()
		}
	}
	return ErrAsyncBusy
}

// count is how many queued items are (operator true) or are not operator items.
func count(q []AsyncToolResult, operator bool) int {
	n := 0
	for _, r := range q {
		if r.Operator == operator {
			n++
		}
	}
	return n
}

// close ends the trigger with its session: every queued item is dropped and counted.
func (t *AsyncTrigger) close() {
	t.mu.Lock()
	n := len(t.queue)
	t.running, t.queue, t.closed = false, nil, true
	t.mu.Unlock()
	for range n {
		t.dropped()
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
			t.close()
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
	turns := 0
	for _, r := range batch {
		if r.Inject() != nil {
			t.dropped()
			continue
		}
		if r.NoTurn {
			continue
		}
		if turns++; turns <= 3 {
			names += " " + r.Call.Name
		}
	}
	if turns == 0 {
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
