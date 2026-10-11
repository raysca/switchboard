# Agent Graph (Multi-Agent Handoff) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** An agent is a graph of nodes (each with its own prompt, tools, LLM and voice) joined by LLM-driven handoff edges, authored and debugged on a canvas, with one node active per call.

**Architecture:** The graph is stored as the agent version's `config_json` (a flat config still works and is read as a one-node graph). At runtime a `core.StageSwap` holds the active node's LLM and TTS; a `core.Handoff` builds the target node on demand when a `transfer_to_<node>` tool fires, flips at idle by riding the existing `AsyncTrigger`, and closes the node it left. A cue sound covers the gap through the paced writer's underlay. The designer is a React Flow canvas whose live view is driven by handoff trace spans.

**Tech Stack:** Go 1.x (core, server, transport), SQLite/Postgres via existing storage, React 19 + TanStack Query + zod + vitest (ui), `@xyflow/react` (new).

**Spec:** [docs/superpowers/specs/2026-10-11-agent-graph-design.md](../specs/2026-10-11-agent-graph-design.md) (with the "Revisions made while planning" section appended by Task 0).

## Global Constraints

- `core/` never imports `server/`; every `core` addition is consumed by `server/` (AGENTS.md: YAGNI, server-consumption).
- Every queue and slice that grows with input is bounded: at most **8 nodes**, **32 edges**, **10 handoffs per call**, cue hard cap **10 s**.
- Goroutines are started with `session.Go(...)` and end on `ctx.Done()`; no `context.Context` in structs; never hold a mutex across I/O.
- Hot audio path: no allocation per 20 ms frame (the cue mix reuses the writer's buffer).
- Durations use monotonic time (`time.Since`, `time.Duration`).
- Errors are wrapped (`fmt.Errorf("...: %w", err)`); sentinels are exported `var ErrX = errors.New(...)`.
- All code passes `go test -race ./core/... ./server/... ./transport/...`; `gofmt -w .` and `make verify` before the final review.
- Commits are conventional (`feat(core): ...`) and end with `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`.
- Existing flat `/api/v1/agents` payloads and stored schema-1 versions keep working unchanged.

## Review Focus

1. **Caller speaks while a handoff is pending** → the new node answers them once; the old node does not answer first, and no second unprompted greeting follows (Task 4, `TestHandoffUserTurnFlipsOnce`).
2. **Target node cannot be built** (credential deleted, provider down) → the old node stays active, the call continues, one `handoff_failed` span is recorded (Task 4, `TestHandoffBuildFailureKeepsOldNode`).
3. **Two transfers in one turn / transfer ping-pong** → only the first commits in a turn; after 10 handoffs transfer tools are no longer offered (Task 4, `TestHandoffPendingAndLimit`).
4. **A node leaks goroutines or connections when left** → the left node is closed exactly once, after in-flight frames drain or the timeout (Task 3, `TestStageSwapRetireWaitsForInflight`; Task 4 closes it on the session supervisor).
5. **A hallucinated tool call to a tool outside the node's list** → rejected, not executed (Task 5, `TestNodeExecutorRejectsForeignTool`).
6. **Cue never stops** (handoff fails, caller hangs up, new node silent) → stops on first audio, barge-in, failure, hard cap (Task 8, `TestCueStopsOnEveryExit`).
7. **A one-node/flat agent behaves exactly as before** (Task 2 and Task 5 regression tests).

---

## File Structure

| File | Responsibility |
|---|---|
| `server/agent/graph.go` (new) | Graph/Node/Edge types, `DecodeGraph`, validation, slug and tool-name helpers |
| `server/agent/graph_test.go` (new) | Validation and flat-wrap tests |
| `server/agent/service.go` (modify) | Accept graphs on create/update, per-node readiness, node tool references |
| `server/management.go`, `server/runtime/mcp.go`, `server/call.go` (modify) | Use the entry node's config / iterate all nodes |
| `core/stage_swap.go` (new) | `StageSwap`: a stage whose implementation is replaced at run time |
| `core/handoff.go` (new) | `Handoff`: commit, build on demand, flip at idle, retire |
| `server/graph_call.go` (new) | Per-node stage specs, transfer tools, node-filtered executors |
| `server/registry.go` (modify) | Build wraps LLM/TTS in `StageSwap` and wires `Handoff` |
| `transport/ws/cue.go` (new) | `CueUnderlay`: background + runtime cue cursor |
| `server/audiofilter.go`, `server/config.go` (modify) | `audio.cues` config and loader |
| `server/timeline.go`, `server/metrics.go` (modify) | `node` on events, handoff metrics |
| `ui/src/api/client.ts` (modify) | Graph DTO + zod schema |
| `ui/src/routes/agents/graph/` (new) | `model.ts` (pure graph ops), `activity.ts` (trace → node state), `Canvas.tsx`, `Inspector.tsx`, `LiveGraph.tsx` |
| `tools/livecheck/scenarios.go` (modify) | S14 two-node handoff |

---

### Task 0: Record the planning revisions in the spec

The code reading changed four details; the spec must match what is built.

**Files:**
- Modify: `docs/superpowers/specs/2026-10-11-agent-graph-design.md`

- [ ] **Step 1: Append this section to the spec**

```markdown
## Revisions made while planning (2026-10-11)

Reading the code changed four details; the design is otherwise as approved.
1. **No `node_id` column, no migration.** MCP tools stay selected per agent version (`agent_mcp_tools` is unchanged). Each node lists the tools it exposes in `tools: ["<mcp_server_id>/<remote_name>"]` (the same reference form as `async_tools`). Readiness and runtime capture still cover the union.
2. **Graphs are detected by shape (a top-level `nodes` key); `schema_version` stays 1.** Layout positions live in the config and are hashed: publishing is an explicit action, so a layout change publishes a new version.
3. **The transfer tool is not special-cased inside the tool loop.** Its result tells the model to say one short sentence and stop; that sentence is the bridge line. The flip then happens when the call is idle, or earlier if the caller speaks first, by riding `AsyncTrigger`.
4. **Credentials are resolved when a node is activated,** not held for the whole call (`callSpec` wipes decrypted secrets after the first build).
5. **Live view data comes from `handoff` trace spans** read through the existing session traces API (polled), not a new transport.
```

- [ ] **Step 2: Commit**

```bash
git add docs/superpowers/specs/2026-10-11-agent-graph-design.md
git commit -m "docs: record agent graph spec revisions found while planning

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 1: Graph model and validation

**Files:**
- Create: `server/agent/graph.go`
- Test: `server/agent/graph_test.go`

**Interfaces:**
- Produces:
  - `type Graph struct { Entry string; Nodes []Node; Edges []Edge; HandoffCue *HandoffCue }`
  - `type Node struct { ID, Name string; Config Config; Tools []string; Position *Position }`
  - `type Edge struct { ID, From, To, Condition string }`
  - `type HandoffCue struct { Name string; AfterMS int }`
  - `func DecodeGraph(raw json.RawMessage) (Graph, error)` (flat config → one-node graph)
  - `func (g Graph) EntryNode() Node`, `func (g Graph) Node(id string) (Node, bool)`, `func (g Graph) EdgesFrom(id string) []Edge`
  - `func TransferToolName(name string) string` → `transfer_to_<slug>`
  - `func EntryConfig(raw json.RawMessage) (Config, error)`
  - consts `MaxNodes = 8`, `MaxEdges = 32`
- Consumes: `DecodeConfig`, `Config`, `ErrInvalidConfig` from `server/agent/config.go`; `SummarizingProviders`.

- [ ] **Step 1: Write the failing tests**

```go
package agent

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const flatCfg = `{"stt":{"provider":"deepgram","credential_id":"c1"},"llm":{"provider":"openai","model":"m","credential_id":"c2","system_prompt":"hi"},"tts":{"provider":"cartesia","voice":"v","credential_id":"c3"}}`

func node(id, name string, extra string) string {
	cfg := `{"llm":{"provider":"openai","model":"m","credential_id":"c2","system_prompt":"` + name + `"},"tts":{"provider":"cartesia","voice":"v","credential_id":"c3"}` + extra + `}`
	return `{"id":"` + id + `","name":"` + name + `","config":` + cfg + `}`
}

func TestDecodeGraphWrapsFlatConfig(t *testing.T) {
	g, err := DecodeGraph(json.RawMessage(flatCfg))
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Nodes) != 1 || g.Entry != g.Nodes[0].ID || len(g.Edges) != 0 {
		t.Fatalf("flat config must become a one-node graph, got %+v", g)
	}
	if g.EntryNode().Config.LLM.Model != "m" {
		t.Fatal("entry config lost")
	}
}

func TestDecodeGraphTwoNodes(t *testing.T) {
	raw := `{"entry":"front","nodes":[` +
		`{"id":"front","name":"Front desk","config":` + flatCfg + `},` +
		node("sales", "Sales", "") + `],` +
		`"edges":[{"id":"e1","from":"front","to":"sales","condition":"wants to buy"}]}`
	g, err := DecodeGraph(json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	if got := TransferToolName("Sales Team"); got != "transfer_to_sales_team" {
		t.Fatalf("tool name = %q", got)
	}
	if len(g.EdgesFrom("front")) != 1 || len(g.EdgesFrom("sales")) != 0 {
		t.Fatal("EdgesFrom wrong")
	}
}

func TestDecodeGraphRejects(t *testing.T) {
	front := `{"id":"front","name":"Front","config":` + flatCfg + `}`
	cases := map[string]string{
		"no entry":          `{"nodes":[` + front + `],"edges":[]}`,
		"entry missing":     `{"entry":"x","nodes":[` + front + `],"edges":[]}`,
		"dup node id":       `{"entry":"front","nodes":[` + front + `,` + front + `],"edges":[]}`,
		"dup slug":          `{"entry":"front","nodes":[` + front + `,` + node("b", "front", "") + `],"edges":[]}`,
		"edge to nowhere":   `{"entry":"front","nodes":[` + front + `],"edges":[{"id":"e","from":"front","to":"zz","condition":"c"}]}`,
		"self edge":         `{"entry":"front","nodes":[` + front + `],"edges":[{"id":"e","from":"front","to":"front","condition":"c"}]}`,
		"empty condition":   `{"entry":"front","nodes":[` + front + `,` + node("b", "B", "") + `],"edges":[{"id":"e","from":"front","to":"b","condition":" "}]}`,
		"dup edge pair":     `{"entry":"front","nodes":[` + front + `,` + node("b", "B", "") + `],"edges":[{"id":"e1","from":"front","to":"b","condition":"c"},{"id":"e2","from":"front","to":"b","condition":"d"}]}`,
		"call-level on non-entry": `{"entry":"front","nodes":[` + front + `,` + node("b", "B", `,"vad_stop_ms":300`) + `],"edges":[]}`,
		"unknown field":     `{"entry":"front","nodes":[` + front + `],"edges":[],"bogus":1}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeGraph(json.RawMessage(raw)); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("want ErrInvalidConfig, got %v", err)
			}
		})
	}
}

func TestDecodeGraphBounds(t *testing.T) {
	var nodes []string
	for i := 0; i <= MaxNodes; i++ {
		nodes = append(nodes, node("n"+strings.Repeat("x", i), "N"+strings.Repeat("x", i), ""))
	}
	raw := `{"entry":"n","nodes":[` + strings.Join(nodes, ",") + `],"edges":[]}`
	if _, err := DecodeGraph(json.RawMessage(raw)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("more than %d nodes must be rejected, got %v", MaxNodes, err)
	}
}

func TestSummarizationNeedsEverySummarizingLLM(t *testing.T) {
	entry := `{"stt":{"provider":"deepgram","credential_id":"c1"},"llm":{"provider":"openai","model":"m","credential_id":"c2"},"tts":{"provider":"cartesia","credential_id":"c3"},"context":{"summarization":{"enabled":true}}}`
	other := `{"id":"b","name":"B","config":{"llm":{"provider":"deepgram-llm-not-summarizing","model":"m","credential_id":"c2"},"tts":{"provider":"cartesia","credential_id":"c3"}}}`
	raw := `{"entry":"a","nodes":[{"id":"a","name":"A","config":` + entry + `},` + other + `],"edges":[]}`
	if _, err := DecodeGraph(json.RawMessage(raw)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("want ErrInvalidConfig, got %v", err)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./server/agent/ -run 'Graph|Summarization' -v`
Expected: FAIL (`undefined: DecodeGraph`).

- [ ] **Step 3: Implement `server/agent/graph.go`**

```go
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

func slug(name string) string { return strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(name), "_"), "_") }

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
```

(`Node.Config` is `json:"-"` because stored JSON keeps the node config as the original raw object; Task 2 stores the caller's canonical JSON, never re-marshals `Graph`.)

- [ ] **Step 4: Run to verify pass**

Run: `go test -race ./server/agent/ -run 'Graph|Summarization' -v`
Expected: PASS. If `"duplicate slug"` fails, check `slug("front")` vs node name `front`.

- [ ] **Step 5: Commit**

```bash
gofmt -l server/agent
git add server/agent/graph.go server/agent/graph_test.go
git commit -m "feat(agent): graph model with strict validation

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 2: Service accepts graphs; readiness and call sites use nodes

**Files:**
- Modify: `server/agent/service.go` (`normalizeConfig` ~L477, `attachedConfig` ~L450, `agentReadiness` ~L345, `SetMCPTools` ~L282, `Update` ~L190)
- Modify: `server/management.go:142`, `server/runtime/mcp.go:120`
- Test: `server/agent/service_test.go` (add), `server/agent/graph_test.go` (add)

**Interfaces:**
- Consumes: `DecodeGraph`, `Graph`, `AttachmentProblems`, `FallbackProblems`, `EntryConfig` (Task 1).
- Produces: `func (g Graph) Problems(providers *provider.Registry, creds []storage.Credential) []string` (per-node attachment problems, prefixed `"<node id>:"` for non-entry nodes); `func checkNodeTools(g Graph, selected []ToolSelection) error`.

- [ ] **Step 1: Write failing tests** in `server/agent/service_test.go` (use the existing test store helper the file already uses; copy its setup for credentials `c1..c3`):

```go
func TestCreateAcceptsGraphAndReportsNodeProblems(t *testing.T) {
	svc, creds := newTestService(t) // existing helper in this file; creds: stt/llm/tts credential ids
	good := graphJSON(creds, "") // helper below
	a, err := svc.Create(context.Background(), CreateRequest{Name: "two node", Config: good})
	if err != nil {
		t.Fatal(err)
	}
	if a.Readiness.State != ReadinessReady {
		t.Fatalf("want ready, got %+v", a.Readiness)
	}
	bad := graphJSON(creds, "missing-cred")
	if _, err := svc.Create(context.Background(), CreateRequest{Name: "bad", Config: bad}); !errors.Is(err, ErrNotAttached) {
		t.Fatalf("want ErrNotAttached naming the node, got %v", err)
	}
}

func TestNodeToolsMustBeSelected(t *testing.T) {
	g, _ := DecodeGraph(json.RawMessage(`{"entry":"a","nodes":[{"id":"a","name":"A","tools":["srv/lookup"],"config":` + flatCfg + `}],"edges":[]}`))
	if err := checkNodeTools(g, nil); err == nil {
		t.Fatal("a node tool that is not selected on the version must be rejected")
	}
	if err := checkNodeTools(g, []ToolSelection{{ServerID: "srv", RemoteName: "lookup"}}); err != nil {
		t.Fatal(err)
	}
}

func TestFlatConfigStillReady(t *testing.T) {
	svc, _ := newTestService(t)
	a, err := svc.Create(context.Background(), CreateRequest{Name: "flat", Config: json.RawMessage(validFlatConfig(t))}) // validFlatConfig: the file's existing flat fixture
	if err != nil || a.Readiness.State != ReadinessReady {
		t.Fatalf("err=%v readiness=%+v", err, a.Readiness)
	}
	if g, _ := DecodeGraph(a.Version.Config); len(g.Nodes) != 1 {
		t.Fatal("a flat agent must read back as a one-node graph")
	}
}
```

`graphJSON(creds, llmCredOverride string)` builds a two-node graph (entry flat config + a `sales` node with its own LLM/TTS) using the same credential ids the fixture creates; when `llmCredOverride != ""` the sales node's LLM `credential_id` is that value.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./server/agent/ -run 'Graph|NodeTools|FlatConfigStill' -v` → FAIL (`checkNodeTools` undefined).

- [ ] **Step 3: Implement**

In `graph.go` add:

```go
// Problems lists attachment problems for every node. Non-entry nodes have no
// STT of their own, so they are checked with the entry's STT borrowed.
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

func checkNodeTools(g Graph, selected []ToolSelection) error {
	have := make(map[string]bool, len(selected))
	for _, s := range selected {
		have[s.ServerID+"/"+s.RemoteName] = true
	}
	for _, n := range g.Nodes {
		for _, t := range n.Tools {
			if !have[t] {
				return fmt.Errorf("%w: node %q lists tool %q that the agent has not selected", ErrInvalidConfig, n.ID, t)
			}
		}
	}
	return nil
}
```

In `service.go`:
- `normalizeConfig`: replace `DecodeConfig(raw)` with `DecodeGraph(raw)` (keep the JSON-object check and `CanonicalJSON`).
- `attachedConfig`: `g, err := DecodeGraph(canonical)`; `problems := g.Problems(s.providers, creds)`; same `ErrNotAttached` wrapping.
- `agentReadiness`: `g, parseErr := DecodeGraph(version.ConfigJSON)`; `AttachmentProblems(...)` line becomes `g.Problems(s.providers, creds)`.
- `create`: after `config, err := s.attachedConfig(...)`, `g, _ := DecodeGraph(config); if err := checkNodeTools(g, req.MCPTools); err != nil { return Agent{}, err }`.
- `Update` (config present): load the current version's `SelectedTools` (`s.store.Agents().SelectedTools(ctx, row.CurrentVersionID)`), convert with `fromStorageSelections`, `checkNodeTools`.
- `SetMCPTools`: before publishing, `g, _ := DecodeGraph(current.ConfigJSON)`; `checkNodeTools(g, tools)` so a tool a node still uses cannot be deselected.
- `management.go:142`: replace the `DecodeConfig` loop body with `g, err := agent.DecodeGraph(a.Version.Config)` and range over `g.Nodes`, checking `n.Config.Chain(stage)` (skip the `stt` stage for non-entry nodes).
- `runtime/mcp.go:120`: `agentCfg, _ := agent.EntryConfig(snapshot.Version.Config)`.
- `server/call.go:140` is changed in Task 5.

- [ ] **Step 4: Run** `go test -race ./server/... 2>&1 | tail -20` → PASS (all existing agent/API tests must stay green: flat configs go through the wrap path).

- [ ] **Step 5: Commit** `feat(agent): create, update and readiness understand graphs`.

---

### Task 3: `core.StageSwap`

**Files:**
- Create: `core/stage_swap.go`
- Test: `core/stage_swap_test.go`

**Interfaces:**
- Produces:
  - `func NewStageSwap(initial Processor) *StageSwap` — implements `Processor`, `Attacher`, `Closer`
  - `func (s *StageSwap) Replace(next Processor) (retire func(timeout time.Duration) error, err error)`
  - `func (s *StageSwap) Session() SessionController` (nil before `Attach`)
  - `func (s *StageSwap) SetBefore(fn func(f Frame, dir Direction))`
  - `func (s *StageSwap) SetContextProvider(fn func() []Message)` — forwarded to the current and every later inner that has it
  - `func (s *StageSwap) SummarizeText(ctx context.Context, system, user string) (string, error)` — forwarded; `ErrNoSummarizer` if the inner has none
  - `var ErrSwapNotAttached`, `ErrNoSummarizer`
- Consumes: `Processor`, `Attacher`, `Closer`, `SessionController`, `Message` (core).

- [ ] **Step 1: Failing tests**

```go
package core

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type probeStage struct {
	frames, closed, attached atomic.Int64
	block                    chan struct{}
}

func (p *probeStage) ProcessFrame(ctx context.Context, f Frame, d Direction, e Emitter) error {
	p.frames.Add(1)
	if p.block != nil {
		<-p.block
	}
	return nil
}
func (p *probeStage) Attach(SessionController, int) error { p.attached.Add(1); return nil }
func (p *probeStage) Close() error                       { p.closed.Add(1); return nil }

type fakeSession struct{ SessionController }

func TestStageSwapRoutesToCurrent(t *testing.T) {
	a, b := &probeStage{}, &probeStage{}
	s := NewStageSwap(a)
	if err := s.Attach(fakeSession{}, 3); err != nil {
		t.Fatal(err)
	}
	_ = s.ProcessFrame(context.Background(), &UserMessageFrame{}, Downstream, nil)
	retire, err := s.Replace(b)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.ProcessFrame(context.Background(), &UserMessageFrame{}, Downstream, nil)
	if a.frames.Load() != 1 || b.frames.Load() != 1 || b.attached.Load() != 1 {
		t.Fatalf("a=%d b=%d attached=%d", a.frames.Load(), b.frames.Load(), b.attached.Load())
	}
	if err := retire(time.Second); err != nil || a.closed.Load() != 1 || b.closed.Load() != 0 {
		t.Fatalf("retire err=%v a.closed=%d b.closed=%d", err, a.closed.Load(), b.closed.Load())
	}
}

func TestStageSwapReplaceBeforeAttach(t *testing.T) {
	if _, err := NewStageSwap(&probeStage{}).Replace(&probeStage{}); !errors.Is(err, ErrSwapNotAttached) {
		t.Fatalf("want ErrSwapNotAttached, got %v", err)
	}
}

func TestStageSwapRetireWaitsForInflight(t *testing.T) {
	a := &probeStage{block: make(chan struct{})}
	s := NewStageSwap(a)
	_ = s.Attach(fakeSession{}, 0)
	go s.ProcessFrame(context.Background(), &UserMessageFrame{}, Downstream, nil)
	for a.frames.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	retire, _ := s.Replace(&probeStage{})
	done := make(chan error, 1)
	go func() { done <- retire(2 * time.Second) }()
	time.Sleep(30 * time.Millisecond)
	if a.closed.Load() != 0 {
		t.Fatal("closed while a frame was still in flight")
	}
	close(a.block)
	if err := <-done; err != nil || a.closed.Load() != 1 {
		t.Fatalf("err=%v closed=%d", err, a.closed.Load())
	}
}

func TestStageSwapRetireTimeoutStillCloses(t *testing.T) {
	a := &probeStage{block: make(chan struct{})}
	s := NewStageSwap(a)
	_ = s.Attach(fakeSession{}, 0)
	go s.ProcessFrame(context.Background(), &UserMessageFrame{}, Downstream, nil)
	for a.frames.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	retire, _ := s.Replace(&probeStage{})
	if err := retire(20 * time.Millisecond); !errors.Is(err, ErrRetireTimeout) || a.closed.Load() != 1 {
		t.Fatalf("a hung stage must be closed after the timeout: err=%v closed=%d", err, a.closed.Load())
	}
	close(a.block)
}
```

- [ ] **Step 2: Run** `go test -race ./core/ -run StageSwap -v` → FAIL (undefined).

- [ ] **Step 3: Implement `core/stage_swap.go`**

```go
package core

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrSwapNotAttached = errors.New("core: stage swap used before Attach")
	ErrNoSummarizer    = errors.New("core: active stage cannot summarize")
	ErrRetireTimeout   = errors.New("core: retired stage did not drain before the timeout")
)

// StageSwap is one pipeline stage whose implementation can be replaced while
// the call runs (a handoff swaps the LLM and the TTS). Frames go to the
// current inner stage; a replaced stage is retired once its in-flight frames
// finish, or after a timeout.
type StageSwap struct {
	cur atomic.Pointer[swapSlot]

	mu       sync.Mutex // guards the fields below; never held across inner calls
	session  SessionController
	index    int
	before   func(Frame, Direction)
	ctxProv  func() []Message
}

type swapSlot struct {
	p        Processor
	inflight atomic.Int64
}

func NewStageSwap(initial Processor) *StageSwap {
	s := &StageSwap{}
	s.cur.Store(&swapSlot{p: initial})
	return s
}

func (s *StageSwap) Attach(sc SessionController, index int) error {
	s.mu.Lock()
	s.session, s.index = sc, index
	s.mu.Unlock()
	return s.prepare(s.cur.Load().p, sc, index)
}

// prepare attaches an inner stage and hands it the context provider.
func (s *StageSwap) prepare(p Processor, sc SessionController, index int) error {
	if a, ok := p.(Attacher); ok {
		if err := a.Attach(sc, index); err != nil {
			return err
		}
	}
	s.mu.Lock()
	fn := s.ctxProv
	s.mu.Unlock()
	if fn != nil {
		if c, ok := p.(interface{ SetContextProvider(func() []Message) }); ok {
			c.SetContextProvider(fn)
		}
	}
	return nil
}

func (s *StageSwap) Session() SessionController {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.session
}

func (s *StageSwap) SetBefore(fn func(Frame, Direction)) {
	s.mu.Lock()
	s.before = fn
	s.mu.Unlock()
}

func (s *StageSwap) ProcessFrame(ctx context.Context, f Frame, dir Direction, e Emitter) error {
	s.mu.Lock()
	before := s.before
	s.mu.Unlock()
	if before != nil {
		before(f, dir) // may Replace: load the slot afterwards
	}
	slot := s.cur.Load()
	slot.inflight.Add(1)
	defer slot.inflight.Add(-1)
	return slot.p.ProcessFrame(ctx, f, dir, e)
}

// Replace attaches next at this stage's position and makes it current. The
// returned retire func closes the replaced stage once its in-flight frames
// finish (or after timeout, returning ErrRetireTimeout); run it off the audio path.
func (s *StageSwap) Replace(next Processor) (func(time.Duration) error, error) {
	s.mu.Lock()
	sc, index := s.session, s.index
	s.mu.Unlock()
	if sc == nil {
		return nil, ErrSwapNotAttached
	}
	if err := s.prepare(next, sc, index); err != nil {
		return nil, err
	}
	old := s.cur.Swap(&swapSlot{p: next})
	return func(timeout time.Duration) error {
		deadline := time.Now().Add(timeout)
		var werr error
		for old.inflight.Load() > 0 {
			if time.Until(deadline) <= 0 {
				werr = ErrRetireTimeout
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if c, ok := old.p.(Closer); ok {
			if err := c.Close(); err != nil && werr == nil {
				werr = err
			}
		}
		return werr
	}, nil
}

func (s *StageSwap) Close() error {
	if c, ok := s.cur.Load().p.(Closer); ok {
		return c.Close()
	}
	return nil
}

func (s *StageSwap) SetContextProvider(fn func() []Message) {
	s.mu.Lock()
	s.ctxProv = fn
	s.mu.Unlock()
	if c, ok := s.cur.Load().p.(interface{ SetContextProvider(func() []Message) }); ok {
		c.SetContextProvider(fn)
	}
}

func (s *StageSwap) SummarizeText(ctx context.Context, system, user string) (string, error) {
	if c, ok := s.cur.Load().p.(interface {
		SummarizeText(context.Context, string, string) (string, error)
	}); ok {
		return c.SummarizeText(ctx, system, user)
	}
	return "", ErrNoSummarizer
}
```

(The retire loop polls at 5 ms off the audio path; a sleep-poll is acceptable here, it runs once per handoff.)

- [ ] **Step 4: Run** `go test -race -count=3 ./core/ -run StageSwap -v` → PASS.

- [ ] **Step 5: Commit** `feat(core): StageSwap replaces a pipeline stage at run time`.

---

### Task 4: `core.Handoff`

**Files:**
- Create: `core/handoff.go`
- Test: `core/handoff_test.go`

**Interfaces:**
- Consumes: `StageSwap` (Task 3), `AsyncTrigger.Deliver`, `AsyncToolResult` (existing), `SessionController`.
- Produces:
  - `type NodeStages struct{ LLM, TTS Processor }`
  - `type BuildNodeFunc func(ctx context.Context, nodeID string) (NodeStages, error)`
  - `type HandoffEvent struct{ Kind HandoffEventKind; From, To string; Err error; Elapsed time.Duration }` with kinds `HandoffCommitted`, `HandoffPreparing`, `HandoffActive`, `HandoffFailed`, `HandoffClosed`
  - `type HandoffConfig struct { Entry string; LLM, TTS *StageSwap; Trigger *AsyncTrigger; Build BuildNodeFunc; Greet func(nodeID string) error; OnEvent func(HandoffEvent); Max int; BuildTimeout, RetireTimeout time.Duration }`
  - `func NewHandoff(cfg HandoffConfig) (*Handoff, error)`
  - `func (h *Handoff) Commit(target string) error` (called from the transfer tool executor)
  - `func (h *Handoff) Active() string`, `func (h *Handoff) CanTransfer() bool` (false once `Max` reached, so the server stops offering transfer tools)
  - `var ErrHandoffPending, ErrHandoffLimit`
- `Greet(nodeID)` adds the user-role "you were just handed this caller" message to the shared history; the server implements it with `ctxPool.AddMessage`.

Behaviour (the contract the tests pin):
1. `Commit(target)` rejects when a handoff is pending (`ErrHandoffPending`) or `Max` committed (`ErrHandoffLimit`); otherwise records `HandoffCommitted` and starts `Build` on `session.Go` with `BuildTimeout`, emitting `HandoffPreparing`.
2. When built, the target is stored as *ready* and an `AsyncToolResult` is delivered to `Trigger`; its `Inject` calls `flip(viaTrigger=true)`.
3. `flip` is idempotent: it runs once per ready target, swaps LLM then TTS (`StageSwap.Replace`), bumps the generation, schedules `retire` of both old stages on `session.Go` (emitting `HandoffClosed`), sets `Active`, emits `HandoffActive`. Via the trigger it also calls `Greet(target)`; if the target was already flipped (a user turn got there first) `Inject` returns `ErrHandoffFlipped` so the trigger starts no extra turn.
4. LLM's `Before` hook: on a downstream `*UserMessageFrame` whose `Turn` does not start with `"async-"`, if a target is ready, `flip(viaTrigger=false)` runs *before* the frame reaches the LLM, so the new node answers the caller and no greeting is added.
5. Build error or timeout → `HandoffFailed`, pending cleared, nothing swapped.

- [ ] **Step 1: Failing tests** (fakes: `probeStage` from Task 3's test file is package-internal and reusable; a `fakeSession` implementing `Go` by `go fn(ctx)`, `IncrementGeneration`, `CurrentGeneration`, `Reenter` recording calls; an `AsyncTrigger` built with `Settle: time.Millisecond, Poll: time.Millisecond`):

```go
func TestHandoffFlipsAtIdleAndClosesOldNode(t *testing.T) {
	h, env := newHandoffEnv(t, nil) // env.oldLLM/oldTTS/newLLM/newTTS are probeStages
	if err := h.Commit("sales"); err != nil {
		t.Fatal(err)
	}
	env.waitActive(t, "sales")
	env.waitClosed(t, env.oldLLM, env.oldTTS) // each closed exactly once
	if env.greeted.Load() != 1 || env.gens.Load() != 1 {
		t.Fatalf("greet=%d gen=%d", env.greeted.Load(), env.gens.Load())
	}
	if env.newLLM.closed.Load() != 0 {
		t.Fatal("the active node must stay open")
	}
}

func TestHandoffUserTurnFlipsOnce(t *testing.T) {
	h, env := newHandoffEnv(t, func(c *HandoffConfig) { c.Trigger = neverIdleTrigger() })
	_ = h.Commit("sales")
	env.waitReady(t) // built, not flipped (trigger never idle)
	_ = env.llm.ProcessFrame(context.Background(), &UserMessageFrame{Turn: "t1"}, Downstream, nil)
	if h.Active() != "sales" || env.newLLM.frames.Load() != 1 || env.oldLLM.frames.Load() != 0 {
		t.Fatalf("the caller's turn must go to the new node: active=%s new=%d old=%d", h.Active(), env.newLLM.frames.Load(), env.oldLLM.frames.Load())
	}
	if env.greeted.Load() != 0 {
		t.Fatal("no greeting when the caller spoke first")
	}
	// the trigger's later Inject must not start a second turn
	if err := env.runPendingInject(); !errors.Is(err, ErrHandoffFlipped) {
		t.Fatalf("want ErrHandoffFlipped, got %v", err)
	}
}

func TestHandoffBuildFailureKeepsOldNode(t *testing.T) {
	h, env := newHandoffEnv(t, func(c *HandoffConfig) {
		c.Build = func(context.Context, string) (NodeStages, error) { return NodeStages{}, errors.New("provider down") }
	})
	_ = h.Commit("sales")
	env.waitEvent(t, HandoffFailed)
	if h.Active() != "front" || env.oldLLM.closed.Load() != 0 {
		t.Fatal("a failed build must leave the old node active and open")
	}
	if err := h.Commit("sales"); err != nil { // pending cleared: a later transfer may try again
		t.Fatalf("commit after failure: %v", err)
	}
}

func TestHandoffPendingAndLimit(t *testing.T) {
	h, env := newHandoffEnv(t, func(c *HandoffConfig) { c.Max = 1; c.Trigger = neverIdleTrigger() })
	_ = h.Commit("sales")
	if err := h.Commit("support"); !errors.Is(err, ErrHandoffPending) {
		t.Fatalf("want ErrHandoffPending, got %v", err)
	}
	env.forceFlip(h)
	if err := h.Commit("front"); !errors.Is(err, ErrHandoffLimit) || h.CanTransfer() {
		t.Fatalf("limit not enforced: %v", err)
	}
}

func TestHandoffBuildTimeout(t *testing.T) {
	h, env := newHandoffEnv(t, func(c *HandoffConfig) {
		c.BuildTimeout = 20 * time.Millisecond
		c.Build = func(ctx context.Context, _ string) (NodeStages, error) {
			<-ctx.Done() // a hung provider: only the timeout ends this
			return NodeStages{}, ctx.Err()
		}
	})
	before := runtime.NumGoroutine()
	_ = h.Commit("sales")
	env.waitEvent(t, HandoffFailed)
	time.Sleep(50 * time.Millisecond)
	if h.Active() != "front" || runtime.NumGoroutine() > before+1 {
		t.Fatalf("active=%s goroutines before=%d after=%d", h.Active(), before, runtime.NumGoroutine())
	}
}
```

`newHandoffEnv` constructs two `StageSwap`s over the old probes, attaches them to the fake session, builds `Handoff`, and wires `Greet` to a counter. Write it at the bottom of the test file (about 60 lines) before Step 2.

- [ ] **Step 2: Run** `go test -race ./core/ -run Handoff -v` → FAIL (undefined).

- [ ] **Step 3: Implement `core/handoff.go`**

```go
package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

var (
	ErrHandoffPending = errors.New("core: a handoff is already in progress")
	ErrHandoffLimit   = errors.New("core: handoff limit reached for this call")
	ErrHandoffFlipped = errors.New("core: handoff already completed by the caller's turn")
)

type HandoffEventKind string

const (
	HandoffCommitted HandoffEventKind = "committed"
	HandoffPreparing HandoffEventKind = "preparing"
	HandoffActive    HandoffEventKind = "active"
	HandoffFailed    HandoffEventKind = "failed"
	HandoffClosed    HandoffEventKind = "closed"
)

type HandoffEvent struct {
	Kind     HandoffEventKind
	From, To string
	Err      error
	Elapsed  time.Duration // preparing→active/failed, monotonic
}

type NodeStages struct{ LLM, TTS Processor }

type BuildNodeFunc func(ctx context.Context, nodeID string) (NodeStages, error)

type HandoffConfig struct {
	Entry         string
	LLM, TTS      *StageSwap
	Trigger       *AsyncTrigger
	Build         BuildNodeFunc
	Greet         func(nodeID string) error // adds the "caller handed to you" message to history
	OnEvent       func(HandoffEvent)
	Max           int           // transfers per call (default 10)
	BuildTimeout  time.Duration // default 10s
	RetireTimeout time.Duration // default 2s
}

// Handoff moves the call between nodes: exactly one node's LLM and TTS exist
// at a time (plus the incoming node while it is being built).
type Handoff struct {
	cfg HandoffConfig

	mu       sync.Mutex
	active   string
	pending  string // committed target, "" when idle
	ready    *NodeStages
	started  time.Time
	count    int
}

func NewHandoff(cfg HandoffConfig) (*Handoff, error) {
	if cfg.LLM == nil || cfg.TTS == nil || cfg.Trigger == nil || cfg.Build == nil || cfg.Entry == "" {
		return nil, errors.New("core: handoff needs LLM, TTS, Trigger, Build and Entry")
	}
	if cfg.Max <= 0 {
		cfg.Max = 10
	}
	if cfg.BuildTimeout <= 0 {
		cfg.BuildTimeout = 10 * time.Second
	}
	if cfg.RetireTimeout <= 0 {
		cfg.RetireTimeout = 2 * time.Second
	}
	h := &Handoff{cfg: cfg, active: cfg.Entry}
	cfg.LLM.SetBefore(h.beforeLLM)
	return h, nil
}

func (h *Handoff) Active() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.active
}

func (h *Handoff) CanTransfer() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.count < h.cfg.Max
}

func (h *Handoff) emit(e HandoffEvent) {
	if h.cfg.OnEvent != nil {
		h.cfg.OnEvent(e)
	}
}

// Commit starts the handoff to target. It returns at once; the node is built
// in the background while the old node finishes its farewell.
func (h *Handoff) Commit(target string) error {
	sc := h.cfg.LLM.Session()
	if sc == nil {
		return ErrSwapNotAttached
	}
	h.mu.Lock()
	switch {
	case h.pending != "":
		h.mu.Unlock()
		return ErrHandoffPending
	case h.count >= h.cfg.Max:
		h.mu.Unlock()
		return ErrHandoffLimit
	}
	from := h.active
	h.pending, h.ready, h.started = target, nil, time.Now()
	h.count++
	h.mu.Unlock()
	h.emit(HandoffEvent{Kind: HandoffCommitted, From: from, To: target})
	h.emit(HandoffEvent{Kind: HandoffPreparing, From: from, To: target})
	if err := sc.Go(func(ctx context.Context) { h.build(ctx, from, target) }); err != nil {
		h.fail(from, target, fmt.Errorf("start build: %w", err))
		return err
	}
	return nil
}

func (h *Handoff) build(ctx context.Context, from, target string) {
	bctx, cancel := context.WithTimeout(ctx, h.cfg.BuildTimeout)
	defer cancel()
	stages, err := h.cfg.Build(bctx, target)
	if err == nil && (stages.LLM == nil || stages.TTS == nil) {
		err = errors.New("build returned no stages")
	}
	if err != nil {
		h.fail(from, target, err)
		return
	}
	h.mu.Lock()
	h.ready = &stages
	h.mu.Unlock()
	h.cfg.Trigger.Deliver(AsyncToolResult{
		Call:   ToolCall{Name: "transfer_to_" + target},
		Inject: func() error { return h.flip(target, true) },
	})
}

func (h *Handoff) fail(from, target string, err error) {
	h.mu.Lock()
	elapsed := time.Since(h.started)
	h.pending, h.ready = "", nil
	h.mu.Unlock()
	h.emit(HandoffEvent{Kind: HandoffFailed, From: from, To: target, Err: err, Elapsed: elapsed})
}

// beforeLLM runs ahead of every frame the LLM stage sees. A caller's own turn
// arriving while a built target waits flips first, so the new node answers.
func (h *Handoff) beforeLLM(f Frame, dir Direction) {
	um, ok := f.(*UserMessageFrame)
	if !ok || dir != Downstream || strings.HasPrefix(um.Turn, "async-") {
		return
	}
	h.mu.Lock()
	target, ready := h.pending, h.ready != nil
	h.mu.Unlock()
	if ready {
		_ = h.flip(target, false)
	}
}

// flip swaps the built target in. It runs at most once per commit. TTS is
// replaced first: if the LLM swap then fails, the call is left on the old
// model with the new voice (degraded but working) and HandoffFailed says so.
func (h *Handoff) flip(target string, viaTrigger bool) error {
	h.mu.Lock()
	if h.pending != target || h.ready == nil {
		h.mu.Unlock()
		return ErrHandoffFlipped
	}
	stages, from, started := *h.ready, h.active, h.started
	h.pending, h.ready, h.active = "", nil, target
	h.mu.Unlock()

	retireTTS, err := h.cfg.TTS.Replace(stages.TTS)
	var retireLLM func(time.Duration) error
	if err == nil {
		retireLLM, err = h.cfg.LLM.Replace(stages.LLM)
	}
	if err != nil {
		h.mu.Lock()
		h.active = from
		h.mu.Unlock()
		h.emit(HandoffEvent{Kind: HandoffFailed, From: from, To: target, Err: err, Elapsed: time.Since(started)})
		return err
	}
	sc := h.cfg.LLM.Session()
	sc.IncrementGeneration() // late frames of the old node are stale
	h.retire(sc, from, retireLLM, retireTTS)
	h.emit(HandoffEvent{Kind: HandoffActive, From: from, To: target, Elapsed: time.Since(started)})
	if viaTrigger && h.cfg.Greet != nil {
		return h.cfg.Greet(target)
	}
	return nil
}

func (h *Handoff) retire(sc SessionController, from string, fns ...func(time.Duration) error) {
	_ = sc.Go(func(context.Context) {
		for _, fn := range fns {
			_ = fn(h.cfg.RetireTimeout)
		}
		h.emit(HandoffEvent{Kind: HandoffClosed, From: from})
	})
}
```

`Greet` runs inside the trigger's `Inject`, so a failed greeting makes the trigger count the result as dropped and start no turn; the caller's next utterance is then answered by the new node.

- [ ] **Step 4: Run** `go test -race -count=3 ./core/ -run 'Handoff|StageSwap' -v` → PASS; also `go vet ./core/`.

- [ ] **Step 5: Commit** `feat(core): Handoff builds the next node on demand and flips at idle`.

---

### Task 5: Server builds nodes, transfer tools, node-filtered executors

**Files:**
- Create: `server/graph_call.go`, `server/graph_call_test.go`
- Modify: `server/call.go` (`callSpec`, `resolveCall`), `server/registry.go` (`CallSpec`, `Build`)

**Interfaces:**
- Consumes: `agent.Graph`, `agent.TransferToolName` (Task 1); `core.Handoff`, `core.StageSwap`, `core.NodeStages` (Tasks 3–4); `runtime.Session` (`Definitions`, `Executor`).
- Produces:
  - `type NodeSpec struct { LLM StageSpec; LLMFallbacks []StageSpec; TTS StageSpec; TTSFallbacks []StageSpec }`
  - `CallSpec.Graph *CallGraph` where `type CallGraph struct { Entry string; Resolve func(ctx context.Context, nodeID string) (NodeSpec, error); Cue *agent.HandoffCue }` (the node display names for the greeting come from `Resolve`'s `NodeSpec.Name`; add `Name string` to `NodeSpec`)
  - `func transferTools(g agent.Graph, nodeID string) []core.ToolDefinition`
  - `func nodeExecutor(inner core.ToolExecutor, allowed map[string]bool, transfer map[string]string, commit func(target string) error) core.ToolExecutor`
  - `func nodeDefinitions(all []core.ToolDefinition, tools []string, ...)` filtering the session's MCP definitions to the node's `tools`

- [ ] **Step 1: Failing tests** (`server/graph_call_test.go`)

```go
func TestTransferToolsFromEdges(t *testing.T) {
	g := twoNodeGraph(t) // front -> sales ("wants to buy")
	defs := transferTools(g, "front")
	if len(defs) != 1 || defs[0].Name != "transfer_to_sales" || !strings.Contains(defs[0].Description, "wants to buy") {
		t.Fatalf("%+v", defs)
	}
	if len(transferTools(g, "sales")) != 0 {
		t.Fatal("sales has no outgoing edges")
	}
}

func TestNodeExecutorRejectsForeignTool(t *testing.T) {
	var ran bool
	inner := core.ToolExecutorFunc(func(context.Context, core.ToolCall) (core.ToolResult, error) { ran = true; return core.ToolResult{Status: core.ToolSucceeded}, nil })
	ex := nodeExecutor(inner, map[string]bool{"mcp_ok": true}, nil, nil)
	res, _ := ex.Execute(context.Background(), core.ToolCall{ID: "1", Name: "mcp_other"})
	if ran || res.Status != core.ToolFailed {
		t.Fatalf("a tool outside the node's list must not run: ran=%v status=%s", ran, res.Status)
	}
}

func TestNodeExecutorTransferCommits(t *testing.T) {
	var got string
	ex := nodeExecutor(nil, nil, map[string]string{"transfer_to_sales": "sales"}, func(target string) error { got = target; return nil })
	res, _ := ex.Execute(context.Background(), core.ToolCall{ID: "1", Name: "transfer_to_sales"})
	if got != "sales" || res.Status != core.ToolSucceeded || !strings.Contains(string(res.Result), "one short sentence") {
		t.Fatalf("got=%q res=%+v", got, res)
	}
}

func TestNodeExecutorTransferRefused(t *testing.T) {
	ex := nodeExecutor(nil, nil, map[string]string{"transfer_to_sales": "sales"}, func(string) error { return core.ErrHandoffPending })
	res, _ := ex.Execute(context.Background(), core.ToolCall{ID: "1", Name: "transfer_to_sales"})
	if res.Status != core.ToolFailed {
		t.Fatalf("a refused transfer must tell the model it failed, got %s", res.Status)
	}
}
```

- [ ] **Step 2: Run** `go test ./server/ -run 'Transfer|NodeExecutor' -v` → FAIL.

- [ ] **Step 3: Implement `server/graph_call.go`**

```go
package server

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"govoice/core"
	"govoice/server/agent"
)

type NodeSpec struct {
	Name         string
	LLM          StageSpec
	LLMFallbacks []StageSpec
	TTS          StageSpec
	TTSFallbacks []StageSpec
}

// CallGraph is what a call needs to move between nodes. Resolve returns a
// node's stage specs with freshly decrypted credentials; the caller wipes them
// once the processors are built.
type CallGraph struct {
	Entry   string
	Resolve func(ctx context.Context, nodeID string) (NodeSpec, error)
	Cue     *agent.HandoffCue
}

func transferTools(g agent.Graph, nodeID string) []core.ToolDefinition {
	edges := g.EdgesFrom(nodeID)
	defs := make([]core.ToolDefinition, 0, len(edges))
	for _, e := range edges {
		to, _ := g.Node(e.To)
		defs = append(defs, core.ToolDefinition{
			Name:        agent.TransferToolName(to.Name),
			Description: "Hand the caller to " + to.Name + ". Use when: " + e.Condition,
			InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
			Policy:      core.ToolPolicy{Timeout: 5 * time.Second},
		})
	}
	return defs
}

const transferOK = `{"status":"transferring","instruction":"Tell the caller in one short sentence that you are connecting them, then stop talking."}`

// nodeExecutor serves one node: its own MCP tools (allowed), and its transfer
// tools (transfer: tool name → target node id). Anything else is refused, so a
// hallucinated call cannot reach another node's tools.
func nodeExecutor(inner core.ToolExecutor, allowed map[string]bool, transfer map[string]string, commit func(target string) error) core.ToolExecutor {
	return core.ToolExecutorFunc(func(ctx context.Context, call core.ToolCall) (core.ToolResult, error) {
		now := time.Now()
		res := func(status core.ToolStatus, body string) core.ToolResult {
			return core.ToolResult{CallID: call.ID, Status: status, Result: json.RawMessage(body), StartedAt: now, EndedAt: time.Now()}
		}
		if target, ok := transfer[call.Name]; ok {
			if err := commit(target); err != nil {
				if errors.Is(err, core.ErrHandoffPending) || errors.Is(err, core.ErrHandoffLimit) {
					return res(core.ToolFailed, `{"error":"transfer_unavailable"}`), nil
				}
				return res(core.ToolFailed, `{"error":"transfer_failed"}`), nil
			}
			return res(core.ToolSucceeded, transferOK), nil
		}
		if !allowed[call.Name] || inner == nil {
			return res(core.ToolFailed, `{"error":"unknown_tool"}`), nil
		}
		return inner.Execute(ctx, call)
	})
}
```

Then wire (each sub-step is its own commit-sized edit):

a. **`callSpec` split.** Extract the credential loop (the `for _, stage := range cfg.Stages()` block) into `func (s *Server) resolveStageSecrets(ctx context.Context, cfg agent.Config, creds []storage.Credential, stages ...string) (map[string][]byte, error)` (skips stages not named). `callSpec` calls it for `stt, llm, tts` with the *entry* config (`graph.EntryNode().Config`, from `agent.DecodeGraph(rs.Snapshot.Version.Config)`). Add `s.nodeSpec(ctx, rs, g, nodeID)` that calls it for `llm, tts` on the node's config and builds `NodeSpec` with `Tools: nodeDefinitions(rs.Definitions, node.Tools)` + `transferTools(g, nodeID)` and `ToolExecutor` set later by `Build` (needs `Handoff.Commit`). `CallSpec.Graph = &CallGraph{Entry: g.Entry, Resolve: func(ctx, id) { return s.nodeSpec(ctx, rs, g, id) }, Cue: g.HandoffCue}` only when `len(g.Nodes) > 1`; a one-node graph leaves `Graph == nil` and the existing path runs unchanged.

b. **`Build` (registry.go ~L498–640).** Wrap the LLM/TTS preparation in two closures defined before `llmProc`:

```go
makeLLM := func(spec StageSpec, fallbacks []StageSpec) (core.Processor, error) {
	spec.History = ctxPool
	fallbacks = slices.Clone(fallbacks)
	if completion != nil {
		spec.SystemPrompt = completionPrompt(spec.SystemPrompt, cfg.Turn.Completion)
		for i := range fallbacks {
			fallbacks[i].SystemPrompt = completionPrompt(fallbacks[i].SystemPrompt, cfg.Turn.Completion)
		}
	}
	if muteTools != nil {
		spec.ToolExecutor = muteTools.Wrap(spec.ToolExecutor, spec.Tools)
		for i := range fallbacks {
			fallbacks[i].ToolExecutor = muteTools.Wrap(fallbacks[i].ToolExecutor, fallbacks[i].Tools)
		}
	}
	if asyncTrigger != nil {
		spec.AsyncSink = asyncTrigger.Deliver
		for i := range fallbacks {
			fallbacks[i].AsyncSink = asyncTrigger.Deliver
		}
	}
	nodeCall := call
	nodeCall.LLMFallbacks = fallbacks
	return r.buildLLM(cfg, nodeCall, spec, build)
}
makeTTS := func(spec StageSpec, fallbacks []StageSpec) (core.Processor, error) {
	nodeCall := call
	nodeCall.TTS, nodeCall.TTSFallbacks = spec, fallbacks
	return r.buildTTS(cfg, nodeCall, build)
}
```

These are the existing lines at `registry.go` ~L498–540 moved verbatim into the closure (the three `if` blocks and the `llmProc, err := r.buildLLM(...)` call), so the existing code path calls them with the entry's spec and a flat agent is byte-for-byte the same pipeline. When `call.Graph != nil`:
- always create `asyncTrigger` (the handoff rides it) even without async tools;
- `llmSwap := core.NewStageSwap(llmProc)`, `ttsSwap := core.NewStageSwap(ttsProc)` replace `llmProc`/`ttsProc` in `procs` (and `llmProc` in the `slices.Index` inserts and `asstCfg.Summary` check);
- build the entry node's executor with `nodeExecutor(..., commit)` where `commit` is `func(t string) error { return handoff.Commit(t) }` (declare `var handoff *core.Handoff` before; assigned after the swaps exist; the closure reads it at call time);
- construct `handoff, err = core.NewHandoff(core.HandoffConfig{Entry: call.Graph.Entry, LLM: llmSwap, TTS: ttsSwap, Trigger: asyncTrigger, Build: buildNode, Greet: greet, OnEvent: r.onHandoff, Max: 10})` where `buildNode` calls `call.Graph.Resolve`, wipes the node secrets after `makeLLM`/`makeTTS`, and returns `core.NodeStages`; `greet` is `func(id string) error { return ctxPool.AddMessage(core.Message{Role: core.RoleUser, Content: "[The caller was just transferred to you (" + names[id] + "). Greet them briefly and continue.]"}) }`.
- Transfer tools are offered only while `handoff.CanTransfer()`; since definitions are fixed per node build, pass `maxTransfers`-aware filtering by checking `CanTransfer()` inside `nodeExecutor`'s commit (returns `ErrHandoffLimit` → `transfer_unavailable`); leaving the tool listed is acceptable.
- `closer`: the pipeline already closes `built` processors on failure; on teardown `StageSwap.Close()` closes only the active node (left nodes are closed by retire).

c. **`r.onHandoff`**: a new `func(core.HandoffEvent)` field set by `BindMetrics`, writing trace spans and timeline events (Task 6).

- [ ] **Step 4: Run** `go test -race ./server/ -run 'Transfer|NodeExecutor' -v` → PASS; then `go test -race ./server/...` → all existing call tests PASS (flat agents: `Graph == nil`).

- [ ] **Step 5: End-to-end test with fakes** `server/graph_handoff_test.go`, modelled on `server/agent_call_test.go`: create a two-node graph agent through the service, run a call with the fake providers (a scripted LLM that calls `transfer_to_sales` on the first user turn, then answers with a node-specific marker string), and assert (a) first reply comes from the front marker, (b) after the transfer the next caller turn is answered with the sales marker, (c) the front node's processors are closed (counter in the fake factory), (d) `sessions.agent_version_id` is unchanged by republishing the agent mid-call.

Run: `go test -race ./server/ -run GraphHandoff -v` → PASS.

- [ ] **Step 6: Commit** `feat(server): graph calls build nodes on demand and hand off through transfer tools`.

---

### Task 6: Timeline `node` field, handoff trace spans, metrics

**Files:**
- Modify: `server/timeline.go`, `server/metrics.go`, `server/registry.go` (`BindMetrics`), `server/server.go` (where `timeline.Record` is called per frame)
- Test: `server/timeline_test.go`, `server/metrics_test.go`

**Interfaces:**
- Produces: `TimelineEvent.Node string \`json:"node,omitempty"\``; event types `EventHandoff`, `EventNodePreparing`, `EventNodeActive`, `EventNodeClosed`, `EventHandoffFailed`; `(*SessionTimeline).SetNode(string)`; metrics `voice_handoffs_total{from,to}`-free form: `voice_handoffs_total`, `voice_handoff_failures_total`, `voice_handoff_seconds` (histogram, commit→active).

(Labels `from`/`to` are node ids, unbounded across tenants; keep the metric unlabelled to respect bounded cardinality. This tightens the spec's metric line; per-node detail lives in the timeline and trace spans.)

- [ ] **Step 1: Failing tests**: (a) `SetNode("sales")` then `Record(TimelineEvent{Type: EventLLMStart})` stamps `Node == "sales"`; (b) `r.onHandoff(core.HandoffEvent{Kind: core.HandoffActive, ...})` increments `voice_handoffs_total` and observes the histogram; (c) a failed event increments the failure counter.
- [ ] **Step 2: Run, expect FAIL.**
- [ ] **Step 3: Implement.** `SessionTimeline` gets `node atomic.Pointer[string]`; `Record` stamps `e.Node` when empty. In `server.go` after the session is created, `registry` `onHandoff` is invoked per call: since `BindMetrics` is process-wide, route per-call events with a per-call closure: change `Build`'s `HandoffConfig.OnEvent` to a function the `CallSpec` provides (`CallSpec.OnHandoff func(core.HandoffEvent)`), set in `server.go` next to `timeline` creation:

```go
call.OnHandoff = func(e core.HandoffEvent) {
	switch e.Kind {
	case core.HandoffActive:
		timeline.SetNode(e.To)
		timeline.Record(TimelineEvent{Type: EventHandoff, Details: e.From + "->" + e.To, DurationMs: e.Elapsed.Milliseconds()})
		timeline.Record(TimelineEvent{Type: EventNodeActive, Details: e.To})
		s.metrics.handoffs.observe(e.Elapsed)
	case core.HandoffPreparing:
		timeline.Record(TimelineEvent{Type: EventNodePreparing, Details: e.To})
	case core.HandoffClosed:
		timeline.Record(TimelineEvent{Type: EventNodeClosed, Details: e.From})
	case core.HandoffFailed:
		timeline.Record(TimelineEvent{Type: EventHandoffFailed, Details: e.To, Error: s.redactor.Redact(e.Err.Error())})
		s.metrics.HandoffFailuresTotal.Add(1)
	}
	if s.mgmt != nil && s.mgmt.traces != nil && call.StoredSessionID != "" { // persisted for session replay
		ev := core.NewTraceEvent("handoff."+string(e.Kind), time.Now(), core.SessionID(call.StoredSessionID), "", 0, core.TracePolicy{},
			map[string]any{"from": e.From, "to": e.To})
		core.StartSpanSafe(context.Background(), s.mgmt.traces, ev).End(e.Err)
	}
}
```

`CallSpec` gains `StoredSessionID string` (set in `callSpec` from `rs.Pin.ID`, a UUIDv7 the trace adapter accepts as the session id, `server/trace/adapter.go:81`). The adapter's attribute filter (`safeKey`) must accept `from` and `to`; if it drops them, rename them to the keys it accepts in both this snippet and the UI's `deriveView`. A test starts a graph call against a store-backed server, hands off once, and asserts `store.Traces().Spans(ctx, sessionID, "")` returns spans named `handoff.preparing` and `handoff.active` with `from`/`to` attributes. The UI reads these `handoff.*` spans (Task 10).

- [ ] **Step 4: Run** `go test -race ./server/ -run 'Timeline|Metrics|Handoff' -v` → PASS; update `docs/observability.md` with the new events and metric names.
- [ ] **Step 5: Commit** `feat(server): handoff timeline events, trace spans and metrics`.

---

### Task 7: Handoff cue (transport underlay, config, timing)

**Files:**
- Create: `transport/ws/cue.go`, `transport/ws/cue_test.go`
- Modify: `transport/ws/transport.go` (~L452, `Config.Underlay`), `transport/ws/pacing.go` (wake on cue), `server/audiofilter.go` (`loadCues`), `server/config.go` (`Audio.Cues`), `server/config_validate.go`, `server/registry.go` (cue timing in Build), `docs/audio-filters.md`

**Interfaces:**
- Produces:
  - `type CueUnderlay struct` implementing `Underlay`; `func NewCueUnderlay(background Underlay) *CueUnderlay`
  - `func (c *CueUnderlay) Start(cur Underlay)` / `Stop(fade time.Duration)` — safe from any goroutine
  - `(*Connection).Cue() *CueUnderlay` (nil when no cues configured)
  - config `audio.cues: {name: {file, gain_db, loop}}`, `func loadCues(cfg *Config, rate int) (map[string]*mixer.Asset, error)`
  - `core` side needs nothing new: the cue controller is a server closure over `Handoff` events (below).

- [ ] **Step 1: Failing tests** (`transport/ws/cue_test.go`):

```go
func TestCueMixesWithAndWithoutBackground(t *testing.T) {
	bg := newTone(t, 1000) // Underlay test double producing a constant sample
	c := NewCueUnderlay(bg)
	dst := make([]byte, 640)
	c.Mix(dst, nil)
	base := sample(dst, 0)
	c.Start(newTone(t, 500))
	c.Mix(dst, nil)
	if sample(dst, 0) != base+500 {
		t.Fatalf("cue must add to the background, got %d", sample(dst, 0))
	}
	c.Stop(0)
	c.Mix(dst, nil)
	if sample(dst, 0) != base {
		t.Fatal("cue must be gone after Stop")
	}
}

func TestCueActiveOnlyWhileSomethingPlays(t *testing.T) {
	c := NewCueUnderlay(nil)
	if c.Active() {
		t.Fatal("no background, no cue: inactive")
	}
	c.Start(newTone(t, 500))
	if !c.Active() {
		t.Fatal("cue must make it active")
	}
}

func TestCueFadeOutHasNoStep(t *testing.T) {
	c := NewCueUnderlay(nil)
	c.Start(newTone(t, 1000))
	c.Stop(40 * time.Millisecond) // 40 ms = 2 frames of 20 ms
	dst := make([]byte, 640)      // 20 ms at 16 kHz mono PCM16
	prev := int16(1000)
	for f := 0; f < 3; f++ {
		c.Mix(dst, nil)
		for i := 0; i+1 < len(dst); i += 2 {
			v := int16(binary.LittleEndian.Uint16(dst[i:]))
			if v > prev || prev-v > 10 { // monotonic, no step bigger than the ramp
				t.Fatalf("frame %d sample %d: %d after %d", f, i/2, v, prev)
			}
			prev = v
		}
	}
	if prev != 0 || c.Active() {
		t.Fatalf("fade must end silent and inactive: last=%d active=%v", prev, c.Active())
	}
}

func TestCueStartWakesIdlePacer(t *testing.T) {
	var wrote atomic.Int64
	cue := NewCueUnderlay(nil)
	cfg := DefaultPacerConfig()
	cfg.EnablePacing = true
	w := NewPacedWriter(cfg, func(context.Context, []byte) error { wrote.Add(1); return nil })
	w.Underlay = cue
	cue.wake = w.wake
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	time.Sleep(50 * time.Millisecond) // the writer is idle: nothing to play
	cue.Start(newTone(t, 500))
	deadline := time.Now().Add(100 * time.Millisecond)
	for wrote.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if wrote.Load() == 0 {
		t.Fatal("a cue started on an idle writer must reach the wire within 100 ms")
	}
}
```

- [ ] **Step 2: Run** `go test -race ./transport/ws/ -run Cue -v` → FAIL.
- [ ] **Step 3: Implement `transport/ws/cue.go`**

```go
package ws

import (
	"sync/atomic"
	"time"
)

// CueUnderlay mixes an optional background with a cue the server starts and
// stops at run time (the handoff gap sound). Mix runs on the paced writer's
// goroutine; Start and Stop may be called from any goroutine.
type CueUnderlay struct {
	bg   Underlay
	cur  atomic.Pointer[cueState]
	wake func() // set by the transport: nudges an idle pacer
}

type cueState struct {
	u        Underlay
	fadeLeft atomic.Int64 // samples; 0 = not fading
	fadeLen  int64
}

func NewCueUnderlay(background Underlay) *CueUnderlay { return &CueUnderlay{bg: background} }

func (c *CueUnderlay) Rate() int {
	if c.bg != nil {
		return c.bg.Rate()
	}
	if s := c.cur.Load(); s != nil {
		return s.u.Rate()
	}
	return 0
}

func (c *CueUnderlay) Active() bool {
	return (c.bg != nil && c.bg.Active()) || c.cueActive()
}

func (c *CueUnderlay) cueActive() bool {
	s := c.cur.Load()
	return s != nil && s.u.Active() && (s.fadeLen == 0 || s.fadeLeft.Load() > 0)
}

func (c *CueUnderlay) Start(u Underlay) {
	c.cur.Store(&cueState{u: u})
	if c.wake != nil {
		c.wake()
	}
}

// Stop ends the cue, fading over fade (0: immediately).
func (c *CueUnderlay) Stop(fade time.Duration) {
	s := c.cur.Load()
	if s == nil {
		return
	}
	if fade <= 0 {
		c.cur.CompareAndSwap(s, nil)
		return
	}
	n := int64(fade) * int64(s.u.Rate()) / int64(time.Second)
	next := &cueState{u: s.u, fadeLen: n}
	next.fadeLeft.Store(n)
	c.cur.CompareAndSwap(s, next)
}

func (c *CueUnderlay) Mix(dst, src []byte) {
	if c.bg != nil && c.bg.Active() {
		c.bg.Mix(dst, src)
		src = dst // the cue adds onto what is already there
	}
	s := c.cur.Load()
	if s == nil || !c.cueActive() {
		if src == nil && (c.bg == nil || !c.bg.Active()) {
			clear(dst)
		} else if c.bg == nil || !c.bg.Active() {
			copy(dst, src)
		}
		return
	}
	s.u.Mix(dst, src)
	if s.fadeLen > 0 {
		fadeSamples(dst, s) // linear ramp over the remaining fade samples
	}
}
```

`fadeSamples` scales each sample of the *cue contribution* — to keep the cue separable from the background, mix the cue into a reused scratch buffer (`scratch []byte` field sized once to `len(dst)`), scale it by `fadeLeft/fadeLen` per sample, then add to `dst`; this replaces the `s.u.Mix(dst, src)` shortcut (write the scratch-based version; the tests above pin the sums). No allocation after the first call.

Wire-up:
- `transport.go`: `Config.Underlay` factory stays; when `Config.Cues` (bool) is set, wrap: `cue := NewCueUnderlay(bgOrNil); pacer.Underlay = cue; conn.cue = cue; cue.wake = pacer.wake`. Add `func (c *Connection) Cue() *CueUnderlay { return c.cue }`.
- `pacing.go`: add `func (w *PacedWriter) wake()` that signals the Run loop's existing wake channel (read the select at `Run` ~L580 and reuse its enqueue-notify channel; the `TestCueStartWakesIdlePacer` test is the guard).
- `server/config.go`: `Audio.Cues map[string]struct{ File string \`yaml:"file"\`; GainDB *float64 \`yaml:"gain_db"\`; Loop *bool \`yaml:"loop"\` }`; `loadCues` mirrors `loadBackground` (same `mixer.LoadAsset`, default gain −30, default loop true); validation: cue names `^[a-z0-9_-]{1,32}$`, at most 8; `agent` validation (Task 2) checks `HandoffCue.Name` against `cfg.Audio.Cues` at call start (an unknown name logs a warning and plays no cue, never fails the call).
- Cue timing, in `Build` next to the `Handoff` construction: when `call.Graph.Cue != nil` and `conn.Cue() != nil`, the `OnEvent` wrapper arms a timer on `HandoffPreparing`:

```go
// after_ms past the old node's silence: start; stop on first new-node audio, caller speech, failure, or 10s.
```

Implementation: on `HandoffPreparing` record `t0`; the cue controller (a small struct in `server/cue.go`) is fed `Observe(frame)` from the existing `session.OnFrame` hook (bot audio frames, `UserStartedSpeakingFrame`). It starts the cue when (a) the handoff is pending, (b) no bot audio for `after_ms` (default 300 ms, monotonic `time.Since`), and stops it (30 ms fade) on the first `OutputAudioFrame` after `HandoffActive`, on `UserStartedSpeakingFrame`, on `HandoffFailed`, and via `time.AfterFunc(10*time.Second, stop)`. It records `cue_start`/`cue_stop` timeline events. Its state is guarded by one mutex (no I/O under it) and its timer is stopped in the session teardown.
- [ ] **Step 4: Run** `go test -race ./transport/ws/ ./server/ -run 'Cue' -v` → PASS (`TestCueStopsOnEveryExit` in `server/cue_test.go` drives the controller with fake frames: first audio, barge-in, failure, hard cap each stop it exactly once; a fast handoff under `after_ms` never starts it).
- [ ] **Step 5: Docs + commit.** Document `audio.cues` and `handoff_cue` in `docs/audio-filters.md`. Commit `feat: handoff cue plays in the handoff gap`.

---

### Task 8: API compatibility and docs

**Files:**
- Modify: `docs/api/v1.md`, `server/api_agent_boundary_test.go`
- Test: same

- [ ] **Step 1: Failing boundary tests** (in the existing boundary test style): (a) `POST /api/v1/agents` with a graph body returns 201 and `current_version.config` echoes `nodes`; (b) `POST` with the old flat body still 201 and its config unchanged; (c) a graph with an edge to a missing node → 400 with `invalid agent config`; (d) `PUT` a new graph publishes version 2 and a call already pinned to version 1 keeps its graph.
- [ ] **Step 2: Run** `go test ./server/ -run AgentBoundary -v`; (a)(c)(d) FAIL if any service path was missed in Task 2 — fix those paths (grep for remaining `DecodeConfig(` uses) rather than special-casing the handler.
- [ ] **Step 3:** document the graph shape (`entry`, `nodes[].{id,name,config,tools,position}`, `edges[]`, `handoff_cue`) and the flat-config compatibility rule in `docs/api/v1.md`.
- [ ] **Step 4: Run** `go test -race ./server/...` → PASS. **Step 5: Commit** `docs/test: graph payloads on the agents API`.

---

### Task 9: Designer UI — model, canvas, inspector

**Files:**
- Modify: `ui/package.json` (`@xyflow/react` pinned to an exact version at least two weeks old; run `npm view @xyflow/react version` and pin the exact string returned), `ui/src/api/client.ts` (graph zod schema and DTO)
- Create: `ui/src/routes/agents/graph/model.ts`, `model.test.ts`, `Canvas.tsx`, `Canvas.test.tsx`, `Inspector.tsx`
- Modify: `ui/src/routes/agents/AgentsView.tsx` (edit mode renders the canvas; the existing `AgentForm` becomes the inspector body — extract it into `Inspector.tsx` with the same props, keeping `AgentsView.test.tsx` green)

**Interfaces:**
- Produces (`model.ts`, pure, no React):

```ts
export type GraphNode = { id: string; name: string; config: AgentConfigV2; tools: string[]; position: { x: number; y: number } };
export type GraphEdge = { id: string; from: string; to: string; condition: string };
export type GraphDraft = { entry: string; nodes: GraphNode[]; edges: GraphEdge[]; handoff_cue?: { name: string; after_ms?: number } };
export const MAX_NODES = 8, MAX_EDGES = 32;
export function fromAgentConfig(raw: unknown): GraphDraft;          // flat config → one node
export function addNode(g: GraphDraft, name: string): GraphDraft;
export function removeNode(g: GraphDraft, id: string): GraphDraft;  // drops its edges; entry cannot be removed
export function connect(g: GraphDraft, from: string, to: string, condition: string): GraphDraft;
export function setEntry(g: GraphDraft, id: string): GraphDraft;
export function validate(g: GraphDraft): string[];                  // same rules as server/agent/graph.go
export function toPayload(g: GraphDraft): unknown;                  // omits node config STT on non-entry nodes
```

- [ ] **Step 1: Failing tests** (`model.test.ts`): flat config becomes one node with `entry` equal to its id; `addNode` stops at `MAX_NODES`; `removeNode` removes incident edges and refuses the entry; `connect` rejects self-edges, duplicates and empty conditions; `validate` reports duplicate slugs (`"Sales Team"` vs `"sales team"`); `toPayload` of a one-node draft round-trips through `fromAgentConfig`.
- [ ] **Step 2: Run** `cd ui && npx vitest run src/routes/agents/graph/model.test.ts` → FAIL.
- [ ] **Step 3: Implement `model.ts`** (pure functions mirroring the Go rules; node ids from `crypto.randomUUID().slice(0, 8)`), the zod `agentGraphSchema` in `client.ts` (`nodes` array of `{id,name,config,tools,position}`, `edges`, optional `handoff_cue`), and make `agentEnvelope` accept both flat and graph `current_version.config` (`z.union`).
- [ ] **Step 4: Canvas.** `Canvas.tsx` renders `<ReactFlow nodes edges nodeTypes onConnect onNodesChange …>`; custom node shows name, LLM model, TTS voice, tool count, and an "entry" badge; edges render `condition` as the label; selecting a node/edge opens `Inspector` (node → the extracted agent form; edge → a single textarea). `Canvas.test.tsx` (testing-library): add node via the toolbar button, connect two nodes by calling the `onConnect` handler prop, type a condition, press Publish → the client mock receives `toPayload(...)` containing two nodes and one edge; Publish is disabled with the `validate` errors listed.
- [ ] **Step 5: Run** `cd ui && npm run typecheck && npx vitest run` → PASS. **Step 6: Commit** `feat(ui): agent designer canvas`.

---

### Task 10: Live graph view and replay

**Files:**
- Create: `ui/src/routes/agents/graph/activity.ts`, `activity.test.ts`, `LiveGraph.tsx`, `LiveGraph.test.tsx`
- Modify: `ui/src/routes/agents/AgentVoiceTest.tsx` (render `LiveGraph` beside the canvas when the agent has more than one node), `ui/src/routes/sessions/*` session detail (replay)

**Interfaces:**
- Produces (`activity.ts`, pure):

```ts
export type NodeState = "idle" | "preparing" | "active" | "visited";
export type Activity = "listening" | "thinking" | "speaking" | "tool" | "interrupted" | "error" | null;
export type GraphView = {
  nodes: Record<string, { state: NodeState; activity: Activity }>;
  traversed: { edgeId: string | null; from: string; to: string; at: string }[]; // in order
  trail: { node: string; since: string }[];
};
export function deriveView(entry: string, spans: TraceSpanDTO[], edges: GraphEdge[]): GraphView;
```

`deriveView` reads spans named `handoff.preparing | handoff.active | handoff.failed | handoff.closed` (attributes `from`, `to`) ordered by `started_at`, finds the edge id by `(from,to)`, and takes the active node's `activity` from the latest span/event of the current turn (`llm`/`tts`/tool spans already in `SessionTracesDTO`). Unknown span shapes are ignored.

- [ ] **Step 1: Failing tests** (`activity.test.ts`): (a) no handoff spans → entry `active`, others `idle`; (b) `preparing` then `active` → target `preparing` then `active`, old node `visited`, one traversed edge with the matching edge id; (c) `failed` after `preparing` → target back to `idle`, old node still `active`; (d) spans out of order are sorted; (e) 8 handoffs produce a trail of 9 entries in order.
- [ ] **Step 2: Run, expect FAIL. Step 3: Implement** `deriveView`; `LiveGraph.tsx` renders the same read-only canvas as Task 9 with state classes (`active` highlighted, `preparing` pulsing, `visited` dimmed, traversed edge animated with its condition), the trail list (clicking a trail entry filters the transcript below to that node's turns), and polls `GET /sessions/{id}/traces` every 500 ms while the voice test call is active (stops on disconnect), through TanStack Query's `refetchInterval` (no new transport).
- [ ] **Step 4: `LiveGraph.test.tsx`**: a mocked query returning three successive trace payloads moves the highlight front→preparing sales→active sales; the edge element gets the animated class on `handoff.active`; the replay mode (`sessionId` of a finished session, no polling) renders the final path.
- [ ] **Step 5: Run** `cd ui && npm run typecheck && npx vitest run && npm run build` → PASS. **Step 6: Commit** `feat(ui): live graph view and session replay`.

---

### Task 11: Live check scenario S14

**Files:**
- Modify: `tools/livecheck/agent.go` (`agentConfig` for a two-node profile), `tools/livecheck/scenarios.go` (S14 entry near S13 at L209, `s14` near `s13` at L763), `.github/workflows/*nightly*` (no change unless it lists scenario ids)

- [ ] **Step 1:** Add `graphAgentConfig(p Profile, cred map[string]string) map[string]any`: a `front` node (existing config, prompt: "Greet the caller. If they ask about prices or buying, call transfer_to_sales.") and a `sales` node (same providers, a different TTS voice from the profile if it has a second, prompt: "You are Sales. Say the word 'catalogue' in your first sentence."), one edge `front→sales`, `handoff_cue` omitted.
- [ ] **Step 2:** `s14`: connect, send the `question` clip variant that asks to buy, then assert with `r.check`: reply 1 completes; after the handoff the next reply contains "catalogue" (STT of the bot audio or the transcript from the server timeline `handoff` event); `handoff` timeline event present; handoff latency metric recorded via `r.m("handoff_ms", …)`. Use the same `client` helpers as `s13`. Mark `class: classHandoff` and add it to the nightly list only after one manual run passes.
- [ ] **Step 3: Run** `go build ./tools/livecheck/ && go vet ./tools/livecheck/`; with provider keys: `go run ./tools/livecheck -scenario S14 -profile google` (manual, 2–3 provider calls, inside the 60-call cap). Record the measured commit→first-audio latency and whether any provider rejected the cross-node tool history (the open risk). If one did, add the plain-text replacement for that provider in `core` history and a test, in a follow-up task before merging.
- [ ] **Step 4: Commit** `test(livecheck): S14 two-node handoff`.

---

### Task 12: Verification, docs, review

- [ ] **Step 1:** `gofmt -w . && make verify` → PASS. `go test -race ./core/... ./server/... ./transport/...` → PASS. `cd ui && npm run typecheck && npx vitest run && npm run build` → PASS.
- [ ] **Step 2:** Update `docs/pipecat-parity-matrix.md` (a Pipecat Flows row: graph agents, handoff by tool call, per-node voice) and `README.md` feature list.
- [ ] **Step 3: Independent adversarial review** (AGENTS.md gate): dispatch a fresh reviewer with this plan, the spec and `git diff main...HEAD`; it must explicitly report on: YAGNI/bloat (is every `core` addition consumed by `server/`), races (`StageSwap`, `Handoff.mu`, cue atomics), goroutine leaks (build, retire, cue timer), monotonic timing, `recover` on transport callbacks, and the seven Review Focus lines. Fix findings, re-run Step 1.
- [ ] **Step 4:** Clean `git status`; final commit `docs: agent graph parity row and README`.
