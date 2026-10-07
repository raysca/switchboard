package core

import (
	"strconv"
	"time"
)

// Latency-relevant DebugEvent names emitted by the session. Each marks the first
// occurrence of a transition (per turn where a turn exists), so observers can
// compute stage latencies from event times alone.
const (
	DebugSessionStarted = "session.started" // attr latency_ms: NewSession -> Start
	DebugUserStopped    = "user.stopped"    // VAD end of user speech
	DebugSTTFinal       = "stt.final"       // first final transcript of the turn
	DebugLLMRequest     = "llm.request"     // user message reached the LLM stage
	DebugLLMFirstToken  = "llm.first_token"
	DebugTTSRequest     = "tts.request" // first synthesis unit of the turn
	DebugBotFirstAudio  = "bot.first_audio"
)

// emitFrameDebug turns a delivered frame into a latency DebugEvent. deliver sees
// a frame once per pipeline hop, so only the first sighting of a (name, key) is
// emitted. It must stay cheap: it runs on the hot path, and with no observers
// it is one atomic load.
func (s *Session) emitFrameDebug(f Frame) {
	if s.observers.n.Load() == 0 {
		return
	}
	var name, turn string
	var gen uint64
	switch v := f.(type) {
	case *UserStoppedSpeakingFrame:
		// Keyed by identity; holding the pointer stops the allocator reusing
		// its address for a later stop (which a %p key would wrongly dedup).
		s.debugMu.Lock()
		dup := s.debugLastStop == v
		s.debugLastStop = v
		s.debugMu.Unlock()
		if !dup {
			s.EmitDebug(NewDebugEvent(DebugUserStopped, time.Now(), SessionID(strconv.FormatUint(s.id, 10)), "", Generation(v.Gen), nil))
		}
		return
	case *FinalTranscriptionFrame:
		name, turn, gen = DebugSTTFinal, v.Turn, v.Gen
	case *UserMessageFrame:
		name, turn, gen = DebugLLMRequest, v.Turn, v.Gen
	case *LLMTextDeltaFrame:
		name, turn, gen = DebugLLMFirstToken, v.Turn, v.Gen
	case *SynthesisUnitFrame:
		name, turn, gen = DebugTTSRequest, v.Turn, v.Gen
	case *OutputAudioFrame:
		if len(v.Samples) == 0 {
			return // empty end-of-unit marker, not audible audio
		}
		name, turn, gen = DebugBotFirstAudio, v.Turn, v.Gen
	default:
		return
	}
	if turn == "" {
		return
	}
	s.debugMu.Lock()
	if s.debugSeen[name] == turn {
		s.debugMu.Unlock()
		return
	}
	if s.debugSeen == nil {
		s.debugSeen = make(map[string]string, 8)
	}
	s.debugSeen[name] = turn
	s.debugMu.Unlock()
	s.EmitDebug(NewDebugEvent(name, time.Now(), SessionID(strconv.FormatUint(s.id, 10)), TurnID(turn), Generation(gen), nil))
}
