package otelexport

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"govoice/core"
	"govoice/core/observe"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// keep: InMemoryExporter.Shutdown clears its spans, which would hide the flush.
type keep struct{ *tracetest.InMemoryExporter }

func (keep) Shutdown(context.Context) error { return nil }

func fixture(cfg Config) (*Exporter, *tracetest.InMemoryExporter, *time.Time) {
	mem := tracetest.NewInMemoryExporter()
	clock := t0
	e := build(cfg, keep{mem}, func() time.Time { return clock })
	return e, mem, &clock
}

func ev(e *Exporter, name string, at time.Time, turn core.TurnID, attrs map[string]any) core.TraceEvent {
	return core.NewTraceEvent(name, at, "7", turn, 1, e.Policy(), attrs)
}

func TestSpanTreeAndAttributes(t *testing.T) {
	e, mem, clock := fixture(Config{})
	st := e.Session("agent-1", "ver-9")
	ctx := context.Background()

	sess := st.StartSpan(ctx, ev(e, "session.start", t0, "", nil))
	turn := st.StartSpan(ctx, ev(e, "turn.start", t0.Add(time.Second), "t1", nil))
	llm := st.StartSpan(ctx, ev(e, "llm.request", t0.Add(2*time.Second), "t1", map[string]any{"provider": "google", "model": "gemini", "quantity": 42, "unit": "tokens"}))
	tts := st.StartSpan(ctx, ev(e, "tts.request", t0.Add(3*time.Second), "t1", map[string]any{"provider": "cartesia"}))
	if _, ok := st.StartSpan(ctx, ev(e, "processor.frame", t0, "t1", nil)).(noop); !ok {
		t.Fatal("frame spans must not be exported")
	}
	llm.AddEvent(ev(e, "llm.failure", t0.Add(2500*time.Millisecond), "t1", map[string]any{"provider": "google"}))
	st.Latency(observe.Measurement{Stage: observe.StageLLMTTFT, Turn: "t1", Value: 300 * time.Millisecond})
	*clock = t0.Add(4 * time.Second)
	llm.End(errors.New("boom secret-token"))
	tts.End(nil)
	turn.End(nil)
	sess.End(nil)
	if err := e.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}

	spans := mem.GetSpans()
	by := map[string]tracetest.SpanStub{}
	for _, s := range spans {
		by[s.Name] = s
	}
	if len(spans) != 4 {
		t.Fatalf("want 4 spans, got %d", len(spans))
	}
	root, turnS, llmS := by["govoice.session"], by["govoice.turn"], by["govoice.llm"]
	if turnS.Parent.SpanID() != root.SpanContext.SpanID() || llmS.Parent.SpanID() != turnS.SpanContext.SpanID() || by["govoice.tts"].Parent.SpanID() != turnS.SpanContext.SpanID() {
		t.Fatal("wrong parentage")
	}
	if llmS.EndTime.Sub(llmS.StartTime) != 2*time.Second {
		t.Fatalf("duration %v", llmS.EndTime.Sub(llmS.StartTime))
	}
	attrs := func(s tracetest.SpanStub) map[string]string {
		m := map[string]string{}
		for _, a := range s.Attributes {
			m[string(a.Key)] = a.Value.Emit()
		}
		return m
	}
	if a := attrs(root); a["govoice.agent.id"] != "agent-1" || a["govoice.agent.version"] != "ver-9" {
		t.Fatalf("root attrs %v", a)
	}
	a := attrs(llmS)
	if a["govoice.provider"] != "google" || a["govoice.model"] != "gemini" || a["govoice.quantity"] != "42" || a["govoice.turn.id"] != "t1" || a["error.class"] != "*errors.errorString" {
		t.Fatalf("llm attrs %v", a)
	}
	if attrs(turnS)["govoice.latency.llm_ttft_ms"] != "300" {
		t.Fatalf("turn attrs %v", attrs(turnS))
	}
	if len(llmS.Events) != 1 || llmS.Events[0].Name != "govoice.llm.failure" {
		t.Fatalf("events %v", llmS.Events)
	}
	if llmS.Status.Description != "" {
		t.Fatal("status description must not carry error text")
	}
}

func TestNoContentByDefault(t *testing.T) {
	for _, capture := range []bool{false, true} {
		e, mem, _ := fixture(Config{CaptureContent: capture})
		st := e.Session("a", "v")
		secret := map[string]any{"transcript": "SECRET-WORDS", "prompt": "SECRET-WORDS", "text": "SECRET-WORDS", "tool.arguments": "SECRET-WORDS", "provider": "p"}
		s := st.StartSpan(context.Background(), ev(e, "llm.request", t0, "t1", secret))
		s.SetAttribute("transcript", "SECRET-WORDS")
		s.AddEvent(ev(e, "llm.failure", t0, "t1", secret))
		s.End(errors.New("SECRET-WORDS"))
		_ = e.Shutdown(context.Background())
		var found bool
		for _, sp := range mem.GetSpans() {
			dump := sp.Name
			for _, a := range sp.Attributes {
				dump += string(a.Key) + a.Value.Emit()
			}
			for _, evn := range sp.Events {
				dump += evn.Name
				for _, a := range evn.Attributes {
					dump += string(a.Key) + a.Value.Emit()
				}
			}
			dump += sp.Status.Description
			found = found || strings.Contains(dump, "SECRET-WORDS")
		}
		// Content is dropped when capture is off. When on, only the explicit
		// opt-in admits transcript/prompt/text (tool.arguments needs a redactor
		// in core and is filtered there); error text is never exported.
		if !capture && found {
			t.Fatal("content leaked with capture_content=false")
		}
		if capture && !found {
			t.Fatal("capture_content=true should admit transcript text")
		}
	}
}

func TestSamplingZeroEmitsNothing(t *testing.T) {
	zero := 0.0
	e, mem, _ := fixture(Config{SamplingRatio: &zero})
	st := e.Session("a", "v")
	st.StartSpan(context.Background(), ev(e, "session.start", t0, "", nil)).End(nil)
	_ = e.Shutdown(context.Background())
	if n := len(mem.GetSpans()); n != 0 {
		t.Fatalf("exported %d spans at ratio 0", n)
	}
}

func TestQueueOverflowDropsAndCounts(t *testing.T) {
	// The worker only exports at 512 spans or on the 2s tick, so a fast worker can drain a
	// size-1 queue forever and never drop (the old flake). Park it inside a hung export
	// first; then the queue holds 1 span and every further span must drop.
	b := blockExp{entered: make(chan struct{}, 1)}
	e := build(Config{QueueSize: 1, ShutdownTimeout: 100 * time.Millisecond}, b, time.Now)
	st := e.Session("a", "v")
	emit := func() { st.StartSpan(context.Background(), ev(e, "session.start", t0, "", nil)).End(nil) }
	for range 512 {
		emit()
	}
	select {
	case <-b.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker never started the export")
	}
	before, _ := e.Stats()
	for range 10 {
		emit()
	}
	if d, _ := e.Stats(); d-before < 9 {
		t.Fatalf("dropped %d while parked, want >= 9 (queue holds 1 while the worker is parked)", d-before)
	}
	_ = e.Shutdown(context.Background())
}

func TestConfigValidate(t *testing.T) {
	if err := (Config{}).Validate(); err != nil {
		t.Fatal("disabled config must validate")
	}
	bad := 2.0
	for _, c := range []Config{
		{Enabled: true},
		{Enabled: true, Endpoint: "http://c:4318/v1/traces", Protocol: "grpc"},
		{Enabled: true, Endpoint: "http://c:4318/v1/traces", SamplingRatio: &bad},
		{Enabled: true, Endpoint: "https://user:pw@c:4318/v1/traces"},
	} {
		if c.Validate() == nil {
			t.Fatalf("expected error for %+v", c)
		}
	}
	if _, err := New(Config{Enabled: true, Endpoint: "http://c:4318", HeadersEnv: map[string]string{"Authorization": "X"}}, func(string) string { return "" }); err == nil {
		t.Fatal("empty header secret env must fail")
	}
}

type blockExp struct{ entered chan struct{} }

func (b blockExp) ExportSpans(ctx context.Context, _ []sdktrace.ReadOnlySpan) error {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return ctx.Err()
}
func (blockExp) Shutdown(context.Context) error { return nil }

func TestHangingCollectorShutdownIsBounded(t *testing.T) {
	b := blockExp{entered: make(chan struct{}, 1)}
	e := build(Config{ShutdownTimeout: 100 * time.Millisecond}, b, time.Now)
	st := e.Session("a", "v")
	for range 600 { // > one 512 batch, so an export is in flight
		st.StartSpan(context.Background(), ev(e, "session.start", t0, "", nil)).End(nil)
	}
	<-b.entered
	start := time.Now()
	if err := e.Shutdown(context.Background()); err == nil {
		t.Fatal("expected timeout error from hung collector")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Shutdown took %v, bound is 100ms", d)
	}
	select {
	case <-e.proc.done: // in-flight export cancelled, worker gone
	case <-time.After(time.Second):
		t.Fatal("export worker leaked past Shutdown")
	}
}

type pass struct{}

func (pass) ProcessFrame(_ context.Context, f core.Frame, d core.Direction, em core.Emitter) error {
	return em.PushDirection(f, d)
}

// A turn the runtime leaves open and a service span a provider never ends are
// both closed and exported when the session ends, and Shutdown waits for the
// asynchronously delivered session end.
func TestUnfinishedSpansClosedAtSessionEnd(t *testing.T) {
	e, mem, _ := fixture(Config{})
	pipe, _ := core.NewPipeline(pass{})
	sess, _ := core.NewSession(9, pipe, core.DefaultResourceBudget())
	st := e.Session("a", "v")
	if err := sess.SetTracer(st, e.Policy()); err != nil {
		t.Fatal(err)
	}
	got := make(chan struct{}, 1)
	sess.OnOutput = func(core.Frame) { got <- struct{}{} }
	if err := sess.Start(); err != nil {
		t.Fatal(err)
	}
	if err := sess.Post(&core.TurnEventFrame{Kind: core.TurnEventStarted, Turn: "t1", Gen: 1, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	<-got
	st.StartSpan(context.Background(), ev(e, "llm.request", t0, "t1", nil)) // never ended
	_ = sess.Close()
	if err := e.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, s := range mem.GetSpans() {
		names[s.Name] = true
	}
	for _, n := range []string{"govoice.session", "govoice.turn", "govoice.llm"} {
		if !names[n] {
			t.Fatalf("%s not exported; got %v", n, names)
		}
	}
}

// gated delays core's delivery to the exporter until gate closes.
type gated struct {
	*SessionTracer
	gate chan struct{}
}

func (g gated) StartSpan(ctx context.Context, e core.TraceEvent) core.Span {
	<-g.gate
	return g.SessionTracer.StartSpan(ctx, e)
}

// Shutdown right after Session.Close must wait for span deliveries still
// queued in core's AsyncTracer (graceful drain), not flush an empty queue.
func TestShutdownWaitsForUndeliveredSession(t *testing.T) {
	e, mem, _ := fixture(Config{})
	pipe, _ := core.NewPipeline(pass{})
	sess, _ := core.NewSession(9, pipe, core.DefaultResourceBudget())
	g := gated{e.Session("a", "v"), make(chan struct{})}
	if err := sess.SetTracer(g, e.Policy()); err != nil {
		t.Fatal(err)
	}
	if err := sess.Start(); err != nil {
		t.Fatal(err)
	}
	_ = sess.Close() // session.start is still undelivered
	done := make(chan error, 1)
	go func() { done <- e.Shutdown(context.Background()) }()
	var err error
	select {
	case err = <-done: // flushed without waiting: the session is lost
		close(g.gate)
	case <-time.After(50 * time.Millisecond):
		close(g.gate)
		err = <-done
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range mem.GetSpans() {
		if s.Name == "govoice.session" {
			return
		}
	}
	t.Fatalf("session span lost: %d spans exported", len(mem.GetSpans()))
}

func TestRedactorMasksCapturedContent(t *testing.T) {
	e, mem, _ := fixture(Config{CaptureContent: true})
	e.SetRedactor(func(s string) string { return strings.ReplaceAll(s, "4111111111111111", "[REDACTED:card]") })
	s := e.Session("a", "v").StartSpan(context.Background(), ev(e, "llm.request", t0, "t1", map[string]any{"transcript": "card 4111111111111111", "provider": "4111111111111111"}))
	s.End(nil)
	_ = e.Shutdown(context.Background())
	var dump string
	for _, sp := range mem.GetSpans() {
		for _, a := range sp.Attributes {
			dump += string(a.Key) + "=" + a.Value.Emit() + ";"
		}
	}
	if !strings.Contains(dump, "card [REDACTED:card]") || strings.Contains(dump, "transcript=card 4111") {
		t.Fatalf("content not redacted: %s", dump)
	}
}

// STT providers trace one "stt.stream" span per provider stream.
func TestSTTStreamSpanExported(t *testing.T) {
	e, mem, _ := fixture(Config{})
	st := e.Session("f", "v")
	ctx := context.Background()
	sess := st.StartSpan(ctx, ev(e, "session.start", t0, "", nil))
	turn := st.StartSpan(ctx, ev(e, "turn.start", t0.Add(time.Second), "t1", nil))
	bound := st.StartSpan(ctx, ev(e, "stt.stream", t0.Add(2*time.Second), "t1", map[string]any{"provider": "deepgram"}))
	unbound := st.StartSpan(ctx, ev(e, "stt.stream", t0.Add(3*time.Second), "", map[string]any{"provider": "deepgram", "text": "SECRET-WORDS"}))
	dangling := st.StartSpan(ctx, ev(e, "stt.stream", t0.Add(3*time.Second), "t1", map[string]any{"provider": "deepgram"}))
	if _, ok := st.StartSpan(ctx, ev(e, "stt.request", t0, "t1", nil)).(noop); !ok {
		t.Fatal("stt.request is not emitted by anyone and must stay unmapped")
	}
	bound.AddEvent(ev(e, "stt.start", t0.Add(2*time.Second), "t1", nil))
	bound.AddEvent(ev(e, "stt.complete", t0.Add(2500*time.Millisecond), "t1", nil))
	bound.End(nil)
	unbound.End(nil)
	turn.End(nil)
	_ = dangling // never ended: must be closed at session end
	sess.End(nil)
	if err := e.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	var stt []tracetest.SpanStub
	var root, turnS tracetest.SpanStub
	for _, s := range mem.GetSpans() {
		switch s.Name {
		case "govoice.stt":
			stt = append(stt, s)
		case "govoice.session":
			root = s
		case "govoice.turn":
			turnS = s
		}
	}
	if len(stt) != 3 {
		t.Fatalf("want 3 stt spans (one closed at session end), got %d", len(stt))
	}
	b, u := stt[0], stt[1]
	if b.Parent.SpanID() != turnS.SpanContext.SpanID() || u.Parent.SpanID() != root.SpanContext.SpanID() {
		t.Fatal("bound stream must hang off the turn, unbound off the session")
	}
	if len(b.Events) != 2 || b.Events[0].Name != "govoice.stt.start" || b.Events[1].Name != "govoice.stt.complete" {
		t.Fatalf("events %v", b.Events)
	}
	var prov string
	for _, a := range b.Attributes {
		if a.Key == "govoice.provider" {
			prov = a.Value.Emit()
		}
	}
	if prov != "deepgram" {
		t.Fatalf("attrs %v", b.Attributes)
	}
	for _, a := range u.Attributes {
		if a.Key == "govoice.text" || strings.Contains(a.Value.Emit(), "SECRET-WORDS") {
			t.Fatal("text leaked by default")
		}
	}
}
