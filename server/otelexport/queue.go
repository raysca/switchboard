package otelexport

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// queueProcessor is a bounded, non-blocking SpanProcessor: OnEnd never waits,
// a full queue drops the span and counts it, and export errors are counted.
// ponytail: own ~50 lines instead of the SDK batch processor because that one
// drops silently, and "drop + count" is a requirement.
type queueProcessor struct {
	exp     sdktrace.SpanExporter
	ch      chan sdktrace.ReadOnlySpan
	stop    chan struct{}
	done    chan struct{}
	once    sync.Once
	cancel  context.CancelFunc // aborts an in-flight export when Shutdown gives up
	ctx     context.Context
	dropped atomic.Uint64
	failed  atomic.Uint64
}

func newQueueProcessor(exp sdktrace.SpanExporter, size int) *queueProcessor {
	q := &queueProcessor{exp: exp, ch: make(chan sdktrace.ReadOnlySpan, size), stop: make(chan struct{}), done: make(chan struct{})}
	q.ctx, q.cancel = context.WithCancel(context.Background())
	go q.run()
	return q
}

func (q *queueProcessor) OnStart(context.Context, sdktrace.ReadWriteSpan) {}

func (q *queueProcessor) OnEnd(s sdktrace.ReadOnlySpan) {
	select {
	case q.ch <- s:
	default:
		q.dropped.Add(1)
	}
}

func (q *queueProcessor) ForceFlush(context.Context) error { return nil } // ponytail: flushed on Shutdown only

func (q *queueProcessor) export(batch []sdktrace.ReadOnlySpan) {
	if len(batch) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(q.ctx, 10*time.Second)
	defer cancel()
	if err := q.exp.ExportSpans(ctx, batch); err != nil {
		q.failed.Add(uint64(len(batch)))
	}
}

func (q *queueProcessor) run() {
	defer close(q.done)
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	var batch []sdktrace.ReadOnlySpan
	for {
		select {
		case s := <-q.ch:
			if batch = append(batch, s); len(batch) >= 512 {
				q.export(batch)
				batch = nil
			}
		case <-tick.C:
			q.export(batch)
			batch = nil
		case <-q.stop:
			for {
				select {
				case s := <-q.ch:
					batch = append(batch, s)
				default:
					q.export(batch)
					return
				}
			}
		}
	}
}

// Shutdown drains the queue, then closes the exporter; ctx bounds the wait.
func (q *queueProcessor) Shutdown(ctx context.Context) error {
	q.once.Do(func() { close(q.stop) })
	select {
	case <-q.done:
		return q.exp.Shutdown(ctx)
	case <-ctx.Done():
		q.cancel() // a hung collector must not keep the worker alive past Shutdown
		return ctx.Err()
	}
}
