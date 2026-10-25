package eval

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"govoice/server/storage"
)

// Case is one simulated call and what it must show. An omitted expect key is not checked. The json
// tags use the same snake_case keys as the YAML ones.
type Case struct {
	Name   string `yaml:"name" json:"name"`
	Agent  string `yaml:"agent,omitempty" json:"agent,omitempty"` // agent name or id; stored cases belong to an agent and leave it empty
	Caller Caller `yaml:"caller" json:"caller"`
	Expect Expect `yaml:"expect,omitempty" json:"expect"`
	// ExpectedAnswer is extra evidence for the correctness judge, not an exact-match assertion.
	ExpectedAnswer string `yaml:"expected_answer,omitempty" json:"expected_answer,omitempty"`
	File           string `yaml:"-" json:"-"`
}

// Caller is exactly one of a fixed script or an LLM persona.
type Caller struct {
	Script  []string `yaml:"script,omitempty" json:"script,omitempty"`
	Persona *Persona `yaml:"persona,omitempty" json:"persona,omitempty"`
}

// Persona is who the simulated caller plays and what it wants.
type Persona struct {
	Who      string `yaml:"who" json:"who"`
	Goal     string `yaml:"goal" json:"goal"`
	MaxTurns int    `yaml:"max_turns,omitempty" json:"max_turns"`
}

// Expect lists the assertions; tools_called entries are "name" (succeeded) or "name:status".
// ToolsOrdered additionally requires the tools_called entries to appear in the call in the listed order.
type Expect struct {
	Path             []string `yaml:"path,omitempty" json:"path,omitempty"`
	ToolsCalled      []string `yaml:"tools_called,omitempty" json:"tools_called,omitempty"`
	ToolsOrdered     bool     `yaml:"tools_ordered,omitempty" json:"tools_ordered,omitempty"`
	ToolsNotCalled   []string `yaml:"tools_not_called,omitempty" json:"tools_not_called,omitempty"`
	Say              []string `yaml:"say,omitempty" json:"say,omitempty"`
	NeverSay         []string `yaml:"never_say,omitempty" json:"never_say,omitempty"`
	EndReason        string   `yaml:"end_reason,omitempty" json:"end_reason,omitempty"`
	MaxTurns         int      `yaml:"max_turns,omitempty" json:"max_turns,omitempty"`
	MedianReplyMSMax int      `yaml:"median_reply_ms_max,omitempty" json:"median_reply_ms_max,omitempty"`
}

const (
	maxScriptLines = 20
	maxLineLen     = 500
	defaultTurns   = 8
	maxPersonaTurn = 20
	maxExpected    = 2000 // runes of expected_answer
)

var toolStatuses = []string{"succeeded", "failed", "cancelled"}

// ParseCases decodes one or more YAML documents, each a case, refusing unknown keys and duplicate
// names. Agent is not required (stored cases belong to an agent). source names the input in errors,
// reported as source#N for the Nth document.
func ParseCases(data []byte, source string) ([]Case, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var cases []Case
	seen := map[string]int{}
	for n := 1; ; n++ {
		var c Case
		if err := dec.Decode(&c); err != nil {
			if errors.Is(err, io.EOF) {
				if len(cases) == 0 {
					return nil, fmt.Errorf("%s: empty file", source)
				}
				return cases, nil
			}
			return nil, fmt.Errorf("%s#%d: %w", source, n, err)
		}
		if reflect.DeepEqual(c, Case{}) { // an empty or comment-only document, e.g. a trailing "---"
			continue
		}
		if err := c.Validate(false); err != nil {
			return nil, fmt.Errorf("%s#%d: %w", source, n, err)
		}
		if prev, dup := seen[c.Name]; dup {
			return nil, fmt.Errorf("%s#%d: name: %q is already used by %s#%d", source, n, c.Name, source, prev)
		}
		seen[c.Name] = n
		c.File = source
		cases = append(cases, c)
	}
}

// LoadCases reads every *.yaml and *.yml in dir (sorted by file name), refusing unknown keys and
// duplicate case names. A file may hold several cases (YAML documents). only, when non-empty, keeps
// the named cases and refuses an unknown name.
func LoadCases(dir string, only []string) ([]Case, error) {
	var files []string
	for _, pat := range []string{"*.yaml", "*.yml"} {
		fs, err := filepath.Glob(filepath.Join(dir, pat))
		if err != nil {
			return nil, err
		}
		files = append(files, fs...)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no *.yaml or *.yml case files in %s", dir)
	}
	slices.Sort(files)
	var cases []Case
	seen := map[string]string{}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		cs, err := ParseCases(data, f)
		if err != nil {
			return nil, err
		}
		for i, c := range cs {
			if err := c.Validate(true); err != nil { // case files name their agent
				return nil, fmt.Errorf("%s#%d: %w", f, i+1, err)
			}
			if prev, dup := seen[c.Name]; dup {
				return nil, fmt.Errorf("%s: name: %q is already used by %s", f, c.Name, prev)
			}
			seen[c.Name] = f
			cases = append(cases, c)
		}
	}
	if len(only) == 0 {
		return cases, nil
	}
	for _, n := range only {
		if _, ok := seen[n]; !ok {
			return nil, fmt.Errorf("no case named %q in %s", n, dir)
		}
	}
	return slices.DeleteFunc(cases, func(c Case) bool { return !slices.Contains(only, c.Name) }), nil
}

// Validate checks the case. requireAgent is true for case files, which name the agent they run against.
func (c *Case) Validate(requireAgent bool) error {
	if strings.TrimSpace(c.Name) == "" {
		return errors.New("name: required")
	}
	if requireAgent && strings.TrimSpace(c.Agent) == "" {
		return errors.New("agent: required")
	}
	if c.Agent != "" && (strings.ContainsAny(c.Agent, `/\`) || strings.Contains(c.Agent, "..")) {
		return fmt.Errorf("agent: %q must be an agent name or id (no path separators or \"..\")", c.Agent)
	}
	switch s, p := c.Caller.Script, c.Caller.Persona; {
	case s != nil && p != nil:
		return errors.New("caller: set exactly one of script or persona, not both")
	case len(s) == 0 && p == nil:
		return errors.New("caller: set exactly one of script or persona")
	case len(s) > maxScriptLines:
		return fmt.Errorf("caller.script: %d lines, at most %d", len(s), maxScriptLines)
	case p != nil:
		if strings.TrimSpace(p.Who) == "" || strings.TrimSpace(p.Goal) == "" {
			return errors.New("caller.persona: who and goal are required")
		}
		if p.MaxTurns == 0 {
			p.MaxTurns = defaultTurns
		}
		if p.MaxTurns < 1 || p.MaxTurns > maxPersonaTurn {
			return fmt.Errorf("caller.persona.max_turns: %d, want 1..%d", p.MaxTurns, maxPersonaTurn)
		}
	}
	for i, l := range c.Caller.Script {
		if n := utf8.RuneCountInString(l); strings.TrimSpace(l) == "" || n > maxLineLen {
			return fmt.Errorf("caller.script[%d]: %d chars, want 1..%d and not blank", i, n, maxLineLen)
		}
	}
	if n := utf8.RuneCountInString(c.ExpectedAnswer); n > maxExpected {
		return fmt.Errorf("expected_answer: %d chars, at most %d", n, maxExpected)
	}
	e := c.Expect
	for _, k := range []struct {
		field string
		pats  []string
	}{{"expect.say", e.Say}, {"expect.never_say", e.NeverSay}} {
		for i, p := range k.pats {
			if p == "" {
				return fmt.Errorf("%s[%d]: empty pattern", k.field, i)
			}
			if _, err := regexp.Compile(p); err != nil {
				return fmt.Errorf("%s[%d]: %w", k.field, i, err)
			}
		}
	}
	if e.EndReason != "" && !slices.Contains(storage.EndReasons, e.EndReason) {
		return fmt.Errorf("expect.end_reason: %q, want one of %s", e.EndReason, strings.Join(storage.EndReasons, ", "))
	}
	for _, k := range []struct {
		field string
		tools []string
	}{{"expect.tools_called", e.ToolsCalled}, {"expect.tools_not_called", e.ToolsNotCalled}} {
		for i, t := range k.tools {
			name, status, has := strings.Cut(t, ":")
			if name == "" || (has && !slices.Contains(toolStatuses, status)) {
				return fmt.Errorf("%s[%d]: %q, want name or name:%s", k.field, i, t, strings.Join(toolStatuses, "|"))
			}
		}
	}
	for i, p := range e.Path {
		if strings.TrimSpace(p) == "" {
			return fmt.Errorf("expect.path[%d]: empty step", i)
		}
	}
	if e.MaxTurns < 0 || e.MedianReplyMSMax < 0 {
		return errors.New("expect.max_turns and expect.median_reply_ms_max must not be negative")
	}
	return nil
}
