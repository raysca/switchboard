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

func TestTranscriptionAndExternalStart(t *testing.T) {
	m := chainManager(t, TranscriptionStart(false))
	i := transcript(0, "a", 1, "hmm", false)
	i.TranscriptFinal = false
	if f, _ := m.Observe(i); startedCount(f) != 0 {
		t.Fatal("interim ignored when useInterim=false")
	}
	if f, _ := m.Observe(transcript(10, "a", 1, "hmm", false)); startedCount(f) != 1 {
		t.Fatal("final transcript should start")
	}
	m = chainManager(t, ExternalStart())
	if f, _ := m.Observe(Observation{At: t0, TurnID: "a", Generation: 1}); startedCount(f) != 0 {
		t.Fatal("no signal, no start")
	}
	if f, _ := m.Observe(Observation{At: t0.Add(time.Millisecond), TurnID: "a", Generation: 1, ExternalStart: true}); startedCount(f) != 1 {
		t.Fatal("external signal should start")
	}
}

func TestWakePhraseGatesThenTimesOut(t *testing.T) {
	m := chainManager(t, WakePhraseStart([]string{"hey bot"}, 2*time.Second), VADStart())
	m.Observe(observation(t0, "a", 1, true))
	if f, _ := m.Advance(t0.Add(300 * time.Millisecond)); startedCount(f) != 0 {
		t.Fatal("asleep: VAD must be blocked")
	}
	m.Observe(observation(t0.Add(400*time.Millisecond), "a", 1, false))
	if f, _ := m.Observe(transcript(1000, "b", 1, "Hey, Bot!", false)); startedCount(f) != 1 {
		t.Fatal("wake phrase should start a turn")
	}
	// Awake: plain VAD speech starts a turn in the next generation.
	m.Observe(observation(t0.Add(1500*time.Millisecond), "c", 2, true))
	if f, _ := m.Advance(t0.Add(1800 * time.Millisecond)); startedCount(f) != 1 {
		t.Fatal("awake: VAD should pass through")
	}
	// After the timeout it sleeps again.
	m.Observe(observation(t0.Add(10*time.Second), "d", 3, true))
	if f, _ := m.Advance(t0.Add(10*time.Second + 300*time.Millisecond)); startedCount(f) != 0 {
		t.Fatal("timed out: VAD must be blocked again")
	}
}

func TestNilStrategyRejected(t *testing.T) {
	cfg := testConfig()
	cfg.StartStrategies = []UserTurnStartStrategy{nil}
	if _, err := NewManager(cfg, SilenceDetector{}); err == nil {
		t.Fatal("want ErrInvalidConfig")
	}
}

func TestWakePhraseStaysAwakeThroughLongBotSpeech(t *testing.T) {
	m := chainManager(t, WakePhraseStart([]string{"hey bot"}, 2*time.Second), VADStart())
	if f, _ := m.Observe(transcript(0, "a", 1, "hey bot", false)); startedCount(f) != 1 {
		t.Fatal("wake phrase should start a turn")
	}
	// Bot talks for 5s (> timeout); its activity keeps the strategy awake.
	for ms := 500; ms <= 5000; ms += 500 {
		o := Observation{At: t0.Add(time.Duration(ms) * time.Millisecond), BotSpeaking: true}
		if ms <= 1000 { // user turn "a" completes on 1s silence
			o.TurnID, o.Generation = "a", 1
		}
		m.Observe(o)
	}
	m.Observe(observation(t0.Add(5200*time.Millisecond), "b", 2, true))
	if f, _ := m.Advance(t0.Add(5500 * time.Millisecond)); startedCount(f) != 1 {
		t.Fatal("bot activity must refresh the wake timeout")
	}
}

func TestWakePhraseNonASCII(t *testing.T) {
	m := chainManager(t, WakePhraseStart([]string{"Привет бот"}, time.Second))
	if f, _ := m.Observe(transcript(0, "a", 1, "Привет, бот!", false)); startedCount(f) != 1 {
		t.Fatal("non-ASCII wake phrase should match")
	}
}
