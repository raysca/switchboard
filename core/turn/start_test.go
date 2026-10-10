package turn

import (
	"testing"
	"time"

	"govoice/core"
)

func startedCount(frames []core.Frame) (n int) {
	for _, e := range eventFrames(frames) {
		if e.Kind == core.TurnEventStarted {
			n++
		}
	}
	return
}

func chainManager(t *testing.T, s ...UserTurnStartStrategy) *Manager {
	t.Helper()
	cfg := testConfig()
	cfg.StartStrategies = s
	m, err := NewManager(cfg, SilenceDetector{})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(t0); err != nil {
		t.Fatal(err)
	}
	return m
}

// obs sends a transcript (no VAD) for a turn at t0+ms.
func transcript(ms int, id core.TurnID, gen core.Generation, text string, bot bool) Observation {
	return Observation{At: t0.Add(time.Duration(ms) * time.Millisecond), TurnID: id, Generation: gen,
		Transcript: text, TranscriptFinal: true, BotSpeaking: bot}
}

func TestMinWordsSuppressesBackchannelButNotFullPhrase(t *testing.T) {
	m := chainManager(t, MinWordsStart(3, true))
	// VAD alone never starts under min-words, even past debounce.
	f, _ := m.Observe(observation(t0, "a", 1, true))
	f2, _ := m.Advance(t0.Add(500 * time.Millisecond))
	if startedCount(f)+startedCount(f2) != 0 {
		t.Fatal("VAD must not start a turn under min-words")
	}
	// Backchannel while the bot speaks: no barge-in.
	f, _ = m.Observe(transcript(600, "a", 1, "uh-huh", true))
	if startedCount(f) != 0 {
		t.Fatal("backchannel started a turn")
	}
	// Full phrase while the bot speaks: barge-in.
	f, _ = m.Observe(transcript(1000, "b", 1, "wait stop talking please", true))
	if startedCount(f) != 1 {
		t.Fatalf("full phrase should start a turn, frames=%v", f)
	}
}

func TestMinWordsSingleWordStartsWhenBotSilent(t *testing.T) {
	m := chainManager(t, MinWordsStart(3, true))
	f, _ := m.Observe(transcript(0, "a", 1, "hello", false))
	if startedCount(f) != 1 {
		t.Fatal("one word should start when bot is silent")
	}
}

func TestChainFirstYesWinsAndVADStillWorksAlongside(t *testing.T) {
	m := chainManager(t, MinWordsStart(3, true), VADStart())
	m.Observe(observation(t0, "a", 1, true))
	f, _ := m.Advance(t0.Add(250 * time.Millisecond))
	if startedCount(f) != 1 {
		t.Fatal("VAD later in the chain should start the turn")
	}
}

func TestExternalStart(t *testing.T) {
	m := chainManager(t, ExternalStart())
	if f, _ := m.Observe(Observation{At: t0, TurnID: "a", Generation: 1}); startedCount(f) != 0 {
		t.Fatal("no signal, no start")
	}
	if f, _ := m.Observe(Observation{At: t0.Add(time.Millisecond), TurnID: "a", Generation: 1, ExternalStart: true}); startedCount(f) != 1 {
		t.Fatal("external signal should start")
	}
}

func TestNilStrategyRejected(t *testing.T) {
	cfg := testConfig()
	cfg.StartStrategies = []UserTurnStartStrategy{nil}
	if _, err := NewManager(cfg, SilenceDetector{}); err == nil {
		t.Fatal("want ErrInvalidConfig")
	}
}
