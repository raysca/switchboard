package core

import (
	"context"
	"reflect"
	"testing"
	"time"
)

type rejectedParentContextKey struct{}

type rejectedParentProbe struct {
	gate    chan struct{}
	entered chan struct{}
	started chan context.Context
	ended   chan string
}

func (p *rejectedParentProbe) StartSpan(ctx context.Context, event TraceEvent) Span {
	if event.Name == "hold" {
		close(p.entered)
		<-p.gate
	}
	if event.Name == "child" {
		p.started <- ctx
	}
	return rejectedParentProbeSpan{p: p, name: event.Name}
}

type rejectedParentProbeSpan struct {
	p    *rejectedParentProbe
	name string
}

func (rejectedParentProbeSpan) SetAttribute(string, any) {}
func (rejectedParentProbeSpan) AddEvent(TraceEvent)      {}
func (s rejectedParentProbeSpan) End(error)              { s.p.ended <- s.name }

func waitRejectedParentEnd(t *testing.T, p *rejectedParentProbe, name string) {
	t.Helper()
	select {
	case got := <-p.ended:
		if got != name {
			t.Fatalf("end=%s, want %s", got, name)
		}
	case <-time.After(time.Second):
		t.Fatalf("missing %s end", name)
	}
}

func assertRejectedParentChild(t *testing.T, p *rejectedParentProbe) {
	t.Helper()
	select {
	case ctx := <-p.started:
		if SpanFromContext(ctx) != nil {
			t.Fatal("rejected parent reached exporter")
		}
		if ctx.Value(rejectedParentContextKey{}) != "kept" {
			t.Fatal("non-span context value lost")
		}
		if reflect.TypeOf(ctx).String() == "*context.valueCtx" {
			t.Fatalf("parent was cleared with a nil context value: %T", ctx)
		}
	case <-time.After(time.Second):
		t.Fatal("child start blocked by rejected parent")
	}
	waitRejectedParentEnd(t, p, "child")
}

func TestAsyncTracerRejectedParentCrossDispatcher(t *testing.T) {
	p := &rejectedParentProbe{started: make(chan context.Context, 1), ended: make(chan string, 3)}
	a := NewAsyncTracer(p, 1)
	a.Close()
	rejected := a.StartSpan(context.Background(), NewTraceEvent("rejected", time.Now(), "s", "", 0, TracePolicy{}, nil))
	b := NewAsyncTracer(p, 4)
	ctx := ContextWithSpan(context.WithValue(context.Background(), rejectedParentContextKey{}, "kept"), rejected)
	child := b.StartSpan(ctx, NewTraceEvent("child", time.Now(), "s", "", 0, TracePolicy{}, nil))
	child.End(nil)
	b.Close()
	assertRejectedParentChild(t, p)
}

func TestAsyncTracerRejectedParentSameDispatcher(t *testing.T) {
	p := &rejectedParentProbe{gate: make(chan struct{}), entered: make(chan struct{}), started: make(chan context.Context, 1), ended: make(chan string, 3)}
	a := NewAsyncTracer(p, 1)
	hold := a.StartSpan(context.Background(), NewTraceEvent("hold", time.Now(), "s", "", 0, TracePolicy{}, nil))
	select {
	case <-p.entered:
	case <-time.After(time.Second):
		t.Fatal("hold span did not enter exporter")
	}
	filler := a.StartSpan(context.Background(), NewTraceEvent("filler", time.Now(), "s", "", 0, TracePolicy{}, nil))
	rejected := a.StartSpan(context.Background(), NewTraceEvent("rejected", time.Now(), "s", "", 0, TracePolicy{}, nil))
	hold.End(nil)
	filler.End(nil)
	close(p.gate)
	waitRejectedParentEnd(t, p, "hold")
	waitRejectedParentEnd(t, p, "filler")
	ctx := ContextWithSpan(context.WithValue(context.Background(), rejectedParentContextKey{}, "kept"), rejected)
	child := a.StartSpan(ctx, NewTraceEvent("child", time.Now(), "s", "", 0, TracePolicy{}, nil))
	child.End(nil)
	a.Close()
	assertRejectedParentChild(t, p)
}
