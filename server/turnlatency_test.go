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
