// Package turnquality replays a committed text sample of Pipecat's
// evals/turn-completion dataset against the Go turn strategies and reports
// false-cut and late-cut rates (docs/benchmarks/turn-quality.md).
//
// What it measures: the dataset is transcripts with a expected verdict
// (complete / incomplete), no audio and no pause timing. So:
//   - speech-timeout is evaluated as "the user went silent after this text";
//     it ends every turn, which is its structural behavior, not a quality result.
//   - the smart-turn analyzer needs audio and is reported as skipped.
//   - llm-completion runs the real marker protocol (core.AssistantContextProcessor)
//     against an Oracle. The built-in oracles are fakes (a keyword heuristic and
//     a label-reading one): they validate plumbing and gate regressions in the
//     Go code, NOT model quality. A real model is plugged in with CommandOracle.
package turnquality

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"govoice/core"
	coreturn "govoice/core/turn"
)

//go:embed data/sample.jsonl
var sampleJSONL []byte

//go:embed data/thresholds.json
var thresholdsJSON []byte

type Utterance struct {
	ID       string `json:"id"`
	Category string `json:"category"`
	User     string `json:"user"`
	Expect   string `json:"expect"` // complete | incomplete
}

// Sample returns the committed utterances.
func Sample() ([]Utterance, error) { return ParseSample(sampleJSONL) }

func ParseSample(b []byte) (out []Utterance, err error) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		var u Utterance
		if err := json.Unmarshal(sc.Bytes(), &u); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, sc.Err()
}

// Oracle stands in for the model: it returns the raw reply to one user utterance.
type Oracle func(ctx context.Context, u Utterance) (string, error)

var (
	openEnd = regexp.MustCompile(`(?i)\b(and|but|or|because|so|if|to|for|with|the|a|an|of|in|on|at|from|my|that|which|is|are|was|i|i'm|i'd|like|love|about|than|then|between|need|want|maybe|um|uh)[\s,.…]*$`)
	waitReq = regexp.MustCompile(`(?i)(hold on|let me think|give me a (second|moment|minute)|one (moment|sec)|hang on|that's (interesting|a good question)|^\s*(hmm+|well|oh wow)[.…\s]*$)`)
)

// HeuristicOracle is a FAKE model: open-ended trailing words -> ◐, requests to
// wait -> ○, otherwise ●. It exists to exercise the pipeline with non-trivial
// errors; its rates say nothing about any real LLM.
func HeuristicOracle(_ context.Context, u Utterance) (string, error) {
	switch {
	case waitReq.MatchString(u.User):
		return core.IncompleteLongMarker, nil
	case openEnd.MatchString(strings.TrimSpace(u.User)):
		return core.IncompleteShortMarker, nil
	}
	return core.CompleteMarker + " Sure, I can help with that.", nil
}

// PerfectOracle reads the dataset label: any miss is a bug in the Go plumbing.
func PerfectOracle(_ context.Context, u Utterance) (string, error) {
	if u.Expect == "incomplete" {
		return core.IncompleteShortMarker, nil
	}
	return core.CompleteMarker + " Sure, I can help with that.", nil
}

// CommandOracle calls a user program per utterance (the opt-in real-model
// path). It gets {"system_prompt","user"} as one JSON line on stdin and must
// print the model's raw reply, marker first, on stdout.
func CommandOracle(argv []string, timeout time.Duration) Oracle {
	return func(ctx context.Context, u Utterance) (string, error) {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		in, _ := json.Marshal(map[string]string{"system_prompt": core.CompletionInstructions, "user": u.User})
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Stdin = bytes.NewReader(append(in, '\n'))
		out, err := cmd.Output()
		return string(out), err
	}
}

// Outcome of one utterance under one strategy.
type Outcome struct {
	Held bool // true: the strategy kept the turn open (no reply)
}

// Rates are fractions of the incomplete (false cut) and complete (late cut) utterances.
type Rates struct {
	Strategy   string  `json:"strategy"`
	Status     string  `json:"status"` // ok | skipped
	Note       string  `json:"note,omitempty"`
	N          int     `json:"n"`
	Incomplete int     `json:"incomplete"`
	Complete   int     `json:"complete"`
	FalseCut   float64 `json:"false_cut"` // user not done, strategy ended the turn
	LateCut    float64 `json:"late_cut"`  // user done, strategy held the turn
	Errors     int     `json:"errors"`
}

func rates(name string, us []Utterance, held func(Utterance) (bool, error)) Rates {
	r := Rates{Strategy: name, Status: "ok", N: len(us)}
	var fc, lc int
	for _, u := range us {
		h, err := held(u)
		if err != nil {
			r.Errors++
			continue
		}
		if u.Expect == "incomplete" {
			r.Incomplete++
			if !h {
				fc++
			}
		} else {
			r.Complete++
			if h {
				lc++
			}
		}
	}
	if r.Incomplete > 0 {
		r.FalseCut = float64(fc) / float64(r.Incomplete)
	}
	if r.Complete > 0 {
		r.LateCut = float64(lc) / float64(r.Complete)
	}
	return r
}

// SpeechTimeoutRates drives the real SpeechTimeout strategy with the user
// silent for longer than its timeout.
func SpeechTimeoutRates(us []Utterance) Rates {
	d := coreturn.SpeechTimeout(700 * time.Millisecond)
	r := rates("speech-timeout", us, func(u Utterance) (bool, error) {
		_, ended := d.Detect(coreturn.DetectionInput{SilenceFor: 800 * time.Millisecond, SilenceAfter: 700 * time.Millisecond, Text: u.User, TextFinal: true})
		return !ended, nil
	})
	r.Note = "text-only: the dataset has no pause timing, so every utterance is followed by 800ms of silence; speech-timeout ends them all by construction"
	return r
}

// SmartTurnRates is skipped: the dataset carries no audio.
func SmartTurnRates(us []Utterance) Rates {
	return Rates{Strategy: "turn-analyzer", Status: "skipped", N: len(us), Note: "smart-turn is audio-based; the turn-completion dataset is transcripts only"}
}

// CompletionRates runs each utterance through the real marker protocol.
func CompletionRates(name string, us []Utterance, o Oracle) Rates {
	return rates(name, us, func(u Utterance) (bool, error) { return runCompletion(u, o) })
}

type oracleLLM struct {
	sess  core.SessionController
	index int
	reply string
}

func (l *oracleLLM) Attach(s core.SessionController, i int) error { l.sess, l.index = s, i; return nil }
func (l *oracleLLM) ProcessFrame(_ context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	um, ok := f.(*core.UserMessageFrame)
	if !ok {
		return e.PushDirection(f, dir)
	}
	if err := e.PushDirection(f, dir); err != nil {
		return err
	}
	gen := l.sess.CurrentGeneration()
	// Stream in 3-rune chunks so markers also split across deltas.
	rs := []rune(l.reply)
	for i := 0; i < len(rs); i += 3 {
		j := min(i+3, len(rs))
		_ = l.sess.Reenter(&core.LLMTextDeltaFrame{Delta: string(rs[i:j]), Turn: um.Turn, Gen: gen}, core.Downstream, l.index+1, gen)
	}
	return l.sess.Reenter(&core.LLMTextFrame{Text: l.reply, Turn: um.Turn, Gen: gen}, core.Downstream, l.index+1, gen)
}

type finalSeen struct {
	done chan struct{}
	once sync.Once
	text string // every delta plus the final text leaving the assistant stage (written before done closes)
}

func (s *finalSeen) ProcessFrame(_ context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	switch v := f.(type) {
	case *core.LLMTextDeltaFrame:
		s.text += v.Delta
	case *core.LLMTextFrame:
		s.once.Do(func() { s.text += v.Text; close(s.done) })
	}
	return e.PushDirection(f, dir)
}

// runCompletion reports whether the turn was held (no reply spoken).
func runCompletion(u Utterance, o Oracle) (bool, error) {
	reply, err := o(context.Background(), u)
	if err != nil {
		return false, err
	}
	var mu sync.Mutex
	var verdict string
	cfg := core.DefaultAssistantContextConfig()
	cfg.Completion = &core.TurnCompletionConfig{OnVerdict: func(v string) { mu.Lock(); verdict = v; mu.Unlock() }}
	ctx := core.NewConversationContext(core.ContextConfig{MaxMessages: 10, MaxTokens: 10000})
	seen := &finalSeen{done: make(chan struct{})}
	tail := &core.PassthroughProcessor{}
	pipe, err := core.NewPipeline(&oracleLLM{reply: reply}, core.NewAssistantContextProcessor(ctx, cfg), seen, tail)
	if err != nil {
		return false, err
	}
	sess, err := core.NewSession(1, pipe, core.DefaultResourceBudget())
	if err != nil {
		return false, err
	}
	if err := sess.Start(); err != nil {
		return false, err
	}
	defer sess.Close()
	if err := sess.Post(&core.UserMessageFrame{Turn: "turn-1", Content: u.User}); err != nil {
		return false, err
	}
	select {
	case <-seen.done:
	case <-time.After(5 * time.Second):
		return false, fmt.Errorf("%s: no reply", u.ID)
	}
	mu.Lock()
	defer mu.Unlock()
	// Judge what left the stage, not just the parsed verdict, so a gate that
	// parses right but still speaks (or leaks a marker) fails.
	held := strings.TrimSpace(seen.text) == ""
	if held != strings.HasPrefix(verdict, "incomplete") || strings.ContainsAny(seen.text, "●◐○") {
		return false, fmt.Errorf("%s: verdict %q but released %q", u.ID, verdict, seen.text)
	}
	return held, nil
}

// Threshold bounds one strategy; Max* < 0 means not enforced.
type Threshold struct {
	MaxFalseCut float64 `json:"max_false_cut"`
	MaxLateCut  float64 `json:"max_late_cut"`
	Calibrated  bool    `json:"calibrated"` // false: placeholder, reported but never fails
}

// Thresholds maps strategy name to its bounds.
type Thresholds map[string]Threshold

func DefaultThresholds() (Thresholds, error) {
	var t Thresholds
	return t, json.Unmarshal(thresholdsJSON, &t)
}

// Check returns one message per violated bound (empty: gate passes).
func (t Thresholds) Check(rs []Rates) (fails []string) {
	for _, r := range rs {
		th, ok := t[r.Strategy]
		if !ok || r.Status != "ok" || !th.Calibrated {
			continue
		}
		if r.Errors > 0 {
			fails = append(fails, fmt.Sprintf("%s: %d oracle errors", r.Strategy, r.Errors))
		}
		if r.FalseCut > th.MaxFalseCut {
			fails = append(fails, fmt.Sprintf("%s: false_cut %.3f > %.3f", r.Strategy, r.FalseCut, th.MaxFalseCut))
		}
		if r.LateCut > th.MaxLateCut {
			fails = append(fails, fmt.Sprintf("%s: late_cut %.3f > %.3f", r.Strategy, r.LateCut, th.MaxLateCut))
		}
	}
	return fails
}

// RunDefault evaluates every offline strategy on the committed sample.
func RunDefault() ([]Rates, error) {
	us, err := Sample()
	if err != nil {
		return nil, err
	}
	return []Rates{
		SpeechTimeoutRates(us),
		SmartTurnRates(us),
		CompletionRates("llm-completion-perfect", us, PerfectOracle),
		CompletionRates("llm-completion-heuristic", us, HeuristicOracle),
	}, nil
}
