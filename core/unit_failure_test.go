package core_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/coretest"
)

type frameRec struct{ frames []core.Frame }

func (r *frameRec) Push(f core.Frame) error { r.frames = append(r.frames, f); return nil }
func (r *frameRec) PushDirection(f core.Frame, _ core.Direction) error {
	r.frames = append(r.frames, f)
	return nil
}

func (r *frameRec) units() (out []*core.SynthesisUnitFrame) {
	for _, f := range r.frames {
		if su, ok := f.(*core.SynthesisUnitFrame); ok {
			out = append(out, su)
		}
	}
	return
}

func assistantMessages(ctx *core.ConversationContext) (n int) {
	for _, m := range ctx.Messages() {
		if m.Role == core.RoleAssistant {
			n++
		}
	}
	return
}

// A unit that fails in TTS with no audio never gets a write completion; the
// failure notice must still close the reply (M1-04).
func TestFailedUnitDoesNotLeaveReplyActive(t *testing.T) {
	ctx := core.NewConversationContext(core.ContextConfig{})
	p := core.NewAssistantContextProcessor(ctx, core.DefaultAssistantContextConfig())
	r := &frameRec{}
	bg := context.Background()
	_ = p.ProcessFrame(bg, &core.UserMessageFrame{Turn: "t1", Content: "hi", Gen: 1}, core.Downstream, r)
	_ = p.ProcessFrame(bg, &core.LLMTextFrame{Turn: "t1", Gen: 1, Text: "The first sentence is here. The second sentence is here."}, core.Downstream, r)
	us := r.units()
	if len(us) != 2 {
		t.Fatalf("units=%d", len(us))
	}
	_ = p.ProcessFrame(bg, &core.WriteCompletionFrame{Turn: "t1", Gen: 1, UnitID: us[0].UnitID, IsFinal: true, Media: time.Second}, core.Upstream, r)
	_ = p.ProcessFrame(bg, &core.WriteCompletionFrame{Turn: "t1", Gen: 1, UnitID: us[1].UnitID, IsFinal: true, Failed: true}, core.Downstream, r)
	if p.Active() {
		t.Fatal("reply still active after its last unit failed")
	}
	if n := assistantMessages(ctx); n != 1 {
		t.Fatalf("assistant messages = %d, want the delivered part committed", n)
	}
}

// A failed unit in the middle does not block the commit of the others.
func TestFailedMiddleUnitIsSkippedInCommit(t *testing.T) {
	ctx := core.NewConversationContext(core.ContextConfig{})
	p := core.NewAssistantContextProcessor(ctx, core.DefaultAssistantContextConfig())
	r := &frameRec{}
	bg := context.Background()
	_ = p.ProcessFrame(bg, &core.UserMessageFrame{Turn: "t1", Content: "hi", Gen: 1}, core.Downstream, r)
	_ = p.ProcessFrame(bg, &core.LLMTextFrame{Turn: "t1", Gen: 1, Text: "Sentence one is here. Sentence two is here. Sentence three is here."}, core.Downstream, r)
	us := r.units()
	if len(us) != 3 {
		t.Fatalf("units=%d", len(us))
	}
	done := func(i int) {
		_ = p.ProcessFrame(bg, &core.WriteCompletionFrame{Turn: "t1", Gen: 1, UnitID: us[i].UnitID, IsFinal: true, Media: time.Second}, core.Upstream, r)
	}
	done(0)
	_ = p.ProcessFrame(bg, &core.WriteCompletionFrame{Turn: "t1", Gen: 1, UnitID: us[1].UnitID, IsFinal: true, Failed: true}, core.Downstream, r)
	done(2)
	if p.Active() || assistantMessages(ctx) != 1 {
		t.Fatalf("active=%v messages=%d", p.Active(), assistantMessages(ctx))
	}
}

// End to end through the reporter: every unit fails in TTS, none is heard, and
// the reply ends without being recorded.
func TestUnitFailureReporterClosesReply(t *testing.T) {
	ctx := core.NewConversationContext(core.ContextConfig{})
	asst := core.NewAssistantContextProcessor(ctx, core.DefaultAssistantContextConfig())
	tts := coretest.NewTTSProcessor(coretest.TTSConfig{FailWith: errors.New("tts down")})
	pipe, err := core.NewPipeline(asst, tts, &core.UnitFailureReporter{})
	if err != nil {
		t.Fatal(err)
	}
	s, err := core.NewSession(1, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_ = s.Post(&core.UserMessageFrame{Turn: "t1", Content: "hi", Gen: s.CurrentGeneration()})
	_ = s.Post(&core.LLMTextFrame{Turn: "t1", Gen: s.CurrentGeneration(), Text: "The first sentence is here. The second sentence is here."})
	for end := time.Now().Add(3 * time.Second); tts.Units.Load() < 2; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("units were not synthesized")
		}
	}
	for end := time.Now().Add(time.Second); asst.Active(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("reply stayed active after every unit failed")
		}
	}
	if n := assistantMessages(ctx); n != 0 {
		t.Fatalf("assistant messages = %d, want 0", n)
	}
}

// gateFrame parks the run loop (see gateStage) so later frames queue up.
type gateFrame struct{}

func (*gateFrame) Type() core.FrameType { return core.FrameInternal }

type gateStage struct{ hold func() }

func (g *gateStage) ProcessFrame(_ context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	if _, ok := f.(*gateFrame); ok {
		g.hold()
		return nil
	}
	return e.PushDirection(f, dir)
}

// A unit that played some audio and then failed is not a no-audio failure,
// even when its ErrorFrame (control queue) overtakes that audio (data queue).
func TestUnitFailureReporterIgnoresUnitWithQueuedAudio(t *testing.T) {
	ctx := core.NewConversationContext(core.ContextConfig{})
	asst := core.NewAssistantContextProcessor(ctx, core.DefaultAssistantContextConfig())
	tts := coretest.NewTTSProcessor(coretest.TTSConfig{FramesPerUnit: 3, FailWith: errors.New("tts dropped"), FailAfterAudio: 1})
	gate := &gateStage{hold: func() {
		for end := time.Now().Add(2 * time.Second); tts.Units.Load() < 1 && time.Now().Before(end); {
			time.Sleep(time.Millisecond)
		}
		time.Sleep(50 * time.Millisecond) // audio (data) and error (control) are both queued now
	}}
	pipe, err := core.NewPipeline(gate, asst, tts, &core.UnitFailureReporter{}, coretest.NewTransportOutput(coretest.TransportConfig{}))
	if err != nil {
		t.Fatal(err)
	}
	s, err := core.NewSession(1, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_ = s.Post(&core.UserMessageFrame{Turn: "t1", Content: "hi", Gen: s.CurrentGeneration()})
	_ = s.Post(&core.LLMTextFrame{Turn: "t1", Gen: s.CurrentGeneration(), Text: "Only one sentence here."})
	_ = s.Post(&gateFrame{})
	for end := time.Now().Add(2 * time.Second); tts.Units.Load() < 1; time.Sleep(time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("unit was not synthesized")
		}
	}
	for end := time.Now().Add(2 * time.Second); asst.Active(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("reply stayed active")
		}
	}
	if n := assistantMessages(ctx); n != 1 {
		t.Fatalf("assistant messages = %d, want the heard part committed", n)
	}
}
