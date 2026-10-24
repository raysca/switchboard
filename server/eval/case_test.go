package eval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadBadCases(t *testing.T) {
	want := map[string]string{
		"unknown-key":     "pth",
		"both-callers":    "caller: set exactly one",
		"neither-caller":  "caller: set exactly one",
		"bad-regexp":      "expect.say[0]",
		"bad-end-reason":  "expect.end_reason",
		"bad-tool-status": "expect.tools_called[0]",
		"persona-turns":   "caller.persona.max_turns",
		"empty-line":      "caller.script[0]",
	}
	files, _ := filepath.Glob("testdata/cases-bad/*.yaml")
	if len(files) != len(want) {
		t.Fatalf("%d bad files, %d expectations", len(files), len(want))
	}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".yaml")
		dir := t.TempDir()
		data, _ := os.ReadFile(f)
		if err := os.WriteFile(filepath.Join(dir, filepath.Base(f)), data, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := LoadCases(dir, nil)
		if err == nil || !strings.Contains(err.Error(), filepath.Base(f)) || !strings.Contains(err.Error(), want[name]) {
			t.Errorf("%s: err = %v, want file name and %q", name, err, want[name])
		}
	}
}

func TestLoadDuplicateNames(t *testing.T) {
	_, err := LoadCases("testdata/cases-dup", nil)
	if err == nil || !strings.Contains(err.Error(), "b.yaml") || !strings.Contains(err.Error(), "already used by") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadGoodCases(t *testing.T) {
	got, err := LoadCases("testdata/cases-good", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []Case{{
		Name: "dealership-buyer-reaches-sales", Agent: "Dealership desk", File: "testdata/cases-good/ok.yaml",
		Caller: Caller{Script: []string{"Hi, I'd like to buy a car.", "What kinds of cars do you sell?"}},
		Expect: Expect{Path: []string{"front", "sales"}, ToolsCalled: []string{"transfer_to_sales", "find_slots:failed"},
			ToolsNotCalled: []string{"end_call"}, Say: []string{"(?i)hatchback|suv"}, NeverSay: []string{"(?i)as an ai"},
			EndReason: "caller_hung_up", MaxTurns: 6, MedianReplyMSMax: 8000},
	}, {
		Name: "persona-case", Agent: "Smile Dental front desk", File: "testdata/cases-good/persona.yaml",
		Caller: Caller{Persona: &Persona{Who: "Jane", Goal: "book a cleaning", MaxTurns: 8}},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	only, err := LoadCases("testdata/cases-good", []string{"persona-case"})
	if err != nil || len(only) != 1 || only[0].Name != "persona-case" {
		t.Fatalf("only = %+v, %v", only, err)
	}
	if _, err := LoadCases("testdata/cases-good", []string{"nope"}); err == nil {
		t.Fatal("unknown -case name accepted")
	}
}

func TestLoadBadInline(t *testing.T) {
	const head = "name: n\nagent: a\n"
	long := strings.Repeat("x", maxLineLen+1)
	var many strings.Builder
	for range maxScriptLines + 1 {
		many.WriteString("    - hi\n")
	}
	for _, tc := range []struct{ name, body, want string }{
		{"duplicate name across documents", head + "caller: {script: [hi]}\n---\n" + head + "caller: {script: [hi]}\n", "already used by"},
		{"second document invalid", head + "caller: {script: [hi]}\n---\nname: m\nagent: a\ncaller: {script: []}\n", "c.yaml#2"},
		{"long expected answer", head + "caller: {script: [hi]}\nexpected_answer: " + strings.Repeat("x", 2001) + "\n", "expected_answer"},
		{"empty say", head + "caller: {script: [hi]}\nexpect: {say: [\"\"]}\n", "expect.say[0]"},
		{"empty never_say", head + "caller: {script: [hi]}\nexpect: {never_say: [\"\"]}\n", "expect.never_say[0]"},
		{"bad never_say regexp", head + "caller: {script: [hi]}\nexpect: {never_say: [\"(\"]}\n", "expect.never_say[0]"},
		{"empty path entry", head + "caller: {script: [hi]}\nexpect: {path: [front, \"\"]}\n", "expect.path[1]"},
		{"empty tool", head + "caller: {script: [hi]}\nexpect: {tools_called: [\"\"]}\n", "expect.tools_called[0]"},
		{"bad not_called status", head + "caller: {script: [hi]}\nexpect: {tools_not_called: [\"a:bogus\"]}\n", "expect.tools_not_called[0]"},
		{"whitespace line", head + "caller: {script: [\"  \\t\"]}\n", "caller.script[0]"},
		{"long line", head + "caller: {script: [" + long + "]}\n", "caller.script[0]"},
		{"too many lines", head + "caller:\n  script:\n" + many.String(), "caller.script"},
		{"script and persona", head + "caller: {script: [], persona: {who: a, goal: b}}\n", "caller: set exactly one"},
		{"persona no who", head + "caller: {persona: {goal: b}}\n", "caller.persona"},
		{"persona no goal", head + "caller: {persona: {who: a}}\n", "caller.persona"},
		{"no name", "agent: a\ncaller: {script: [hi]}\n", "name: required"},
		{"no agent", "name: n\ncaller: {script: [hi]}\n", "agent: required"},
		{"negative max_turns", head + "caller: {script: [hi]}\nexpect: {max_turns: -1}\n", "expect.max_turns"},
		{"agent with slash", "name: n\nagent: ../secret\ncaller: {script: [hi]}\n", "agent:"},
		{"agent with dotdot", "name: n\nagent: a..b\ncaller: {script: [hi]}\n", "agent:"},
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "c.yaml"), []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadCases(dir, nil); err == nil || !strings.Contains(err.Error(), "c.yaml") || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want c.yaml and %q", tc.name, err, tc.want)
		}
	}
}

func TestLoadAcceptsYml(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "c.yml"), []byte("name: n\nagent: Dealership desk\ncaller: {script: [hi]}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cs, err := LoadCases(dir, nil); err != nil || len(cs) != 1 {
		t.Fatalf("cases = %v, err = %v", cs, err)
	}
}

func TestParseCasesMultiDocument(t *testing.T) {
	src := "name: a\ncaller: {script: [hi]}\nexpected_answer: Open until 5.\nexpect: {tools_called: [x, y], tools_ordered: true}\n---\nname: b\ncaller: {persona: {who: w, goal: g}}\n"
	cs, err := ParseCases([]byte(src), "export.yaml")
	if err != nil || len(cs) != 2 {
		t.Fatalf("cases = %+v, err = %v", cs, err)
	}
	if cs[0].Agent != "" || cs[0].ExpectedAnswer != "Open until 5." || !cs[0].Expect.ToolsOrdered || cs[1].Caller.Persona.MaxTurns != defaultTurns {
		t.Errorf("parsed = %+v", cs)
	}
	for _, bad := range []string{"", "---\n", "name: a\ncaller: {script: [hi]}\nbogus: 1\n"} {
		if _, err := ParseCases([]byte(bad), "x.yaml"); err == nil || !strings.Contains(err.Error(), "x.yaml") {
			t.Errorf("%q: err = %v", bad, err)
		}
	}
	// Case files name their agent; stored cases do not.
	if _, err := ParseCases([]byte("name: a\ncaller: {script: [hi]}\n"), "x.yaml"); err != nil {
		t.Errorf("agent required of a stored case: %v", err)
	}
}

func TestCaseJSONUsesYAMLKeys(t *testing.T) {
	c := Case{Name: "n", Caller: Caller{Script: []string{"hi"}}, Expect: Expect{ToolsCalled: []string{"a"}, ToolsOrdered: true, MedianReplyMSMax: 5}, ExpectedAnswer: "x"}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"tools_called"`, `"tools_ordered"`, `"median_reply_ms_max"`, `"expected_answer"`, `"script"`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("%s missing from %s", key, b)
		}
	}
}

// Empty and comment-only YAML documents (a trailing "---") hold no case and are skipped.
func TestParseCasesSkipsEmptyDocuments(t *testing.T) {
	doc := "name: a\ncaller: {script: [hi]}\n"
	for name, in := range map[string]string{
		"trailing separator": doc + "---\n",
		"comment document":   doc + "---\n# nothing here\n---\n",
		"leading separator":  "---\n" + doc,
	} {
		cs, err := ParseCases([]byte(in), "f")
		if err != nil || len(cs) != 1 || cs[0].Name != "a" {
			t.Errorf("%s: %v, %v", name, cs, err)
		}
	}
	for _, in := range []string{"", "---\n", "# only a comment\n"} {
		if _, err := ParseCases([]byte(in), "f"); err == nil || !strings.Contains(err.Error(), "empty") {
			t.Errorf("%q: err = %v, want an empty-file error", in, err)
		}
	}
}
