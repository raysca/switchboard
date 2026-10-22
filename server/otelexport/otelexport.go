// Package otelexport bridges the core trace model to OpenTelemetry. It is
// optional: when disabled the server never constructs it and no OTel code runs.
// Span tree: govoice.session -> govoice.turn -> stt|llm|tts|tool.
//
// Privacy: transcript, prompt, tool arguments/results and audio never reach an
// attribute unless Config.CaptureContent is set; credentials are never read.
package otelexport

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"govoice/core"
	"govoice/core/observe"
)

// Config is the `otel` section of the server config. Disabled by default.
type Config struct {
	Enabled bool `yaml:"enabled"`
	// Endpoint is the OTLP/HTTP traces URL, e.g. https://collector:4318/v1/traces.
	Endpoint string `yaml:"endpoint"`
	// Protocol is "http/protobuf" (the only supported value; empty selects it).
	Protocol string `yaml:"protocol"`
	// HeadersEnv maps header name -> environment variable holding its value, so
	// secrets never live in the config file or logs.
	HeadersEnv map[string]string `yaml:"headers_env"`
	// SamplingRatio in [0,1]; nil means 1 (sample everything). 0 exports nothing.
	SamplingRatio *float64 `yaml:"sampling_ratio"`
	ServiceName   string   `yaml:"service_name"`
	// ResourceAttributes are static, non-secret resource attributes.
	ResourceAttributes map[string]string `yaml:"resource_attributes"`
	// CaptureContent admits transcript/prompt text into spans. Default false.
	CaptureContent bool `yaml:"capture_content"`
	// QueueSize bounds buffered finished spans (default 2048); overflow drops.
	QueueSize int `yaml:"queue_size"`
	// ShutdownTimeout bounds the final flush (default 5s).
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
}

// Validate checks an enabled config.
func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	if p := strings.ToLower(c.Protocol); p != "" && p != "http/protobuf" && p != "http" {
		return fmt.Errorf("otel.protocol %q is not supported; use http/protobuf", c.Protocol)
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("otel.endpoint must be an http(s) URL when otel.enabled")
	}
	if u.User != nil || u.RawQuery != "" {
		return errors.New("otel.endpoint must not carry credentials or a query; use headers_env")
	}
	if c.SamplingRatio != nil && (*c.SamplingRatio < 0 || *c.SamplingRatio > 1) {
		return errors.New("otel.sampling_ratio must be within [0,1]")
	}
	if c.QueueSize < 0 || c.ShutdownTimeout < 0 {
		return errors.New("otel.queue_size and otel.shutdown_timeout must be >= 0")
	}
	return nil
}

// Exporter owns the tracer provider and span queue. Safe for concurrent use.
type Exporter struct {
	tp      *sdktrace.TracerProvider
	proc    *queueProcessor
	capture bool
	redact  func(string) string // optional; masks content attributes
	timeout time.Duration
	now     func() time.Time
	live    atomic.Int64 // session trace queues opened but not yet drained
}

// New builds an exporter that sends OTLP/HTTP to cfg.Endpoint. getenv resolves
// header secrets (os.Getenv in production).
func New(cfg Config, getenv func(string) string) (*Exporter, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	headers := make(map[string]string, len(cfg.HeadersEnv))
	for name, env := range cfg.HeadersEnv {
		v := getenv(env)
		if v == "" {
			return nil, fmt.Errorf("otel.headers_env[%s]: environment variable %s is empty", name, env)
		}
		headers[name] = v
	}
	opts := []otlptracehttp.Option{otlptracehttp.WithEndpointURL(cfg.Endpoint), otlptracehttp.WithTimeout(10 * time.Second)}
	if len(headers) > 0 {
		opts = append(opts, otlptracehttp.WithHeaders(headers))
	}
	exp, err := otlptracehttp.New(context.Background(), opts...)
	if err != nil {
		return nil, err
	}
	return build(cfg, exp, time.Now), nil
}

func build(cfg Config, exp sdktrace.SpanExporter, now func() time.Time) *Exporter {
	ratio := 1.0
	if cfg.SamplingRatio != nil {
		ratio = *cfg.SamplingRatio
	}
	name := cfg.ServiceName
	if name == "" {
		name = "govoice"
	}
	attrs := []attribute.KeyValue{attribute.String("service.name", name)}
	for k, v := range cfg.ResourceAttributes {
		attrs = append(attrs, attribute.String(k, v))
	}
	qs := cfg.QueueSize
	if qs <= 0 {
		qs = 2048
	}
	proc := newQueueProcessor(exp, qs)
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))),
		sdktrace.WithResource(resource.NewSchemaless(attrs...)),
		sdktrace.WithSpanProcessor(proc),
	)
	to := cfg.ShutdownTimeout
	if to <= 0 {
		to = 5 * time.Second
	}
	return &Exporter{tp: tp, proc: proc, capture: cfg.CaptureContent, timeout: to, now: now}
}

// SetRedactor masks string content attributes (transcript, prompt, text, ...)
// before export. Call before the first Session.
func (e *Exporter) SetRedactor(f func(string) string) { e.redact = f }

// Policy is the core.TracePolicy a session should run with.
func (e *Exporter) Policy() core.TracePolicy {
	if e.capture {
		return core.TracePolicy{Content: core.TraceText}
	}
	return core.TracePolicy{Content: core.TraceMetadata}
}

// Stats reports spans dropped on queue overflow and spans lost to export errors.
func (e *Exporter) Stats() (dropped, failed uint64) {
	return e.proc.dropped.Load(), e.proc.failed.Load()
}

// Shutdown waits for every session's trace queue to drain (core delivers span
// starts and ends asynchronously, possibly after Session.Close returns), then
// flushes queued spans; the
// whole sequence is bounded by the configured timeout and ctx.
func (e *Exporter) Shutdown(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	// ponytail: poll; half the budget at most so the flush keeps the rest.
	for wait := time.Now().Add(e.timeout / 2); e.live.Load() > 0 && ctx.Err() == nil && time.Now().Before(wait); {
		time.Sleep(5 * time.Millisecond)
	}
	return e.tp.Shutdown(ctx)
}

// Session returns the core.Tracer for one call. Install it with
// Session.SetTracer(t, e.Policy()) and feed latency via t.Latency.
func (e *Exporter) Session(flowID, flowVersion string) *SessionTracer {
	return &SessionTracer{e: e, tr: e.tp.Tracer("govoice"), fl: []attribute.KeyValue{
		attribute.String("govoice.flow.id", flowID), attribute.String("govoice.flow.version", flowVersion),
	}, turns: map[core.TurnID]trace.Span{}, open: map[*span]struct{}{}}
}

// SessionTracer implements core.Tracer for a single session.
type SessionTracer struct {
	e  *Exporter
	tr trace.Tracer
	fl []attribute.KeyValue

	mu       sync.Mutex
	root     trace.Span
	rootCtx  context.Context // trace parentage
	turns    map[core.TurnID]trace.Span
	turnCtxs map[core.TurnID]context.Context // trace parentage
	open     map[*span]struct{}              // unended non-root spans, closed when the root ends
}

var serviceSpans = map[string]string{
	"stt.stream": "govoice.stt", "llm.request": "govoice.llm", "tts.request": "govoice.tts", "tool.execute": "govoice.tool",
}

// StartSpan maps session/turn/service trace events to spans; every other
// event (per-frame and per-processor spans) is intentionally not exported.
func (t *SessionTracer) StartSpan(_ context.Context, ev core.TraceEvent) core.Span {
	t.mu.Lock()
	defer t.mu.Unlock()
	start := trace.WithTimestamp(ev.Time)
	var (
		name  string
		onEnd func()
	)
	parent := context.Background()
	switch ev.Name {
	case "session.start":
		name = "govoice.session"
	case "turn.start":
		name = "govoice.turn"
		if t.rootCtx != nil {
			parent = t.rootCtx
		}
	default:
		var ok bool
		if name, ok = serviceSpans[ev.Name]; !ok {
			return noop{}
		}
		if c := t.turnCtxs[ev.TurnID]; c != nil {
			parent = c
		} else if t.rootCtx != nil {
			parent = t.rootCtx
		}
	}
	ctx, sp := t.tr.Start(parent, name, start)
	s := &span{sp: sp, capture: t.e.capture, redact: t.e.redact, now: t.e.now}
	switch ev.Name {
	case "session.start":
		t.root, t.rootCtx = sp, ctx
		sp.SetAttributes(t.fl...)
		sp.SetAttributes(attribute.String("govoice.session.id", string(ev.SessionID)))
		onEnd = func() {
			// Close turns/services the runtime never ended (cancel, wedged
			// provider) so they are exported rather than lost.
			t.mu.Lock()
			left := make([]*span, 0, len(t.open))
			for c := range t.open {
				left = append(left, c)
			}
			t.mu.Unlock()
			for _, c := range left {
				c.End(errUnfinished)
			}
		}
	case "turn.start":
		t.turns[ev.TurnID] = sp
		if t.turnCtxs == nil {
			t.turnCtxs = map[core.TurnID]context.Context{}
		}
		t.turnCtxs[ev.TurnID] = ctx
		turn := ev.TurnID
		t.open[s] = struct{}{}
		onEnd = func() { t.mu.Lock(); delete(t.turns, turn); delete(t.turnCtxs, turn); delete(t.open, s); t.mu.Unlock() }
	default:
		t.open[s] = struct{}{}
		onEnd = func() { t.mu.Lock(); delete(t.open, s); t.mu.Unlock() }
	}
	if ev.TurnID != "" {
		sp.SetAttributes(attribute.String("govoice.turn.id", string(ev.TurnID)))
	}
	s.onEnd = onEnd
	for k, v := range ev.Attributes() {
		s.SetAttribute(k, v)
	}
	return s
}

// AsyncTraceOpened and AsyncTraceDrained bracket core's asynchronous delivery,
// counting the session from Session.Start (not from when its session.start is
// delivered) so Shutdown cannot flush before a closed session's spans arrive.
func (t *SessionTracer) AsyncTraceOpened()  { t.e.live.Add(1) }
func (t *SessionTracer) AsyncTraceDrained() { t.e.live.Add(-1) }

// Latency records a P0-2 measurement on its turn span (startup on the session).
func (t *SessionTracer) Latency(m observe.Measurement) {
	t.mu.Lock()
	sp := t.turns[m.Turn]
	if m.Turn == "" {
		sp = t.root
	}
	t.mu.Unlock()
	if sp != nil {
		sp.SetAttributes(attribute.Float64("govoice.latency."+string(m.Stage)+"_ms", float64(m.Value)/float64(time.Millisecond)))
	}
}

// errUnfinished marks spans closed by session teardown (error.class only).
type unfinishedError struct{}

func (unfinishedError) Error() string { return "unfinished at session end" }

var errUnfinished error = unfinishedError{}

type noop struct{}

func (noop) SetAttribute(string, any) {}
func (noop) AddEvent(core.TraceEvent) {}
func (noop) End(error)                {}

type span struct {
	sp      trace.Span
	capture bool
	redact  func(string) string
	now     func() time.Time
	onEnd   func()
	ended   atomic.Bool
}

var contentKeys = map[string]bool{"transcript": true, "prompt": true, "text": true, "tool.arguments": true, "tool.result": true}

func (s *span) kv(key string, v any) (attribute.KeyValue, bool) {
	if str, ok := v.(string); ok && s.redact != nil && contentKeys[key] {
		v = s.redact(str)
	}
	return kv(s.capture, key, v)
}

func kv(capture bool, key string, v any) (attribute.KeyValue, bool) {
	if contentKeys[key] && !capture {
		return attribute.KeyValue{}, false
	}
	k := "govoice." + key
	switch x := v.(type) {
	case string:
		return attribute.String(k, x), true
	case bool:
		return attribute.Bool(k, x), true
	case int:
		return attribute.Int(k, x), true
	case int8, int16, int32, int64, uint8, uint16, uint32:
		return attribute.Int64(k, toInt64(x)), true
	case uint, uint64:
		return attribute.Int64(k, toInt64(x)), true
	case float32:
		return attribute.Float64(k, float64(x)), true
	case float64:
		return attribute.Float64(k, x), true
	}
	return attribute.KeyValue{}, false
}

func toInt64(v any) int64 {
	switch x := v.(type) {
	case int8:
		return int64(x)
	case int16:
		return int64(x)
	case int32:
		return int64(x)
	case int64:
		return x
	case uint8:
		return int64(x)
	case uint16:
		return int64(x)
	case uint32:
		return int64(x)
	case uint:
		return int64(x)
	case uint64:
		return int64(x)
	}
	return 0
}

func (s *span) SetAttribute(key string, v any) {
	if a, ok := s.kv(key, v); ok {
		s.sp.SetAttributes(a)
	}
}

// AddEvent records e (including provider failure/failover events) as a span event.
func (s *span) AddEvent(e core.TraceEvent) {
	var attrs []attribute.KeyValue
	for k, v := range e.Attributes() {
		if a, ok := s.kv(k, v); ok {
			attrs = append(attrs, a)
		}
	}
	s.sp.AddEvent("govoice."+e.Name, trace.WithTimestamp(e.Time), trace.WithAttributes(attrs...))
}

// End closes the span. Only the error's Go type is exported, never its message,
// which may echo user content or secrets.
func (s *span) End(err error) {
	if !s.ended.CompareAndSwap(false, true) {
		return
	}
	if err != nil {
		s.sp.SetAttributes(attribute.String("error.class", fmt.Sprintf("%T", err)))
		s.sp.SetStatus(codes.Error, "")
	}
	end := s.now()
	if s.onEnd != nil {
		s.onEnd()
	}
	s.sp.End(trace.WithTimestamp(end))
}

var (
	_ core.Tracer             = (*SessionTracer)(nil)
	_ core.AsyncTraceObserver = (*SessionTracer)(nil)
)
