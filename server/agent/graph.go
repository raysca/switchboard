package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"govoice/server/provider"
	"govoice/server/storage"
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

var (
	nonSlug = regexp.MustCompile(`[^a-z0-9]+`)
	// Node ids and cue names are tenant data that reach logs, trace attributes
	// and the UI: keep them short and plain.
	nodeID  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)
	cueName = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)
)

const maxCueAfterMS = 10000

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
		case !nodeID.MatchString(rn.ID) || ids[rn.ID]:
			return Graph{}, invalid("node ids must be unique and match %s", nodeID)
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
	if c := g.HandoffCue; c != nil && (!cueName.MatchString(c.Name) || c.AfterMS < 0 || c.AfterMS > maxCueAfterMS) {
		return Graph{}, invalid("handoff_cue needs a name matching %s and after_ms between 0 and %d", cueName, maxCueAfterMS)
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

// Problems lists attachment problems for every node. Non-entry nodes have no
// STT of their own, so they are checked with the entry's STT borrowed; their
// problems are prefixed with the node id.
func (g Graph) Problems(reg *provider.Registry, creds []storage.Credential) []string {
	entry := g.EntryNode().Config
	var out []string
	for _, n := range g.Nodes {
		c := n.Config
		prefix := ""
		if n.ID != g.Entry {
			c.STT, prefix = entry.STT, n.ID+":"
		}
		for _, p := range append(AttachmentProblems(c, reg, creds), FallbackProblems(c, reg, creds)...) {
			out = append(out, prefix+p)
		}
	}
	return out
}

// checkNodeTools rejects a node that exposes a tool the agent version has not selected.
func checkNodeTools(g Graph, selected []ToolSelection) error {
	have := make(map[string]bool, len(selected))
	for _, s := range selected {
		have[s.ServerID+"/"+s.RemoteName] = true
	}
	for _, n := range g.Nodes {
		for _, t := range n.Tools {
			if !have[t] {
				return invalid("node %q lists tool %q that the agent has not selected", n.ID, t)
			}
		}
	}
	return nil
}
