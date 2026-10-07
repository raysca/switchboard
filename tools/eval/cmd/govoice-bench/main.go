// govoice-bench measures framework latency overhead: a live in-process session
// with fake LLM/TTS and a simulated STT wait. framework_overhead is
// voice_to_voice minus the configured delays, i.e. what the pipeline adds between
// the user-stopped frame and the first bot audio frame. Delays default to 0
// because each nonzero delay is a Go timer whose host overshoot (~1-2 ms on
// darwin) dwarfs the framework cost; set them for a realistic turn shape only.
// No audio input, VAD, STT or turn manager is exercised (see baseline.md).
// Fake providers only; no network, no cost. Re-run and diff the JSON.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"govoice/core"
	"govoice/core/observe"
)

type Stat struct {
	N                  int
	P50, P95, P99, Max float64 // milliseconds
}

type Report struct {
	Machine                string // GOOS/GOARCH/NCPU: baselines never compare across machines
	Turns, Sessions        int
	SimSTT, SimLLM, SimTTS string
	Stages                 map[string]Stat
	Overhead               Stat // voice_to_voice - (stt+llm+tts simulated), includes timer slop
	GoroutinesBefore       int
	GoroutinesAfter        int
	RSSKBBefore            int
	RSSKBAfter             int
}

// percentile is nearest-rank over a sorted slice (p in 0..100).
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	return sorted[max(i, 0)]
}

func stat(ms []float64) Stat {
	s := append([]float64(nil), ms...)
	sort.Float64s(s)
	return Stat{len(s), percentile(s, 50), percentile(s, 95), percentile(s, 99), percentile(s, 100)}
}

func rssKB() int {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return n
}

type cfg struct {
	turns, sessions int
	stt, llm, tts   time.Duration
	inject          time.Duration // perf-gate self-test: extra LLM delay NOT subtracted from overhead
}

// runSession drives n sequential turns and appends its measurements to out.
func runSession(id uint64, c cfg, mu *sync.Mutex, out map[observe.Stage][]float64, over *[]float64) (err error) {
	llm := core.NewFakeLLMProcessor(core.FakeLLMConfig{ResponseChunks: []string{"Hello there."}, ChunkDelay: c.llm + c.inject})
	tts := core.NewFakeTTSProcessor(core.FakeTTSConfig{FramesPerUnit: 2, FrameDelay: c.tts})
	cx := core.NewConversationContext(core.ContextConfig{MaxMessages: 10, MaxTokens: 1000})
	pipe, err := core.NewPipeline(
		core.NewUserContextProcessor(cx, core.UserContextConfig{TurnPrefix: "turn"}),
		core.NewAssistantContextProcessor(cx, core.DefaultAssistantContextConfig()),
		llm, tts, core.NewFakeTransportOutput(core.FakeTransportConfig{}))
	if err != nil {
		return err
	}
	s, err := core.NewSession(id, pipe, core.DefaultResourceBudget())
	if err != nil {
		return err
	}
	v2v := make(chan struct{}, 4)
	sim := float64((c.stt + c.llm + c.tts).Microseconds()) / 1000
	if _, err := observe.Attach(s, func(m observe.Measurement) {
		if m.Stage == observe.StageStartup {
			return // NewSession->Start is only the Attach call here, and ms-truncated
		}
		ms := float64(m.Value.Microseconds()) / 1000
		mu.Lock()
		out[m.Stage] = append(out[m.Stage], ms)
		if m.Stage == observe.StageVoiceToVoice {
			*over = append(*over, ms-sim)
		}
		mu.Unlock()
		if m.Stage == observe.StageVoiceToVoice {
			v2v <- struct{}{}
		}
	}); err != nil {
		return err
	}
	if err := s.Start(); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, s.Close()) }() // a teardown timeout is a finding
	for i := 1; i <= c.turns; i++ {
		if err := s.Post(&core.UserStoppedSpeakingFrame{Timestamp: time.Now()}); err != nil {
			return fmt.Errorf("session %d turn %d: %w (cause: %v)", id, i, err, s.Err())
		}
		time.Sleep(c.stt) // simulated STT final latency
		if err := s.Post(&core.FinalTranscriptionFrame{Turn: fmt.Sprintf("turn-%d", i), Text: "Hello", Gen: 1, Timestamp: time.Now()}); err != nil {
			return fmt.Errorf("session %d turn %d: %w (cause: %v)", id, i, err, s.Err())
		}
		select {
		case <-v2v:
		case <-time.After(10*time.Second + c.stt + c.llm + c.tts):
			return fmt.Errorf("session %d turn %d: no voice_to_voice", id, i)
		}
	}
	return nil
}

func run(c cfg) (Report, error) {
	runtime.GC()
	r := Report{Machine: fmt.Sprintf("%s/%s/%d", runtime.GOOS, runtime.GOARCH, runtime.NumCPU()), Turns: c.turns, Sessions: c.sessions, SimSTT: c.stt.String(), SimLLM: c.llm.String(), SimTTS: c.tts.String(),
		GoroutinesBefore: runtime.NumGoroutine(), RSSKBBefore: rssKB()}
	var mu sync.Mutex
	out := map[observe.Stage][]float64{}
	var over []float64
	var wg sync.WaitGroup
	errs := make([]error, c.sessions)
	for i := 0; i < c.sessions; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs[i] = runSession(uint64(i+1), c, &mu, out, &over) }()
	}
	wg.Wait()
	for deadline := time.Now().Add(2 * time.Second); runtime.NumGoroutine() > r.GoroutinesBefore && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond) // let session goroutines drain
	}
	runtime.GC()
	r.GoroutinesAfter, r.RSSKBAfter = runtime.NumGoroutine(), rssKB()
	r.Stages = map[string]Stat{}
	for st, v := range out {
		r.Stages[string(st)] = stat(v)
	}
	r.Overhead = stat(over)
	return r, errors.Join(errs...)
}

func (r Report) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "turns=%d sessions=%d simulated stt=%s llm=%s tts=%s\n\n| stage | n | p50 ms | p95 ms | p99 ms | max ms |\n|---|---|---|---|---|---|\n", r.Turns, r.Sessions, r.SimSTT, r.SimLLM, r.SimTTS)
	row := func(name string, s Stat) {
		fmt.Fprintf(&b, "| %s | %d | %.2f | %.2f | %.2f | %.2f |\n", name, s.N, s.P50, s.P95, s.P99, s.Max)
	}
	for _, st := range observe.Stages {
		if s, ok := r.Stages[string(st)]; ok {
			row(string(st), s)
		}
	}
	row("framework_overhead", r.Overhead)
	fmt.Fprintf(&b, "\ngoroutines %d -> %d, rss %d KB -> %d KB\n", r.GoroutinesBefore, r.GoroutinesAfter, r.RSSKBBefore, r.RSSKBAfter)
	return b.String()
}

func main() {
	var c cfg
	flag.IntVar(&c.turns, "turns", 200, "turns per session")
	flag.IntVar(&c.sessions, "sessions", 1, "concurrent sessions")
	flag.DurationVar(&c.stt, "stt", 0, "simulated STT final delay after user stop")
	flag.DurationVar(&c.llm, "llm", 0, "simulated LLM time to first token")
	flag.DurationVar(&c.tts, "tts", 0, "simulated TTS first-audio delay")
	flag.DurationVar(&c.inject, "inject-delay", 0, "self-test only: extra LLM chunk delay counted as framework overhead (proves the perf gate fails)")
	jsonOut := flag.String("json-out", "", "also write the JSON report to this file")
	compare := flag.String("compare", "", "baseline JSON (from -json) to compare against; exit 1 on regression")
	asJSON := flag.Bool("json", false, "emit JSON instead of markdown")
	flag.Parse()
	r, err := run(c)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if *jsonOut != "" {
		raw, _ := json.MarshalIndent(r, "", "  ")
		if err := os.WriteFile(*jsonOut, append(raw, '\n'), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}
	if *compare != "" {
		os.Exit(compareFile(*compare, r))
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(r)
		return
	}
	fmt.Print(r.Markdown())
}
