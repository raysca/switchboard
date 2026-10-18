package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"govoice/core"
	"govoice/server/catalog"
)

// probeTimeout bounds one stage probe; a variable so a test can shorten it.
var probeTimeout = 15 * time.Second

const probeDetailMax = 300

// probeIDs numbers probe sessions from a range a call id (a small counter) never reaches.
var probeIDs atomic.Uint64

func init() { probeIDs.Store(1 << 62) }

// ProbeResult is what one minimal request to a provider's stage showed.
type ProbeResult struct {
	Stage     string `json:"stage"`
	Status    string `json:"status"` // "ok" | "error" | "skipped"
	LatencyMS *int64 `json:"latency_ms,omitempty"`
	Detail    string `json:"detail,omitempty"`
	Circuit   string `json:"circuit"`
}

// probeSink ends a one-processor pipeline: it signals the first frame the probe waits for and any error.
type probeSink struct {
	want  func(core.Frame) bool
	first chan struct{}
	fail  chan error
}

func (p *probeSink) ProcessFrame(_ context.Context, f core.Frame, _ core.Direction, _ core.Emitter) error {
	if ef, ok := f.(*core.ErrorFrame); ok && ef.Err != nil {
		select {
		case p.fail <- ef.Err:
		default:
		}
		return nil
	}
	if p.want != nil && p.want(f) {
		select {
		case p.first <- struct{}{}:
		default:
		}
	}
	return nil
}

// probeProcessor drives one stage processor with the smallest request that proves it works and returns the
// time to the first token (llm) or first audio (tts). An stt probe sends 200 ms of silence and reports 0:
// no transcript is expected, so only a failure is visible. The session, and with it the processor, is closed before returning.
func probeProcessor(ctx context.Context, proc core.Processor, stage string) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	sink := &probeSink{first: make(chan struct{}, 1), fail: make(chan error, 1)}
	switch stage {
	case "llm":
		// the first streamed delta is the first token; the complete LLMTextFrame comes only at the end
		sink.want = func(f core.Frame) bool {
			switch f.(type) {
			case *core.LLMTextDeltaFrame, *core.LLMTextFrame:
				return true
			}
			return false
		}
	case "tts":
		sink.want = func(f core.Frame) bool { a, ok := f.(*core.OutputAudioFrame); return ok && len(a.Samples) > 0 }
	}
	pipe, err := core.NewPipeline(proc, sink)
	if err != nil {
		return 0, fmt.Errorf("probe pipeline: %w", err)
	}
	sess, err := core.NewSession(probeIDs.Add(1), pipe, core.DefaultResourceBudget())
	if err != nil {
		return 0, fmt.Errorf("probe session: %w", err)
	}
	// Session.Close closes the processors, bounded and with recover; closing again here could block forever.
	defer func() { _ = sess.Close() }()
	if err := sess.Start(); err != nil {
		return 0, fmt.Errorf("probe session: %w", err)
	}
	gen := sess.CurrentGeneration()
	start := time.Now()
	switch stage {
	case "llm":
		_ = sess.Post(&core.UserMessageFrame{Turn: "probe", Gen: gen, Content: "ping"})
	case "tts":
		_ = sess.Post(&core.SynthesisUnitFrame{Turn: "probe", Gen: gen, UnitID: 1, Text: "ok", IsFinal: true})
	case "stt":
		probeSilence(sess, gen)
	}
	settle := (<-chan time.Time)(nil)
	if stage == "stt" {
		settle = time.After(time.Second) // a failing stream answers well inside this
	}
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-sink.first:
			return time.Since(start), nil
		case err := <-sink.fail:
			return 0, err
		case <-settle:
			return 0, sess.Err()
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-tick.C:
			if err := sess.Err(); err != nil {
				return 0, err
			}
		}
	}
}

// probeSilence plays 200 ms of 16 kHz silence as one short user turn.
func probeSilence(sess *core.Session, gen uint64) {
	now := time.Now()
	_ = sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: gen, Timestamp: now})
	_ = sess.Post(&core.TurnEventFrame{Kind: core.TurnEventStarted, Turn: "probe", Gen: core.Generation(gen), At: now})
	for i := 0; i < 10; i++ {
		_ = sess.Post(&core.InputAudioFrame{Samples: make([]byte, 640), Encoding: core.EncodingPCM16LE, SampleRate: 16000, Channels: 1, Gen: gen, Seq: uint64(i), Timestamp: time.Now()})
	}
	stop := time.Now()
	_ = sess.Post(&core.UserStoppedSpeakingFrame{Seq: 1, Gen: gen, Timestamp: stop})
	_ = sess.Post(&core.TurnEventFrame{Kind: core.TurnEventStopped, Turn: "probe", Gen: core.Generation(gen), At: stop, Reason: core.TurnReasonSilence})
	_ = sess.Post(&core.TurnEventFrame{Kind: core.TurnEventCompleted, Turn: "probe", Gen: core.Generation(gen), At: time.Now(), Reason: core.TurnReasonSilence})
}

// probeDetail is the failure class and a redacted message cut to probeDetailMax bytes.
func probeDetail(class string, err error, redact func(string) string) string {
	msg := redact(err.Error())
	if len(msg) > probeDetailMax {
		msg = msg[:probeDetailMax]
	}
	return class + ": " + msg
}

// probeStage checks one stage of a provider: catalog default model or voice, the environment secret, the
// registry's own factory, then probeProcessor. It never touches the call, admission or session machinery.
func (s *Server) probeStage(ctx context.Context, provider, stage string) (res ProbeResult) {
	res = ProbeResult{Stage: stage}
	defer func() { res.Circuit = s.metrics.circuitState(provider, stage) }()
	spec := StageSpec{Provider: provider}
	var ok bool
	if p, found := s.catalog.Provider(provider); found && p.Services[stage] != nil {
		svc := p.Services[stage]
		switch stage {
		case catalog.ServiceLLM:
			e, found := catalog.DefaultEntry(svc.Models)
			spec.Model, ok = e.ID, found
			spec.SystemPrompt = "Reply with the single word ok."
		case catalog.ServiceTTS:
			e, found := catalog.DefaultEntry(svc.Voices)
			spec.Voice, ok = e.ID, found
		case catalog.ServiceSTT:
			e, found := catalog.DefaultEntry(svc.Models)
			spec.Model, ok = e.ID, found
			lang, _ := catalog.DefaultEntry(svc.Languages)
			spec.Language = lang.ID
		}
	}
	if !ok {
		res.Status, res.Detail = "skipped", "no default model or voice in the catalog for this service"
		return res
	}
	secret, err := s.resolver.Secret(provider, stage)
	if err != nil {
		res.Status, res.Detail = "error", probeDetail("credential", err, s.redactor.Redact)
		return res
	}
	defer clear(secret)
	spec.Secret = secret
	// The redactor knows patterns; the probe also knows the exact secret it was given.
	redact := func(m string) string {
		if len(secret) > 0 {
			m = strings.ReplaceAll(m, string(secret), "[redacted]")
		}
		return s.redactor.Redact(m)
	}
	proc, err := s.registry.BuildStage(s.cfg, stage, spec)
	if err != nil {
		res.Status, res.Detail = "error", probeDetail("build", err, redact)
		return res
	}
	d, err := probeProcessor(ctx, proc, stage)
	if err != nil {
		class := ClassifyStageError(stage, err)
		if errors.Is(err, context.DeadlineExceeded) {
			class = "timeout"
		}
		res.Status, res.Detail = "error", probeDetail(class, err, redact)
		return res
	}
	res.Status = "ok"
	if stage != catalog.ServiceSTT {
		ms := d.Milliseconds()
		res.LatencyMS = &ms
	}
	return res
}

// circuitState is the worst breaker state this process reports for a provider's stage, or "unknown" when
// none is reported (echo, or a provider not used yet).
func (m *Metrics) circuitState(provider, stage string) string {
	var codes []int64
	static := map[[2]string]*atomic.Int64{
		{"google", "stt"}: &m.STTCircuitStateGoogle, {"xai", "stt"}: &m.STTCircuitStateXAI,
		{"google", "llm"}: &m.LLMCircuitStateGoogle, {"xai", "llm"}: &m.LLMCircuitStateXAI,
		{"google", "tts"}: &m.TTSCircuitStateGoogle, {"xai", "tts"}: &m.TTSCircuitStateXAI,
	}
	if g, ok := static[[2]string{provider, stage}]; ok {
		codes = append(codes, g.Load())
	}
	for k, v := range m.circuits.snapshot() {
		if k.provider == provider && k.stage == stage {
			codes = append(codes, v)
		}
	}
	if len(codes) == 0 {
		return "unknown"
	}
	worst := int64(0)
	for _, c := range codes {
		if c == 1 || (c == 2 && worst == 0) {
			worst = c
			if c == 1 {
				break
			}
		}
	}
	return [...]string{"closed", "open", "half_open"}[worst]
}
