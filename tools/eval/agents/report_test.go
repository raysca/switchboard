package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixedReport() Report {
	return Report{Schema: reportSchema, Mode: "echo", Results: []CaseResult{
		{Case: "b-fails", Runs: []RunResult{{SessionID: "s2", AgentVersionID: "v1", ConfigHash: "h2", Path: []string{"front"},
			Tools: []string{}, EndReason: "caller_hung_up", TurnCount: 1, MedianReplyMS: 90,
			Errors: []string{"turn 1: no reply within 45s"}, Transcript: []Line{{"caller", "hi"}, {"agent", strings.Repeat("x", 600)}},
			Failures: []string{"path: want [front sales], got [front]"}}}},
		{Case: "a-passes", Passed: true, Runs: []RunResult{{SessionID: "s1", AgentVersionID: "v7", ConfigHash: "abc", Path: []string{"front", "sales"},
			Tools: []string{"transfer_to_sales:succeeded"}, EndReason: "caller_hung_up", TurnCount: 3, MedianReplyMS: 380,
			Errors: []string{}, Transcript: []Line{{"caller", "I want a car"}, {"agent", "Sure."}}, Failures: []string{}}}},
	}}
}

const wantText = `PASS a-passes (echo, 1/1 runs)
  run 1: session s1, agent version v7, config abc
    path: front > sales
    tools: transfer_to_sales:succeeded
    end: caller_hung_up, 3 turns, median reply 380 ms
    caller: I want a car
    agent: Sure.
FAIL b-fails (echo, 0/1 runs)
  run 1: session s2, agent version v1, config h2
    path: front
    tools: none
    end: caller_hung_up, 1 turns, median reply 90 ms
    caller: hi
    agent: ` + "%s" + `
    ERROR turn 1: no reply within 45s
    FAIL path: want [front sales], got [front]
note: echo mode checks the plumbing (handoffs, tools, hang-up), never prompts or model behaviour
agent-eval: 1 passed, 1 failed (echo)
`

// Path, tools and end reason come from the server: control characters in them cannot forge report lines.
func TestReportTextEscapesServerStrings(t *testing.T) {
	r := fixedReport()
	run := &r.Results[0].Runs[0]
	run.Path, run.Tools, run.EndReason = []string{"a\nFAKE: pass"}, []string{"t\nFAKE: pass"}, "e\nFAKE: pass"
	var b bytes.Buffer
	WriteText(&b, r)
	if strings.Contains(b.String(), "\nFAKE") {
		t.Errorf("a server string broke out of its line:\n%s", b.String())
	}
}

func TestReportText(t *testing.T) {
	var b bytes.Buffer
	WriteText(&b, fixedReport())
	want := strings.Replace(wantText, "%s", strings.Repeat("x", 500)+"...", 1)
	if b.String() != want {
		t.Errorf("text report:\n%s\nwant:\n%s", b.String(), want)
	}
}

func TestReportJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.json")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil { // an existing wider file is tightened
		t.Fatal(err)
	}
	if err := WriteJSON(path, fixedReport()); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}
	raw, _ := os.ReadFile(path)
	var got struct {
		Schema  string
		Mode    string
		Results []struct {
			Case   string
			Passed bool
			Runs   []map[string]any
		}
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Schema != "govoice.agent-eval/v1" || got.Mode != "echo" || len(got.Results) != 2 || got.Results[0].Case != "a-passes" || !got.Results[0].Passed {
		t.Fatalf("json = %s", raw)
	}
	for _, k := range []string{"session_id", "agent_version_id", "config_hash", "path", "tools", "end_reason", "turn_count", "median_reply_ms", "errors", "transcript", "failures"} {
		if _, ok := got.Results[0].Runs[0][k]; !ok {
			t.Errorf("run lacks %q", k)
		}
	}
	if strings.Contains(string(raw), "time") {
		t.Errorf("report carries a timestamp: %s", raw)
	}
}

func TestReportQuotesControlCharacters(t *testing.T) {
	r := fixedReport()
	r.Results[1].Runs[0].Transcript = []Line{{"agent", "a\x1b[31mred\nnext\ttab\u0085 ok é"}}
	r.Results[1].Runs[0].Failures = []string{"never_say: matched \"\x1b[2J\""}
	var b bytes.Buffer
	WriteText(&b, r)
	out := b.String()
	if strings.ContainsAny(out, "\x1b\t\u0085") || !strings.Contains(out, `agent: a\x1b[31mred\nnext\ttab\x85 ok é`) || !strings.Contains(out, `matched "\x1b[2J"`) {
		t.Errorf("control characters reach the terminal:\n%q", out)
	}
}
