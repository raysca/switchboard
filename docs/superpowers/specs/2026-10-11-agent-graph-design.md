# Agent Graph (Multi-Agent Handoff) Design

**Date:** 2026-10-11
**Status:** scope approved in conversation; awaiting written-spec review.

## Intent

An operator designs a voice agent as a graph on a canvas: nodes are agents (prompt, tools, LLM, voice), edges are handoffs ("when the caller wants to buy, go to Sales"). A front agent greets and routes; a sales agent takes over mid-call. The operator builds and tests the whole graph in one designer view. Success:
- a two-node graph (front, sales) can be drawn, published and called end to end with the stock Pipecat web client;
- a handoff swaps prompt, tools, LLM and TTS voice at a turn boundary, keeps the conversation history, and honours barge-in and generation fencing;
- a call is pinned to one immutable published graph, so editing or republishing never changes a live call;
- every existing single-agent agent and API client keeps working unchanged.

Decisions made with the user on 2026-10-11:
1. **Handoffs are LLM tool calls**; the edge condition is the tool description.
2. **A handoff may change the LLM config and the TTS voice or provider.** STT is fixed for the call.
3. **Providers live in the node inspector,** not as separate canvas nodes.
4. **Owned nodes, no shared library.** Each graph defines its own nodes; there is no cross-graph agent reuse.
5. **The canvas is the only way to author agents.** The graph *is* the agent: no `flows` table.

## Current behaviour (main at e5c2259)

- An agent has immutable versions. `agent_versions.config_json` holds one `agent.Config` (STT, LLM, TTS, context limits, failover, recording; `server/agent/config.go`). `agents.current_version_id` points at the latest.
- A call pins one `agent_version_id`: `StartSession` resolves the current version and writes a `sessions` row; a DB trigger checks that the row's provenance matches (`001_initial.sql`). Later edits only move the pointer.
- MCP tool selections are rows in `agent_mcp_tools`, keyed by `agent_version_id`. Readiness and runtime capture are resolved at call start (`server/agent/service.go`).
- `callSpec` turns a pinned config into a `CallSpec` (`server/call.go`), and `Registry.Build` builds one pipeline: STT, LLM (wrapped by `LLMFailover`), TTS. The system prompt is baked into the LLM processor when it is built (`server/registry.go:219`).
- `schema_version` is stored on every version but is always `1`.
- There is no handoff, transfer or node concept in `core/` or `server/`.

## Design

### 1. Data model: the graph is the agent version

`agent_versions.config_json` at **schema_version 2**:

	{
	  "entry": "front",
	  "nodes": [
	    {"id": "front", "name": "Front desk", "config": { ...today's agent.Config... }, "position": {"x": 0, "y": 0}},
	    {"id": "sales", "name": "Sales",      "config": { ... }}
	  ],
	  "edges": [
	    {"id": "e1", "from": "front", "to": "sales", "condition": "The caller wants pricing or to buy."}
	  ]
	}

- Each node's `config` is the existing `agent.Config`, unchanged, so its validation, failover and credential logic are reused.
- Call-level settings that cannot change mid-call (STT, VAD stop, push-to-talk, recording, context limits, `async_tools`) are read from the **entry node** only. A non-entry node's STT, VAD, recording and context settings are ignored and rejected by validation if set, so the designer never shows a field that does nothing.
- `position` is canvas layout only and is excluded from the config hash (otherwise dragging a node would publish a new behavioural version).
- **Compatibility.** Reads wrap a schema-1 config as a one-node graph with no edges; writes of the flat shape (existing `/api/v1/agents` clients, existing tests) are accepted and wrapped the same way. No data migration; old rows stay at schema 1.
- A one-node graph is a plain single agent, so `server/call.go` has one call path.

**Validation** (in `server/agent`, run on create and update): exactly one entry that exists; node ids unique and slug-safe; names unique after slugging (they become tool names `transfer_to_<slug>`); every edge endpoint exists; no self-edge; no duplicate (from, to) pair; non-empty condition; at most 8 nodes and 32 edges (bounded, per the project's resource rules); every node's `config` passes the existing config validation.

### 2. Storage

- `agent_mcp_tools` gains `node_id TEXT NOT NULL DEFAULT ''` and the primary key becomes `(agent_version_id, node_id, mcp_server_id, remote_name)`. Schema-1 rows keep `node_id = ''`, read as the sole node. Migration for sqlite and postgres.
- `sessions` is unchanged. Its `agent_version_id` is the pinned graph; its provenance trigger still holds.
- Readiness (`agentReadiness`) and runtime capture cover the **union of all nodes' tools** and all nodes' credentials. A graph is ready only if every node is ready, so a handoff can never fail on resolution mid-call.

### 3. Runtime: swapping the active node

The core mechanism lives in `core/` and knows nothing about servers or graphs: a **stage switch**, a processor that holds N pre-built stage processors (LLM or TTS) and forwards frames to whichever is active.

- **Pre-build.** At call start the server builds every node's LLM chain (including that node's own failover wrapper and its system prompt with the turn-completion suffix) and every node's TTS. Only the active ones receive frames. Building at call start, rather than at handoff, keeps handoff latency off the audio path. Cost: up to 8 LLM and TTS processors per call. Provider connections must therefore be opened lazily by each processor, not at build time; the plan must confirm this per provider and keep the call's resource use bounded.
- **Shared history.** All nodes share the one context pool, so history carries across. System prompts are not part of the history, so nothing needs rewriting.
- **Handoff tool.** For the active node, the server injects one tool per outgoing edge, `transfer_to_<slug>`, whose description is the edge condition, next to the node's own tools. The tool is **terminal**: the old node's tool loop does not run a follow-up round.
- **Flip.** When a transfer tool executes, the switch records the target. The flip happens at the end of the old node's turn, once any bridge sentence it already produced has been spoken or interrupted. The new node's LLM then starts a turn on the shared history (it greets, or answers the caller's interruption), reusing the existing async-trigger mechanism (`core/async_trigger.go`) that starts a model turn once the call is idle.
- **Generation fencing.** The flip bumps the generation, so late frames from the old node's LLM or TTS are dropped like any interrupted turn. Barge-in during a pending flip does not cancel the handoff (the tool call already committed); the new node answers the interruption.
- **Voice change.** The TTS switch flips with the LLM switch. Text already queued for the old voice is flushed or interrupted by the generation bump, never spoken in the new voice.
- **Failover** is unchanged within a node; a node's fallback chain is its own.
- **Loops.** Edges may form cycles (Sales back to Front). A per-call cap on handoffs (default 10) ends a ping-pong by removing transfer tools from then on.
- **Tool history across nodes.** Past tool calls from another node stay in the shared history as call/result pairs. Whether each provider accepts history containing tools absent from the current tool list is an open risk: verify with the live provider tests; if one rejects it, replace the other node's tool exchanges with plain text for that provider.

### 4. Server surface

- `/api/v1/agents` accepts and returns the graph config (schema 2) as well as the flat one. A `PUT` publishes a new version atomically (one row plus its tool rows), so a half-published graph is impossible.
- Observability: each handoff is a timeline event (`handoff`: from node, to node, edge id, agent version), recorded on the same call timeline. Turn rows carry the node id so transcripts show which agent spoke. Metrics: `voice_handoffs_total{from,to}` and a handoff-latency histogram.
- Existing readiness and the "agent not ready" error path are reused; the not-ready reason names the node.

### 5. Designer UI

- New route replacing the agent edit form: a canvas with a node inspector, plus the voice test beside it. Library: React Flow (`@xyflow/react`); hand-building drag, zoom and edge routing is not justified.
- Nodes show name, LLM, voice and tool count. Edges show their condition as a label. The entry node is marked and can be changed.
- The inspector for a selected node is today's agent form (`AgentsView.tsx`) moved into a side panel; an edge selection shows the condition editor.
- A new agent starts as a single entry node, so the single-agent case feels like today's form.
- **Test panel.** The existing voice test (`AgentVoiceTest.tsx`) runs against the draft or published graph and highlights the active node live, driven by the `handoff` timeline events.
- Publish is an explicit action; unpublished canvas edits are held client-side and validated with the server's rules.

## Testing

- **core:** stage switch unit tests under `-race`: frames reach only the active stage; flip at turn end; generation fencing drops late old-node frames; barge-in during a pending flip; handoff cap; no goroutine leaks on close.
- **server:** graph validation table tests; schema-1 wrapping; tool-row migration on both dialects; readiness fails with the node named; atomic publish; session pins the graph and a later publish does not change a live call; two-node scripted-LLM call end to end using the fake providers (`server/agent_call_test.go` style).
- **ui:** vitest for canvas validation, add/remove node and edge, inspector editing, publish payload.
- **live:** a new livecheck scenario in the nightly critical path (two nodes, one handoff, different voices) against google and xai, within the existing 60-call cap. This is also where the tool-history risk above gets answered.
- Verification gate per AGENTS.md: `gofmt -w .`, `make verify`, independent adversarial review including the YAGNI audit.

## Out of scope

Shared or reusable agents across graphs; STT or language switching mid-call; deterministic (non-LLM) edge conditions; nested graphs; per-node recording or VAD settings; A/B or traffic-split between graph versions.

## Risks

- **Pre-building every node's stages** may open too many provider connections per call. Mitigation: lazy connection in each processor and the 8-node cap; measure in the plan's first task.
- **Cross-node tool history** may be rejected by some providers (see section 3).
- **Handoff latency** (tool call, old-node drain, new-node first token) adds a visible gap; the bridge sentence from the old node is the mitigation, and the timeline events make the gap measurable.
- **Replacing the agent form** with a canvas is a UI-wide change; the one-node case must stay as quick to use as the form.
