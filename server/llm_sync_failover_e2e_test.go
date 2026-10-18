package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"govoice/core"
	"govoice/providers/google"
	"govoice/server/flow"
)

// syncDownLLM is a primary whose request cannot start: it forwards the user turn, then reports a provider
// outage synchronously, as a processor whose client refuses the request does.
type syncDownLLM struct{ calls atomic.Int32 }

func (p *syncDownLLM) ProcessFrame(_ context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	if _, ok := f.(*core.UserMessageFrame); !ok {
		return e.PushDirection(f, dir)
	}
	p.calls.Add(1)
	if err := e.PushDirection(f, dir); err != nil {
		return err
	}
	return &google.ProviderError{Capability: "llm", BaseErr: google.ErrProviderUnavailable, SafeMessage: "injected outage",
		StatusCode: http.StatusServiceUnavailable, Retryable: true}
}

// userTurnCounter is a stage after the LLM that counts the user messages reaching it.
type userTurnCounter struct {
	core.Processor
	n *atomic.Int32
}

func (u userTurnCounter) Attach(s core.SessionController, i int) error {
	if a, ok := u.Processor.(core.Attacher); ok {
		return a.Attach(s, i)
	}
	return nil
}

func (u userTurnCounter) Close() error {
	if c, ok := u.Processor.(core.Closer); ok {
		return c.Close()
	}
	return nil
}

func (u userTurnCounter) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	if _, ok := f.(*core.UserMessageFrame); ok {
		u.n.Add(1)
	}
	return u.Processor.ProcessFrame(ctx, f, dir, e)
}

// A primary LLM failing synchronously on every turn: the fallback answers each turn and the call lives on.
func TestCallBehaviorASynchronousLLMFailureFailsOverAndTheCallSurvives(t *testing.T) {
	r := newFlowRigE2E(t, newProbe())
	down := &syncDownLLM{}
	healthy := r.s.registry.factories["echo.llm"]
	r.s.registry.Register("echo.llm", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		if spec.Model == "downmodel" {
			return down, nil
		}
		return healthy(cfg, spec)
	})
	var userTurns atomic.Int32
	healthyTTS := r.s.registry.factories["echo.tts"]
	r.s.registry.Register("echo.tts", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		p, err := healthyTTS(cfg, spec)
		if err != nil {
			return nil, err
		}
		return userTurnCounter{Processor: p, n: &userTurns}, nil
	})
	f, err := r.s.flows.Create(t.Context(), flow.CreateRequest{Name: "Failover", Config: json.RawMessage(
		`{"stt":{"provider":"echo","model":"s"},` +
			`"llm":{"provider":"echo","model":"downmodel","system_prompt":"Failover","fallbacks":[{"provider":"echo","model":"m"}]},` +
			`"tts":{"provider":"echo","voice":"failvoice"}}`)})
	if err != nil || f.Readiness.State != flow.ReadinessReady {
		t.Fatalf("flow: %+v err=%v", f.Readiness, err)
	}
	c := r.dial(f.ID)
	c.say("first question")
	c.waitReplies("first question", 1)
	c.say("second question")
	c.waitReplies("second question", 1)
	if a, b := c.replies("first question"), c.replies("second question"); a != 1 || b != 1 {
		t.Fatalf("answers: first %d, second %d, want each exactly once", a, b)
	}
	if c.ended() {
		t.Fatal("a synchronous LLM failure ended the call")
	}
	if n := userTurns.Load(); n != 2 {
		t.Fatalf("the stages after the LLM saw %d user messages, want one per question (2)", n)
	}
	if n := down.calls.Load(); n != 2 {
		t.Fatalf("the primary was tried %d times, want once per turn", n)
	}
	rec := httptest.NewRecorder()
	r.s.handleMetrics(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if want := `voice_llm_failover_total{from="echo",to="echo",class="server_error"} 2`; !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("metrics lack %s", want)
	}
}

// refuseFirstTurnLLM answers like a provider whose session refused the generation task: it forwards the
// user turn, re-enters a non-fatal ErrTooManyTasks error for it, and returns nil. Later turns go to inner.
type refuseFirstTurnLLM struct {
	core.Processor
	s     core.SessionController
	index int
	calls atomic.Int32
}

func (p *refuseFirstTurnLLM) Attach(s core.SessionController, index int) error {
	p.s, p.index = s, index
	if a, ok := p.Processor.(core.Attacher); ok {
		return a.Attach(s, index)
	}
	return nil
}

func (p *refuseFirstTurnLLM) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	um, ok := f.(*core.UserMessageFrame)
	if !ok || p.calls.Add(1) > 1 {
		return p.Processor.ProcessFrame(ctx, f, dir, e)
	}
	if err := e.PushDirection(f, dir); err != nil {
		return err
	}
	return p.s.Reenter(&core.ErrorFrame{Err: core.ErrTooManyTasks, Turn: core.TurnID(um.Turn), Gen: core.Generation(um.Gen)}, core.Downstream, p.index, uint64(um.Gen))
}

// A refused task loses that turn only: no fallback answers it, the call lives, the next turn is answered.
func TestCallBehaviorARefusedTaskLosesTheTurnNotTheCall(t *testing.T) {
	r := newFlowRigE2E(t, newProbe())
	var primary atomic.Pointer[refuseFirstTurnLLM]
	healthy := r.s.registry.factories["echo.llm"]
	r.s.registry.Register("echo.llm", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		inner, err := healthy(cfg, spec)
		if err != nil {
			return nil, err
		}
		switch spec.Model {
		case "refusemodel":
			p := &refuseFirstTurnLLM{Processor: inner}
			primary.Store(p)
			return p, nil
		}
		return inner, nil
	})
	f, err := r.s.flows.Create(t.Context(), flow.CreateRequest{Name: "Refused", Config: json.RawMessage(
		`{"stt":{"provider":"echo","model":"s"},` +
			`"llm":{"provider":"echo","model":"refusemodel","system_prompt":"Refused","fallbacks":[{"provider":"echo","model":"m"}]},` +
			`"tts":{"provider":"echo","voice":"refusevoice"}}`)})
	if err != nil || f.Readiness.State != flow.ReadinessReady {
		t.Fatalf("flow: %+v err=%v", f.Readiness, err)
	}
	c := r.dial(f.ID)
	c.say("first question")
	eventually(t, "the refused turn to reach the primary", func() bool {
		p := primary.Load()
		return p != nil && p.calls.Load() >= 1
	})
	c.say("second question")
	c.waitReplies("second question", 1)
	if a, b := c.replies("first question"), c.replies("second question"); a != 0 || b != 1 {
		t.Fatalf("answers: first %d (want 0, the lost turn), second %d (want 1)", a, b)
	}
	if c.ended() {
		t.Fatal("a refused task ended the call")
	}
	// The client is told about the lost turn with one non-fatal error (a fatal one would have ended the call).
	if n := countOf(c.wireLog(), "msg:error"); n != 1 {
		t.Fatalf("client got %d RTVI errors for the refused turn, want 1: %v", n, c.wireLog())
	}
	rec := httptest.NewRecorder()
	r.s.handleMetrics(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if strings.Contains(rec.Body.String(), "voice_llm_failover_total{") {
		t.Fatalf("a refused task switched providers:\n%s", rec.Body.String())
	}
}

// refuseFirstUnitTTS answers like a TTS provider whose session refused a synthesis task: it takes the
// first unit, re-enters a non-fatal ErrTooManyTasks error for it, and returns nil. Later units go to inner.
type refuseFirstUnitTTS struct {
	core.Processor
	s     core.SessionController
	index int
	units atomic.Int32
}

func (p *refuseFirstUnitTTS) Attach(s core.SessionController, index int) error {
	p.s, p.index = s, index
	if a, ok := p.Processor.(core.Attacher); ok {
		return a.Attach(s, index)
	}
	return nil
}

func (p *refuseFirstUnitTTS) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	su, ok := f.(*core.SynthesisUnitFrame)
	if !ok || p.units.Add(1) > 1 {
		return p.Processor.ProcessFrame(ctx, f, dir, e)
	}
	return p.s.Reenter(&core.ErrorFrame{Err: core.ErrTooManyTasks, Turn: core.TurnID(su.Turn), Gen: core.Generation(su.Gen), UnitID: su.UnitID}, core.Downstream, p.index, uint64(su.Gen))
}

// A TTS unit refused by the session's own task budget is lost, not failed over: the healthy primary keeps
// the later units, no fallback speaks, no failover is counted, and the call lives.
func TestCallBehaviorARefusedTTSUnitIsLostWithoutFailover(t *testing.T) {
	r := newFlowRigE2E(t, newProbe())
	var primary atomic.Pointer[refuseFirstUnitTTS]
	healthy := r.s.registry.factories["echo.tts"]
	r.s.registry.Register("echo.tts", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		inner, err := healthy(cfg, spec)
		if err != nil || spec.Voice != "refusevoice" {
			return inner, err
		}
		p := &refuseFirstUnitTTS{Processor: inner}
		primary.Store(p)
		return p, nil
	})
	f, err := r.s.flows.Create(t.Context(), flow.CreateRequest{Name: "RefusedTTS", Config: json.RawMessage(
		`{"stt":{"provider":"echo","model":"s"},"llm":{"provider":"echo","model":"m","system_prompt":"RefusedTTS"},` +
			`"tts":{"provider":"echo","voice":"refusevoice","fallbacks":[{"provider":"echo","voice":"okvoice"}]}}`)})
	if err != nil || f.Readiness.State != flow.ReadinessReady {
		t.Fatalf("flow: %+v err=%v", f.Readiness, err)
	}
	c := r.dial(f.ID)
	c.say("First sentence here. Second sentence here.")
	eventually(t, "the later unit spoken by the primary", func() bool { return r.spokenBy("refusevoice", "Second sentence") })
	if r.spokenBy("okvoice", "First sentence") || r.spokenBy("okvoice", "Second sentence") {
		t.Fatal("the fallback spoke: a refused task demoted the primary")
	}
	if c.ended() {
		t.Fatal("a refused TTS task ended the call")
	}
	rec := httptest.NewRecorder()
	r.s.handleMetrics(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if strings.Contains(rec.Body.String(), "voice_tts_failover_total{") {
		t.Fatalf("a refused task switched providers:\n%s", rec.Body.String())
	}
}
