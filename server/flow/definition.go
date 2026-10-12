package flow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Definition limits keep a stored flow and a call's step set bounded.
const (
	MaxSteps     = 8
	MaxRoutes    = 32
	maxCondition = 512
)

// Definition is the stored flow: steps (each a full per-step Config), handoff
// routes, and the call-level route cue. A flat Config is a one-step Definition.
type Definition struct {
	Start    string    `json:"start"`
	Steps    []Step    `json:"steps"`
	Routes   []Route   `json:"routes"`
	RouteCue *RouteCue `json:"route_cue,omitempty"`
}

type Position struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

type Step struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Config is the step's stages. STT, VAD, recording, context limits and
	// async_tools are call-level: only the start step may set them.
	Config Config `json:"-"`
	// Tools lists the MCP tools ("<mcp_server_id>/<remote_name>") this step
	// exposes, a subset of the flow version's selected tools.
	Tools    []string  `json:"tools,omitempty"`
	Position *Position `json:"position,omitempty"`
}

type Route struct {
	ID        string `json:"id"`
	From      string `json:"from"`
	To        string `json:"to"`
	Condition string `json:"condition"`
}

// RouteCue names a server-configured sound (audio.cues) that plays while the
// call is between steps, once the gap exceeds AfterMS (0: default 300 ms).
type RouteCue struct {
	Name    string `json:"name"`
	AfterMS int    `json:"after_ms,omitempty"`
}

type rawStep struct {
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Config   json.RawMessage `json:"config"`
	Tools    []string        `json:"tools"`
	Position *Position       `json:"position"`
}

type rawDefinition struct {
	Start    string    `json:"start"`
	Steps    []rawStep `json:"steps"`
	Routes   []Route   `json:"routes"`
	RouteCue *RouteCue `json:"route_cue"`
}

var (
	nonSlug = regexp.MustCompile(`[^a-z0-9]+`)
	// Step ids and cue names are tenant data that reach logs, trace attributes
	// and the UI: keep them short and plain.
	stepID  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)
	cueName = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)
)

const maxCueAfterMS = 10000

func slug(name string) string {
	return strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(name), "_"), "_")
}

// TransferToolName is the tool the model calls to hand off to the named step.
func TransferToolName(name string) string { return "transfer_to_" + slug(name) }

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidConfig, fmt.Sprintf(format, a...))
}

// legacyKeys are the pre-rename config keys. Stored versions are immutable and
// hashed, so they are read as written.
var legacyKeys = map[string]string{"entry": "start", "nodes": "steps", "edges": "routes", "handoff_cue": "route_cue"}

// Decode strictly decodes a stored config. A flat Config (no "steps")
// becomes a one-step flow named "main".
func Decode(raw json.RawMessage) (Definition, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil || probe == nil {
		return Definition{}, invalid("config must be a JSON object")
	}
	if _, ok := probe["nodes"]; ok {
		for old, key := range legacyKeys {
			if v, ok := probe[old]; ok {
				if _, dup := probe[key]; dup {
					return Definition{}, invalid("config mixes %q and %q", old, key)
				}
				probe[key] = v
				delete(probe, old)
			}
		}
		raw, _ = json.Marshal(probe) // ponytail: re-encoding a map that just decoded cannot fail
	}
	if _, ok := probe["steps"]; !ok {
		cfg, err := DecodeConfig(raw)
		if err != nil {
			return Definition{}, err
		}
		return Definition{Start: "main", Steps: []Step{{ID: "main", Name: "main", Config: cfg}}}, nil
	}
	var rg rawDefinition
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rg); err != nil {
		return Definition{}, invalid("%v", err)
	}
	if len(rg.Steps) == 0 || len(rg.Steps) > MaxSteps || len(rg.Routes) > MaxRoutes {
		return Definition{}, invalid("a flow has 1 to %d steps and at most %d routes", MaxSteps, MaxRoutes)
	}
	g := Definition{Start: rg.Start, Routes: rg.Routes, RouteCue: rg.RouteCue, Steps: make([]Step, 0, len(rg.Steps))}
	ids, slugs := map[string]bool{}, map[string]bool{}
	for _, rn := range rg.Steps {
		name := strings.TrimSpace(rn.Name)
		s := slug(name)
		switch {
		case !stepID.MatchString(rn.ID) || ids[rn.ID]:
			return Definition{}, invalid("step ids must be unique and match %s", stepID)
		case name == "" || len(name) > 64 || s == "":
			return Definition{}, invalid("step %q needs a name of 1 to 64 characters with a letter or digit", rn.ID)
		case slugs[s]:
			return Definition{}, invalid("step names must be unique (%q)", name)
		}
		ids[rn.ID], slugs[s] = true, true
		cfg, err := DecodeConfig(rn.Config)
		if err != nil {
			return Definition{}, fmt.Errorf("step %q: %w", rn.ID, err)
		}
		g.Steps = append(g.Steps, Step{ID: rn.ID, Name: name, Config: cfg, Tools: rn.Tools, Position: rn.Position})
	}
	if c := g.RouteCue; c != nil && (!cueName.MatchString(c.Name) || c.AfterMS < 0 || c.AfterMS > maxCueAfterMS) {
		return Definition{}, invalid("route_cue needs a name matching %s and after_ms between 0 and %d", cueName, maxCueAfterMS)
	}
	if !ids[g.Start] {
		return Definition{}, invalid("start %q is not a step", g.Start)
	}
	pairs := map[[2]string]bool{}
	for _, e := range g.Routes {
		key := [2]string{e.From, e.To}
		switch {
		case e.ID == "" || !ids[e.From] || !ids[e.To] || e.From == e.To:
			return Definition{}, invalid("route %q must join two different existing steps", e.ID)
		case strings.TrimSpace(e.Condition) == "" || len(e.Condition) > maxCondition:
			return Definition{}, invalid("route %q needs a condition of 1 to %d characters", e.ID, maxCondition)
		case pairs[key]:
			return Definition{}, invalid("route %q duplicates another route between the same steps", e.ID)
		}
		pairs[key] = true
	}
	if err := g.validateCallLevel(); err != nil {
		return Definition{}, err
	}
	return g, nil
}

// validateCallLevel rejects call-level settings on non-start steps and a
// summarization setting some step's LLM cannot honour.
func (g Definition) validateCallLevel() error {
	entry := g.StartStep().Config
	for _, n := range g.Steps {
		c := n.Config
		if n.ID != g.Start {
			if c.STT.Provider != "" || c.VADStopMS != 0 || c.STTP99MS != 0 || c.Recording != (RecordingConfig{}) ||
				c.Context.MaxMessages != 0 || c.Context.MaxTokens != 0 || len(c.AsyncTools) != 0 || c.Context.Summarization != (SummarizationConfig{}) {
				return invalid("step %q sets flow settings (stt, vad_stop_ms, stt_p99_ms, recording, context, async_tools); set them on the start step", n.ID)
			}
		}
		if entry.Context.Summarization.Enabled && !SummarizingProviders[c.LLM.Provider] {
			return invalid("step %q: llm provider %q does not support the start step's context summarization", n.ID, c.LLM.Provider)
		}
	}
	return nil
}

func (g Definition) StartStep() Step {
	n, _ := g.Step(g.Start)
	return n
}

func (g Definition) Step(id string) (Step, bool) {
	for _, n := range g.Steps {
		if n.ID == id {
			return n, true
		}
	}
	return Step{}, false
}

func (g Definition) RoutesFrom(id string) []Route {
	var out []Route
	for _, e := range g.Routes {
		if e.From == id {
			out = append(out, e)
		}
	}
	return out
}

// StartConfig is the call-level config of a stored flow, flat or flow.
func StartConfig(raw json.RawMessage) (Config, error) {
	g, err := Decode(raw)
	if err != nil {
		return Config{}, err
	}
	return g.StartStep().Config, nil
}

// Problems lists attachment problems for every step. Non-start steps have no
// STT of their own, so they are checked with the entry's STT borrowed; their
// problems are prefixed with the step id.
func (g Definition) Problems(av Availability) (reasons, missingEnv []string) {
	entry := g.StartStep().Config
	for _, n := range g.Steps {
		c := n.Config
		prefix := ""
		if n.ID != g.Start {
			c.STT, prefix = entry.STT, n.ID+":"
		}
		r1, e1 := AttachmentProblems(c, av)
		r2, e2 := FallbackProblems(c, av)
		for _, p := range append(r1, r2...) {
			reasons = append(reasons, prefix+p)
		}
		missingEnv = append(missingEnv, e1...)
		missingEnv = append(missingEnv, e2...)
	}
	return reasons, uniq(missingEnv)
}

// checkStepTools rejects a step that exposes a tool the flow version has not selected.
func checkStepTools(g Definition, selected []ToolSelection) error {
	have := make(map[string]bool, len(selected))
	for _, s := range selected {
		have[s.ServerID+"/"+s.RemoteName] = true
	}
	for _, n := range g.Steps {
		for _, t := range n.Tools {
			if !have[t] {
				return invalid("step %q lists tool %q that the flow has not selected", n.ID, t)
			}
		}
	}
	return nil
}
