package eval

import (
	"context"
	"errors"
	"fmt"
)

// Outcome is how one scorer judged one call.
type Outcome string

const (
	Pass    Outcome = "pass"
	Fail    Outcome = "fail"
	Skipped Outcome = "skipped" // nothing to check, no judge configured, or no stored history; never a pass or a fail
	Errored Outcome = "error"   // the scorer or its judge failed
)

// ErrSkipped is returned by a scorer that has nothing to check; the error text becomes the reason.
var ErrSkipped = errors.New("skipped")

// skip is the error of a scorer with nothing to check; its text is the skip reason.
type skip string

func (s skip) Error() string      { return string(s) }
func (skip) Is(target error) bool { return target == ErrSkipped }

// Reasons a result is skipped.
const (
	reasonNoJudge   = "no judge configured"
	reasonNoHistory = "no stored history"
)

// Result is one scorer's verdict on one call.
type Result struct {
	Scorer     string
	Version    int
	Score      float64
	Outcome    Outcome
	Reason     string
	JudgeModel string
}

// Scorer turns the input of one call into a score in [0,1] and a short reason. A judged scorer asks
// the Judge it is given; a deterministic one ignores it.
type Scorer interface {
	Name() string
	Version() int
	Judged() bool
	DefaultThreshold() float64
	Score(ctx context.Context, in Input, j *Judge) (float64, string, error)
}

// Judge is the model a model-graded scorer asks: one tool-less completion.
type Judge struct {
	Ask   AskFunc
	Model string
}

// JudgeFor resolves the judge of a scorer; nil means none is configured.
type JudgeFor func(scorer string) *Judge

// Builtins lists the built-in scorers in their fixed order.
func Builtins() []Scorer {
	return []Scorer{expectations{}, toolCallAccuracy{}, correctness, answerRelevancy, toxicity}
}

// Lookup finds a built-in scorer by name.
func Lookup(name string) (Scorer, bool) {
	for _, s := range Builtins() {
		if s.Name() == name {
			return s, true
		}
	}
	return nil, false
}

// RunScorers scores one call with every built-in scorer. thresholds overrides a scorer's default by
// name. A model-graded scorer without a judge is skipped, never passed.
func RunScorers(ctx context.Context, in Input, judges JudgeFor, thresholds map[string]float64) []Result {
	all := Builtins()
	out := make([]Result, 0, len(all))
	for _, s := range all {
		var j *Judge
		if judges != nil {
			j = judges(s.Name())
		}
		out = append(out, runOne(ctx, s, in, j, thresholds))
	}
	return out
}

func runOne(ctx context.Context, s Scorer, in Input, j *Judge, thresholds map[string]float64) (r Result) {
	r = Result{Scorer: s.Name(), Version: s.Version()}
	if s.Judged() {
		switch {
		case j == nil || j.Ask == nil:
			r.Outcome, r.Reason = Skipped, reasonNoJudge
			return r
		case len(in.Turns) == 0:
			r.Outcome, r.Reason = Skipped, reasonNoHistory
			return r
		}
		r.JudgeModel = j.Model
	}
	defer func() {
		if p := recover(); p != nil {
			r.Outcome, r.Score, r.Reason = Errored, 0, "scorer panicked"
		}
	}()
	score, reason, err := s.Score(ctx, in, j)
	switch {
	case errors.Is(err, ErrSkipped):
		r.Outcome, r.Reason = Skipped, err.Error()
	case err != nil:
		r.Outcome, r.Reason = Errored, err.Error()
	default:
		th, ok := thresholds[s.Name()]
		if !ok {
			th = s.DefaultThreshold()
		}
		r.Score, r.Reason = score, reason
		if score >= th {
			r.Outcome = Pass
		} else {
			r.Outcome = Fail
			if reason == "" {
				r.Reason = fmt.Sprintf("score %.2f below %.2f", score, th)
			}
		}
	}
	return r
}

// CaseRunPassed reports whether no scorer failed or errored (skipped results do not count against a call).
func CaseRunPassed(rs []Result) bool {
	for _, r := range rs {
		if r.Outcome == Fail || r.Outcome == Errored {
			return false
		}
	}
	return true
}
