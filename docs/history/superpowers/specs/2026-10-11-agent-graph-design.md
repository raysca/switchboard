> **Historical** (moved to docs/history by Phase 8, 2026-10-21): written for the design at the time; it may name packages, files, endpoints and targets that no longer exist. Paths inside are as written then; see docs/history/README.md.

# Agent Graph (Multi-Agent Handoff) Design

**Date:** 2026-10-11
**Status:** scope approved in conversation; revised 2026-10-11 after review (one active node, no pre-build; live graph visualisation); awaiting written-spec review.

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
6. **Exactly one node is active at a time.** Only the active node's LLM and TTS exist; a node is built when it is activated and closed when it is left. Nothing is pre-built.
7. **The designer shows the call moving through the graph,** live while testing and as a replay of past sessions.
8. **A handoff cue covers the gap.** A tone or bed sound plays while the call is between agents, so the caller never hears dead air.

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

### 3. Runtime: one active node

Think of the call as a token moving through the graph: exactly one node holds it. The core mechanism lives in `core/` and knows nothing about servers or graphs: a **node host**, a processor that owns the single active node's stage processors (LLM chain and TTS) and forwards frames to them. The server hands it a `build(nodeID)` function; the host calls it on activation and closes what it replaced.

- **Activate on demand.** Call start builds only the entry node. A node's LLM chain (with its own failover wrapper and its system prompt with the turn-completion suffix) and TTS are built when the node is activated and `Close()`d when it is left. Provider connections are therefore never open for nodes the call is not in. Peak per call: one active node, plus the incoming node during a handoff (below).
- **Shared state lives outside nodes.** The context pool (history), STT, VAD, turn manager, transport and recording are call-level. A node holds no conversation state, so closing it loses nothing; system prompts are not part of the history, so nothing needs rewriting.
- **Handoff tool.** For the active node, the server injects one tool per outgoing edge, `transfer_to_<slug>`, whose description is the edge condition, next to the node's own tools. The tool is **terminal**: the old node's tool loop does not run a follow-up round.
- **Commit, build, flip.** When a transfer tool executes the handoff is committed: the host starts building the target node *immediately* (state `preparing`), concurrently with the old node finishing any bridge sentence it already produced. This hides the target's connection time (a TTS connect is the slow part) behind speech the caller is already hearing. The flip happens once both are done: the old node has drained or been interrupted, and the target is built. Then the old node is closed, the generation is bumped, and the new node's LLM starts a turn on the shared history (it greets, or answers the caller's interruption), reusing the existing async-trigger mechanism (`core/async_trigger.go`). If building serially after the drain proves fast enough in the live tests, the overlap is dropped and the call never holds more than one node.
- **Closing a left node.** Closing cancels the node's in-flight generation (the generation bump already makes its late frames droppable), then calls `Close()` on its processors. Text still queued for the old voice is flushed or interrupted by the bump, never spoken in the new voice. Closing runs on the session supervisor (`session.Go`) and is bounded by a timeout so a hung provider cannot stall the call.
- **Build failure.** If the target cannot be built (provider outage; credentials were checked at call start), the flip does not happen: the old node stays active, a `handoff_failed` event is recorded, and the call continues. The caller may have heard "let me transfer you"; the next caller turn is answered by the old node. Within a node, the existing failover chain still applies.
- **Barge-in during a pending handoff** does not cancel it (the tool call already committed); the new node answers the interruption.
- **Loops.** Edges may form cycles (Sales back to Front). A per-call cap on handoffs (default 10) ends a ping-pong by removing transfer tools from then on. Re-entering a node builds it again; nothing is cached.
- **Tool history across nodes.** Past tool calls from another node stay in the shared history as call/result pairs. Whether each provider accepts history containing tools absent from the current tool list is an open risk: verify with the live provider tests; if one rejects it, replace the other node's tool exchanges with plain text for that provider.

### 3a. Handoff cue (tone or bed)

The gap the caller hears is from the end of the old node's audio to the first audio of the new node (build, LLM first token, TTS first byte). A cue fills it.

- **Reuse the output underlay.** The paced writer already mixes a per-session `Underlay` (`mixer.Cursor`) under speech and emits it alone as 20 ms non-speech frames while the queue is empty (`transport/ws/pacing.go`); those frames are uninterruptible, ignored by recording and heard-text, and never change speaking state. A cue is the same kind of audio, so no new mixing path is needed.
- **Runtime control.** Today the underlay is fixed per session at connect (`PacerConfig.Underlay`, a factory in `server/audiofilter.go`). The one change in `transport/ws`: the session underlay becomes a small wrapper that mixes the background (if any) with an optional *cue cursor* that the server can start and stop at runtime. The stop is a short fade (reusing the mixer's gain helpers) so it never clicks.
- **Sounds are operator-provided files,** loaded once at startup like the background (`mixer.LoadAsset`: PCM16 WAV, size-capped, resampled once, gain baked in). Server config gains a named set, `audio.cues: {name: {file, gain_db}}`. The graph refers to one by name. Nothing is uploaded through the API.
- **Graph setting.** The entry node's config (call-level) gains `handoff_cue: {name, after_ms}`; empty means no cue. Tone versus bed is just which file the operator supplies (a short chime plays once; a looping bed plays until stopped), through the existing `loop` behaviour of the cursor, so there is no separate mode field.
- **When it plays.** The host starts the cue when the old node's audio has drained and `after_ms` (default 300 ms) has passed without the new node producing audio. A handoff that completes sooner plays no cue, so fast handoffs stay silent instead of getting a clipped sound. It stops, with the fade, on the new node's first audio frame, on barge-in (the caller speaking), on `handoff_failed`, and after a hard cap (default 10 s) so a stuck handoff cannot play forever.
- **Events.** The timeline records `cue_start` and `cue_stop` (with the reason), and the live graph view shows a cue as a badge on the traversed edge. Handoff latency is measured both with and without the cue, so the cue never hides a regression.
- **Why not the old node's speech instead?** The old node's bridge sentence is still the first mitigation, since the model can say it for free; the cue covers whatever gap remains after it.

### 4. Server surface

- `/api/v1/agents` accepts and returns the graph config (schema 2) as well as the flat one. A `PUT` publishes a new version atomically (one row plus its tool rows), so a half-published graph is impossible.
- Observability: `TimelineEvent` (`server/timeline.go`) gains a `node` field, set on every event the active node produces, so the existing `llm_*`, `tts_*`, `interruption` and `error` events say which node they came from. New event types: `handoff` (from, to, edge id), `node_preparing`, `node_active`, `node_closed` and `handoff_failed`. Turn rows carry the node id so transcripts show which agent spoke. Metrics: `voice_handoffs_total{from,to}` and a handoff-latency histogram (commit to first audio from the new node).
- Existing readiness and the "agent not ready" error path are reused; the not-ready reason names the node.

### 5. Designer UI

- New route replacing the agent edit form: a canvas with a node inspector, plus the voice test beside it. Library: React Flow (`@xyflow/react`); hand-building drag, zoom and edge routing is not justified.
- Nodes show name, LLM, voice and tool count. Edges show their condition as a label. The entry node is marked and can be changed.
- The inspector for a selected node is today's agent form (`AgentsView.tsx`) moved into a side panel; an edge selection shows the condition editor.
- A new agent starts as a single entry node, so the single-agent case feels like today's form.
- **Live graph view (debugging while authoring).** The test panel sits beside the canvas and runs the draft or published graph. The canvas is driven by the call's timeline events, nothing new on the wire beyond section 4:
  - the active node is highlighted, with its live activity derived from the events: *listening* (`speech_start` to `speech_stop`), *thinking* (`llm_start` to `llm_first_token`), *speaking* (`tts_audio`), *tool running*, *interrupted*, *error*;
  - a node being built shows *preparing*; a node that was left is dimmed and marked visited;
  - when a handoff commits, the traversed edge animates and shows its condition and the moment of the flip;
  - a trail lists the nodes visited with timestamps, and selecting a trail entry or a node filters the transcript and timeline to that node;
  - the same view replays a past session from its stored timeline, so a bad handoff can be debugged after the call.
- Publish is an explicit action; unpublished canvas edits are held client-side and validated with the server's rules.

## Testing

- **core:** node host unit tests under `-race`: frames reach only the active node; the left node is closed exactly once and its processors are not used after the flip; at most two nodes exist during a handoff and one otherwise (count builds and closes); generation fencing drops late old-node frames; barge-in during a pending flip; failed build keeps the old node active; hung close is bounded; handoff cap; no goroutine leaks.
- **server:** graph validation table tests; schema-1 wrapping; tool-row migration on both dialects; readiness fails with the node named; atomic publish; session pins the graph and a later publish does not change a live call; two-node scripted-LLM call end to end using the fake providers (`server/agent_call_test.go` style).
- **audio/transport:** the cue wrapper under `-race`: cue mixes with and without a background; start and stop at runtime; fade on stop; cue frames stay non-speech (not recorded, no speaking state); a cue shorter than the gap loops, a one-shot ends cleanly; the hard cap stops it.
- **server:** the host plays no cue when the new node's audio arrives before `after_ms`, and stops it on first audio, barge-in and failure.
- **ui:** vitest for canvas validation, add/remove node and edge, inspector editing, publish payload, and the live view (a scripted event stream moves the highlight, animates the edge on `handoff`, shows `preparing`, dims left nodes, and a stored timeline replays the same path).
- **live:** a new livecheck scenario in the nightly critical path (two nodes, one handoff, different voices) against google and xai, within the existing 60-call cap. This is also where the tool-history risk above gets answered.
- Verification gate per AGENTS.md: `gofmt -w .`, `make verify`, independent adversarial review including the YAGNI audit.

## Out of scope

Per-edge cues or uploading cue sounds through the API; shared or reusable agents across graphs; STT or language switching mid-call; deterministic (non-LLM) edge conditions; nested graphs; per-node recording or VAD settings; A/B or traffic-split between graph versions.

## Risks

- **Handoff latency** now includes building the target node. The overlap with the old node's bridge sentence hides most of it; the commit-to-first-audio histogram measures it, and the live test sets the budget.
- **A cue sounds worse than silence** if it starts late or cuts abruptly; the `after_ms` threshold and the fade are the guards, and the live test listens to a recording of one handoff.
- **Cleanup on a left node:** a processor whose `Close()` does not stop its goroutines would leak per handoff. The node host tests count goroutines; the plan audits each provider's `Close()`.
- **Cross-node tool history** may be rejected by some providers (see section 3).
- **Replacing the agent form** with a canvas is a UI-wide change; the one-node case must stay as quick to use as the form.

## Revisions made while planning and building (2026-10-11)

Reading the code and reviewing the branch changed these details; the design is otherwise as approved above.
1. **No `node_id` column, no migration.** MCP tools stay selected per agent version (`agent_mcp_tools` is unchanged). Each node lists the tools it exposes in `tools: ["<mcp_server_id>/<remote_name>"]` (the same reference form as `async_tools`). Readiness and runtime capture still cover the union. A flat agent has no `tools` list and exposes every selected tool; the designer seeds the entry node's `tools` from the version's selected tools when it turns a flat agent into a graph, so adding a node does not remove them.
2. **Graphs are detected by shape (a top-level `nodes` key); `schema_version` stays 1.** Layout positions live in the config and are hashed: publishing is an explicit action, so a layout change publishes a new version. Node ids match `^[A-Za-z0-9_-]{1,32}$` and `handoff_cue.name` matches `^[a-z0-9_-]{1,32}$` with `after_ms` 0 to 10000, because they reach logs, trace attributes and the UI.
3. **The transfer tool is not special-cased inside the tool loop.** Its result tells the model to say one short sentence and stop; that sentence is the bridge line. The flip then happens when the call is idle (riding `AsyncTrigger`), or just before the caller's next turn reaches the model. An idle flip bumps the generation; a flip at the caller's turn does not, because that turn already fenced the old node and the new node's reply must carry its generation.
4. **Credentials are resolved when a node is activated,** not held for the whole call.
5. **Live view data** comes from `handoff.*` trace spans (polled through the session traces API) for which node holds the call, and from the voice client's speaking state for what it is doing. Replay of a finished session on the session page is not built (the API cannot return the pinned version's graph).
6. **Handoff metrics are unlabelled** (`voice_handoffs_total`, `voice_handoff_failures_total`, `voice_handoff_seconds`): node ids are tenant data, so labels would have unbounded cardinality. Per-node detail lives in timeline events (every event carries `node` once a handoff has happened) and trace spans.
7. **A node that is built but never installed is closed** when the call ends or the flip fails (`Handoff.Close`, registered on the LLM stage).
