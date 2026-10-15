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

// startKeys name the stages whose start offsets are stored.
var startKeys = map[observe.Stage]string{observe.StageLLMTTFT: "llm_start_ms", observe.StageTTSTTFB: "tts_start_ms"}

// turnLatencies collects a call's per-turn stage latencies from the observer
// goroutine for the turn recorder, which takes a turn's figures when it writes
// that turn's row.
type turnLatencies struct {
	mu  sync.Mutex
	per map[core.TurnID]map[observe.Stage]stageTime
}

// stageTime is one measured stage: how long it took and when it ended (zero when unknown).
type stageTime struct {
	value time.Duration
	end   time.Time
}

func newTurnLatencies() *turnLatencies {
	return &turnLatencies{per: map[core.TurnID]map[observe.Stage]stageTime{}}
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
		st = map[observe.Stage]stageTime{}
		t.per[m.Turn] = st
	}
	st[m.Stage] = stageTime{m.Value, m.At} // a later measurement (the last model request of a tool loop) replaces
}

// take returns the turn's figures as a JSON object (integer milliseconds) and forgets the turn. Besides
// each stage's duration it carries when the model and voice stages started, as offsets from the turn's
// origin (the earliest measured stage start: the caller finishing, or the model request for a typed turn),
// so a reply can be drawn as a waterfall. A stage with no end time keeps its duration and gets no offset.
func (t *turnLatencies) take(turn string) string {
	t.mu.Lock()
	st := t.per[core.TurnID(turn)]
	delete(t.per, core.TurnID(turn))
	t.mu.Unlock()
	out := make(map[string]int64, len(st)+2)
	var origin time.Time
	for stage, s := range st {
		out[stageKeys[stage]] = s.value.Milliseconds()
		if start := s.end.Add(-s.value); !s.end.IsZero() && (origin.IsZero() || start.Before(origin)) {
			origin = start
		}
	}
	if !origin.IsZero() {
		for stage, key := range startKeys {
			if s, ok := st[stage]; ok && !s.end.IsZero() {
				out[key] = max(s.end.Add(-s.value).Sub(origin).Milliseconds(), 0)
			}
		}
	}
	b, _ := json.Marshal(out) // a map of strings to integers cannot fail
	return string(b)
}
