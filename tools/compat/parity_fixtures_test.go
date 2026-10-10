package compat_test

// Paired Go-vs-Pipecat fixtures. Goldens in testdata/parity/*.json are written by
// tools/compat/parity/gen_goldens.py (make parity-regen), which executes the pinned
// pipecat-impl components on scripted inputs. These tests run the same inputs through
// go-voice and need no Python: each scenario must equal the Pipecat output, or carry a
// declared intentional_difference (reason + the go-voice output) that must still hold.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/audio/dtmf"
	"govoice/core/turn"
)

const parityRevision = "2967e1c09ad484076089bc88427d305fe8f79fa9"

type parityStep struct {
	T     int    `json:"t"`
	Ev    string `json:"ev"`
	Text  string `json:"text"`
	ID    string `json:"id"`
	Digit string `json:"digit"`
	Chunk string `json:"chunk"`
}

type parityUnit struct {
	Text  string `json:"text"`
	DurMs int    `json:"dur_ms"`
	Words []struct {
		W string `json:"w"`
		S int    `json:"s"`
		E int    `json:"e"`
	} `json:"words"`
}

type parityMsg struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
	ID   string `json:"id"`
	Turn int    `json:"turn"`
}

type parityScenario struct {
	ID            string          `json:"id"`
	PipecatModule string          `json:"pipecat_module"`
	Params        json.RawMessage `json:"params"`
	Steps         []parityStep    `json:"steps"`
	Units         []parityUnit    `json:"units"`
	InterruptMs   *int            `json:"interrupt_ms"`
	Messages      []parityMsg     `json:"messages"`
	Pipecat       []string        `json:"pipecat"`
	Diff          *struct {
		Reason string   `json:"reason"`
		Go     []string `json:"go"`
	} `json:"intentional_difference"`
}

type parityGolden struct {
	Reference struct {
		Repository string `json:"repository"`
		Revision   string `json:"revision"`
		Generator  string `json:"generator"`
	} `json:"reference"`
	Area      string           `json:"area"`
	Scenarios []parityScenario `json:"scenarios"`
}

// runParity loads testdata/parity/<area>.json and checks every scenario against run.
func runParity(t *testing.T, area string, run func(t *testing.T, sc parityScenario) []string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "parity", area+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var g parityGolden
	if err := json.Unmarshal(data, &g); err != nil {
		t.Fatal(err)
	}
	if g.Reference.Repository != "pipecat-impl" || g.Reference.Revision != parityRevision || g.Area != area || len(g.Scenarios) == 0 {
		t.Fatalf("unrecognized golden header: %+v", g.Reference)
	}
	for _, sc := range g.Scenarios {
		t.Run(sc.ID, func(t *testing.T) {
			got := run(t, sc)
			want := sc.Pipecat
			if sc.Diff != nil {
				if strings.TrimSpace(sc.Diff.Reason) == "" {
					t.Fatal("intentional_difference without a reason")
				}
				if reflect.DeepEqual(sc.Diff.Go, sc.Pipecat) {
					t.Fatal("declared difference equals the Pipecat output: drop it from DIFFS in gen_goldens.py")
				}
				want = sc.Diff.Go
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("go-voice %q\n want   %q\n pipecat %q (intentional difference declared: %v)", got, want, sc.Pipecat, sc.Diff != nil)
			}
		})
	}
}

// TestParityGoldensPinToReference checks the pinned checkout is the one the goldens came from
// and that every cited Pipecat module exists in it.
func TestParityGoldensPinToReference(t *testing.T) {
	refDir := filepath.Join("..", "..", "pipecat-impl")
	if head, err := exec.Command("git", "-C", refDir, "rev-parse", "HEAD").Output(); err != nil || strings.TrimSpace(string(head)) != parityRevision {
		t.Fatalf("pipecat-impl is not at %s: %q %v", parityRevision, head, err)
	}
	files, _ := filepath.Glob(filepath.Join("testdata", "parity", "*.json"))
	total := 0
	for _, f := range files {
		data, _ := os.ReadFile(f)
		var g parityGolden
		if err := json.Unmarshal(data, &g); err != nil {
			t.Fatal(err)
		}
		for _, sc := range g.Scenarios {
			total++
			if _, err := os.Stat(filepath.Join(refDir, "src", "pipecat", sc.PipecatModule)); err != nil {
				t.Errorf("%s/%s cites missing module %s", g.Area, sc.ID, sc.PipecatModule)
			}
		}
	}
	if total < 12 {
		t.Fatalf("only %d paired scenarios, want >= 12", total)
	}
}

// ---------------------------------------------------------------- turn start/stop

type startSpec struct {
	Kind       string   `json:"kind"`
	MinWords   int      `json:"min_words"`
	UseInterim bool     `json:"use_interim"`
	Phrases    []string `json:"phrases"`
	TimeoutMs  int      `json:"timeout_ms"`
}

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

func TestParityTurnStartStop(t *testing.T) {
	runParity(t, "turn", func(t *testing.T, sc parityScenario) []string {
		var p struct {
			Start  []startSpec `json:"start"`
			StopMs int         `json:"stop_ms"`
			EndMs  int         `json:"end_ms"`
		}
		if err := json.Unmarshal(sc.Params, &p); err != nil {
			t.Fatal(err)
		}
		cfg := turn.DefaultConfig()
		// Pipecat's VAD analyzer owns start/stop debounce; neutralize the Manager's so only strategy policy is compared.
		cfg.StartDebounce, cfg.StopDebounce, cfg.MinimumSpeechDuration = 0, 0, 0
		cfg.SilenceTimeout, cfg.MaxTurnDuration, cfg.InitialSilenceTimeout = ms(p.StopMs), time.Hour, time.Hour
		for _, s := range p.Start {
			switch s.Kind {
			case "vad":
				cfg.StartStrategies = append(cfg.StartStrategies, turn.VADStart())
			case "min_words":
				cfg.StartStrategies = append(cfg.StartStrategies, turn.MinWordsStart(s.MinWords, s.UseInterim))
			case "transcription", "wake":
				t.Skipf("start strategy %q is not implemented in go-voice (unused by the server)", s.Kind)
			default:
				t.Fatalf("unknown start strategy %q", s.Kind)
			}
		}
		m, err := turn.NewManager(cfg, turn.SpeechTimeout(0))
		if err != nil {
			t.Fatal(err)
		}
		base := time.Unix(1000, 0)
		if err := m.Start(base); err != nil {
			t.Fatal(err)
		}
		var out []string
		n, vad, bot := 1, false, false
		collect := func(at int, frames []core.Frame, err error) {
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range frames {
				if e, ok := f.(*core.TurnEventFrame); ok {
					switch e.Kind {
					case core.TurnEventStarted:
						out = append(out, fmt.Sprintf("%d:turn_started", at))
					case core.TurnEventCompleted:
						out = append(out, fmt.Sprintf("%d:turn_stopped", at))
						n++
					}
				}
			}
		}
		cur := 0
		advance := func(to int) {
			for ; cur+10 <= to; cur += 10 {
				fr, err := m.Advance(base.Add(ms(cur + 10)))
				collect(cur+10, fr, err)
			}
		}
		for _, st := range sc.Steps {
			advance(st.T)
			obs := turn.Observation{At: base.Add(ms(st.T)), TurnID: core.TurnID(fmt.Sprintf("turn-%d", n)), Generation: 1}
			switch st.Ev {
			case "vad_start":
				vad = true
			case "vad_stop":
				vad = false
			case "bot_start":
				bot = true
			case "bot_stop":
				bot = false
			case "interim", "final":
				obs.Transcript, obs.TranscriptFinal = st.Text, st.Ev == "final"
			default:
				t.Fatalf("unknown step %q", st.Ev)
			}
			obs.Speaking, obs.BotSpeaking = vad, bot
			fr, err := m.Observe(obs)
			collect(st.T, fr, err)
		}
		advance(p.EndMs)
		return out
	})
}

// ---------------------------------------------------------------- user idle

type idleClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*idleTimer
}
type idleTimer struct {
	c    *idleClock
	ch   chan time.Time
	at   time.Time
	done bool
}

func (t *idleTimer) C() <-chan time.Time { return t.ch }
func (t *idleTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := !t.done
	t.done = true
	return was
}
func (c *idleClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *idleClock) TimerAt(at time.Time) core.IdleTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &idleTimer{c: c, ch: make(chan time.Time, 1), at: at}
	if !at.After(c.now) {
		t.done = true
		t.ch <- c.now
	}
	c.timers = append(c.timers, t)
	return t
}

// next is the earliest live deadline (zero if none).
func (c *idleClock) next() (at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, t := range c.timers {
		if !t.done && (at.IsZero() || t.at.Before(at)) {
			at = t.at
		}
	}
	return at
}
func (c *idleClock) advanceTo(to time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = to
	for _, t := range c.timers {
		if !t.done && !t.at.After(to) {
			t.done = true
			t.ch <- to
		}
	}
}

// idleSession is the minimal SessionController the controller needs; it records prompts.
type idleSession struct {
	clk    *idleClock
	base   time.Time
	mu     sync.Mutex
	events []string
	wg     sync.WaitGroup
	ctx    context.Context
}

func (s *idleSession) ID() uint64                  { return 1 }
func (s *idleSession) Context() context.Context    { return s.ctx }
func (s *idleSession) CurrentGeneration() uint64   { return 1 }
func (s *idleSession) IncrementGeneration() uint64 { return 2 }
func (s *idleSession) Go(fn func(context.Context)) error {
	s.wg.Add(1)
	go func() { defer s.wg.Done(); fn(s.ctx) }()
	return nil
}
func (s *idleSession) Reenter(core.Frame, core.Direction, int, uint64) error { return nil }
func (s *idleSession) EmitDebug(e core.DebugEvent) {
	if e.Name == core.DebugIdleUserPrompt {
		s.mu.Lock()
		s.events = append(s.events, fmt.Sprintf("%d:idle", s.clk.Now().Sub(s.base).Milliseconds()))
		s.mu.Unlock()
	}
}
func (s *idleSession) got() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}

type nopEmitter struct{}

func (nopEmitter) Push(core.Frame) error                          { return nil }
func (nopEmitter) PushDirection(core.Frame, core.Direction) error { return nil }

func TestParityUserIdle(t *testing.T) {
	runParity(t, "idle", func(t *testing.T, sc parityScenario) []string {
		var p struct {
			TimeoutMs int `json:"timeout_ms"`
			EndMs     int `json:"end_ms"`
		}
		if err := json.Unmarshal(sc.Params, &p); err != nil {
			t.Fatal(err)
		}
		base := time.Unix(1000, 0)
		clk := &idleClock{now: base}
		ctx, cancel := context.WithCancel(context.Background())
		sess := &idleSession{clk: clk, base: base, ctx: ctx}
		defer func() { cancel(); sess.wg.Wait() }()
		ctl, err := core.NewIdleController(core.IdleConfig{UserTimeout: ms(p.TimeoutMs), MaxPrompts: 1, Clock: clk,
			History: core.NewConversationContext(core.ContextConfig{})})
		if err != nil {
			t.Fatal(err)
		}
		if err := ctl.Attach(sess, 0); err != nil {
			t.Fatal(err)
		}
		// The controller runs on its own goroutine: after every input or clock move, give it a
		// moment to park before the next one. Expected prompts are awaited, so a slow run waits
		// rather than misses one.
		settle := func() { time.Sleep(20 * time.Millisecond) }
		want := sc.Pipecat
		if sc.Diff != nil {
			want = sc.Diff.Go
		}
		settle()
		stepTo := func(target time.Time) {
			for {
				at := clk.next()
				if at.IsZero() || at.After(target) {
					break
				}
				clk.advanceTo(at)
				deadline := time.Now().Add(3 * time.Second)
				for len(sess.got()) < countUpTo(want, at.Sub(base)) && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
				settle()
			}
			clk.advanceTo(target)
			settle()
		}
		for _, st := range sc.Steps {
			stepTo(base.Add(ms(st.T)))
			var f core.Frame
			dir := core.Downstream
			switch st.Ev {
			case "bot_start":
				f, dir = &core.SpeakingStateFrame{State: core.SpeakingStarted}, core.Upstream
			case "bot_stop":
				f, dir = &core.SpeakingStateFrame{State: core.SpeakingStopped, Reason: core.SpeakingStopDrained}, core.Upstream
			case "user_start":
				f = &core.UserStartedSpeakingFrame{}
			case "user_stop":
				f = &core.UserStoppedSpeakingFrame{}
			default:
				t.Fatalf("unknown step %q", st.Ev)
			}
			if err := ctl.ProcessFrame(ctx, f, dir, nopEmitter{}); err != nil {
				t.Fatal(err)
			}
			settle()
		}
		stepTo(base.Add(ms(p.EndMs)))
		return append([]string{}, sess.got()...)
	})
}

// countUpTo is how many expected "<ms>:idle" entries are due at or before d.
func countUpTo(want []string, d time.Duration) (n int) {
	for _, w := range want {
		var at int
		if _, err := fmt.Sscanf(w, "%d:idle", &at); err == nil && ms(at) <= d {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------- user mute

type fixedClock struct{ now time.Time }
type neverTimer struct{}

func (neverTimer) C() <-chan time.Time                 { return nil }
func (neverTimer) Stop() bool                          { return true }
func (c fixedClock) Now() time.Time                    { return c.now }
func (c fixedClock) NewTimer(time.Duration) turn.Timer { return neverTimer{} }

func TestParityUserMute(t *testing.T) {
	runParity(t, "mute", func(t *testing.T, sc parityScenario) []string {
		var p struct {
			Strategy string `json:"strategy"`
		}
		if err := json.Unmarshal(sc.Params, &p); err != nil {
			t.Fatal(err)
		}
		tools := turn.NewToolActivity()
		strategies, err := turn.ParseMuteStrategies([]string{p.Strategy}, tools)
		if err != nil {
			t.Fatal(err)
		}
		proc, err := turn.NewProcessor(turn.DefaultConfig(), turn.ProcessorOptions{Clock: fixedClock{time.Unix(1000, 0)}, Mute: strategies})
		if err != nil {
			t.Fatal(err)
		}
		defer proc.Close()
		type call struct {
			release chan error
			entered chan struct{}
			done    chan struct{}
		}
		var mu sync.Mutex
		calls := map[string]*call{}
		get := func(id string) *call { mu.Lock(); defer mu.Unlock(); return calls[id] }
		exec := tools.Wrap(core.ToolExecutorFunc(func(_ context.Context, c core.ToolCall) (core.ToolResult, error) {
			k := get(c.ID)
			close(k.entered)
			return core.ToolResult{}, <-k.release
		}), nil)
		var out []string
		prev := false
		send := func(f core.Frame, dir core.Direction) {
			if err := proc.ProcessFrame(context.Background(), f, dir, nopEmitter{}); err != nil {
				t.Fatal(err)
			}
		}
		for _, st := range sc.Steps {
			switch st.Ev {
			case "bot_start":
				send(&core.SpeakingStateFrame{State: core.SpeakingStarted}, core.Upstream)
			case "bot_stop":
				send(&core.SpeakingStateFrame{State: core.SpeakingStopped}, core.Upstream)
			case "tool_start":
				k := &call{make(chan error, 1), make(chan struct{}), make(chan struct{})}
				mu.Lock()
				calls[st.ID] = k
				mu.Unlock()
				go func() {
					defer close(k.done)
					_, _ = exec.Execute(context.Background(), core.ToolCall{ID: st.ID, Name: "f"})
				}()
				<-k.entered
			case "tool_result", "tool_cancel":
				k := get(st.ID)
				k.release <- map[string]error{"tool_result": nil, "tool_cancel": context.Canceled}[st.Ev]
				<-k.done
			case "probe", "error":
				// A Pipecat ErrorFrame reaches the strategy; go-voice's processor never sees LLM/TTS errors,
				// so this step is a plain re-evaluation.
			default:
				t.Fatalf("unknown step %q", st.Ev)
			}
			send(&core.InputAudioFrame{}, core.Downstream) // re-evaluates mute state
			if now := proc.Muted(); now != prev {
				out = append(out, fmt.Sprintf("%d:%s", st.T, map[bool]string{true: "muted", false: "unmuted"}[now]))
				prev = now
			}
		}
		return out
	})
}

// ---------------------------------------------------------------- DTMF aggregation

func TestParityDTMFAggregator(t *testing.T) {
	runParity(t, "dtmf", func(t *testing.T, sc parityScenario) []string {
		var p struct {
			TimeoutMs int `json:"timeout_ms"`
			TailMs    int `json:"tail_ms"`
		}
		if err := json.Unmarshal(sc.Params, &p); err != nil {
			t.Fatal(err)
		}
		sink := &core.PassthroughProcessor{}
		pipe, err := core.NewPipeline(dtmf.NewAggregator(dtmf.AggregatorConfig{Timeout: ms(p.TimeoutMs)}), sink)
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
		last := 0
		for _, st := range sc.Steps {
			time.Sleep(ms(st.T - last))
			last = st.T
			if err := s.Post(&core.InputDTMFFrame{Digit: st.Digit, Timestamp: time.Now()}); err != nil {
				t.Fatal(err)
			}
		}
		time.Sleep(ms(p.TailMs)) // let an idle-timeout flush land; tail 0 = end of stream right away
		_ = s.Close()            // Pipecat flushes pending digits at EndFrame, go-voice drops them
		var out []string
		for _, f := range sink.GetFrames() {
			switch v := f.(type) {
			case *core.InterruptionFrame:
				out = append(out, "interrupt")
			case *core.FinalTranscriptionFrame:
				out = append(out, "text:"+v.Text)
			}
		}
		return out
	})
}

// ---------------------------------------------------------------- text aggregation + interrupted reply

type captureEmitter struct{ frames []core.Frame }

func (e *captureEmitter) Push(f core.Frame) error { e.frames = append(e.frames, f); return nil }
func (e *captureEmitter) PushDirection(f core.Frame, _ core.Direction) error {
	e.frames = append(e.frames, f)
	return nil
}

func feed(t *testing.T, p *core.AssistantContextProcessor, e *captureEmitter, frames ...core.Frame) {
	t.Helper()
	for _, f := range frames {
		if err := p.ProcessFrame(context.Background(), f, core.Downstream, e); err != nil {
			t.Fatal(err)
		}
	}
}

func TestParitySentenceAggregation(t *testing.T) {
	runParity(t, "text", func(t *testing.T, sc parityScenario) []string {
		// Production defaults (min unit 10 runes, soft cap 150, no first-clause cut).
		p := core.NewAssistantContextProcessor(core.NewConversationContext(core.ContextConfig{}), core.DefaultAssistantContextConfig())
		e := &captureEmitter{}
		feed(t, p, e, &core.UserMessageFrame{Turn: "t", Gen: 1})
		full := ""
		for _, st := range sc.Steps {
			full += st.Chunk
			feed(t, p, e, &core.LLMTextDeltaFrame{Turn: "t", Gen: 1, Delta: st.Chunk})
		}
		feed(t, p, e, &core.LLMTextFrame{Turn: "t", Gen: 1, Text: full})
		out := []string{}
		for _, f := range e.frames {
			if u, ok := f.(*core.SynthesisUnitFrame); ok {
				out = append(out, u.Text)
			}
		}
		return out
	})
}

func TestParityInterruptedAssistantReply(t *testing.T) {
	runParity(t, "assistant", func(t *testing.T, sc parityScenario) []string {
		store := core.NewConversationContext(core.ContextConfig{MaxMessages: 10, MaxTokens: 1000})
		// Min unit 2 so each scripted unit stays its own synthesis unit; splitting itself is the "text" area.
		p := core.NewAssistantContextProcessor(store, core.AssistantContextConfig{MinUnitLength: 2, MaxUnitLength: 100})
		e := &captureEmitter{}
		var texts []string
		for _, u := range sc.Units {
			texts = append(texts, u.Text)
		}
		all := strings.Join(texts, " ")
		feed(t, p, e, &core.UserMessageFrame{Turn: "t", Gen: 1},
			&core.LLMTextDeltaFrame{Turn: "t", Gen: 1, Delta: all}, &core.LLMTextFrame{Turn: "t", Gen: 1, Text: all})
		offset := 0
		for i, u := range sc.Units {
			played := u.DurMs
			if sc.InterruptMs != nil {
				played = min(played, *sc.InterruptMs-offset)
			}
			offset += u.DurMs
			if played <= 0 {
				break
			}
			wc := &core.WriteCompletionFrame{Turn: "t", Gen: 1, UnitID: uint64(i + 1), Media: ms(played), IsFinal: played == u.DurMs}
			for _, w := range u.Words {
				if w.S < played {
					wc.WordTimings = append(wc.WordTimings, core.WordTiming{Word: w.W, Start: ms(w.S), End: ms(w.E)})
				}
			}
			feed(t, p, e, wc)
		}
		if sc.InterruptMs != nil {
			feed(t, p, e, &core.InterruptionFrame{Turn: "t", Gen: 1})
		}
		for _, m := range store.Messages() {
			if m.Role == core.RoleAssistant {
				return []string{"content:" + m.Content, fmt.Sprintf("interrupted:%t", m.Partial)}
			}
		}
		return []string{"no_message"}
	})
}

// ---------------------------------------------------------------- summarization cut point

func TestParitySummarizationCutPoint(t *testing.T) {
	runParity(t, "summary", func(t *testing.T, sc parityScenario) []string {
		var p struct {
			KeepTurns int `json:"keep_turns"`
		}
		if err := json.Unmarshal(sc.Params, &p); err != nil {
			t.Fatal(err)
		}
		c := core.NewConversationContext(core.ContextConfig{MaxMessages: 200, MaxTokens: 1_000_000})
		now := time.Now()
		for _, m := range sc.Messages {
			turn := fmt.Sprintf("t%d", m.Turn)
			var err error
			switch m.Kind {
			case "system":
				err = c.SetSystemInstructionChecked(m.Text, now)
			case "user":
				err = c.AddMessage(core.Message{Role: core.RoleUser, Content: m.Text, TurnID: turn})
			case "assistant":
				err = c.AddMessage(core.Message{Role: core.RoleAssistant, Content: m.Text, TurnID: turn})
			case "tool_call":
				err = c.AddToolCall(turn, 1, m.ID, "f", []byte("{}"), now)
			case "tool_result":
				err = c.AddToolResult(turn, 1, m.ID, []byte(`"ok"`), "", now)
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		all := c.Messages()
		var span []core.Message
		sum := core.NewContextSummarizer(c, core.SummaryConfig{TriggerMessages: 1, KeepTurns: p.KeepTurns,
			Summarizer: func(_ context.Context, msgs []core.Message) (string, error) { span = msgs; return "s", nil }},
			func(fn func(context.Context)) error { fn(context.Background()); return nil }, nil)
		sum.Maybe()
		if len(span) == 0 {
			return []string{"summarize:0", "last_index:-1"}
		}
		last := span[len(span)-1]
		for i, m := range all {
			if m.Content == last.Content && m.TurnID == last.TurnID && m.Kind == last.Kind && m.ToolCallID == last.ToolCallID {
				return []string{fmt.Sprintf("summarize:%d", len(span)), fmt.Sprintf("last_index:%d", i)}
			}
		}
		t.Fatal("summarized message not found in context")
		return nil
	})
}

// ---------------------------------------------------------------- constants

func TestParityTurnCompletionMarkers(t *testing.T) {
	runParity(t, "constants", func(t *testing.T, sc parityScenario) []string {
		return []string{"complete:" + core.CompleteMarker, "short:" + core.IncompleteShortMarker, "long:" + core.IncompleteLongMarker}
	})
}
