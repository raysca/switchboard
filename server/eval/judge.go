package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	maxEvidence     = 16 << 10 // bytes of evidence sent to a judge
	maxPromptEvid   = 2 << 10  // bytes of one step's system prompt in the evidence
	maxJudgeReason  = 300      // runes of a judge's reason kept
	judgeReplyShape = `Reply with only a JSON object: {"score": <number from 0 to 1>, "reason": "<at most 300 characters>"}.`
)

// askJudge sends the rubric and the evidence of one call to the judge and parses its verdict. A
// transport failure or a reply that is not a score in [0,1] is an error, never a skip.
func askJudge(ctx context.Context, j *Judge, rubric string, in Input) (float64, string, error) {
	reply, err := j.Ask(ctx, rubric+"\n"+judgeReplyShape, evidence(in))
	if err != nil {
		return 0, "", fmt.Errorf("judge: %w", err)
	}
	i := strings.IndexByte(reply, '{')
	if i < 0 {
		return 0, "", errors.New("judge: reply has no JSON object")
	}
	var v struct {
		Score  *float64 `json:"score"`
		Reason string   `json:"reason"`
	}
	if err := json.NewDecoder(strings.NewReader(reply[i:])).Decode(&v); err != nil {
		return 0, "", errors.New("judge: reply is not a JSON score")
	}
	if v.Score == nil || *v.Score < 0 || *v.Score > 1 {
		return 0, "", errors.New("judge: score missing or outside 0..1")
	}
	return *v.Score, Truncate(strings.TrimSpace(v.Reason), maxJudgeReason), nil
}

// evidence renders what a judge may see of one call: the agent's per-step instructions, the step path,
// tool names with outcomes, the expected answer when the case has one, and the turns. The whole is
// capped; the turns are cut first.
func evidence(in Input) string {
	var b strings.Builder
	steps := make([]string, 0, len(in.Prompts))
	for id := range in.Prompts {
		steps = append(steps, id)
	}
	slices.Sort(steps)
	for _, id := range steps {
		fmt.Fprintf(&b, "Instructions of step %q:\n%s\n\n", id, cut(in.Prompts[id], maxPromptEvid))
	}
	fmt.Fprintf(&b, "Steps visited: %s\n", strings.Join(in.Path, " > "))
	b.WriteString("Tools called (name, outcome, step):\n")
	for _, t := range in.Tools {
		fmt.Fprintf(&b, "- %s %s %s\n", t.Name, t.Status, t.Step)
	}
	if in.Case != nil && in.Case.ExpectedAnswer != "" {
		fmt.Fprintf(&b, "Expected answer (extra evidence): %s\n", in.Case.ExpectedAnswer)
	}
	b.WriteString("\nConversation:\n")
	head := min(b.Len(), maxEvidence)
	out := cut(b.String(), head)
	room := maxEvidence - len(out)
	var turns strings.Builder
	for _, l := range in.Turns {
		line := l.Who + ": " + l.Text + "\n"
		if turns.Len()+len(line) > room {
			break
		}
		turns.WriteString(line)
	}
	return out + turns.String()
}

// cut keeps at most n bytes of s without splitting a rune.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
