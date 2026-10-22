package core

import (
	"context"
	"sync"
	"sync/atomic"
)

const defaultTraceQueueSize = 256

type traceOperationKind uint8

const (
	traceStart traceOperationKind = iota + 1
	traceAttribute
	traceEvent
	traceEnd
)

type traceOperation struct {
	kind   traceOperationKind
	ctx    context.Context // trace parentage
	parent Span
	proxy  *asyncSpan
	event  TraceEvent
	key    string
	value  any
	err    error
}

// AsyncTracer isolates runtime callers from slow exporters. It preserves
// operation order and span parentage on one worker; a full queue drops the
// incoming operation rather than blocking the voice runtime.
type AsyncTracer struct {
	tracer   Tracer
	queue    chan traceOperation
	wake     chan struct{}
	capacity int
	mu       sync.Mutex
	closed   bool
	active   int

	terminalMu      sync.Mutex
	terminals       []traceOperation
	terminalPending map[*asyncSpan]struct{}
}

// AsyncTraceObserver is optionally implemented by a tracer wrapped in an
// AsyncTracer: AsyncTraceOpened runs synchronously in NewAsyncTracer, and
// AsyncTraceDrained runs on the worker after Close once every queued
// operation has been delivered. Exporters use the pair to flush only after
// span ends that are still in flight when the session has already closed.
type AsyncTraceObserver interface {
	AsyncTraceOpened()
	AsyncTraceDrained()
}

// NewAsyncTracer creates a bounded asynchronous dispatcher. A non-positive
// queue size selects the conservative 256-operation default.
func NewAsyncTracer(tracer Tracer, queueSize int) *AsyncTracer {
	if queueSize <= 0 {
		queueSize = defaultTraceQueueSize
	}
	if tracer == nil {
		tracer = NoopTracer{}
	}
	dispatcher := &AsyncTracer{
		tracer: tracer,
		queue:  make(chan traceOperation, queueSize),
		wake:   make(chan struct{}, 1),
		// One callback can be active while the bounded queue is full.
		capacity:        queueSize + 1,
		terminalPending: make(map[*asyncSpan]struct{}),
	}
	if o, ok := tracer.(AsyncTraceObserver); ok {
		o.AsyncTraceOpened()
	}
	go dispatcher.run()
	return dispatcher
}

// Close stops accepting new work and lets the worker drain queued operations.
// It does not wait for exporter callbacks, which may be blocked independently.
func (t *AsyncTracer) Close() {
	t.mu.Lock()
	if !t.closed {
		t.closed = true
		close(t.queue)
	}
	t.mu.Unlock()
}

func (t *AsyncTracer) StartSpan(ctx context.Context, event TraceEvent) Span {
	if ctx == nil {
		ctx = context.Background()
	}
	proxy := &asyncSpan{tracer: t, ready: make(chan struct{})}
	if t.enqueueStart(traceOperation{kind: traceStart, ctx: ctx, parent: SpanFromContext(ctx), proxy: proxy, event: event}) {
		proxy.accepted.Store(true)
	} else {
		close(proxy.ready)
	}
	return proxy
}

// enqueueStart reserves one bounded terminal slot before admitting a start.
// A caller that never ends a span therefore eventually disables new trace
// starts rather than allowing an unbounded terminal backlog.
func (t *AsyncTracer) enqueueStart(operation traceOperation) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.active >= t.capacity {
		return false
	}
	select {
	case t.queue <- operation:
		t.active++
		return true
	default:
		return false
	}
}

func (t *AsyncTracer) enqueue(operation traceOperation) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return false
	}
	select {
	case t.queue <- operation:
		return true
	default:
		return false
	}
}

// enqueueTerminal is deliberately separate from the bounded best-effort
// operation queue. Once a start was accepted, its single terminal operation
// is retained until the worker can pair it with that span. This prevents an
// overloaded tracer from leaking accepted spans while keeping ordinary
// attributes/events non-blocking and bounded.
func (t *AsyncTracer) enqueueTerminal(operation traceOperation) {
	t.terminalMu.Lock()
	if _, exists := t.terminalPending[operation.proxy]; !exists {
		t.terminalPending[operation.proxy] = struct{}{}
		t.terminals = append(t.terminals, operation)
	}
	t.terminalMu.Unlock()
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

type asyncSpan struct {
	tracer   *AsyncTracer
	accepted atomic.Bool
	mu       sync.Mutex
	ended    bool
	pending  atomic.Int64
	// Written and read only by the dispatcher worker. Child starts can arrive
	// after the parent has ended and left the worker's active-span map.
	resolved Span
	ready    chan struct{} // close publishes immutable resolved across dispatchers
}

// A rejected parent must not be reinstalled as the child's parent, nor be
// replaced with a nil span value. Mask only the trace-parent key so the child
// starts at the trace root while cancellation and other context values survive.
type spanSuppressedContext struct{ context.Context }

func (c spanSuppressedContext) Value(key any) any {
	if _, ok := key.(spanContextKey); ok {
		return nil
	}
	return c.Context.Value(key)
}

func (s *asyncSpan) SetAttribute(key string, value any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return
	}
	s.pending.Add(1)
	if !s.tracer.enqueue(traceOperation{kind: traceAttribute, proxy: s, key: key, value: value}) {
		s.pending.Add(-1)
	}
}

func (s *asyncSpan) AddEvent(event TraceEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return
	}
	s.pending.Add(1)
	if !s.tracer.enqueue(traceOperation{kind: traceEvent, proxy: s, event: event}) {
		s.pending.Add(-1)
	}
}

func (s *asyncSpan) End(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return
	}
	s.ended = true
	if s.accepted.Load() {
		s.tracer.enqueueTerminal(traceOperation{kind: traceEnd, proxy: s, err: err})
	}
}

func (t *AsyncTracer) run() {
	spans := make(map[*asyncSpan]Span)
	queue := t.queue
	for {
		if queue == nil {
			t.drainTerminals(spans)
			if !t.hasTerminals() {
				if o, ok := t.tracer.(AsyncTraceObserver); ok {
					o.AsyncTraceDrained()
				}
				return
			}
			// A terminal without a started span can only belong to an operation
			// that was not accepted. Drop it rather than retaining state forever.
			t.dropUnstartedTerminals(spans)
			continue
		}
		// Preserve normal-operation order before ending a span. In particular,
		// a session close may enqueue terminals while child starts are still in
		// the bounded queue. Terminals become eligible as soon as that queue is
		// drained, including after Close closes it.
		if len(queue) == 0 {
			t.drainTerminals(spans)
		}
		var operation traceOperation
		var ok bool
		select {
		case operation, ok = <-queue:
			if !ok {
				queue = nil
				continue
			}
		case <-t.wake:
			continue
		}
		if operation.kind == traceStart {
			ctx := operation.ctx
			if operation.parent != nil {
				if parent := spansFor(operation.parent, spans); parent != nil {
					ctx = ContextWithSpan(ctx, parent)
				} else {
					ctx = spanSuppressedContext{ctx}
				}
			}
			spans[operation.proxy] = StartSpanSafe(ctx, t.tracer, operation.event)
			operation.proxy.resolved = spans[operation.proxy]
			close(operation.proxy.ready)
			continue
		}
		span := spans[operation.proxy]
		if span == nil {
			continue
		}
		switch operation.kind {
		case traceAttribute:
			span.SetAttribute(operation.key, operation.value)
		case traceEvent:
			span.AddEvent(operation.event)
		case traceEnd:
			span.End(operation.err)
			delete(spans, operation.proxy)
		}
		if operation.kind == traceAttribute || operation.kind == traceEvent {
			operation.proxy.pending.Add(-1)
		}
	}
}

func (t *AsyncTracer) drainTerminals(spans map[*asyncSpan]Span) {
	t.terminalMu.Lock()
	deferred := t.terminals[:0]
	ready := make([]traceOperation, 0, len(t.terminals))
	for _, operation := range t.terminals {
		span := spans[operation.proxy]
		if span == nil || operation.proxy.pending.Load() != 0 {
			deferred = append(deferred, operation)
			continue
		}
		ready = append(ready, operation)
		delete(t.terminalPending, operation.proxy)
	}
	t.terminals = deferred
	t.terminalMu.Unlock()
	// Exporter code is always invoked after the dispatcher lock is released;
	// exporters may block or re-enter tracing without deadlocking admission.
	for _, operation := range ready {
		span := spans[operation.proxy]
		span.End(operation.err)
		delete(spans, operation.proxy)
		t.releaseStart()
	}
}

func (t *AsyncTracer) releaseStart() {
	t.mu.Lock()
	if t.active > 0 {
		t.active--
	}
	t.mu.Unlock()
}

func (t *AsyncTracer) hasTerminals() bool {
	t.terminalMu.Lock()
	defer t.terminalMu.Unlock()
	return len(t.terminals) != 0
}

func (t *AsyncTracer) dropUnstartedTerminals(spans map[*asyncSpan]Span) {
	t.terminalMu.Lock()
	deferred := t.terminals[:0]
	for _, operation := range t.terminals {
		if _, started := spans[operation.proxy]; started {
			deferred = append(deferred, operation)
			continue
		}
		delete(t.terminalPending, operation.proxy)
		t.releaseStart()
	}
	t.terminals = deferred
	t.terminalMu.Unlock()
}

func spansFor(parent Span, spans map[*asyncSpan]Span) Span {
	if proxy, ok := parent.(*asyncSpan); ok {
		parent = spans[proxy]
		if parent == nil {
			<-proxy.ready
			parent = proxy.resolved
		}
	}
	if guarded, ok := parent.(*guardedSpan); ok {
		return guarded.inner
	}
	return parent
}
