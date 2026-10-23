package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"govoice/server/storage"
)

// Case is one simulated call and what it must show. An omitted expect key is not checked.
type Case struct {
	Name   string `yaml:"name"`
	Agent  string `yaml:"agent"` // agent name or id
	Caller Caller `yaml:"caller"`
	Expect Expect `yaml:"expect"`
	File   string `yaml:"-"`
}

// Caller is exactly one of a fixed script or an LLM persona.
type Caller struct {
	Script  []string `yaml:"script"`
	Persona *Persona `yaml:"persona"`
}

// Persona is who the simulated caller plays and what it wants.
type Persona struct {
	Who      string `yaml:"who"`
	Goal     string `yaml:"goal"`
	MaxTurns int    `yaml:"max_turns"`
}

// Expect lists the assertions; tools_called entries are "name" (succeeded) or "name:status".
type Expect struct {
	Path             []string `yaml:"path"`
	ToolsCalled      []string `yaml:"tools_called"`
	ToolsNotCalled   []string `yaml:"tools_not_called"`
	Say              []string `yaml:"say"`
	NeverSay         []string `yaml:"never_say"`
	EndReason        string   `yaml:"end_reason"`
	MaxTurns         int      `yaml:"max_turns"`
	MedianReplyMSMax int      `yaml:"median_reply_ms_max"`
}

const (
	maxScriptLines = 20
	maxLineLen     = 500
	defaultTurns   = 8
	maxPersonaTurn = 20
)

var toolStatuses = []string{"succeeded", "failed", "cancelled"}

// LoadCases reads every *.yaml and *.yml in dir (sorted by file name), refusing unknown keys and
// duplicate case names. only, when non-empty, keeps the named cases and refuses an unknown name.
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
		c, err := loadCase(f)
		if err != nil {
			return nil, err
		}
		if prev, dup := seen[c.Name]; dup {
			return nil, fmt.Errorf("%s: name: %q is already used by %s", f, c.Name, prev)
		}
		seen[c.Name] = f
		cases = append(cases, c)
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

func loadCase(path string) (Case, error) {
	f, err := os.Open(path)
	if err != nil {
		return Case{}, err
	}
	defer f.Close()
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	var c Case
	if err := dec.Decode(&c); err != nil {
		if errors.Is(err, io.EOF) {
			err = errors.New("empty file")
		}
		return Case{}, fmt.Errorf("%s: %w", path, err)
	}
	if err := dec.Decode(&yaml.Node{}); !errors.Is(err, io.EOF) {
		return Case{}, fmt.Errorf("%s: one case per file", path)
	}
	c.File = path
	if err := c.validate(); err != nil {
		return Case{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

func (c *Case) validate() error {
	if strings.TrimSpace(c.Name) == "" {
		return errors.New("name: required")
	}
	if strings.TrimSpace(c.Agent) == "" {
		return errors.New("agent: required")
	}
	if strings.ContainsAny(c.Agent, `/\`) || strings.Contains(c.Agent, "..") {
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
