package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Graph limits keep a stored agent and a call's node set bounded.
const (
	MaxNodes     = 8
	MaxEdges     = 32
	maxCondition = 512
)

// Graph is the stored agent: nodes (each a full per-node Config), handoff
// edges, and the call-level handoff cue. A flat Config is a one-node Graph.
type Graph struct {
	Entry      string      `json:"entry"`
	Nodes      []Node      `json:"nodes"`
	Edges      []Edge      `json:"edges"`
	HandoffCue *HandoffCue `json:"handoff_cue,omitempty"`
}

type Position struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

type Node struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Config is the node's stages. STT, VAD, recording, context limits and
	// async_tools are call-level: only the entry node may set them.
	Config Config `json:"-"`
	// Tools lists the MCP tools ("<mcp_server_id>/<remote_name>") this node
	// exposes, a subset of the agent version's selected tools.
	Tools    []string  `json:"tools,omitempty"`
	Position *Position `json:"position,omitempty"`
}

type Edge struct {
	ID        string `json:"id"`
	From      string `json:"from"`
	To        string `json:"to"`
	Condition string `json:"condition"`
}

// HandoffCue names a server-configured sound (audio.cues) that plays while the
// call is between agents, once the gap exceeds AfterMS (0: default 300 ms).
type HandoffCue struct {
	Name    string `json:"name"`
	AfterMS int    `json:"after_ms,omitempty"`
}

type rawNode struct {
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Config   json.RawMessage `json:"config"`
	Tools    []string        `json:"tools"`
	Position *Position       `json:"position"`
}

type rawGraph struct {
	Entry      string      `json:"entry"`
	Nodes      []rawNode   `json:"nodes"`
	Edges      []Edge      `json:"edges"`
	HandoffCue *HandoffCue `json:"handoff_cue"`
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slug(name string) string {
	return strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(name), "_"), "_")
}

// TransferToolName is the tool the model calls to hand off to the named node.
func TransferToolName(name string) string { return "transfer_to_" + slug(name) }

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidConfig, fmt.Sprintf(format, a...))
}

// DecodeGraph strictly decodes a stored config. A flat Config (no "nodes")
// becomes a one-node graph named "main".
func DecodeGraph(raw json.RawMessage) (Graph, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil || probe == nil {
		return Graph{}, invalid("config must be a JSON object")
	}
	if _, ok := probe["nodes"]; !ok {
		cfg, err := DecodeConfig(raw)
		if err != nil {
			return Graph{}, err
		}
		return Graph{Entry: "main", Nodes: []Node{{ID: "main", Name: "main", Config: cfg}}}, nil
	}
	var rg rawGraph
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rg); err != nil {
		return Graph{}, invalid("%v", err)
	}
	if len(rg.Nodes) == 0 || len(rg.Nodes) > MaxNodes || len(rg.Edges) > MaxEdges {
		return Graph{}, invalid("a graph has 1 to %d nodes and at most %d edges", MaxNodes, MaxEdges)
	}
	g := Graph{Entry: rg.Entry, Edges: rg.Edges, HandoffCue: rg.HandoffCue, Nodes: make([]Node, 0, len(rg.Nodes))}
	ids, slugs := map[string]bool{}, map[string]bool{}
	for _, rn := range rg.Nodes {
		name := strings.TrimSpace(rn.Name)
		s := slug(name)
		switch {
		case rn.ID == "" || ids[rn.ID]:
			return Graph{}, invalid("node ids must be non-empty and unique")
		case name == "" || len(name) > 64 || s == "":
			return Graph{}, invalid("node %q needs a name of 1 to 64 characters with a letter or digit", rn.ID)
		case slugs[s]:
			return Graph{}, invalid("node names must be unique (%q)", name)
		}
		ids[rn.ID], slugs[s] = true, true
		cfg, err := DecodeConfig(rn.Config)
		if err != nil {
			return Graph{}, fmt.Errorf("node %q: %w", rn.ID, err)
		}
		g.Nodes = append(g.Nodes, Node{ID: rn.ID, Name: name, Config: cfg, Tools: rn.Tools, Position: rn.Position})
	}
	if !ids[g.Entry] {
		return Graph{}, invalid("entry %q is not a node", g.Entry)
	}
	pairs := map[[2]string]bool{}
	for _, e := range g.Edges {
		key := [2]string{e.From, e.To}
		switch {
		case e.ID == "" || !ids[e.From] || !ids[e.To] || e.From == e.To:
			return Graph{}, invalid("edge %q must join two different existing nodes", e.ID)
		case strings.TrimSpace(e.Condition) == "" || len(e.Condition) > maxCondition:
			return Graph{}, invalid("edge %q needs a condition of 1 to %d characters", e.ID, maxCondition)
		case pairs[key]:
			return Graph{}, invalid("edge %q duplicates another edge between the same nodes", e.ID)
		}
		pairs[key] = true
	}
	if err := g.validateCallLevel(); err != nil {
		return Graph{}, err
	}
	return g, nil
}

// validateCallLevel rejects call-level settings on non-entry nodes and a
// summarization setting some node's LLM cannot honour.
func (g Graph) validateCallLevel() error {
	entry := g.EntryNode().Config
	for _, n := range g.Nodes {
		c := n.Config
		if n.ID != g.Entry {
			if c.STT.Provider != "" || c.VADStopMS != 0 || c.STTP99MS != 0 || c.Recording != (RecordingConfig{}) ||
				c.Context.MaxMessages != 0 || c.Context.MaxTokens != 0 || len(c.AsyncTools) != 0 || c.Context.Summarization != (SummarizationConfig{}) {
				return invalid("node %q sets call-level fields (stt, vad_stop_ms, stt_p99_ms, recording, context, async_tools); set them on the entry node", n.ID)
			}
		}
		if entry.Context.Summarization.Enabled && !SummarizingProviders[c.LLM.Provider] {
			return invalid("node %q: llm provider %q does not support the entry node's context summarization", n.ID, c.LLM.Provider)
		}
	}
	return nil
}

func (g Graph) EntryNode() Node {
	n, _ := g.Node(g.Entry)
	return n
}

func (g Graph) Node(id string) (Node, bool) {
	for _, n := range g.Nodes {
		if n.ID == id {
			return n, true
		}
	}
	return Node{}, false
}

func (g Graph) EdgesFrom(id string) []Edge {
	var out []Edge
	for _, e := range g.Edges {
		if e.From == id {
			out = append(out, e)
		}
	}
	return out
}

// EntryConfig is the call-level config of a stored agent, flat or graph.
func EntryConfig(raw json.RawMessage) (Config, error) {
	g, err := DecodeGraph(raw)
	if err != nil {
		return Config{}, err
	}
	return g.EntryNode().Config, nil
}
