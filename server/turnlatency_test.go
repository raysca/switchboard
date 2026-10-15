package server

import (
	"encoding/json"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/observe"
)

func TestTurnLatenciesStoreWhatWasMeasuredInMilliseconds(t *testing.T) {
	l := newTurnLatencies()
	l.observe(observe.Measurement{Stage: observe.StageSTTFinal, Turn: "t1", Value: 180 * time.Millisecond})
	l.observe(observe.Measurement{Stage: observe.StageLLMTTFT, Turn: "t1", Value: 420 * time.Millisecond})
	l.observe(observe.Measurement{Stage: observe.StageVoiceToVoice, Turn: "t1", Value: 900 * time.Millisecond})
	l.observe(observe.Measurement{Stage: observe.StageStartup, Value: time.Second}) // no turn: not stored
	var got map[string]int64
	if err := json.Unmarshal([]byte(l.take("t1")), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got["stt_final_ms"] != 180 || got["llm_ttft_ms"] != 420 || got["voice_to_voice_ms"] != 900 {
		t.Fatalf("got %v", got)
	}
	if l.take("t1") != "{}" {
		t.Fatal("a turn must be forgotten once taken")
	}
}

func TestATurnWithNoMeasurementsIsAnEmptyObject(t *testing.T) {
	if got := newTurnLatencies().take("never-measured"); got != "{}" {
		t.Fatalf("got %q", got)
	}
}

func TestTurnLatenciesAreBounded(t *testing.T) {
	l := newTurnLatencies()
	for i := 0; i < 10*maxTurnLatencies; i++ {
		l.observe(observe.Measurement{Stage: observe.StageVoiceToVoice, Turn: core.TurnID(string(rune('A' + i))), Value: time.Millisecond})
	}
	l.mu.Lock()
	n := len(l.per)
	l.mu.Unlock()
	if n > maxTurnLatencies {
		t.Fatalf("%d turns held, cap %d", n, maxTurnLatencies)
	}
}

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

func latencies(t *testing.T, l *turnLatencies, turn string) map[string]int64 {
	t.Helper()
	var got map[string]int64
	if err := json.Unmarshal([]byte(l.take(turn)), &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestTurnLatenciesCarryWhenTheModelAndVoiceStagesStarted(t *testing.T) {
	t0 := time.Unix(1000, 0)
	l := newTurnLatencies()
	l.observe(observe.Measurement{Stage: observe.StageSTTFinal, Turn: "t", Value: ms(410), At: t0.Add(ms(410))})
	l.observe(observe.Measurement{Stage: observe.StageLLMTTFT, Turn: "t", Value: ms(380), At: t0.Add(ms(900))})
	l.observe(observe.Measurement{Stage: observe.StageTTSTTFB, Turn: "t", Value: ms(260), At: t0.Add(ms(1165))})
	l.observe(observe.Measurement{Stage: observe.StageVoiceToVoice, Turn: "t", Value: ms(1165), At: t0.Add(ms(1165))})
	want := map[string]int64{"stt_final_ms": 410, "llm_start_ms": 520, "llm_ttft_ms": 380, "tts_start_ms": 905, "tts_ttfb_ms": 260, "voice_to_voice_ms": 1165}
	got := latencies(t, l, "t")
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s = %d, want %d (%v)", k, got[k], v, got)
		}
	}
}

func TestATypedTurnMeasuresFromItsModelRequest(t *testing.T) {
	t0 := time.Unix(2000, 0)
	l := newTurnLatencies()
	l.observe(observe.Measurement{Stage: observe.StageLLMTTFT, Turn: "t", Value: ms(300), At: t0.Add(ms(300))})
	l.observe(observe.Measurement{Stage: observe.StageTTSTTFB, Turn: "t", Value: ms(200), At: t0.Add(ms(700))})
	got := latencies(t, l, "t")
	if got["llm_start_ms"] != 0 || got["tts_start_ms"] != 500 || got["llm_ttft_ms"] != 300 || got["tts_ttfb_ms"] != 200 {
		t.Fatalf("got %v", got)
	}
	if _, ok := got["stt_final_ms"]; ok {
		t.Fatal("no stt measured, none stored")
	}
}

func TestOnlyWhatWasMeasuredGetsAnOffsetAndNeverANegativeOne(t *testing.T) {
	t0 := time.Unix(3000, 0)
	l := newTurnLatencies()
	l.observe(observe.Measurement{Stage: observe.StageLLMTTFT, Turn: "only", Value: ms(250), At: t0.Add(ms(250))})
	if got := latencies(t, l, "only"); got["llm_start_ms"] != 0 || len(got) != 2 {
		t.Fatalf("one stage: %v", got)
	}
	// A stage whose end time is missing (zero) keeps its duration and gets no offset.
	l.observe(observe.Measurement{Stage: observe.StageLLMTTFT, Turn: "noat", Value: ms(250)})
	if got := latencies(t, l, "noat"); len(got) != 1 || got["llm_ttft_ms"] != 250 {
		t.Fatalf("no At: %v", got)
	}
	// A later measurement of a stage replaces the earlier (the last model request of a tool loop).
	l.observe(observe.Measurement{Stage: observe.StageSTTFinal, Turn: "loop", Value: ms(100), At: t0.Add(ms(100))})
	l.observe(observe.Measurement{Stage: observe.StageLLMTTFT, Turn: "loop", Value: ms(200), At: t0.Add(ms(400))})
	l.observe(observe.Measurement{Stage: observe.StageLLMTTFT, Turn: "loop", Value: ms(150), At: t0.Add(ms(1200))})
	got := latencies(t, l, "loop")
	if got["llm_ttft_ms"] != 150 || got["llm_start_ms"] != 1050 {
		t.Fatalf("last request must win: %v", got)
	}
	for k, v := range got {
		if v < 0 {
			t.Fatalf("%s negative: %d", k, v)
		}
	}
}
