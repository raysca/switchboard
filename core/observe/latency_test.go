package observe

import (
	"testing"
	"time"

	"govoice/core"
	"govoice/core/coretest"
)

var t0 = time.Unix(1000, 0)

func ev(name string, turn core.TurnID, at time.Duration, attrs map[string]any) core.DebugEvent {
	return core.NewDebugEvent(name, t0.Add(at), "1", turn, 1, attrs)
}

func TestTrackerComputesStages(t *testing.T) {
	var got []Measurement
	tr := New(func(m Measurement) { got = append(got, m) })
	for _, e := range []core.DebugEvent{
		ev(core.DebugSessionStarted, "", 0, map[string]any{"latency_ms": int64(12)}),
		ev(core.DebugUserStopped, "", 100*time.Millisecond, nil),
		ev(core.DebugSTTFinal, "t1", 250*time.Millisecond, nil),
		ev(core.DebugLLMRequest, "t1", 260*time.Millisecond, nil),
		ev(core.DebugLLMFirstToken, "t1", 560*time.Millisecond, nil),
		ev(core.DebugTTSRequest, "t1", 600*time.Millisecond, nil),
		ev(core.DebugBotFirstAudio, "t1", 900*time.Millisecond, nil),
		ev(core.DebugBotFirstAudio, "t1", 920*time.Millisecond, nil), // later chunk: ignored
	} {
		tr.Handle(e)
	}
	want := []struct {
		s Stage
		v time.Duration
	}{
		{StageStartup, 12 * time.Millisecond},
		{StageSTTFinal, 150 * time.Millisecond},
		{StageLLMTTFT, 300 * time.Millisecond},
		{StageTTSTTFB, 300 * time.Millisecond},
		{StageVoiceToVoice, 800 * time.Millisecond},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d measurements %+v", len(got), got)
	}
	for i, w := range want {
		if got[i].Stage != w.s || got[i].Value != w.v {
			t.Errorf("[%d] = %s %v, want %s %v", i, got[i].Stage, got[i].Value, w.s, w.v)
		}
	}
}

// A turn with no user-stop (e.g. proactive greeting) still yields LLM/TTS stages but no v2v.
func TestTrackerNoUserStopNoVoiceToVoice(t *testing.T) {
	var got []Measurement
	tr := New(func(m Measurement) { got = append(got, m) })
	tr.Handle(ev(core.DebugLLMRequest, "g", 0, nil))
	tr.Handle(ev(core.DebugLLMFirstToken, "g", 50*time.Millisecond, nil))
	tr.Handle(ev(core.DebugTTSRequest, "g", 60*time.Millisecond, nil))
	tr.Handle(ev(core.DebugBotFirstAudio, "g", 100*time.Millisecond, nil))
	for _, m := range got {
		if m.Stage == StageVoiceToVoice || m.Stage == StageSTTFinal {
			t.Fatalf("unexpected %s", m.Stage)
		}
	}
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
}

// End to end through a live session with the core fakes: measurements must
// arrive in pipeline order and be consistent (stt+...<=voice_to_voice).
func TestEndToEndOrdering(t *testing.T) {
	llm := coretest.NewLLMProcessor(coretest.LLMConfig{ResponseChunks: []string{"hi ", "there"}, ChunkDelay: 5 * time.Millisecond})
	tts := coretest.NewTTSProcessor(coretest.TTSConfig{FramesPerUnit: 2, FrameDelay: 5 * time.Millisecond})
	out := coretest.NewTransportOutput(coretest.TransportConfig{})
	cx := core.NewConversationContext(core.ContextConfig{MaxMessages: 10, MaxTokens: 1000})
	pipe, err := core.NewPipeline(core.NewUserContextProcessor(cx, core.UserContextConfig{TurnPrefix: "turn"}), core.NewAssistantContextProcessor(cx, core.DefaultAssistantContextConfig()), llm, tts, out)
	if err != nil {
		t.Fatal(err)
	}
	s, err := core.NewSession(7, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	ch := make(chan Measurement, 16)
	if _, err := Attach(s, func(m Measurement) { ch <- m }); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if m := <-ch; m.Stage != StageStartup {
		t.Fatalf("first = %s", m.Stage)
	}
	_ = s.Post(&core.UserStoppedSpeakingFrame{Timestamp: time.Now()})
	time.Sleep(10 * time.Millisecond)
	_ = s.Post(&core.FinalTranscriptionFrame{Turn: "turn-1", Text: "Hello", Gen: 1, Timestamp: time.Now()})

	var order []Stage
	vals := map[Stage]time.Duration{}
	for len(order) < 4 {
		select {
		case m := <-ch:
			order = append(order, m.Stage)
			vals[m.Stage] = m.Value
		case <-time.After(5 * time.Second):
			t.Fatalf("timeout; got %v", order)
		}
	}
	want := []Stage{StageSTTFinal, StageLLMTTFT, StageTTSTTFB, StageVoiceToVoice}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
	if vals[StageVoiceToVoice] < vals[StageSTTFinal]+vals[StageLLMTTFT] {
		t.Fatalf("v2v %v < stt %v + ttft %v", vals[StageVoiceToVoice], vals[StageSTTFinal], vals[StageLLMTTFT])
	}
}

// Barge-in: the user interrupts t1 after its first audio and again before t2's
// first audio would have been heard; each turn pairs with its own user stop, the
// abandoned t2 never yields v2v, and t3 is measured from the latest stop.
func TestTrackerBargeIn(t *testing.T) {
	var got []Measurement
	tr := New(func(m Measurement) { got = append(got, m) })
	ms := time.Millisecond
	for _, e := range []core.DebugEvent{
		ev(core.DebugUserStopped, "", 0, nil),
		ev(core.DebugSTTFinal, "t1", 100*ms, nil),
		ev(core.DebugLLMRequest, "t1", 110*ms, nil),
		ev(core.DebugLLMFirstToken, "t1", 200*ms, nil),
		ev(core.DebugTTSRequest, "t1", 210*ms, nil),
		ev(core.DebugBotFirstAudio, "t1", 400*ms, nil), // v2v t1 = 400
		ev(core.DebugUserStopped, "", 1000*ms, nil),    // barge-in over t1
		ev(core.DebugSTTFinal, "t2", 1050*ms, nil),
		ev(core.DebugLLMRequest, "t2", 1060*ms, nil),
		ev(core.DebugUserStopped, "", 1500*ms, nil), // barge-in before t2 speaks
		ev(core.DebugLLMFirstToken, "t2", 1510*ms, nil),
		ev(core.DebugSTTFinal, "t3", 1600*ms, nil),
		ev(core.DebugBotFirstAudio, "t1", 1610*ms, nil), // late stale chunk: ignored
		ev(core.DebugLLMRequest, "t3", 1620*ms, nil),
		ev(core.DebugLLMFirstToken, "t3", 1700*ms, nil),
		ev(core.DebugTTSRequest, "t3", 1710*ms, nil),
		ev(core.DebugBotFirstAudio, "t3", 1900*ms, nil), // v2v t3 = 400 from 3rd stop
	} {
		tr.Handle(e)
	}
	v2v := map[core.TurnID]time.Duration{}
	stt := map[core.TurnID]time.Duration{}
	for _, m := range got {
		if m.Value < 0 {
			t.Fatalf("negative %+v", m)
		}
		switch m.Stage {
		case StageVoiceToVoice:
			v2v[m.Turn] = m.Value
		case StageSTTFinal:
			stt[m.Turn] = m.Value
		}
	}
	if len(v2v) != 2 || v2v["t1"] != 400*ms || v2v["t3"] != 400*ms {
		t.Fatalf("v2v = %v", v2v)
	}
	if stt["t1"] != 100*ms || stt["t2"] != 50*ms || stt["t3"] != 100*ms {
		t.Fatalf("stt = %v", stt)
	}
}
