// Package observe derives per-turn latency measurements from core DebugEvents.
// It is provider-neutral and knows nothing about metrics backends: results go to
// a Sink. Pipecat equivalents: user_bot_latency, service_metrics, startup_timing
// and turn_tracking observers, collapsed into one event-time state machine.
package observe

import (
	"time"

	"govoice/core"
)

// Stage names are a fixed, bounded set, safe as a metric label.
type Stage string

const (
	StageStartup      Stage = "startup"        // session create -> start
	StageSTTFinal     Stage = "stt_final"      // user stopped -> final transcript
	StageLLMTTFT      Stage = "llm_ttft"       // LLM request -> first token
	StageTTSTTFB      Stage = "tts_ttfb"       // first synthesis request -> first bot audio
	StageVoiceToVoice Stage = "voice_to_voice" // user stopped -> first bot audio
)

// Stages lists every Stage a Tracker can emit.
var Stages = []Stage{StageStartup, StageSTTFinal, StageLLMTTFT, StageTTSTTFB, StageVoiceToVoice}

// Measurement is one computed latency. Turn is empty for startup.
type Measurement struct {
	Stage Stage
	Turn  core.TurnID
	Value time.Duration
	At    time.Time // event time that completed the measurement
}

// Sink receives measurements on the observer goroutine; keep it quick.
type Sink func(Measurement)

type turnState struct {
	stop, llmReq, ttsReq time.Time // zero = not seen
}

// maxTurns bounds state if turns start but never reach bot audio (interrupted).
const maxTurns = 32

// Tracker is a single-goroutine state machine over time-ordered DebugEvents.
type Tracker struct {
	sink     Sink
	lastStop time.Time
	turns    map[core.TurnID]*turnState
}

func New(sink Sink) *Tracker {
	return &Tracker{sink: sink, turns: make(map[core.TurnID]*turnState)}
}

func (t *Tracker) turn(id core.TurnID) *turnState {
	st := t.turns[id]
	if st == nil {
		if len(t.turns) >= maxTurns {
			t.turns = make(map[core.TurnID]*turnState) // ponytail: drop-all beats an LRU for a rare overflow
		}
		st = &turnState{}
		t.turns[id] = st
	}
	return st
}

func (t *Tracker) emit(s Stage, turn core.TurnID, from, to time.Time) {
	if from.IsZero() || to.Before(from) {
		return
	}
	t.sink(Measurement{Stage: s, Turn: turn, Value: to.Sub(from), At: to})
}

// Handle consumes one DebugEvent. Unknown events are ignored.
func (t *Tracker) Handle(e core.DebugEvent) {
	switch e.Name {
	case core.DebugSessionStarted:
		if ms, ok := e.Attributes()["latency_ms"].(int64); ok && ms >= 0 {
			t.sink(Measurement{Stage: StageStartup, Value: time.Duration(ms) * time.Millisecond, At: e.Time})
		}
	case core.DebugUserStopped:
		t.lastStop = e.Time
	case core.DebugSTTFinal:
		st := t.turn(e.TurnID)
		if st.stop.IsZero() && !t.lastStop.IsZero() {
			st.stop, t.lastStop = t.lastStop, time.Time{} // a stop belongs to one turn
			t.emit(StageSTTFinal, e.TurnID, st.stop, e.Time)
		}
	case core.DebugLLMRequest:
		t.turn(e.TurnID).llmReq = e.Time
	case core.DebugLLMFirstToken:
		if st := t.turns[e.TurnID]; st != nil {
			t.emit(StageLLMTTFT, e.TurnID, st.llmReq, e.Time)
			st.llmReq = time.Time{}
		}
	case core.DebugTTSRequest:
		t.turn(e.TurnID).ttsReq = e.Time
	case core.DebugBotFirstAudio:
		if st := t.turns[e.TurnID]; st != nil {
			t.emit(StageTTSTTFB, e.TurnID, st.ttsReq, e.Time)
			t.emit(StageVoiceToVoice, e.TurnID, st.stop, e.Time)
			delete(t.turns, e.TurnID)
		}
	}
}

// Attach subscribes a Tracker to s. Attaching after Start is safe; only the
// startup measurement (emitted once by Start) is missed.
func Attach(s *core.Session, sink Sink) (cancel func(), err error) {
	_, cancel, err = s.Subscribe(0, New(sink).Handle)
	return cancel, err
}
