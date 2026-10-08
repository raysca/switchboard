// Package provtest is the stage-level provider conformance suite: the SAME
// checks run against every STT, TTS and LLM provider, built exactly as the
// server builds them (server.Registry.BuildStage). Adding a provider is one
// entry in providers.go.
//
// Live runs spend real provider credit and are guarded by GOVOICE_LIVE=1 and a
// global request budget (-max-calls). A dry mode runs the same checks against
// in-process fakes (fake.go) so the suite logic is unit-tested without network.
package provtest

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"govoice/core"
	"govoice/core/netpool"
)

type Stage string

const (
	STT Stage = "stt"
	TTS Stage = "tts"
	LLM Stage = "llm"
)

var AllStages = []Stage{STT, TTS, LLM}

type Status string

const (
	Pass Status = "pass"
	Fail Status = "fail"
	Skip Status = "skip"
)

// Check is one named assertion with an optional headline number (ms, WER...).
type Check struct {
	Name   string  `json:"name"`
	Status Status  `json:"status"`
	Detail string  `json:"detail,omitempty"`
	Value  float64 `json:"value,omitempty"`
	Unit   string  `json:"unit,omitempty"`
}

// Result is one provider/stage run.
type Result struct {
	Provider string  `json:"provider"`
	Stage    Stage   `json:"stage"`
	Status   Status  `json:"status"` // fail if any check failed; skip if the target did not run
	Reason   string  `json:"reason,omitempty"`
	Checks   []Check `json:"checks,omitempty"`
	Calls    int     `json:"calls"`                     // planned units spent (estimate)
	Actual   int     `json:"actual_requests,omitempty"` // measured at the provider boundary
	Seconds  float64 `json:"seconds"`
}

// Variant selects how Target.Build constructs the processor.
type Variant struct {
	BadCredential bool
	BadModel      bool // LLM only
	Tools         *ToolKit
}

// ToolKit is the trivial get_weather-like tool of the LLM tool round trip.
type ToolKit struct {
	Defs    []core.ToolDefinition
	Exec    core.ToolExecutor
	History *core.ConversationContext
}

// Target is one provider stage under test.
type Target struct {
	Provider string
	Stage    Stage
	Build    func(Variant) (core.Processor, error)
	// BadSecret is the literal invalid credential Build(BadCredential) uses;
	// it must never appear in an error.
	BadSecret string
	Classify  func(error) string // failover class of an error ("client_error" wanted for bad credentials)
}

// Budget counts provider requests across the whole run and aborts it when the
// guard is exceeded. One unit = one synthesis unit, one STT stream or one LLM
// generation (a tool round trip counts two).
//
// used counts these planned units. Actual requests are MEASURED at the provider boundary
// (netpool's voice_provider_requests_total: HTTP calls, WebSocket dials, gRPC opens, retries
// included): the budget refuses further units once either figure reaches max.
type Budget struct {
	mu   sync.Mutex
	max  int
	used int
	base map[string]uint64 // netpool counters when the budget was created
}

var ErrBudget = errors.New("provider call budget exceeded")

func NewBudget(max int) *Budget { return &Budget{max: max, base: netpool.Requests()} }

// Actual is the provider requests measured since the budget was created.
func (b *Budget) Actual() int {
	if b == nil {
		return 0
	}
	return sinceRequests(b.base)
}

// sinceRequests sums the netpool request counters above base.
func sinceRequests(base map[string]uint64) int {
	n := 0
	for k, v := range netpool.Requests() {
		n += int(v - base[k])
	}
	return n
}

// Spend reserves n calls; it fails (and spends nothing) when the budget would be exceeded.
func (b *Budget) Spend(n int) error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.max > 0 && b.used+n > b.max {
		return fmt.Errorf("%w (%d used, max %d)", ErrBudget, b.used, b.max)
	}
	if a := sinceRequests(b.base); b.max > 0 && a+n > b.max { // measured: planned units undercount
		return fmt.Errorf("%w (%d actual requests, max %d)", ErrBudget, a, b.max)
	}
	b.used += n
	return nil
}

// Exhausted reports whether no further call fits.
func (b *Budget) Exhausted() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.max > 0 && (b.used >= b.max || sinceRequests(b.base) >= b.max)
}

func (b *Budget) Used() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

// Options tune a suite run.
type Options struct {
	Clips   []Clip
	Budget  *Budget
	Timeout time.Duration // per provider wait (default 30 s)
	MaxWER  float64       // per clip when the clip sets no max_wer (default 0.25)
	Idle    time.Duration // idle gap before the second STT turn (default 2 s)
	Fast    bool          // stream STT audio faster than real time (dry mode only)
}

func (o *Options) defaults() {
	if o.Timeout == 0 {
		o.Timeout = 30 * time.Second
	}
	if o.MaxWER == 0 {
		o.MaxWER = 0.25
	}
	if o.Idle == 0 {
		o.Idle = 2 * time.Second
	}
}

// run collects checks for one target.
type run struct {
	t      Target
	o      Options
	checks []Check
	calls  int
}

func (r *run) add(name string, ok bool, detail string, args ...any) {
	st := Pass
	if !ok {
		st = Fail
	}
	r.checks = append(r.checks, Check{Name: name, Status: st, Detail: Redact(fmt.Sprintf(detail, args...))})
}

func (r *run) metric(name string, ok bool, v float64, unit, detail string, args ...any) {
	r.add(name, ok, detail, args...)
	c := &r.checks[len(r.checks)-1]
	c.Value, c.Unit = v, unit
}

func (r *run) skip(name, why string) {
	r.checks = append(r.checks, Check{Name: name, Status: Skip, Detail: why})
}

// spend reserves provider calls; false means the budget is spent.
func (r *run) spend(n int, name string) bool {
	if err := r.o.Budget.Spend(n); err != nil {
		r.skip(name, err.Error())
		return false
	}
	r.calls += n
	return true
}

// RunTarget runs the stage's whole suite against t.
func RunTarget(t Target, o Options) Result {
	o.defaults()
	start := time.Now()
	reqBase := netpool.Requests()
	r := &run{t: t, o: o}
	switch t.Stage {
	case TTS:
		r.ttsSuite()
	case STT:
		r.sttSuite()
	case LLM:
		r.llmSuite()
	}
	res := Result{Provider: t.Provider, Stage: t.Stage, Checks: r.checks, Calls: r.calls, Actual: sinceRequests(reqBase), Seconds: time.Since(start).Seconds(), Status: Pass}
	ran, budget := 0, 0
	for _, c := range r.checks {
		switch {
		case c.Status == Fail:
			res.Status = Fail
		case c.Status == Pass:
			ran++
		case strings.Contains(c.Detail, ErrBudget.Error()):
			budget++
		}
	}
	// A partial or empty run is not a pass: say what did not run.
	if res.Status == Pass && (ran == 0 || budget > 0) {
		res.Status, res.Reason = Skip, fmt.Sprintf("incomplete: %d checks passed, %d not run (budget)", ran, budget)
	}
	return res
}

// Merge combines repeated runs of one target: a check fails if any run failed,
// numbers are medians.
func Merge(runs []Result) Result {
	out := runs[0]
	out.Checks = slices.Clone(out.Checks)
	out.Calls, out.Actual, out.Seconds = 0, 0, 0
	vals := map[string][]float64{}
	for _, r := range runs {
		out.Calls += r.Calls
		out.Actual += r.Actual
		out.Seconds += r.Seconds
		if r.Status == Fail {
			out.Status = Fail
		}
		for _, c := range r.Checks {
			if c.Unit != "" { // zero is a value too (WER 0.00)
				vals[c.Name] = append(vals[c.Name], c.Value)
			}
		}
	}
	for _, r := range runs[1:] {
		for _, d := range r.Checks {
			if !slices.ContainsFunc(out.Checks, func(c Check) bool { return c.Name == d.Name }) {
				out.Checks = append(out.Checks, d) // e.g. a check that a later run reached
			}
		}
	}
	for i, c := range out.Checks {
		for _, r := range runs[1:] {
			for _, d := range r.Checks {
				if d.Name == c.Name && d.Status == Fail && out.Checks[i].Status != Fail {
					out.Checks[i].Status, out.Checks[i].Detail = Fail, d.Detail
				}
			}
		}
		if v := vals[c.Name]; len(v) > 0 {
			sort.Float64s(v)
			out.Checks[i].Value = v[len(v)/2]
		}
	}
	return out
}

// ---- frame rig -------------------------------------------------------------

type stamped struct {
	at time.Time
	f  core.Frame
}

// recorder is the shared fake downstream sink: it timestamps every frame.
type recorder struct {
	mu   sync.Mutex
	recs []stamped
}

func (r *recorder) ProcessFrame(_ context.Context, f core.Frame, _ core.Direction, _ core.Emitter) error {
	r.mu.Lock()
	r.recs = append(r.recs, stamped{time.Now(), f})
	r.mu.Unlock()
	return nil
}

func (r *recorder) snapshot() []stamped {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.recs)
}

// frames returns the recorded frames of type T since index from, with times.
func frames[T core.Frame](r *recorder) (out []T, at []time.Time) {
	for _, s := range r.snapshot() {
		if v, ok := s.f.(T); ok {
			out, at = append(out, v), append(at, s.at)
		}
	}
	return
}

// rig is processor -> recorder inside a real core.Session.
type rig struct {
	proc core.Processor
	sess *core.Session
	sink *recorder
}

func newRig(p core.Processor) (*rig, error) {
	sink := &recorder{}
	pipe, err := core.NewPipeline(p, sink)
	if err != nil {
		return nil, err
	}
	sess, err := core.NewSession(1, pipe, core.DefaultResourceBudget())
	if err != nil {
		return nil, err
	}
	if err := sess.Start(); err != nil {
		return nil, err
	}
	return &rig{p, sess, sink}, nil
}

// close tears the session and processor down; leaked is the session's count of
// supervised tasks that did not exit.
func (g *rig) close() (leaked int) {
	_ = g.sess.Close()
	if c, ok := g.proc.(core.Closer); ok {
		_ = c.Close()
	}
	return g.sess.LeakedTasks()
}

func (g *rig) gen() uint64 { return g.sess.CurrentGeneration() }

func (g *rig) errs() (out []error) {
	es, _ := frames[*core.ErrorFrame](g.sink)
	for _, e := range es {
		out = append(out, e.Err)
	}
	// A processor that returns an error fails the session instead of emitting a frame.
	if err := g.sess.Err(); err != nil {
		out = append(out, err)
	}
	return
}

// waitFor polls cond until it holds or d passes.
func waitFor(d time.Duration, cond func() bool) bool {
	for until := time.Now().Add(d); ; time.Sleep(2 * time.Millisecond) {
		if cond() {
			return true
		}
		if time.Now().After(until) {
			return false
		}
	}
}

// settleGoroutines returns the goroutine count once it stops falling.
func settleGoroutines(wait time.Duration) int {
	n := runtime.NumGoroutine()
	for until := time.Now().Add(wait); time.Now().Before(until); time.Sleep(50 * time.Millisecond) {
		runtime.GC()
		if m := runtime.NumGoroutine(); m < n {
			n = m
		} else if m == n && time.Until(until) < wait/2 {
			break
		}
	}
	return n
}

// goroutineSlack allows for pooled HTTP/gRPC connections that outlive a processor.
const goroutineSlack = 6

// leakAfter closes g and compares the goroutine count with base (taken before
// the processor was built).
func (r *run) leakAfter(g *rig, base int) {
	leaked := g.close()
	after := settleGoroutines(2 * time.Second)
	r.metric("close_leaks", leaked == 0 && after <= base+goroutineSlack, float64(after-base), "goroutines",
		"leaked session tasks=%d, goroutines %d -> %d (slack %d)", leaked, base, after, goroutineSlack)
}

// badCredential builds with the invalid credential and drives `drive` (which
// issues one request) and returns the error the pipeline would see.
func (r *run) badCredential(drive func(g *rig) error) {
	const name = "bad_credential"
	if !r.spend(1, name) {
		return
	}
	start := time.Now()
	var got error
	p, err := r.t.Build(Variant{BadCredential: true})
	if err != nil {
		got = err
	} else {
		g, gerr := newRig(p)
		if gerr != nil {
			r.add(name, false, "rig: %v", gerr)
			return
		}
		got = drive(g)
		g.close()
	}
	r.errorClass(name, got, start)
}

// errorClass asserts err is a fast, redacted client error.
func (r *run) errorClass(name string, err error, start time.Time) {
	if err == nil {
		r.add(name, false, "no error for an invalid request")
		return
	}
	elapsed := time.Since(start)
	class := r.t.Classify(err)
	text := err.Error()
	leaked := Leaks(text) || (r.t.BadSecret != "" && containsFold(text, r.t.BadSecret))
	ok := class == classClient && !leaked && elapsed < 10*time.Second
	if r.t.BadSecret != "" {
		text = strings.ReplaceAll(text, r.t.BadSecret, "[redacted]")
	}
	r.metric(name, ok, float64(elapsed.Milliseconds()), "ms", "class=%q want %q, secret-in-error=%v, elapsed=%v (no retry: <10s), err=%.120s",
		class, classClient, leaked, elapsed.Round(time.Millisecond), Redact(text))
}

const classClient = "client_error" // server/agent.ClassClient

func containsFold(s, sub string) bool {
	return sub != "" && strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}
