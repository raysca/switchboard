package server

import (
	"encoding/json"
	"sync"
	"time"

	"govoice/core"
	"govoice/core/observe"
)

// maxTurnLatencies bounds the turns held while they wait for their reply row.
const maxTurnLatencies = 32

// stageKeys name the stored stages; startup has no turn and is not stored.
var stageKeys = map[observe.Stage]string{
	observe.StageSTTFinal:     "stt_final_ms",
	observe.StageLLMTTFT:      "llm_ttft_ms",
	observe.StageTTSTTFB:      "tts_ttfb_ms",
	observe.StageVoiceToVoice: "voice_to_voice_ms",
}

// turnLatencies collects a call's per-turn stage latencies from the observer
// goroutine for the turn recorder, which takes a turn's figures when it writes
// that turn's row.
type turnLatencies struct {
	mu  sync.Mutex
	per map[core.TurnID]map[observe.Stage]time.Duration
}

func newTurnLatencies() *turnLatencies {
	return &turnLatencies{per: map[core.TurnID]map[observe.Stage]time.Duration{}}
}

func (t *turnLatencies) observe(m observe.Measurement) {
	if m.Turn == "" {
		return
	}
	if _, ok := stageKeys[m.Stage]; !ok {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.per[m.Turn]
	if st == nil {
		if len(t.per) >= maxTurnLatencies {
			clear(t.per) // ponytail: turns that never got a row; dropping all beats tracking age
		}
		st = map[observe.Stage]time.Duration{}
		t.per[m.Turn] = st
	}
	st[m.Stage] = m.Value
}

// take returns the turn's figures as a JSON object (integer milliseconds) and forgets the turn.
func (t *turnLatencies) take(turn string) string {
	t.mu.Lock()
	st := t.per[core.TurnID(turn)]
	delete(t.per, core.TurnID(turn))
	t.mu.Unlock()
	out := make(map[string]int64, len(st))
	for stage, d := range st {
		out[stageKeys[stage]] = d.Milliseconds()
	}
	b, _ := json.Marshal(out) // a map of strings to integers cannot fail
	return string(b)
}
