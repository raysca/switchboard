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

// LoadCases reads every *.yaml in dir (sorted by file name), refusing unknown keys and
// duplicate case names. only, when non-empty, keeps the named cases and refuses an unknown name.
func LoadCases(dir string, only []string) ([]Case, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no *.yaml case files in %s", dir)
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
	switch s, p := c.Caller.Script, c.Caller.Persona; {
	case len(s) > 0 && p != nil:
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
		if n := len(l); n < 1 || n > maxLineLen {
			return fmt.Errorf("caller.script[%d]: %d chars, want 1..%d", i, n, maxLineLen)
		}
	}
	e := c.Expect
	for _, k := range []struct {
		field string
		pats  []string
	}{{"expect.say", e.Say}, {"expect.never_say", e.NeverSay}} {
		for i, p := range k.pats {
			if _, err := regexp.Compile(p); err != nil {
				return fmt.Errorf("%s[%d]: %w", k.field, i, err)
			}
		}
	}
	if e.EndReason != "" && !slices.Contains(storage.EndReasons, e.EndReason) {
		return fmt.Errorf("expect.end_reason: %q, want one of %s", e.EndReason, strings.Join(storage.EndReasons, ", "))
	}
	for i, t := range e.ToolsCalled {
		name, status, has := strings.Cut(t, ":")
		if name == "" || (has && !slices.Contains(toolStatuses, status)) {
			return fmt.Errorf("expect.tools_called[%d]: %q, want name or name:%s", i, t, strings.Join(toolStatuses, "|"))
		}
	}
	if e.MaxTurns < 0 || e.MedianReplyMSMax < 0 {
		return errors.New("expect.max_turns and expect.median_reply_ms_max must not be negative")
	}
	return nil
}
