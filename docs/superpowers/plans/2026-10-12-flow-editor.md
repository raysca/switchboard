# Flow Editor Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the agent graph designer with one flow editor where the graph never leaves the screen: steps are added and finished in a tabbed side panel, routes are drawn on the graph, flow settings and test calls slide in from the right, and publishing goes through a confirmation that lists what changes.

**Architecture:** The pure graph model (`ui/src/routes/agents/graph/model.ts`) gains draft routes, per-step issues and blank flows. A generic `SidePanel` primitive hosts three panels (step, flow settings, test call) over the existing React Flow `Canvas`. The live-call view logic moves out of `LiveGraph` into a `useCallView` hook so the editor's main canvas shows the call. `FlowEditor` replaces `GraphDesigner` and `Inspector`, which are deleted. No server or API change.

**Tech Stack:** React 19, TypeScript 7, `@xyflow/react` 12, `@tanstack/react-query` 5, vitest 5 + Testing Library, Playwright (`e2e/tests-graph`).

**Spec:** `docs/superpowers/specs/2026-10-12-switchboard-console-design.md` (Sections 1, 2, 4, 5). Visual rules: `docs/superpowers/specs/2026-10-12-ui-switchboard-design.md`.

## Global Constraints

- UI copy uses Flow / Step / Route / Start step / Flow settings. Never "agent", "node" or "handoff" in new copy. API names (`agent_id`, `/api/v1/agents`, `AgentDTO`, `GraphNode`) stay until the code rename (spec S6) lands.
- Colours only through tokens (`var(--paper)`, `--surface`, `--ink`, `--ink-2`, `--rule`, `--caller`, `--lamp`, `--ok`, `--bad`). No hex or `rgb(` in TSX or below `/* end tokens */` in `styles.css` (guard: `ui/src/test/noHardcodedColors.test.ts`; add every new TSX file to its `CONVERTED` list).
- Lamp amber only for the live call. "Needs setup" uses a dashed `--ink-2` border and `--bad` text/marker.
- Sentence-case labels; no all-caps or monospace labels; no drop shadows; one radius `var(--radius)`.
- Focus ring stays the global 3px `--caller` outline; every icon-only button has an accessible name; touch targets ≥ 44px.
- `prefers-reduced-motion: reduce` disables the panel slide.
- Limits mirror the server: 8 steps (`MAX_NODES`), 32 routes (`MAX_EDGES`), conditions ≤ 512 characters, `vad_stop_ms` 200–2000, `max_parallel` 1–16, `context.max_messages` ≥ 2.
- A test call runs the **published** version (spec S1). Never claim it tests the draft.
- Keep these test ids and names working: `start-voice-test-btn`, `connection-state`, `mute-btn`, `disconnect-btn`, `send-text-btn`, `transcript-user`, `transcript-bot`, `bot-streaming-text`, `agent-readiness`, `session-summary`; labels `Management key`, `STT provider`, `LLM provider`, `TTS provider`, `Voice`, `Sound name`, `Start after (ms)`; the list named `Path taken`.
- Gate for every task: `cd ui && npx vitest run && npx tsc --noEmit`.
- Commits are conventional (`feat(ui): …`) and end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Review Focus

1. **Keyboard-only routes:** someone who cannot drag must still create, edit and remove routes. Expect: the Routes tab's `Route to` select and `Add route` button produce the same route as a drag. Pinned in Task 5.
2. **Empty conditions never reach the server:** a drawn route starts empty. Expect: the step shows `Needs setup`, publish is blocked with `The route from A to B needs a condition.`, and `onSubmit` is never called. Pinned in Tasks 1 and 9.
3. **Focus after drawing a route:** the panel opens on the Routes tab and the new condition field, not the panel heading, has focus. Pinned in Task 2 (an `autoFocus` field keeps focus) and Task 5 (a drawn route's condition is focused).
4. **Test call vs draft:** with unpublished changes the test panel must say it runs the published version. Expect the note `This call runs v1, the published version.`; a never-published flow cannot start a test. Pinned in Task 9.
5. **Deleting or re-pointing the start step:** the start step has no `Delete step`; `Make start step` moves listening, turn-taking, memory and recording with it (`setEntry`). Pinned in Task 5.

---

### Task 1: Model — blank flows, draft routes, step and flow issues

**Files:**
- Modify: `ui/src/routes/agents/graph/model.ts`
- Test: `ui/src/routes/agents/graph/model.test.ts`

**Interfaces:**
- Produces:
  - `type StepTab = "basics" | "routes" | "tools" | "advanced"`
  - `type StepIssue = { tab: StepTab; message: string }`
  - `emptyFlow(): GraphDraft`
  - `stepIssues(g: GraphDraft, id: string): StepIssue[]`
  - `flowIssues(g: GraphDraft): string[]`
  - `isUnreachable(g: GraphDraft, id: string): boolean`
  - `addDraftRoute(g: GraphDraft, from: string, to: string): GraphDraft` (empty condition; same graph back when refused)
  - `uniqueName(g: GraphDraft, base: string): string`
  - `addNode` on an empty graph creates a blank start step.
  - `validate` also reports routes with empty conditions and recording without disclosure.

- [ ] **Step 1: Write the failing tests** — append to `model.test.ts` (extend the import list with `addDraftRoute, emptyFlow, flowIssues, isUnreachable, stepIssues, uniqueName`):

```ts
describe("flow authoring", () => {
  const ready = { llm: { provider: "google", model: "gemini" }, tts: { provider: "google", voice: "v" } };
  const two = () =>
    fromAgentConfig({
      entry: "a",
      nodes: [
        { id: "a", name: "Reception", config: { stt: { provider: "google" }, ...ready } },
        { id: "b", name: "Billing", config: ready },
      ],
      edges: [],
    });

  it("starts a blank flow with no steps, and the first step added becomes the start step", () => {
    const g = addNode(emptyFlow(), "New step");
    expect(g.nodes).toHaveLength(1);
    expect(g.entry).toBe(g.nodes[0].id);
    expect(g.nodes[0].config).toEqual({});
  });

  it("names new steps uniquely", () => {
    const g = addNode(addNode(emptyFlow(), "New step"), "New step 2");
    expect(uniqueName(emptyFlow(), "New step")).toBe("New step");
    expect(uniqueName(g, "New step")).toBe("New step 3");
  });

  it("adds a draft route with an empty condition and refuses self, duplicate and unknown routes", () => {
    const g = addDraftRoute(two(), "a", "b");
    expect(g.edges).toEqual([expect.objectContaining({ from: "a", to: "b", condition: "" })]);
    expect(addDraftRoute(g, "a", "b")).toBe(g);
    expect(addDraftRoute(g, "a", "a")).toBe(g);
    expect(addDraftRoute(g, "a", "zzz")).toBe(g);
  });

  it("reports what a step still needs, by the tab that fixes it", () => {
    const blank = addNode(emptyFlow(), "New step");
    expect(stepIssues(blank, blank.entry)).toEqual([
      { tab: "basics", message: "Choose a model." },
      { tab: "basics", message: "Choose a voice." },
    ]);
    const routed = addDraftRoute(two(), "a", "b");
    expect(stepIssues(routed, "a")).toEqual([{ tab: "routes", message: "Give the route to Billing a condition." }]);
    expect(stepIssues(two(), "b")).toEqual([]);
  });

  it("flags duplicate and empty step names on the basics tab", () => {
    const g = { ...two(), nodes: two().nodes.map((n) => ({ ...n, name: n.id === "b" ? "reception" : n.name })) };
    expect(stepIssues(g, "b")).toContainEqual({ tab: "basics", message: "Another step has this name." });
    const unnamed = { ...two(), nodes: two().nodes.map((n) => ({ ...n, name: n.id === "b" ? "  " : n.name })) };
    expect(stepIssues(unnamed, "b")).toContainEqual({ tab: "basics", message: "Give the step a name." });
  });

  it("reports flow-level problems: speech-to-text and recording disclosure", () => {
    const noStt = addNode(emptyFlow(), "New step");
    expect(flowIssues(noStt)).toEqual(["Choose speech-to-text in Flow settings."]);
    const g = two();
    const rec = { ...g, nodes: g.nodes.map((n) => (n.id === "a" ? { ...n, config: { ...n.config, recording: { enabled: true, disclosure_confirmed: false } } } : n)) };
    expect(flowIssues(rec)).toEqual(["Confirm that callers are told the call is recorded."]);
    expect(flowIssues(g)).toEqual([]);
    expect(flowIssues(emptyFlow())).toEqual([]);
  });

  it("knows which steps no route leads to", () => {
    expect(isUnreachable(two(), "b")).toBe(true);
    expect(isUnreachable(two(), "a")).toBe(false);
    expect(isUnreachable(addDraftRoute(two(), "a", "b"), "b")).toBe(false);
  });

  it("blocks publishing while a route has no condition", () => {
    expect(validate(addDraftRoute(two(), "a", "b"))).toContain("The route from Reception to Billing needs a condition.");
  });
});
```

- [ ] **Step 2: Run the tests to see them fail**

Run: `cd ui && npx vitest run src/routes/agents/graph/model.test.ts`
Expected: FAIL — `emptyFlow is not a function` (and the other new exports are missing).

- [ ] **Step 3: Implement in `model.ts`**

Add below the type declarations:

```ts
export type StepTab = "basics" | "routes" | "tools" | "advanced";
export type StepIssue = { tab: StepTab; message: string };

/** A flow with no steps yet: the editor shows "Start with one step." */
export function emptyFlow(): GraphDraft {
  return { entry: "", nodes: [], edges: [] };
}

/** "New step", then "New step 2", "New step 3"… so step names stay unique. */
export function uniqueName(g: GraphDraft, base: string): string {
  const taken = new Set(g.nodes.map((n) => slug(n.name)));
  if (!taken.has(slug(base))) return base;
  for (let i = 2; ; i++) if (!taken.has(slug(`${base} ${i}`))) return `${base} ${i}`;
}

const blank = (v: unknown) => !String(v ?? "").trim();
const nameIn = (g: GraphDraft, id: string) => g.nodes.find((n) => n.id === id)?.name.trim() || "Untitled step";

/** What one step still needs before it can take calls, by the panel tab that fixes it. */
export function stepIssues(g: GraphDraft, id: string): StepIssue[] {
  const n = g.nodes.find((x) => x.id === id);
  if (!n) return [];
  const issues: StepIssue[] = [];
  if (!slug(n.name)) issues.push({ tab: "basics", message: "Give the step a name." });
  else if (g.nodes.some((o) => o.id !== id && slug(o.name) === slug(n.name))) issues.push({ tab: "basics", message: "Another step has this name." });
  const llm = asRecord(n.config.llm);
  if (blank(llm.provider) || blank(llm.model)) issues.push({ tab: "basics", message: "Choose a model." });
  if (blank(asRecord(n.config.tts).provider)) issues.push({ tab: "basics", message: "Choose a voice." });
  for (const e of g.edges)
    if (e.from === id && blank(e.condition)) issues.push({ tab: "routes", message: `Give the route to ${nameIn(g, e.to)} a condition.` });
  return issues;
}

/** Problems that belong to the whole flow (header state line and Flow settings). */
export function flowIssues(g: GraphDraft): string[] {
  if (g.nodes.length === 0) return [];
  const start = g.nodes.find((n) => n.id === g.entry);
  if (!start) return ["Choose a start step."];
  const out: string[] = [];
  if (blank(asRecord(start.config.stt).provider)) out.push("Choose speech-to-text in Flow settings.");
  const rec = asRecord(start.config.recording);
  if (rec.enabled === true && rec.disclosure_confirmed !== true) out.push("Confirm that callers are told the call is recorded.");
  return out;
}

/** A step no route leads to; callers can never reach it. The start step is always reachable. */
export function isUnreachable(g: GraphDraft, id: string): boolean {
  return id !== g.entry && !g.edges.some((e) => e.to === id);
}

/** A route drawn on the graph: no condition yet, the step panel asks for one. */
export function addDraftRoute(g: GraphDraft, from: string, to: string): GraphDraft {
  const known = (id: string) => g.nodes.some((n) => n.id === id);
  if (from === to || !known(from) || !known(to) || g.edges.length >= MAX_EDGES) return g;
  if (g.edges.some((e) => e.from === from && e.to === to)) return g;
  return { ...g, edges: [...g.edges, { id: newId(), from, to, condition: "" }] };
}
```

Change `addNode` so a blank flow's first step becomes the start step (insert at the top of the function, after the `MAX_NODES` guard):

```ts
  if (g.nodes.length === 0) {
    const id = newId();
    return { ...g, entry: id, nodes: [{ id, name, config: {}, tools: [], position: { x: 0, y: 0 } }] };
  }
```

In `validate`, after the per-node loop, add:

```ts
  for (const e of g.edges)
    if (blank(e.condition)) errors.push(`The route from ${nameIn(g, e.from)} to ${nameIn(g, e.to)} needs a condition.`);
  errors.push(...flowIssues(g).filter((m) => !m.startsWith("Choose speech-to-text")));
```

(The STT requirement is already reported by the loop as `<name>: STT provider is required.`; keep that message, it is what the server says.)

- [ ] **Step 4: Run the tests to see them pass**

Run: `cd ui && npx vitest run src/routes/agents/graph/model.test.ts`
Expected: PASS (old and new cases).

- [ ] **Step 5: Commit**

```bash
git add ui/src/routes/agents/graph/model.ts ui/src/routes/agents/graph/model.test.ts
git commit -m "feat(ui): blank flows, draft routes and per-step issues in the graph model

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 2: `SidePanel` primitive

**Files:**
- Create: `ui/src/components/SidePanel.tsx`
- Test: `ui/src/components/SidePanel.test.tsx`
- Modify: `ui/src/styles.css` (append after the last rule), `ui/src/test/noHardcodedColors.test.ts` (add `"components/SidePanel.tsx"` to `CONVERTED`)

**Interfaces:**
- Produces: `SidePanel(props: { title: string; onClose: () => void; children: ReactNode; footer?: ReactNode; wide?: boolean; onToggleWide?: () => void; returnFocusTo?: HTMLElement | null })`. Renders `<aside aria-labelledby>` with an `h2` title, a `Widen`/`Narrow` toggle when `onToggleWide` is given, and a close button named `Close <title>`. Focus moves to the heading on mount unless something inside already has focus (an `autoFocus` field). Escape closes. Focus returns to `returnFocusTo` on unmount.

- [ ] **Step 1: Write the failing test** — `ui/src/components/SidePanel.test.tsx`:

```tsx
import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { SidePanel } from "./SidePanel";

function Harness({ autoFocusField = false }: { autoFocusField?: boolean }) {
  const [open, setOpen] = useState(false);
  const [wide, setWide] = useState(false);
  const [opener, setOpener] = useState<HTMLElement | null>(null);
  return (
    <>
      <button onClick={(e) => { setOpener(e.currentTarget); setOpen(true); }}>Open</button>
      {open && (
        <SidePanel title="Flow settings" onClose={() => setOpen(false)} wide={wide} onToggleWide={() => setWide((w) => !w)} returnFocusTo={opener} footer={<button>Done</button>}>
          <label>Name<input autoFocus={autoFocusField} /></label>
        </SidePanel>
      )}
    </>
  );
}

describe("SidePanel", () => {
  it("is labelled by its title, focuses it, and returns focus to the opener on Escape", async () => {
    const user = userEvent.setup();
    render(<Harness />);
    await user.click(screen.getByRole("button", { name: "Open" }));
    const panel = screen.getByRole("complementary", { name: "Flow settings" });
    expect(screen.getByRole("heading", { name: "Flow settings" })).toHaveFocus();
    expect(panel).toContainElement(screen.getByRole("button", { name: "Done" }));
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("complementary")).toBeNull();
    expect(screen.getByRole("button", { name: "Open" })).toHaveFocus();
  });

  it("leaves focus on a field that asked for it", async () => {
    const user = userEvent.setup();
    render(<Harness autoFocusField />);
    await user.click(screen.getByRole("button", { name: "Open" }));
    expect(screen.getByLabelText("Name")).toHaveFocus();
  });

  it("closes from its close button and toggles width", async () => {
    const user = userEvent.setup();
    render(<Harness />);
    await user.click(screen.getByRole("button", { name: "Open" }));
    const widen = screen.getByRole("button", { name: "Widen" });
    await user.click(widen);
    expect(screen.getByRole("button", { name: "Narrow" })).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByRole("complementary")).toHaveClass("side-panel--wide");
    await user.click(screen.getByRole("button", { name: "Close Flow settings" }));
    expect(screen.queryByRole("complementary")).toBeNull();
  });
});
```

- [ ] **Step 2: Run it to see it fail**

Run: `cd ui && npx vitest run src/components/SidePanel.test.tsx`
Expected: FAIL — `Failed to resolve import "./SidePanel"`.

- [ ] **Step 3: Implement** — `ui/src/components/SidePanel.tsx`:

```tsx
import { useEffect, useId, useRef, type ReactNode } from "react";

export type SidePanelProps = {
  title: string;
  onClose: () => void;
  children: ReactNode;
  footer?: ReactNode;
  wide?: boolean;
  onToggleWide?: () => void;
  /** Where focus goes back to when the panel closes: the step or button that opened it. */
  returnFocusTo?: HTMLElement | null;
};

/** Slides over the right of its positioned parent; what is behind it stays visible and usable. */
export function SidePanel({ title, onClose, children, footer, wide, onToggleWide, returnFocusTo }: SidePanelProps) {
  const id = useId();
  const panel = useRef<HTMLElement>(null);
  const heading = useRef<HTMLHeadingElement>(null);
  const back = useRef(returnFocusTo);
  useEffect(() => {
    // An autoFocus field inside (a route just drawn) keeps focus; otherwise start at the title.
    if (!panel.current?.contains(document.activeElement)) heading.current?.focus();
    const target = back.current;
    return () => target?.focus();
  }, []);
  return (
    <aside
      ref={panel}
      aria-labelledby={id}
      className={`side-panel${wide ? " side-panel--wide" : ""}`}
      onKeyDown={(e) => {
        if (e.key === "Escape") {
          e.stopPropagation();
          onClose();
        }
      }}
    >
      <div className="side-panel__head">
        <h2 id={id} ref={heading} tabIndex={-1}>
          {title}
        </h2>
        {onToggleWide && (
          <button type="button" className="action-button-small" aria-pressed={!!wide} onClick={onToggleWide}>
            {wide ? "Narrow" : "Widen"}
          </button>
        )}
        <button type="button" className="icon-button" aria-label={`Close ${title}`} onClick={onClose}>
          ×
        </button>
      </div>
      <div className="side-panel__body">{children}</div>
      {footer && <div className="side-panel__foot">{footer}</div>}
    </aside>
  );
}
```

Append to `ui/src/styles.css`:

```css
/* Side panel: slides over the right of a position: relative parent. */
.side-panel {
  position: absolute;
  inset: 0 0 0 auto;
  width: min(440px, 100%);
  display: flex;
  flex-direction: column;
  background: var(--surface);
  border-left: 1px solid var(--rule);
  z-index: 5;
  animation: side-panel-in 0.2s ease-out;
}
.side-panel--wide {
  width: min(720px, 100%);
}
.side-panel__head {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 12px 16px;
  border-bottom: 1px solid var(--rule);
}
.side-panel__head h2 {
  flex: 1;
  margin: 0;
  font-size: 18px;
}
.side-panel__body {
  flex: 1;
  overflow: auto;
  padding: 16px;
  display: flex;
  flex-direction: column;
  gap: 16px;
}
.side-panel__foot {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 12px 16px;
  border-top: 1px solid var(--rule);
}
.icon-button {
  min-width: 44px;
  min-height: 44px;
  border: 0;
  background: transparent;
  color: var(--ink-2);
  font-size: 22px;
}
@keyframes side-panel-in {
  from {
    transform: translateX(24px);
    opacity: 0;
  }
  to {
    transform: none;
    opacity: 1;
  }
}
@media (prefers-reduced-motion: reduce) {
  .side-panel {
    animation: none;
  }
}
```

- [ ] **Step 4: Run it to see it pass, plus the colour guard**

Run: `cd ui && npx vitest run src/components/SidePanel.test.tsx src/test/noHardcodedColors.test.ts`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add ui/src/components/SidePanel.tsx ui/src/components/SidePanel.test.tsx ui/src/styles.css ui/src/test/noHardcodedColors.test.ts
git commit -m "feat(ui): side panel primitive with focus return and reduced motion

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 3: Canvas — step status, start badge, draft routes

**Files:**
- Modify: `ui/src/routes/agents/graph/Canvas.tsx`, `ui/src/styles.css`
- Test: `ui/src/routes/agents/graph/Canvas.test.tsx` (create)

**Interfaces:**
- Consumes: `stepIssues`, `isUnreachable` (Task 1).
- Produces: step cards with a status line (`Speaking`/`Listening`/`Thinking`/`Running a tool`/`Preparing…`/`Visited` during a call; otherwise `Needs setup`, `No route leads here` or `Ready`; read-only canvases show no setup status), the badge text `Start`, class `agent-node--incomplete`, and edges with empty conditions labelled `Set a condition` with class `agent-edge--draft`. New prop `height?: number` keeps its default of 420.

- [ ] **Step 1: Write the failing test** — `ui/src/routes/agents/graph/Canvas.test.tsx`:

```tsx
import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { ReactFlowProvider } from "@xyflow/react";
import { Canvas } from "./Canvas";
import { addDraftRoute, fromAgentConfig } from "./model";

const ready = { llm: { provider: "google", model: "gemini" }, tts: { provider: "google", voice: "v" } };
const graph = fromAgentConfig({
  entry: "a",
  nodes: [
    { id: "a", name: "Reception", config: { stt: { provider: "google" }, ...ready } },
    { id: "b", name: "Billing", config: {} },
    { id: "c", name: "Support", config: ready },
  ],
  edges: [{ id: "e1", from: "a", to: "b", condition: "asks about an invoice" }],
});

const card = (name: string) => screen.getByText(name).closest(".agent-node") as HTMLElement;

describe("Canvas step cards", () => {
  it("shows Ready, Needs setup and No route leads here", () => {
    render(<ReactFlowProvider><Canvas graph={graph} /></ReactFlowProvider>);
    expect(card("Reception")).toHaveTextContent("Ready");
    expect(card("Reception")).toHaveTextContent("Start");
    expect(card("Billing")).toHaveTextContent("Needs setup");
    expect(card("Billing")).toHaveClass("agent-node--incomplete");
    expect(card("Support")).toHaveTextContent("No route leads here");
  });

  it("labels a route with no condition", () => {
    render(<ReactFlowProvider><Canvas graph={addDraftRoute(graph, "a", "c")} /></ReactFlowProvider>);
    expect(screen.getByText("Set a condition")).toBeInTheDocument();
  });

  it("shows no setup status on a read-only canvas", () => {
    render(<ReactFlowProvider><Canvas graph={graph} readOnly /></ReactFlowProvider>);
    expect(card("Billing")).not.toHaveTextContent("Needs setup");
  });
});
```

- [ ] **Step 2: Run it to see it fail**

Run: `cd ui && npx vitest run src/routes/agents/graph/Canvas.test.tsx`
Expected: FAIL — `Ready` / `Start` not found (the badge says `entry`).

- [ ] **Step 3: Implement in `Canvas.tsx`**

Replace `AgentNodeData` and `AgentNode`:

```tsx
type AgentNodeData = { name: string; model: string; voice: string; entry: boolean; setup: "ready" | "incomplete" | "unreachable" | null; visual?: NodeVisual };

const ACTIVITY_LABEL: Record<Exclude<Activity, null>, string> = {
  listening: "Listening",
  thinking: "Thinking",
  speaking: "Speaking",
  tool: "Running a tool",
  interrupted: "Interrupted",
  error: "Error",
};
const SETUP_LABEL = { ready: "Ready", incomplete: "Needs setup", unreachable: "No route leads here" } as const;

function AgentNode({ data, selected }: NodeProps<Node<AgentNodeData>>) {
  const state = data.visual?.state ?? "idle";
  const activity = data.visual?.activity ?? null;
  const status =
    state === "preparing" ? "Preparing…"
    : state === "active" ? (activity ? ACTIVITY_LABEL[activity] : "On the call")
    : state === "visited" ? "Visited"
    : data.setup ? SETUP_LABEL[data.setup]
    : null;
  const incomplete = state === "idle" && data.setup === "incomplete";
  return (
    <div className={`agent-node agent-node--${state}${incomplete ? " agent-node--incomplete" : ""}${selected ? " agent-node--selected" : ""}`} data-state={state}>
      <Handle type="target" position={Position.Left} />
      <div className="agent-node__title">
        {data.name || "Untitled step"}
        {data.entry && <span className="agent-node__badge">Start</span>}
      </div>
      <div className="agent-node__meta">{data.model && data.voice ? `${data.model} · ${data.voice}` : data.model || "No model or voice yet"}</div>
      {status && <div className="agent-node__status">{status}</div>}
      <Handle type="source" position={Position.Right} />
    </div>
  );
}
```

In `Canvas`, import `isUnreachable, stepIssues` from `./model`, and in the `derived` memo set:

```tsx
          setup: readOnly ? null : stepIssues(graph, n.id).length ? "incomplete" : isUnreachable(graph, n.id) ? "unreachable" : "ready",
```

(add `readOnly` and `graph.edges` to the memo's dependency list). In the `edges` memo:

```tsx
        label: !e.condition.trim() ? "Set a condition" : e.condition.length > 40 ? `${e.condition.slice(0, 37)}…` : e.condition,
        className: [
          !e.condition.trim() ? "agent-edge--draft" : "",
          traversedEdges?.includes(e.id) ? `agent-edge--traversed${e.id === justHandedEdge ? " agent-edge--just" : ""}` : "",
        ].join(" ").trim() || undefined,
```

Append to `styles.css`:

```css
.agent-node--incomplete {
  border-style: dashed;
  border-color: var(--ink-2);
}
.agent-node--incomplete .agent-node__status {
  color: var(--bad);
}
.agent-canvas .agent-edge--draft .react-flow__edge-path {
  stroke: var(--ink-2);
  stroke-dasharray: 5 4;
}
```

Update existing assertions that read the old words: run `cd ui && grep -rn "\"left\"\|'left'\|>entry<\|preparing…" src --include=*.test.tsx` and change any match to the new labels (`Visited`, `Start`, `Preparing…`).

- [ ] **Step 4: Run the graph tests**

Run: `cd ui && npx vitest run src/routes/agents/graph`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add ui/src/routes/agents/graph/Canvas.tsx ui/src/routes/agents/graph/Canvas.test.tsx ui/src/styles.css
git commit -m "feat(ui): step cards show setup status; routes without a condition draw as drafts

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 4: `useCallView` hook; the voice test reports the call

**Files:**
- Create: `ui/src/routes/agents/graph/useCallView.ts`
- Modify: `ui/src/routes/agents/graph/LiveGraph.tsx`, `ui/src/routes/agents/AgentVoiceTest.tsx`
- Test: `ui/src/routes/agents/AgentVoiceTest.graph.test.tsx` (add cases); `LiveGraph.test.tsx` must pass unchanged.

**Interfaces:**
- Produces:
  - `type CallInfo = { sessionId: string | null; userSpeaking: boolean; botSpeaking: boolean; live: boolean }`
  - `const NO_CALL: CallInfo`
  - `useCallView(graph: GraphDraft, call: CallInfo, client: ApiClient, pollMs?: number): { view: GraphView; traversedEdges: string[]; justHanded: string | undefined }`
  - `AgentVoiceTest` props `hideGraph?: boolean` (no embedded `LiveGraph`) and `onCall?: (call: CallInfo) => void` (called on every change and with `NO_CALL` on unmount; pass a stable function such as a state setter).

- [ ] **Step 1: Write the failing tests** — in `AgentVoiceTest.graph.test.tsx`, extend `renderTest` to take extra props (`function renderTest(a: AgentDTO, extra: Partial<AgentVoiceTestProps> = {})`, spread `{...extra}` onto `<AgentVoiceTest>`, import `type AgentVoiceTestProps` from `./AgentVoiceTest`), add this fixture under the existing `agent`/`node` helpers, and add the two cases inside the `describe`:

```tsx
const graphAgent = agent({
  entry: "front",
  nodes: [node("front", "Front"), node("sales", "Sales")],
  edges: [{ id: "e1", from: "front", to: "sales", condition: "buy" }],
});
```

```tsx
  it("leaves the graph to the page when asked", () => {
    renderTest(graphAgent, { hideGraph: true });
    expect(screen.queryByRole("list", { name: /path taken/i })).toBeNull();
  });

  it("reports the idle call to the page, and NO_CALL when it goes away", () => {
    const onCall = vi.fn();
    const { unmount } = renderTest(graphAgent, { onCall });
    expect(onCall).toHaveBeenLastCalledWith({ sessionId: null, userSpeaking: false, botSpeaking: false, live: false });
    unmount();
    expect(onCall).toHaveBeenLastCalledWith({ sessionId: null, userSpeaking: false, botSpeaking: false, live: false });
  });
```

- [ ] **Step 2: Run them to see them fail**

Run: `cd ui && npx vitest run src/routes/agents/AgentVoiceTest.graph.test.tsx`
Expected: FAIL — the path list is still rendered; `onCall` never called.

- [ ] **Step 3: Implement**

`ui/src/routes/agents/graph/useCallView.ts` — the body moved from `LiveGraph`:

```ts
import { useEffect, useMemo, useRef, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import type { ApiClient } from "../../../api/client";
import { deriveView } from "./activity";
import type { GraphDraft } from "./model";

/** What a voice test tells the page about its call. */
export type CallInfo = { sessionId: string | null; userSpeaking: boolean; botSpeaking: boolean; live: boolean };
export const NO_CALL: CallInfo = { sessionId: null, userSpeaking: false, botSpeaking: false, live: false };

const HANDOFF_MS = 450;

/**
 * Which step holds the call, which it is preparing, and the routes taken,
 * from the server's handoff trace spans plus what the voice client hears.
 */
export function useCallView(graph: GraphDraft, call: CallInfo, client: ApiClient, pollMs = 500) {
  const { sessionId, userSpeaking, botSpeaking, live } = call;
  const traces = useQuery({
    queryKey: ["session-traces", sessionId],
    queryFn: () => client.getSessionTraces(sessionId as string),
    enabled: !!sessionId,
    refetchInterval: live ? pollMs : false,
    retry: false,
  });

  // "Thinking" = the caller finished speaking and the step has not started yet.
  const [thinking, setThinking] = useState(false);
  const wasSpeaking = useRef(false);
  useEffect(() => {
    if (botSpeaking || userSpeaking) setThinking(false);
    else if (wasSpeaking.current) setThinking(true);
    wasSpeaking.current = userSpeaking;
  }, [userSpeaking, botSpeaking]);

  const view = useMemo(
    () => deriveView(graph.entry, traces.data?.spans ?? [], graph.edges, { userSpeaking, botSpeaking, thinking }),
    [graph.entry, graph.edges, traces.data, userSpeaking, botSpeaking, thinking],
  );
  const traversedEdges = useMemo(() => view.traversed.flatMap((t) => (t.edgeId ? [t.edgeId] : [])), [view]);

  // The route just taken is marked for a beat so the canvas can draw the cable.
  const [justHanded, setJustHanded] = useState<string | undefined>();
  const seen = useRef<number | null>(null);
  useEffect(() => {
    const count = traversedEdges.length;
    const grew = seen.current !== null && count > seen.current;
    seen.current = count;
    if (!live || !grew) return;
    setJustHanded(traversedEdges[count - 1]);
    const t = setTimeout(() => setJustHanded(undefined), HANDOFF_MS);
    return () => clearTimeout(t);
  }, [traversedEdges, live]);

  return { view, traversedEdges, justHanded };
}
```

`LiveGraph.tsx`: delete the query, thinking, view, traversed and justHanded code and the `HANDOFF_MS` constant; replace with:

```tsx
  const { view, traversedEdges, justHanded } = useCallView(graph, { sessionId, userSpeaking, botSpeaking, live }, client, pollMs);
```

Keep `nameOf`, `preparing`, `activity` and the JSX unchanged. Remove the now-unused imports (`useEffect`, `useMemo`, `useRef`, `useState`, `useQuery`, `deriveView`) and import `useCallView` from `./useCallView`.

`AgentVoiceTest.tsx`: add to `AgentVoiceTestProps`:

```tsx
  /** The page shows the call on its own graph (the flow editor): skip the embedded one. */
  hideGraph?: boolean;
  /** Told whenever the call changes; must be stable (a state setter). */
  onCall?: (call: CallInfo) => void;
```

Destructure `hideGraph, onCall`, import `{ NO_CALL, type CallInfo }` from `./graph/useCallView`, and after `inCall` is computed add:

```tsx
  useEffect(() => {
    onCall?.({ sessionId: state.sessionId, userSpeaking: state.isUserSpeaking, botSpeaking: state.isBotSpeaking, live: inCall });
  }, [onCall, state.sessionId, state.isUserSpeaking, state.isBotSpeaking, inCall]);
  useEffect(() => () => onCall?.(NO_CALL), [onCall]);
```

Change `{graph && (` before `<div className="stage__graph">` to `{graph && !hideGraph && (`.

- [ ] **Step 4: Run the voice and live-graph tests**

Run: `cd ui && npx vitest run src/routes/agents`
Expected: PASS, including `LiveGraph.test.tsx` and `LiveGraph.cable.test.tsx` unchanged.

- [ ] **Step 5: Commit**

```bash
git add ui/src/routes/agents/graph/useCallView.ts ui/src/routes/agents/graph/LiveGraph.tsx ui/src/routes/agents/AgentVoiceTest.tsx ui/src/routes/agents/AgentVoiceTest.graph.test.tsx
git commit -m "refactor(ui): useCallView drives any canvas; the voice test reports its call

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 5: `StepPanel` — Basics, Routes and Tools tabs

**Files:**
- Create: `ui/src/routes/agents/graph/StepPanel.tsx`
- Test: `ui/src/routes/agents/graph/StepPanel.test.tsx`
- Modify: `ui/src/styles.css`, `ui/src/test/noHardcodedColors.test.ts` (add `"routes/agents/graph/StepPanel.tsx"`)

**Interfaces:**
- Consumes: `SidePanel` (Task 2); `stepIssues`, `addDraftRoute`, `addNode`, `uniqueName`, `updateNode`, `updateEdge`, `removeEdge`, `removeNode`, `setEntry`, `MAX_NODES`, `StepTab` (Task 1 and existing); `StageFields`.
- Produces:

```ts
export type StepPanelProps = {
  graph: GraphDraft;
  stepId: string;
  providers: ProviderStatus[];
  flowTools: string[]; // "<mcp_server_id>/<remote_name>" selected on the flow
  tab: StepTab;
  onTab: (tab: StepTab) => void;
  focusRoute?: string; // edge id whose condition field takes focus
  onFocusRoute: (edgeId: string | undefined) => void;
  focusName?: boolean; // a step just added: its name field takes focus, selected
  wide: boolean;
  onToggleWide: () => void;
  onChange: (next: GraphDraft) => void;
  onClose: () => void;
  returnFocusTo?: HTMLElement | null;
};
export function StepPanel(props: StepPanelProps): JSX.Element | null;
```

Advanced tab content arrives in Task 6. Until then this task renders `<p className="subtext">Fallbacks and tuning.</p>` for it; Task 6 replaces that line.

- [ ] **Step 1: Write the failing test** — `ui/src/routes/agents/graph/StepPanel.test.tsx`:

```tsx
import { describe, expect, it, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import type { ProviderStatus } from "../../../api/client";
import { StepPanel } from "./StepPanel";
import { addDraftRoute, fromAgentConfig, type GraphDraft, type StepTab } from "./model";

const svc = { available: true, custom: true, models: [{ id: "gemini", default: true }], voices: [{ id: "aoede", default: true }] };
const providers: ProviderStatus[] = [
  { id: "google", display_name: "Google", capabilities: ["llm", "stt", "tts"], detected: true, env: ["GOOGLE_API_KEY"], services: { stt: svc, llm: svc, tts: svc } },
];
const ready = { llm: { provider: "google", model: "gemini" }, tts: { provider: "google", voice: "aoede" } };
const start = () =>
  fromAgentConfig({
    entry: "a",
    nodes: [
      { id: "a", name: "Reception", config: { stt: { provider: "google" }, ...ready } },
      { id: "b", name: "Billing", config: {} },
    ],
    edges: [],
  });

function Harness({ initial = start(), stepId = "b", initialTab = "basics" as StepTab, flowTools = [] as string[], onChange = vi.fn() }) {
  const [graph, setGraph] = useState<GraphDraft>(initial);
  const [tab, setTab] = useState<StepTab>(initialTab);
  const [focusRoute, setFocusRoute] = useState<string | undefined>();
  return (
    <div style={{ position: "relative" }}>
      <StepPanel
        graph={graph} stepId={stepId} providers={providers} flowTools={flowTools}
        tab={tab} onTab={setTab} focusRoute={focusRoute} onFocusRoute={setFocusRoute}
        wide={false} onToggleWide={vi.fn()} onClose={vi.fn()}
        onChange={(g) => { setGraph(g); onChange(g); }}
      />
    </div>
  );
}

describe("StepPanel", () => {
  it("lists what a new step needs and marks the Basics tab", () => {
    render(<Harness />);
    expect(screen.getByRole("tab", { name: /basics.*needs attention/i })).toHaveAttribute("aria-selected", "true");
    const note = screen.getByRole("note");
    expect(note).toHaveTextContent("Choose a model.");
    expect(note).toHaveTextContent("Choose a voice.");
  });

  it("finishes a step from Basics: name, instructions, model, voice", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(<Harness onChange={onChange} />);
    await user.clear(screen.getByLabelText("Step name"));
    await user.type(screen.getByLabelText("Step name"), "Billing desk");
    await user.type(screen.getByLabelText("Instructions"), "Explain invoices.");
    await user.selectOptions(screen.getByLabelText("LLM provider"), "google");
    await user.selectOptions(screen.getByLabelText("TTS provider"), "google");
    expect(screen.queryByRole("note")).toBeNull();
    const last = onChange.mock.calls.at(-1)![0] as GraphDraft;
    const b = last.nodes.find((n) => n.id === "b")!;
    expect(b.name).toBe("Billing desk");
    expect(b.config).toMatchObject({ llm: { provider: "google", model: "gemini", system_prompt: "Explain invoices." }, tts: { provider: "google", voice: "aoede" } });
  });

  it("adds a route from the keyboard, focuses its condition, and flags it until filled", async () => {
    const user = userEvent.setup();
    render(<Harness stepId="a" initialTab="routes" />);
    await user.selectOptions(screen.getByLabelText("Route to"), "b");
    await user.click(screen.getByRole("button", { name: "Add route" }));
    const condition = screen.getByLabelText("Condition for the route to Billing");
    expect(condition).toHaveFocus();
    expect(screen.getByRole("tab", { name: /routes.*needs attention/i })).toBeInTheDocument();
    await user.type(condition, "asks about an invoice");
    expect(screen.getByRole("tab", { name: /routes/i })).not.toHaveAccessibleName(/needs attention/i);
    await user.click(screen.getByRole("button", { name: "Remove the route to Billing" }));
    expect(screen.queryByLabelText("Condition for the route to Billing")).toBeNull();
  });

  it("routes to a brand-new step in one go", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(<Harness stepId="a" initialTab="routes" onChange={onChange} />);
    await user.click(screen.getByRole("button", { name: "Route to a new step" }));
    const g = onChange.mock.calls.at(-1)![0] as GraphDraft;
    expect(g.nodes.map((n) => n.name)).toEqual(["Reception", "Billing", "New step"]);
    expect(g.edges).toEqual([expect.objectContaining({ from: "a", condition: "" })]);
    expect(screen.getByLabelText("Condition for the route to New step")).toHaveFocus();
  });

  it("chooses which of the flow's tools a step can call", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(<Harness stepId="a" initialTab="tools" flowTools={["crm/lookup", "crm/verify"]} onChange={onChange} />);
    await user.click(screen.getByLabelText("crm/verify"));
    expect((onChange.mock.calls.at(-1)![0] as GraphDraft).nodes[0].tools).toContain("crm/verify");
    expect(screen.getByRole("tab", { name: /tools/i })).toHaveTextContent("1");
  });

  it("offers Delete step and Make start step only on other steps", () => {
    const { unmount } = render(<Harness stepId="a" />);
    expect(screen.queryByRole("button", { name: "Delete step" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Make start step" })).toBeNull();
    unmount();
    render(<Harness stepId="b" />);
    expect(screen.getByRole("button", { name: "Delete step" })).toBeVisible();
    expect(screen.getByRole("button", { name: "Make start step" })).toBeVisible();
  });

  it("moves the flow settings when another step becomes the start", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(<Harness stepId="b" onChange={onChange} />);
    await user.click(screen.getByRole("button", { name: "Make start step" }));
    const g = onChange.mock.calls.at(-1)![0] as GraphDraft;
    expect(g.entry).toBe("b");
    expect(g.nodes.find((n) => n.id === "b")!.config.stt).toEqual({ provider: "google" });
    expect(g.nodes.find((n) => n.id === "a")!.config.stt).toBeUndefined();
  });
});
```

Also add this case, which uses a pre-built draft route (it covers a route drawn on the canvas):

```tsx
  it("focuses the condition of a route drawn on the graph", () => {
    const g = addDraftRoute(start(), "a", "b");
    function Drawn() {
      const [graph, setGraph] = useState(g);
      return <div style={{ position: "relative" }}><StepPanel graph={graph} stepId="a" providers={providers} flowTools={[]} tab="routes" onTab={vi.fn()} focusRoute={graph.edges[0].id} onFocusRoute={vi.fn()} wide={false} onToggleWide={vi.fn()} onClose={vi.fn()} onChange={setGraph} /></div>;
    }
    render(<Drawn />);
    expect(screen.getByLabelText("Condition for the route to Billing")).toHaveFocus();
    expect(within(screen.getByRole("complementary")).getByRole("heading", { name: "Reception" })).toBeInTheDocument();
  });
```

- [ ] **Step 2: Run it to see it fail**

Run: `cd ui && npx vitest run src/routes/agents/graph/StepPanel.test.tsx`
Expected: FAIL — `Failed to resolve import "./StepPanel"`.

- [ ] **Step 3: Implement** — `ui/src/routes/agents/graph/StepPanel.tsx`:

```tsx
import { useState } from "react";
import type { ProviderStatus } from "../../../api/client";
import { SidePanel } from "../../../components/SidePanel";
import { StageFields, type StageValue } from "../StageFields";
import {
  MAX_NODES,
  addDraftRoute,
  addNode,
  removeEdge,
  removeNode,
  setEntry,
  stepIssues,
  uniqueName,
  updateEdge,
  updateNode,
  type GraphDraft,
  type StepTab,
} from "./model";

const TABS: { id: StepTab; label: string }[] = [
  { id: "basics", label: "Basics" },
  { id: "routes", label: "Routes" },
  { id: "tools", label: "Tools" },
  { id: "advanced", label: "Advanced" },
];
const rec = (v: unknown): Record<string, unknown> =>
  v && typeof v === "object" && !Array.isArray(v) ? (v as Record<string, unknown>) : {};
const str = (v: unknown): string => (typeof v === "string" ? v : "");

export type StepPanelProps = {
  graph: GraphDraft;
  stepId: string;
  providers: ProviderStatus[];
  /** MCP tools selected on the flow ("<mcp_server_id>/<remote_name>"); a step may call a subset. */
  flowTools: string[];
  tab: StepTab;
  onTab: (tab: StepTab) => void;
  /** The route whose condition takes focus: one just drawn or added. */
  focusRoute?: string;
  onFocusRoute: (edgeId: string | undefined) => void;
  /** A step just added: its name takes focus, selected, so typing renames it. */
  focusName?: boolean;
  wide: boolean;
  onToggleWide: () => void;
  onChange: (next: GraphDraft) => void;
  onClose: () => void;
  returnFocusTo?: HTMLElement | null;
};

/** Everything about one step, in tabs; Basics alone makes a step ready. */
export function StepPanel(props: StepPanelProps) {
  const { graph, stepId, providers, flowTools, tab, onTab, focusRoute, onFocusRoute, focusName, wide, onToggleWide, onChange, onClose, returnFocusTo } = props;
  const [routeTarget, setRouteTarget] = useState("");
  const step = graph.nodes.find((n) => n.id === stepId);
  if (!step) return null;

  const isStart = step.id === graph.entry;
  const issues = stepIssues(graph, step.id);
  const routes = graph.edges.filter((e) => e.from === step.id);
  const targets = graph.nodes.filter((n) => n.id !== step.id && !routes.some((r) => r.to === n.id));
  const nameOf = (id: string) => graph.nodes.find((n) => n.id === id)?.name.trim() || "Untitled step";
  const llm = rec(step.config.llm);
  const tts = rec(step.config.tts);
  const patchStage = (cap: "llm" | "tts", patch: Record<string, unknown>) =>
    onChange(updateNode(graph, step.id, { config: { ...step.config, [cap]: { ...rec(step.config[cap]), ...patch } } }));
  const warn = (t: StepTab) => issues.some((i) => i.tab === t);
  const count = (t: StepTab) => (t === "routes" ? routes.length : t === "tools" ? step.tools.length : null);
  const basicsIssues = issues.filter((i) => i.tab === "basics");

  function focusLastRoute(next: GraphDraft) {
    onChange(next);
    onFocusRoute(next.edges[next.edges.length - 1].id);
  }
  function addRoute() {
    const next = addDraftRoute(graph, step.id, routeTarget);
    if (next === graph) return;
    focusLastRoute(next);
    setRouteTarget("");
  }
  function routeToNewStep() {
    const withStep = addNode(graph, uniqueName(graph, "New step"));
    if (withStep === graph) return;
    focusLastRoute(addDraftRoute(withStep, step.id, withStep.nodes[withStep.nodes.length - 1].id));
  }

  return (
    <SidePanel
      title={step.name.trim() || "Untitled step"}
      onClose={onClose}
      wide={wide}
      onToggleWide={onToggleWide}
      returnFocusTo={returnFocusTo}
      footer={
        <>
          {!isStart && (
            <>
              <button type="button" className="action-button-small action-button-danger" onClick={() => { onChange(removeNode(graph, step.id)); onClose(); }}>
                Delete step
              </button>
              <button type="button" className="action-button-small" onClick={() => onChange(setEntry(graph, step.id))}>
                Make start step
              </button>
            </>
          )}
          <span className="grow" />
          <button type="button" className="action-button" onClick={onClose}>
            Done
          </button>
        </>
      }
    >
      <div role="tablist" aria-label="Step settings" className="tabs">
        {TABS.map((t) => (
          <button
            key={t.id}
            type="button"
            role="tab"
            id={`step-tab-${t.id}`}
            aria-selected={tab === t.id}
            aria-controls="step-tabpanel"
            className={tab === t.id ? "tab tab--current" : "tab"}
            onClick={() => onTab(t.id)}
          >
            {t.label}
            {count(t.id) !== null && <span className="tab__count">{count(t.id)}</span>}
            {warn(t.id) && <span className="tab__warn" role="img" aria-label="needs attention" />}
          </button>
        ))}
      </div>

      <div role="tabpanel" id="step-tabpanel" aria-labelledby={`step-tab-${tab}`} className="step-panel__tab">
        {tab === "basics" && (
          <>
            {basicsIssues.length > 0 && (
              <div role="note" className="note note--bad">
                <p>Before this step can take calls:</p>
                <ul className="plain-list">
                  {basicsIssues.map((i) => (
                    <li key={i.message}>{i.message}</li>
                  ))}
                </ul>
              </div>
            )}
            <label className="form-field" htmlFor="step-name">
              <span>Step name</span>
              <input
                id="step-name"
                type="text"
                value={step.name}
                maxLength={64}
                autoFocus={focusName}
                onFocus={(e) => focusName && e.currentTarget.select()}
                onChange={(e) => onChange(updateNode(graph, step.id, { name: e.target.value }))}
              />
            </label>
            <label className="form-field" htmlFor="step-instructions">
              <span>Instructions</span>
              <textarea
                id="step-instructions"
                rows={wide ? 12 : 6}
                value={str(llm.system_prompt)}
                placeholder="What should this step do? For example: greet the caller and find out why they are calling."
                onChange={(e) => patchStage("llm", { system_prompt: e.target.value })}
              />
              <span className="subtext">The model only sees this while the call is on this step.</span>
            </label>
            <div className={wide ? "grid-2" : "step-panel__stack"}>
              <fieldset className="stage-box">
                <legend>Model</legend>
                <StageFields
                  kind="llm"
                  providers={providers}
                  idPrefix={`step-${step.id}-llm`}
                  value={{ provider: str(llm.provider), model: str(llm.model) }}
                  onChange={(v: StageValue) => patchStage("llm", { provider: v.provider, model: v.model ?? "" })}
                />
              </fieldset>
              <fieldset className="stage-box">
                <legend>Voice</legend>
                <StageFields
                  kind="tts"
                  providers={providers}
                  idPrefix={`step-${step.id}-tts`}
                  value={{ provider: str(tts.provider), voice: str(tts.voice) }}
                  onChange={(v: StageValue) => patchStage("tts", { provider: v.provider, voice: v.voice ?? "" })}
                />
              </fieldset>
            </div>
          </>
        )}

        {tab === "routes" && (
          <>
            <p className="subtext">
              When the model decides a condition is true, the caller moves to that step. You can also drag from a step’s right edge on the graph.
            </p>
            {routes.length === 0 && <p className="subtext">No routes. The call stays on this step until it ends.</p>}
            <ul className="plain-list route-list">
              {routes.map((r) => (
                <li key={r.id} className="route-list__item">
                  <label className="form-field" htmlFor={`route-${r.id}`}>
                    <span>Condition for the route to {nameOf(r.to)}</span>
                    <textarea
                      id={`route-${r.id}`}
                      rows={2}
                      maxLength={512}
                      value={r.condition}
                      autoFocus={r.id === focusRoute}
                      aria-invalid={!r.condition.trim()}
                      placeholder="For example: the caller asks about an invoice"
                      onChange={(e) => onChange(updateEdge(graph, r.id, e.target.value))}
                      onBlur={() => r.id === focusRoute && onFocusRoute(undefined)}
                    />
                  </label>
                  <button type="button" className="action-button-small action-button-danger" onClick={() => onChange(removeEdge(graph, r.id))}>
                    Remove the route to {nameOf(r.to)}
                  </button>
                </li>
              ))}
            </ul>
            <div className="row row--end">
              <label className="form-field grow" htmlFor="route-target">
                <span>Route to</span>
                <select id="route-target" value={routeTarget} onChange={(e) => setRouteTarget(e.target.value)}>
                  <option value="">Choose a step</option>
                  {targets.map((n) => (
                    <option key={n.id} value={n.id}>
                      {n.name.trim() || "Untitled step"}
                    </option>
                  ))}
                </select>
              </label>
              <button type="button" className="action-button-small" disabled={!routeTarget} onClick={addRoute}>
                Add route
              </button>
            </div>
            <button type="button" className="action-button-small" disabled={graph.nodes.length >= MAX_NODES} onClick={routeToNewStep}>
              Route to a new step
            </button>
          </>
        )}

        {tab === "tools" &&
          (flowTools.length === 0 ? (
            <p className="subtext">This flow has no tools. Choose MCP tools for the flow first.</p>
          ) : (
            <fieldset className="stage-box">
              <legend>Tools this step can call</legend>
              {flowTools.map((t) => (
                <label key={t} className="check-row" htmlFor={`step-tool-${t}`}>
                  <input
                    id={`step-tool-${t}`}
                    type="checkbox"
                    checked={step.tools.includes(t)}
                    onChange={(e) =>
                      onChange(updateNode(graph, step.id, { tools: e.target.checked ? [...step.tools, t] : step.tools.filter((x) => x !== t) }))
                    }
                  />
                  <code>{t}</code>
                </label>
              ))}
              {graph.nodes.length === 1 && <p className="subtext">With one step, it can call every tool the flow has.</p>}
            </fieldset>
          ))}

        {tab === "advanced" && <p className="subtext">Fallbacks and tuning.</p>}
      </div>
    </SidePanel>
  );
}
```

Append to `styles.css`:

```css
.tabs {
  display: flex;
  gap: 2px;
  border-bottom: 1px solid var(--rule);
  margin: -16px -16px 0;
  padding: 0 10px;
}
.tab {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  min-height: 44px;
  padding: 0 10px;
  border: 0;
  border-bottom: 2px solid transparent;
  background: transparent;
  color: var(--ink-2);
  font: inherit;
}
.tab--current {
  color: var(--ink);
  font-weight: 600;
  border-bottom-color: var(--caller);
}
.tab__count {
  color: var(--ink-2);
  font-variant-numeric: tabular-nums;
}
.tab__warn {
  width: 8px;
  height: 8px;
  border-radius: 1px;
  background: var(--bad);
}
.step-panel__tab,
.step-panel__stack {
  display: flex;
  flex-direction: column;
  gap: 16px;
}
.route-list__item {
  display: flex;
  flex-direction: column;
  gap: 6px;
  padding: 10px 0;
  border-bottom: 1px solid var(--rule);
}
.check-row {
  display: flex;
  align-items: center;
  gap: 10px;
  min-height: 44px;
}
```

- [ ] **Step 4: Run it to see it pass, plus the colour guard**

Run: `cd ui && npx vitest run src/routes/agents/graph/StepPanel.test.tsx src/test/noHardcodedColors.test.ts`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add ui/src/routes/agents/graph/StepPanel.tsx ui/src/routes/agents/graph/StepPanel.test.tsx ui/src/styles.css ui/src/test/noHardcodedColors.test.ts
git commit -m "feat(ui): tabbed step panel with keyboard routes and per-step tools

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 6: Advanced tab — fallbacks and tuning

**Files:**
- Create: `ui/src/routes/agents/graph/StepAdvanced.tsx`
- Modify: `ui/src/routes/agents/graph/StepPanel.tsx` (replace the Advanced placeholder line), `ui/src/test/noHardcodedColors.test.ts` (add `"routes/agents/graph/StepAdvanced.tsx"`)
- Test: `ui/src/routes/agents/graph/StepAdvanced.test.tsx`

**Interfaces:**
- Consumes: `updateNode`, `GraphDraft`, `GraphNode`; `StageFields`.
- Produces: `StepAdvanced({ graph, step, providers, onChange }: { graph: GraphDraft; step: GraphNode; providers: ProviderStatus[]; onChange: (g: GraphDraft) => void })`. It writes `llm.fallbacks: {provider, model}[]`, `tts.fallbacks: {provider, voice?}[]`, `llm.max_parallel` (omitted at 1), and `tts.clean_text: false` (omitted when on). These are the server's field names (`server/agent/fallback.go`, `server/agent/config.go`).

- [ ] **Step 1: Write the failing test** — `ui/src/routes/agents/graph/StepAdvanced.test.tsx`:

```tsx
import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import type { ProviderStatus } from "../../../api/client";
import { StepAdvanced } from "./StepAdvanced";
import { fromAgentConfig, type GraphDraft } from "./model";

const svc = { available: true, custom: true, models: [{ id: "gemini", default: true }], voices: [{ id: "aoede", default: true }] };
const providers: ProviderStatus[] = [
  { id: "google", display_name: "Google", capabilities: ["llm", "tts"], detected: true, env: [], services: { llm: svc, tts: svc } },
];
const g0 = fromAgentConfig({ llm: { provider: "google", model: "gemini" }, tts: { provider: "google", voice: "aoede" }, stt: { provider: "google" } });

function Harness({ onChange }: { onChange: (g: GraphDraft) => void }) {
  const [g, setG] = useState(g0);
  return <StepAdvanced graph={g} step={g.nodes[0]} providers={providers} onChange={(n) => { setG(n); onChange(n); }} />;
}
const last = (fn: ReturnType<typeof vi.fn>) => (fn.mock.calls.at(-1)![0] as GraphDraft).nodes[0].config;

describe("StepAdvanced", () => {
  it("adds and removes a model fallback", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(<Harness onChange={onChange} />);
    const model = screen.getByRole("group", { name: "Model fallbacks" });
    await user.selectOptions(model.querySelector("select")!, "google");
    await user.click(screen.getByRole("button", { name: "Add model fallback" }));
    expect(last(onChange).llm).toMatchObject({ fallbacks: [{ provider: "google", model: "gemini" }] });
    await user.click(screen.getByRole("button", { name: "Remove model fallback 1" }));
    expect(last(onChange).llm).not.toHaveProperty("fallbacks");
  });

  it("sets tool calls at once within 1 to 16, omitting the default", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(<Harness onChange={onChange} />);
    const input = screen.getByLabelText("Tool calls at once");
    await user.clear(input);
    await user.type(input, "40");
    expect(last(onChange).llm).toMatchObject({ max_parallel: 16 });
    // Emptying the field falls back to 1, the server default, which is stored by omission.
    await user.clear(input);
    expect(last(onChange).llm).not.toHaveProperty("max_parallel");
    expect(input).toHaveValue(1);
  });

  it("turns text cleaning off explicitly and back on by omission", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(<Harness onChange={onChange} />);
    const box = screen.getByLabelText(/Clean text before speaking/);
    expect(box).toBeChecked();
    await user.click(box);
    expect(last(onChange).tts).toMatchObject({ clean_text: false });
    await user.click(box);
    expect(last(onChange).tts).not.toHaveProperty("clean_text");
  });
});
```

- [ ] **Step 2: Run it to see it fail**

Run: `cd ui && npx vitest run src/routes/agents/graph/StepAdvanced.test.tsx`
Expected: FAIL — `Failed to resolve import "./StepAdvanced"`.

- [ ] **Step 3: Implement** — `ui/src/routes/agents/graph/StepAdvanced.tsx`:

```tsx
import { useState } from "react";
import type { ProviderStatus } from "../../../api/client";
import { StageFields, type StageValue } from "../StageFields";
import { updateNode, type GraphDraft, type GraphNode } from "./model";

type Fallback = { provider: string; model?: string; voice?: string };
type Stage = Record<string, unknown>;
const rec = (v: unknown): Stage => (v && typeof v === "object" && !Array.isArray(v) ? (v as Stage) : {});
const fallbacksOf = (stage: Stage): Fallback[] =>
  Array.isArray(stage.fallbacks) ? stage.fallbacks.map((f) => rec(f) as Fallback).filter((f) => typeof f.provider === "string") : [];
const without = (stage: Stage, key: string): Stage => {
  const { [key]: _drop, ...rest } = stage;
  return rest;
};

type Props = { graph: GraphDraft; step: GraphNode; providers: ProviderStatus[]; onChange: (g: GraphDraft) => void };

/** Optional depth for one step: fallbacks and tuning. Nothing here is needed to take calls. */
export function StepAdvanced({ graph, step, providers, onChange }: Props) {
  const llm = rec(step.config.llm);
  const tts = rec(step.config.tts);
  const setStage = (cap: "llm" | "tts", stage: Stage) => onChange(updateNode(graph, step.id, { config: { ...step.config, [cap]: stage } }));
  const setFallbacks = (cap: "llm" | "tts", stage: Stage, items: Fallback[]) =>
    setStage(cap, items.length ? { ...stage, fallbacks: items } : without(stage, "fallbacks"));
  const parallel = Number(llm.max_parallel) || 1;

  return (
    <>
      <FallbackList kind="llm" title="Model fallbacks" what="model" hint="Tried in order if the model stops answering during a call." items={fallbacksOf(llm)} providers={providers} idPrefix={`step-${step.id}-llm-fallback`} onChange={(items) => setFallbacks("llm", llm, items)} />
      <FallbackList kind="tts" title="Voice fallbacks" what="voice" hint="Tried in order if the voice stops answering." items={fallbacksOf(tts)} providers={providers} idPrefix={`step-${step.id}-tts-fallback`} onChange={(items) => setFallbacks("tts", tts, items)} />
      <fieldset className="stage-box">
        <legend>Tuning</legend>
        <label className="form-field" htmlFor={`step-${step.id}-parallel`}>
          <span>Tool calls at once</span>
          <input
            id={`step-${step.id}-parallel`}
            type="number"
            min={1}
            max={16}
            value={parallel}
            onChange={(e) => {
              const n = Math.min(16, Math.max(1, Math.round(Number(e.target.value) || 1)));
              setStage("llm", n > 1 ? { ...llm, max_parallel: n } : without(llm, "max_parallel"));
            }}
          />
          <span className="subtext">1 to 16. Higher is faster when one turn needs several lookups.</span>
        </label>
        <label className="check-row" htmlFor={`step-${step.id}-clean`}>
          <input
            id={`step-${step.id}-clean`}
            type="checkbox"
            checked={tts.clean_text !== false}
            onChange={(e) => setStage("tts", e.target.checked ? without(tts, "clean_text") : { ...tts, clean_text: false })}
          />
          <span>Clean text before speaking (strip markdown, links and emoji)</span>
        </label>
      </fieldset>
    </>
  );
}

type ListProps = {
  kind: "llm" | "tts";
  title: string;
  what: "model" | "voice";
  hint: string;
  items: Fallback[];
  providers: ProviderStatus[];
  idPrefix: string;
  onChange: (items: Fallback[]) => void;
};

function FallbackList({ kind, title, what, hint, items, providers, idPrefix, onChange }: ListProps) {
  const [draft, setDraft] = useState<StageValue>({ provider: "" });
  const ready = !!draft.provider && (kind === "tts" || !!(draft.model ?? "").trim());
  const add = () => {
    onChange([...items, kind === "tts" ? { provider: draft.provider, ...(draft.voice ? { voice: draft.voice } : {}) } : { provider: draft.provider, model: draft.model ?? "" }]);
    setDraft({ provider: "" });
  };
  return (
    <fieldset className="stage-box">
      <legend>{title}</legend>
      <p className="subtext">{hint}</p>
      {items.length > 0 && (
        <ol className="plain-list">
          {items.map((f, i) => (
            <li key={`${f.provider}-${f.model ?? f.voice ?? ""}-${i}`} className="row">
              <span className="grow">
                {f.provider}
                {(f.model ?? f.voice) ? ` · ${f.model ?? f.voice}` : ""}
              </span>
              <button type="button" className="action-button-small" onClick={() => onChange(items.filter((_, j) => j !== i))}>
                Remove {what} fallback {i + 1}
              </button>
            </li>
          ))}
        </ol>
      )}
      <StageFields kind={kind} providers={providers} idPrefix={idPrefix} value={draft} onChange={setDraft} />
      <button type="button" className="action-button-small" disabled={!ready} onClick={add}>
        Add {what} fallback
      </button>
    </fieldset>
  );
}
```

In `StepPanel.tsx` replace `{tab === "advanced" && <p className="subtext">Fallbacks and tuning.</p>}` with:

```tsx
        {tab === "advanced" && <StepAdvanced graph={graph} step={step} providers={providers} onChange={onChange} />}
```

and add `import { StepAdvanced } from "./StepAdvanced";`.

- [ ] **Step 4: Run the panel tests**

Run: `cd ui && npx vitest run src/routes/agents/graph/StepAdvanced.test.tsx src/routes/agents/graph/StepPanel.test.tsx src/test/noHardcodedColors.test.ts`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add ui/src/routes/agents/graph/StepAdvanced.tsx ui/src/routes/agents/graph/StepAdvanced.test.tsx ui/src/routes/agents/graph/StepPanel.tsx ui/src/test/noHardcodedColors.test.ts
git commit -m "feat(ui): step fallbacks and tuning on the Advanced tab

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 7: `FlowSettingsPanel`

**Files:**
- Create: `ui/src/routes/agents/graph/FlowSettingsPanel.tsx`
- Test: `ui/src/routes/agents/graph/FlowSettingsPanel.test.tsx`
- Modify: `ui/src/test/noHardcodedColors.test.ts` (add `"routes/agents/graph/FlowSettingsPanel.tsx"`)

**Interfaces:**
- Consumes: `SidePanel`, `StageFields`, `flowIssues`, `updateNode`, `GraphDraft`.
- Produces:

```ts
export type FlowSettingsPanelProps = {
  graph: GraphDraft;
  providers: ProviderStatus[];
  name: string;
  description: string;
  onName: (v: string) => void;
  onDescription: (v: string) => void;
  onChange: (g: GraphDraft) => void;
  onClose: () => void;
  returnFocusTo?: HTMLElement | null;
};
```

It edits the start step's call-level config (`stt`, `vad_stop_ms`, `context.max_messages`, `context.max_tokens`, `context.summarization.enabled`, `recording.enabled`, `recording.disclosure_confirmed`) and `graph.handoff_cue`. Labels: `Flow name`, `Description`, `STT provider`/`STT model`/`STT language` (from `StageFields`), `Wait after the caller stops (ms)`, `Max messages`, `Max tokens`, `Summarise older turns`, `Record calls`, `Callers are told the call is recorded`, `Sound name`, `Start after (ms)`.

- [ ] **Step 1: Write the failing test** — `ui/src/routes/agents/graph/FlowSettingsPanel.test.tsx`:

```tsx
import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import type { ProviderStatus } from "../../../api/client";
import { FlowSettingsPanel } from "./FlowSettingsPanel";
import { emptyFlow, fromAgentConfig, type GraphDraft } from "./model";

const svc = { available: true, custom: true, models: [{ id: "nova-3", default: true }], languages: [{ id: "en", default: true }] };
const providers: ProviderStatus[] = [
  { id: "deepgram", display_name: "Deepgram", capabilities: ["stt"], detected: true, env: [], services: { stt: svc } },
];
const two = fromAgentConfig({
  entry: "a",
  nodes: [
    { id: "a", name: "Reception", config: { llm: { provider: "g", model: "m" }, tts: { provider: "g" } } },
    { id: "b", name: "Billing", config: { llm: { provider: "g", model: "m" }, tts: { provider: "g" } } },
  ],
  edges: [{ id: "e1", from: "a", to: "b", condition: "invoice" }],
});

function Harness({ initial = two, onChange = vi.fn() }: { initial?: GraphDraft; onChange?: (g: GraphDraft) => void }) {
  const [g, setG] = useState(initial);
  const [name, setName] = useState("Front desk");
  return (
    <div style={{ position: "relative" }}>
      <FlowSettingsPanel graph={g} providers={providers} name={name} description="" onName={setName} onDescription={vi.fn()} onClose={vi.fn()} onChange={(n) => { setG(n); onChange(n); }} />
    </div>
  );
}
const startCfg = (fn: ReturnType<typeof vi.fn>) => {
  const g = fn.mock.calls.at(-1)![0] as GraphDraft;
  return g.nodes.find((n) => n.id === g.entry)!.config;
};

describe("FlowSettingsPanel", () => {
  it("says speech-to-text is missing, and sets it on the start step", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(<Harness onChange={onChange} />);
    expect(screen.getByRole("note")).toHaveTextContent("Choose speech-to-text in Flow settings.");
    await user.selectOptions(screen.getByLabelText("STT provider"), "deepgram");
    expect(startCfg(onChange).stt).toEqual({ provider: "deepgram", model: "nova-3", language: "en" });
    expect(screen.queryByRole("note")).toBeNull();
  });

  it("writes turn-taking and memory, and clears them when emptied", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(<Harness onChange={onChange} />);
    await user.type(screen.getByLabelText("Wait after the caller stops (ms)"), "480");
    expect(startCfg(onChange).vad_stop_ms).toBe(480);
    await user.clear(screen.getByLabelText("Wait after the caller stops (ms)"));
    expect(startCfg(onChange)).not.toHaveProperty("vad_stop_ms");
    await user.type(screen.getByLabelText("Max messages"), "40");
    await user.click(screen.getByLabelText("Summarise older turns"));
    expect(startCfg(onChange).context).toEqual({ max_messages: 40, summarization: { enabled: true } });
  });

  it("requires the recording disclosure once recording is on", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(<Harness onChange={onChange} />);
    expect(screen.getByLabelText("Callers are told the call is recorded")).toBeDisabled();
    await user.click(screen.getByLabelText("Record calls"));
    expect(screen.getByRole("note")).toHaveTextContent("Confirm that callers are told the call is recorded.");
    await user.click(screen.getByLabelText("Callers are told the call is recorded"));
    expect(startCfg(onChange).recording).toEqual({ enabled: true, disclosure_confirmed: true });
  });

  it("edits the route sound only when the flow has routes", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(<Harness onChange={onChange} />);
    await user.type(screen.getByLabelText("Sound name"), "chime");
    await user.type(screen.getByLabelText("Start after (ms)"), "50");
    expect((onChange.mock.calls.at(-1)![0] as GraphDraft).handoff_cue).toEqual({ name: "chime", after_ms: 50 });
  });

  it("asks for a step first on an empty flow, and renames the flow", async () => {
    const user = userEvent.setup();
    render(<Harness initial={emptyFlow()} />);
    expect(screen.getByText("Add a step first; these settings live on the start step.")).toBeVisible();
    expect(screen.queryByLabelText("Sound name")).toBeNull();
    await user.clear(screen.getByLabelText("Flow name"));
    await user.type(screen.getByLabelText("Flow name"), "After hours");
    expect(screen.getByLabelText("Flow name")).toHaveValue("After hours");
  });
});
```

- [ ] **Step 2: Run it to see it fail**

Run: `cd ui && npx vitest run src/routes/agents/graph/FlowSettingsPanel.test.tsx`
Expected: FAIL — `Failed to resolve import "./FlowSettingsPanel"`.

- [ ] **Step 3: Implement** — `ui/src/routes/agents/graph/FlowSettingsPanel.tsx`:

```tsx
import type { ProviderStatus } from "../../../api/client";
import { SidePanel } from "../../../components/SidePanel";
import { StageFields, type StageValue } from "../StageFields";
import { flowIssues, updateNode, type GraphDraft, type NodeConfig } from "./model";

type Rec = Record<string, unknown>;
const rec = (v: unknown): Rec => (v && typeof v === "object" && !Array.isArray(v) ? (v as Rec) : {});
const str = (v: unknown): string => (typeof v === "string" ? v : "");
const without = (o: Rec, key: string): Rec => {
  const { [key]: _drop, ...rest } = o;
  return rest;
};
/** An empty number field removes the key: the server default applies. */
const numberOrNothing = (raw: string): number | null => (raw.trim() === "" ? null : Math.round(Number(raw)));

export type FlowSettingsPanelProps = {
  graph: GraphDraft;
  providers: ProviderStatus[];
  name: string;
  description: string;
  onName: (v: string) => void;
  onDescription: (v: string) => void;
  onChange: (g: GraphDraft) => void;
  onClose: () => void;
  returnFocusTo?: HTMLElement | null;
};

/** What the whole call shares. Stored on the start step (server: call-level config). */
export function FlowSettingsPanel({ graph, providers, name, description, onName, onDescription, onChange, onClose, returnFocusTo }: FlowSettingsPanelProps) {
  const start = graph.nodes.find((n) => n.id === graph.entry);
  const cfg: NodeConfig = start?.config ?? {};
  const stt = rec(cfg.stt);
  const context = rec(cfg.context);
  const recording = rec(cfg.recording);
  const problems = flowIssues(graph);
  const setCfg = (next: NodeConfig) => start && onChange(updateNode(graph, start.id, { config: next }));
  const setKey = (key: string, value: unknown) => setCfg(value === null || value === undefined ? without(cfg, key) : { ...cfg, [key]: value });
  const setContext = (next: Rec) => setKey("context", Object.keys(next).length ? next : null);
  const cue = graph.handoff_cue;

  return (
    <SidePanel title="Flow settings" onClose={onClose} returnFocusTo={returnFocusTo}>
      {problems.length > 0 && (
        <div role="note" className="note note--bad">
          <ul className="plain-list">
            {problems.map((p) => (
              <li key={p}>{p}</li>
            ))}
          </ul>
        </div>
      )}
      <p className="subtext">Shared by every step for the whole call.</p>
      <label className="form-field" htmlFor="flow-name">
        <span>Flow name</span>
        <input id="flow-name" type="text" maxLength={128} value={name} onChange={(e) => onName(e.target.value)} />
      </label>
      <label className="form-field" htmlFor="flow-description">
        <span>Description</span>
        <textarea id="flow-description" rows={2} maxLength={1024} value={description} onChange={(e) => onDescription(e.target.value)} />
      </label>

      {!start ? (
        <p className="subtext">Add a step first; these settings live on the start step.</p>
      ) : (
        <>
          <fieldset className="stage-box">
            <legend>Listening</legend>
            <StageFields
              kind="stt"
              providers={providers}
              idPrefix="flow-stt"
              value={{ provider: str(stt.provider), model: str(stt.model), language: str(stt.language) }}
              onChange={(v: StageValue) => setKey("stt", { ...stt, provider: v.provider, model: v.model ?? "", language: v.language ?? "" })}
            />
          </fieldset>
          <fieldset className="stage-box">
            <legend>Turn-taking</legend>
            <label className="form-field" htmlFor="flow-vad">
              <span>Wait after the caller stops (ms)</span>
              <input
                id="flow-vad"
                type="number"
                min={200}
                max={2000}
                placeholder="Server default"
                value={typeof cfg.vad_stop_ms === "number" ? cfg.vad_stop_ms : ""}
                onChange={(e) => setKey("vad_stop_ms", numberOrNothing(e.target.value))}
              />
              <span className="subtext">200 to 2000. Shorter replies sooner; longer interrupts the caller less.</span>
            </label>
          </fieldset>
          <fieldset className="stage-box">
            <legend>Memory</legend>
            <div className="grid-2">
              <label className="form-field" htmlFor="flow-max-messages">
                <span>Max messages</span>
                <input
                  id="flow-max-messages"
                  type="number"
                  min={2}
                  placeholder="Server default"
                  value={typeof context.max_messages === "number" ? context.max_messages : ""}
                  onChange={(e) => {
                    const n = numberOrNothing(e.target.value);
                    setContext(n === null ? without(context, "max_messages") : { ...context, max_messages: n });
                  }}
                />
              </label>
              <label className="form-field" htmlFor="flow-max-tokens">
                <span>Max tokens</span>
                <input
                  id="flow-max-tokens"
                  type="number"
                  min={1}
                  placeholder="Server default"
                  value={typeof context.max_tokens === "number" ? context.max_tokens : ""}
                  onChange={(e) => {
                    const n = numberOrNothing(e.target.value);
                    setContext(n === null ? without(context, "max_tokens") : { ...context, max_tokens: n });
                  }}
                />
              </label>
            </div>
            <label className="check-row" htmlFor="flow-summarise">
              <input
                id="flow-summarise"
                type="checkbox"
                checked={rec(context.summarization).enabled === true}
                onChange={(e) => setContext(e.target.checked ? { ...context, summarization: { enabled: true } } : without(context, "summarization"))}
              />
              <span>Summarise older turns</span>
            </label>
          </fieldset>
          <fieldset className="stage-box">
            <legend>Recording</legend>
            <label className="check-row" htmlFor="flow-record">
              <input
                id="flow-record"
                type="checkbox"
                checked={recording.enabled === true}
                onChange={(e) => setKey("recording", e.target.checked ? { enabled: true, disclosure_confirmed: recording.disclosure_confirmed === true } : null)}
              />
              <span>Record calls</span>
            </label>
            <label className="check-row" htmlFor="flow-disclosure">
              <input
                id="flow-disclosure"
                type="checkbox"
                disabled={recording.enabled !== true}
                checked={recording.disclosure_confirmed === true}
                onChange={(e) => setKey("recording", { enabled: true, disclosure_confirmed: e.target.checked })}
              />
              <span>Callers are told the call is recorded</span>
            </label>
          </fieldset>
        </>
      )}

      {graph.edges.length > 0 && (
        <fieldset className="stage-box">
          <legend>Route sound</legend>
          <p className="subtext">Plays while the caller moves between steps. Sounds are set up on the server (audio.cues).</p>
          <div className="grid-2">
            <label className="form-field" htmlFor="cue-name">
              <span>Sound name</span>
              <input
                id="cue-name"
                type="text"
                placeholder="none"
                value={cue?.name ?? ""}
                onChange={(e) => {
                  const v = e.target.value.trim();
                  const { handoff_cue: _drop, ...rest } = graph;
                  onChange(v ? { ...rest, handoff_cue: { ...(cue ?? {}), name: v } } : rest);
                }}
              />
            </label>
            <label className="form-field" htmlFor="cue-after">
              <span>Start after (ms)</span>
              <input
                id="cue-after"
                type="number"
                min={0}
                max={10000}
                disabled={!cue}
                value={cue?.after_ms ?? ""}
                onChange={(e) => {
                  if (!cue) return;
                  const n = numberOrNothing(e.target.value);
                  onChange({ ...graph, handoff_cue: { name: cue.name, ...(n && n > 0 ? { after_ms: n } : {}) } });
                }}
              />
            </label>
          </div>
        </fieldset>
      )}
    </SidePanel>
  );
}
```

- [ ] **Step 4: Run it to see it pass**

Run: `cd ui && npx vitest run src/routes/agents/graph/FlowSettingsPanel.test.tsx src/test/noHardcodedColors.test.ts`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add ui/src/routes/agents/graph/FlowSettingsPanel.tsx ui/src/routes/agents/graph/FlowSettingsPanel.test.tsx ui/src/test/noHardcodedColors.test.ts
git commit -m "feat(ui): flow settings panel for listening, turn-taking, memory, recording and route sound

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 8: What changes, and the publish dialog

**Files:**
- Create: `ui/src/routes/agents/graph/diff.ts`, `ui/src/routes/agents/graph/PublishDialog.tsx`
- Test: `ui/src/routes/agents/graph/diff.test.ts`, `ui/src/routes/agents/graph/PublishDialog.test.tsx`
- Modify: `ui/src/styles.css`, `ui/src/test/noHardcodedColors.test.ts` (add `"routes/agents/graph/PublishDialog.tsx"`)

**Interfaces:**
- Produces:
  - `describeChanges(before: GraphDraft | null, after: GraphDraft): string[]` (empty for `null`).
  - `PublishDialog(props: { flowName: string; version: number; firstVersion: boolean; changes: string[]; problems: string[]; pending: boolean; onConfirm: () => void; onCancel: () => void })`. A `role="dialog"` named `Publish <name> v<n>?`. It focuses its heading, Escape cancels, and the confirm button `Publish v<n>` is disabled while there are problems or while pending.

- [ ] **Step 1: Write the failing tests**

`ui/src/routes/agents/graph/diff.test.ts`:

```ts
import { describe, expect, it } from "vitest";
import { describeChanges } from "./diff";
import { addDraftRoute, addNode, fromAgentConfig, setEntry, updateEdge, updateNode } from "./model";

const ready = { llm: { provider: "google", model: "gemini", system_prompt: "Hi." }, tts: { provider: "google", voice: "aoede" } };
const base = fromAgentConfig({
  entry: "a",
  nodes: [
    { id: "a", name: "Reception", config: { stt: { provider: "google" }, ...ready } },
    { id: "b", name: "Billing", config: ready },
  ],
  edges: [{ id: "e1", from: "a", to: "b", condition: "invoice" }],
});

describe("describeChanges", () => {
  it("says nothing for a first version or an unchanged draft", () => {
    expect(describeChanges(null, base)).toEqual([]);
    expect(describeChanges(base, base)).toEqual([]);
  });

  it("names step, model, voice, instruction and route changes", () => {
    let g = updateNode(base, "b", { name: "Accounts", config: { ...ready, llm: { ...ready.llm, model: "flash", system_prompt: "Bills." }, tts: { provider: "google", voice: "puck" } } });
    g = updateEdge(g, "e1", "asks about a bill");
    g = addNode(g, "Support");
    g = addDraftRoute(g, "a", g.nodes[2].id);
    g = updateEdge(g, g.edges[1].id, "has a fault");
    expect(describeChanges(base, g)).toEqual([
      "Added step Support",
      "Renamed Billing to Accounts",
      "Accounts: model gemini → flash",
      "Accounts: voice aoede → puck",
      "Accounts: instructions changed",
      "Route Reception → Accounts: condition changed",
      "New route Reception → Support",
    ]);
  });

  it("reports removed steps and routes, a new start step and flow settings", () => {
    let g = { ...base, edges: [] };
    g = updateNode(g, "a", { config: { ...g.nodes[0].config, vad_stop_ms: 480 } });
    g = setEntry(g, "b");
    expect(describeChanges(base, g)).toEqual(["Removed route Reception → Billing", "Start step is now Billing", "Flow settings: turn-taking changed"]);
  });
});
```

`ui/src/routes/agents/graph/PublishDialog.test.tsx`:

```tsx
import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { PublishDialog } from "./PublishDialog";

const props = { flowName: "Front desk", version: 8, firstVersion: false, changes: ["Added step Support"], problems: [] as string[], pending: false, onConfirm: vi.fn(), onCancel: vi.fn() };

describe("PublishDialog", () => {
  it("explains what publishing does and lists the changes", async () => {
    const user = userEvent.setup();
    const onConfirm = vi.fn();
    render(<PublishDialog {...props} onConfirm={onConfirm} />);
    const dialog = screen.getByRole("dialog", { name: "Publish Front desk v8?" });
    expect(screen.getByRole("heading", { name: "Publish Front desk v8?" })).toHaveFocus();
    expect(dialog).toHaveTextContent("Calls already in progress finish on the version they started with.");
    expect(dialog).toHaveTextContent("Added step Support");
    await user.click(screen.getByRole("button", { name: "Publish v8" }));
    expect(onConfirm).toHaveBeenCalledOnce();
  });

  it("blocks publishing while there are problems", () => {
    render(<PublishDialog {...props} problems={["The route from Reception to Billing needs a condition."]} />);
    expect(screen.getByRole("alert")).toHaveTextContent("needs a condition");
    expect(screen.getByRole("button", { name: "Publish v8" })).toBeDisabled();
  });

  it("cancels with Escape or Keep editing", async () => {
    const user = userEvent.setup();
    const onCancel = vi.fn();
    render(<PublishDialog {...props} onCancel={onCancel} />);
    await user.keyboard("{Escape}");
    await user.click(screen.getByRole("button", { name: "Keep editing" }));
    expect(onCancel).toHaveBeenCalledTimes(2);
  });

  it("says when this is the first version", () => {
    render(<PublishDialog {...props} version={1} firstVersion changes={[]} />);
    expect(screen.getByText("This is the first version of this flow.")).toBeVisible();
  });
});
```

- [ ] **Step 2: Run them to see them fail**

Run: `cd ui && npx vitest run src/routes/agents/graph/diff.test.ts src/routes/agents/graph/PublishDialog.test.tsx`
Expected: FAIL — modules not found.

- [ ] **Step 3: Implement**

`ui/src/routes/agents/graph/diff.ts`:

```ts
import type { GraphDraft, GraphEdge } from "./model";

type Rec = Record<string, unknown>;
const rec = (v: unknown): Rec => (v && typeof v === "object" && !Array.isArray(v) ? (v as Rec) : {});
const s = (v: unknown): string => (typeof v === "string" ? v : "");
const same = (a: unknown, b: unknown) => JSON.stringify(a ?? null) === JSON.stringify(b ?? null);
const FLOW_SETTINGS = [
  ["stt", "listening"],
  ["vad_stop_ms", "turn-taking"],
  ["context", "memory"],
  ["recording", "recording"],
] as const;

/** Plain-language changes from the published version to the draft, for the publish dialog. */
export function describeChanges(before: GraphDraft | null, after: GraphDraft): string[] {
  if (!before) return [];
  const out: string[] = [];
  const nameIn = (g: GraphDraft, id: string) => g.nodes.find((n) => n.id === id)?.name.trim() || "Untitled step";
  const old = new Map(before.nodes.map((n) => [n.id, n]));
  const now = new Map(after.nodes.map((n) => [n.id, n]));

  for (const n of after.nodes) if (!old.has(n.id)) out.push(`Added step ${n.name}`);
  for (const n of before.nodes) if (!now.has(n.id)) out.push(`Removed step ${n.name}`);
  for (const n of after.nodes) {
    const o = old.get(n.id);
    if (!o) continue;
    const [ol, nl, ot, nt] = [rec(o.config.llm), rec(n.config.llm), rec(o.config.tts), rec(n.config.tts)];
    if (o.name !== n.name) out.push(`Renamed ${o.name} to ${n.name}`);
    if (s(ol.provider) !== s(nl.provider) || s(ol.model) !== s(nl.model)) out.push(`${n.name}: model ${s(ol.model) || "none"} → ${s(nl.model) || "none"}`);
    if (s(ot.provider) !== s(nt.provider) || s(ot.voice) !== s(nt.voice))
      out.push(`${n.name}: voice ${s(ot.voice) || s(ot.provider) || "none"} → ${s(nt.voice) || s(nt.provider) || "none"}`);
    if (s(ol.system_prompt) !== s(nl.system_prompt)) out.push(`${n.name}: instructions changed`);
    if (!same(ol.fallbacks, nl.fallbacks) || !same(ot.fallbacks, nt.fallbacks) || !same(ol.max_parallel, nl.max_parallel) || !same(ot.clean_text, nt.clean_text))
      out.push(`${n.name}: advanced settings changed`);
    if (!same([...o.tools].sort(), [...n.tools].sort())) out.push(`${n.name}: tools changed`);
  }

  const key = (e: GraphEdge) => `${e.from}>${e.to}`;
  const oldEdges = new Map(before.edges.map((e) => [key(e), e]));
  const newEdges = new Map(after.edges.map((e) => [key(e), e]));
  for (const e of after.edges) {
    const o = oldEdges.get(key(e));
    if (!o) out.push(`New route ${nameIn(after, e.from)} → ${nameIn(after, e.to)}`);
    else if (o.condition !== e.condition) out.push(`Route ${nameIn(after, e.from)} → ${nameIn(after, e.to)}: condition changed`);
  }
  for (const e of before.edges) if (!newEdges.has(key(e))) out.push(`Removed route ${nameIn(before, e.from)} → ${nameIn(before, e.to)}`);

  if (before.entry !== after.entry) out.push(`Start step is now ${nameIn(after, after.entry)}`);
  const oldStart = rec(old.get(before.entry)?.config);
  const newStart = rec(now.get(after.entry)?.config);
  for (const [k, label] of FLOW_SETTINGS) if (!same(oldStart[k], newStart[k])) out.push(`Flow settings: ${label} changed`);
  if (!same(before.handoff_cue, after.handoff_cue)) out.push("Flow settings: route sound changed");
  return out;
}
```

The order in the Task 8 test (renames and edits before the new route) follows this code: added steps, removed steps, per-step changes in `after` order, then routes, start step and flow settings. If the test's expected array disagrees with a correct implementation, fix the test's order, not the code.

`ui/src/routes/agents/graph/PublishDialog.tsx`:

```tsx
import { useEffect, useId, useRef } from "react";

export type PublishDialogProps = {
  flowName: string;
  version: number;
  firstVersion: boolean;
  changes: string[];
  problems: string[];
  pending: boolean;
  onConfirm: () => void;
  onCancel: () => void;
};

/** Publishing is the one irreversible step: say what changes and what happens to live calls. */
export function PublishDialog({ flowName, version, firstVersion, changes, problems, pending, onConfirm, onCancel }: PublishDialogProps) {
  const id = useId();
  const heading = useRef<HTMLHeadingElement>(null);
  useEffect(() => heading.current?.focus(), []);
  const blocked = problems.length > 0;
  return (
    <div className="dialog-backdrop">
      <div
        role="dialog"
        aria-modal="true"
        aria-labelledby={id}
        className="dialog"
        onKeyDown={(e) => {
          if (e.key === "Escape") onCancel();
        }}
      >
        <h2 id={id} ref={heading} tabIndex={-1}>
          Publish {flowName.trim() || "this flow"} v{version}?
        </h2>
        <p>New calls get v{version} straight away. Calls already in progress finish on the version they started with.</p>
        {blocked ? (
          <div role="alert" className="note note--bad">
            <p>Fix these before publishing:</p>
            <ul className="plain-list">
              {problems.map((p) => (
                <li key={p}>{p}</li>
              ))}
            </ul>
          </div>
        ) : firstVersion ? (
          <p className="subtext">This is the first version of this flow.</p>
        ) : changes.length ? (
          <section aria-labelledby={`${id}-changes`}>
            <h3 id={`${id}-changes`}>What changes</h3>
            <ul className="plain-list">
              {changes.map((c, i) => (
                <li key={i}>{c}</li>
              ))}
            </ul>
          </section>
        ) : (
          <p className="subtext">No changes since v{version - 1}.</p>
        )}
        <div className="row row--end">
          <button type="button" className="action-button-small" onClick={onCancel}>
            Keep editing
          </button>
          <button type="button" className="action-button" disabled={blocked || pending} onClick={onConfirm}>
            {pending ? "Publishing…" : `Publish v${version}`}
          </button>
        </div>
      </div>
    </div>
  );
}
```

Append to `styles.css`:

```css
.dialog-backdrop {
  position: fixed;
  inset: 0;
  z-index: 20;
  display: flex;
  align-items: flex-start;
  justify-content: center;
  padding: 64px 16px;
  background: color-mix(in srgb, var(--ink) 50%, transparent);
}
.dialog {
  width: min(640px, 100%);
  display: flex;
  flex-direction: column;
  gap: 14px;
  padding: 24px;
  background: var(--surface);
  border-radius: var(--radius);
}
.dialog h2 {
  margin: 0;
}
```

- [ ] **Step 4: Run them to see them pass**

Run: `cd ui && npx vitest run src/routes/agents/graph/diff.test.ts src/routes/agents/graph/PublishDialog.test.tsx src/test/noHardcodedColors.test.ts`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add ui/src/routes/agents/graph/diff.ts ui/src/routes/agents/graph/diff.test.ts ui/src/routes/agents/graph/PublishDialog.tsx ui/src/routes/agents/graph/PublishDialog.test.tsx ui/src/styles.css ui/src/test/noHardcodedColors.test.ts
git commit -m "feat(ui): publish dialog that lists what changes and what happens to live calls

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 9: `FlowEditor` page

**Files:**
- Create: `ui/src/routes/agents/graph/FlowEditor.tsx`
- Test: `ui/src/routes/agents/graph/FlowEditor.test.tsx`
- Modify: `ui/src/styles.css`, `ui/src/test/noHardcodedColors.test.ts` (add `"routes/agents/graph/FlowEditor.tsx"`)

**Interfaces:**
- Consumes: everything from Tasks 1–8; `Canvas`, `AgentVoiceTest` (`hideGraph`, `onCall`), `Status`.
- Produces:

```ts
export type FlowEditorProps = {
  agent: AgentDTO | null;
  providers: ProviderStatus[];
  client: ApiClient;
  keyValue: string | null;
  pending: boolean;
  onSubmit: (input: CreateAgentInput) => void;
  onCancel: () => void;
};
export function FlowEditor(props: FlowEditorProps): JSX.Element;
```

The heading is an `h2` with id `flow-editor-title`. Buttons: `Flow settings`, `Test call`, `Publish`, `Close`, `Add step` / `Add first step`.

- [ ] **Step 1: Write the failing test** — `ui/src/routes/agents/graph/FlowEditor.test.tsx`:

```tsx
import { describe, expect, it, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ReactFlowProvider } from "@xyflow/react";
import type { AgentDTO, ApiClient, ProviderStatus } from "../../../api/client";
import { FlowEditor, type FlowEditorProps } from "./FlowEditor";

const svc = { available: true, custom: true, models: [{ id: "gemini", default: true }], voices: [{ id: "aoede", default: true }], languages: [{ id: "en", default: true }] };
const providers: ProviderStatus[] = [
  { id: "google", display_name: "Google", capabilities: ["llm", "stt", "tts"], detected: true, env: [], services: { stt: svc, llm: svc, tts: svc } },
];
const flat = { stt: { provider: "google", model: "gemini" }, llm: { provider: "google", model: "gemini", system_prompt: "Front desk." }, tts: { provider: "google", voice: "aoede" } };
const agent: AgentDTO = {
  id: "a1", name: "Front desk", description: "", enabled: true, archived: false,
  current_version: { id: "v1", number: 1, schema_version: 1, config: flat, config_hash: "h", created_at: "", mcp_tools: [] },
  readiness: { state: "ready", reasons: [] }, created_at: "", updated_at: "",
};
const client = { issueVoiceToken: vi.fn(), getSessionTraces: vi.fn() } as unknown as ApiClient;

function setup(props: Partial<FlowEditorProps> = {}) {
  const onSubmit = vi.fn();
  render(
    <QueryClientProvider client={new QueryClient()}>
      <ReactFlowProvider>
        <FlowEditor agent={null} providers={providers} client={client} keyValue="k" pending={false} onSubmit={onSubmit} onCancel={vi.fn()} {...props} />
      </ReactFlowProvider>
    </QueryClientProvider>,
  );
  return { onSubmit, user: userEvent.setup() };
}

describe("FlowEditor", () => {
  it("starts a new flow blank and opens the first step's panel on Basics, name focused", async () => {
    const { user } = setup();
    expect(screen.getByRole("heading", { name: "New flow" })).toBeVisible();
    expect(screen.getByRole("status")).toHaveTextContent("No steps yet");
    await user.click(screen.getByRole("button", { name: "Add first step" }));
    expect(screen.getByRole("complementary", { name: "New step" })).toBeVisible();
    expect(screen.getByLabelText("Step name")).toHaveFocus();
    expect(screen.getByRole("status")).toHaveTextContent("1 step needs setup");
  });

  it("cannot test a flow that was never published", () => {
    setup();
    expect(screen.getByRole("button", { name: "Test call" })).toBeDisabled();
  });

  it("blocks publish until every step and the flow are set up, then sends the graph", async () => {
    const { user, onSubmit } = setup();
    await user.click(screen.getByRole("button", { name: "Add first step" }));
    await user.click(screen.getByRole("button", { name: "Publish" }));
    const dialog = screen.getByRole("dialog");
    expect(within(dialog).getByRole("alert")).toBeVisible();
    expect(within(dialog).getByRole("button", { name: "Publish v1" })).toBeDisabled();
    await user.click(within(dialog).getByRole("button", { name: "Keep editing" }));

    const step = screen.getByRole("complementary", { name: "New step" });
    await user.selectOptions(within(step).getByLabelText("LLM provider"), "google");
    await user.selectOptions(within(step).getByLabelText("TTS provider"), "google");
    await user.click(screen.getByRole("button", { name: "Flow settings" }));
    await user.type(screen.getByLabelText("Flow name"), "After hours");
    await user.selectOptions(screen.getByLabelText("STT provider"), "google");
    await user.click(screen.getByRole("button", { name: "Publish" }));
    await user.click(screen.getByRole("button", { name: "Publish v1" }));
    expect(onSubmit).toHaveBeenCalledWith(expect.objectContaining({ name: "After hours", config: expect.objectContaining({ llm: expect.objectContaining({ provider: "google" }) }) }));
  });

  it("says the test call runs the published version while the draft differs", async () => {
    const { user } = setup({ agent });
    await user.click(screen.getByText("main"));
    await user.type(screen.getByLabelText("Instructions"), " Be brief.");
    await user.click(screen.getByRole("button", { name: "Test call" }));
    const panel = screen.getByRole("complementary", { name: "Test call" });
    expect(within(panel).getByRole("note")).toHaveTextContent("This call runs v1, the published version. Publish to hear your changes.");
    expect(within(panel).getByTestId("start-voice-test-btn")).toBeVisible();
  });

  it("lists the changes since the published version in the publish dialog", async () => {
    const { user } = setup({ agent });
    await user.click(screen.getByRole("button", { name: "Add step" }));
    await user.click(screen.getByRole("button", { name: "Publish" }));
    expect(screen.getByRole("dialog", { name: "Publish Front desk v2?" })).toHaveTextContent("Added step New step");
  });
});
```

- [ ] **Step 2: Run it to see it fail**

Run: `cd ui && npx vitest run src/routes/agents/graph/FlowEditor.test.tsx`
Expected: FAIL — `Failed to resolve import "./FlowEditor"`.

- [ ] **Step 3: Implement** — `ui/src/routes/agents/graph/FlowEditor.tsx`:

```tsx
import { useMemo, useRef, useState } from "react";
import type { AgentDTO, ApiClient, CreateAgentInput, ProviderStatus } from "../../../api/client";
import { SidePanel } from "../../../components/SidePanel";
import { Status } from "../../../components/primitives";
import { AgentVoiceTest } from "../AgentVoiceTest";
import { Canvas, type Selection } from "./Canvas";
import { describeChanges } from "./diff";
import { FlowSettingsPanel } from "./FlowSettingsPanel";
import {
  MAX_NODES,
  addDraftRoute,
  addNode,
  emptyFlow,
  flowIssues,
  fromAgentConfig,
  stepIssues,
  toPayload,
  uniqueName,
  updateNode,
  validate,
  type GraphDraft,
  type StepTab,
} from "./model";
import { PublishDialog } from "./PublishDialog";
import { StepPanel } from "./StepPanel";
import { NO_CALL, useCallView, type CallInfo } from "./useCallView";

type Panel = { kind: "step"; id: string } | { kind: "settings" } | { kind: "call" } | null;

const sortKeys = (_k: string, v: unknown) =>
  v && typeof v === "object" && !Array.isArray(v) ? Object.fromEntries(Object.entries(v).sort(([a], [b]) => (a < b ? -1 : 1))) : v;
// The server stores configs key-sorted; compare that way so key order is not a "change".
const canonical = (v: unknown) => JSON.stringify(v, sortKeys);

export type FlowEditorProps = {
  agent: AgentDTO | null;
  providers: ProviderStatus[];
  client: ApiClient;
  keyValue: string | null;
  pending: boolean;
  onSubmit: (input: CreateAgentInput) => void;
  onCancel: () => void;
};

/** One page for a flow: the graph stays put; steps, settings and test calls slide in from the right. */
export function FlowEditor({ agent, providers, client, keyValue, pending, onSubmit, onCancel }: FlowEditorProps) {
  const flowTools = useMemo(() => (agent?.current_version?.mcp_tools ?? []).map((t) => `${t.mcp_server_id}/${t.remote_name}`), [agent]);
  const published = useMemo(() => (agent?.current_version ? fromAgentConfig(agent.current_version.config, flowTools) : null), [agent, flowTools]);
  const [name, setName] = useState(agent?.name ?? "");
  const [description, setDescription] = useState(agent?.description ?? "");
  const [graph, setGraph] = useState<GraphDraft>(() => published ?? emptyFlow());
  const [panel, setPanel] = useState<Panel>(null);
  const [tab, setTab] = useState<StepTab>("basics");
  const [focusRoute, setFocusRoute] = useState<string | undefined>();
  const [freshStep, setFreshStep] = useState<string | null>(null);
  const [wide, setWide] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const [call, setCall] = useState<CallInfo>(NO_CALL);
  const opener = useRef<HTMLElement | null>(null);

  const version = agent?.current_version?.number ?? 0;
  const unpublished = !published || canonical(toPayload(published)) !== canonical(toPayload(graph));
  const needSetup = graph.nodes.filter((n) => stepIssues(graph, n.id).length > 0).length;
  const flowProblems = flowIssues(graph);
  const problems = [...(name.trim() ? [] : ["Give the flow a name in Flow settings."]), ...validate(graph)];
  const callable = !!agent && (agent.readiness.state === "ready" || agent.readiness.state === "ready_with_tool_warning");
  // The call runs the published version, so the live view follows that graph.
  const live = useCallView(published ?? graph, call, client);

  const state: { tone: "neutral" | "good" | "bad"; text: string } =
    graph.nodes.length === 0 ? { tone: "neutral", text: "No steps yet" }
    : needSetup > 0 ? { tone: "bad", text: `${needSetup} ${needSetup === 1 ? "step needs" : "steps need"} setup` }
    : flowProblems.length > 0 ? { tone: "bad", text: flowProblems[0] }
    : { tone: "good", text: unpublished ? "Ready to publish" : "Published" };

  function open(next: Panel, from?: HTMLElement | null) {
    opener.current = from ?? (document.activeElement as HTMLElement | null);
    setPanel(next);
  }
  function close() {
    setPanel(null);
    setFocusRoute(undefined);
    setFreshStep(null);
  }
  /** Lands on the first tab with a problem; re-selecting the same step keeps the current tab. */
  function openStep(g: GraphDraft, id: string, forced?: StepTab) {
    const issues = stepIssues(g, id);
    const sameStep = panel?.kind === "step" && panel.id === id;
    setTab(forced ?? issues[0]?.tab ?? (sameStep ? tab : "basics"));
    if (!forced) setFocusRoute(undefined);
    setFreshStep(null);
    open({ kind: "step", id });
  }
  function addStep() {
    const next = addNode(graph, uniqueName(graph, "New step"));
    if (next === graph) return;
    const id = next.nodes[next.nodes.length - 1].id;
    setGraph(next);
    openStep(next, id, "basics");
    setFreshStep(id);
  }
  function drawRoute(from: string, to: string) {
    const next = addDraftRoute(graph, from, to);
    if (next === graph) return;
    setGraph(next);
    setFocusRoute(next.edges[next.edges.length - 1].id);
    openStep(next, from, "routes");
  }
  function select(s: Selection) {
    if (call.live) return;
    if (s?.kind === "node") openStep(graph, s.id);
    else if (s?.kind === "edge") {
      const e = graph.edges.find((x) => x.id === s.id);
      if (e) {
        setFocusRoute(e.id);
        openStep(graph, e.from, "routes");
      }
    } else if (panel?.kind === "step") close();
  }
  function publish() {
    onSubmit({
      name: name.trim(),
      description: description.trim(),
      enabled: agent?.enabled ?? true,
      config: toPayload(graph) as CreateAgentInput["config"],
    });
    setConfirming(false);
  }

  const stageClass = ["flow-editor__stage", panel ? "flow-editor__stage--panel" : "", panel?.kind === "step" && wide ? "flow-editor__stage--wide" : ""].join(" ").trim();

  return (
    <div className="flow-editor">
      <header className="flow-editor__head">
        <h2 id="flow-editor-title" tabIndex={-1}>
          {name.trim() || "New flow"}
        </h2>
        <span className="chip">{agent ? `v${version}${unpublished ? " · unpublished changes" : ""}` : "Never published"}</span>
        <Status tone={state.tone}>{state.text}</Status>
        <span className="grow" />
        <button type="button" className="action-button-small" aria-pressed={panel?.kind === "settings"} onClick={(e) => open({ kind: "settings" }, e.currentTarget)}>
          Flow settings
        </button>
        <button
          type="button"
          className="action-button-small"
          disabled={!callable}
          title={!agent ? "Publish once to test" : !callable ? "This flow is not ready to take calls" : undefined}
          onClick={(e) => open({ kind: "call" }, e.currentTarget)}
        >
          Test call
        </button>
        <button type="button" className="action-button" disabled={pending} onClick={() => setConfirming(true)}>
          {pending ? "Publishing…" : "Publish"}
        </button>
        <button type="button" className="action-button-small" onClick={onCancel}>
          Close
        </button>
      </header>

      <div className="flow-editor__toolbar">
        {graph.nodes.length > 0 && (
          <button type="button" className="action-button-small" disabled={graph.nodes.length >= MAX_NODES || call.live} title={graph.nodes.length >= MAX_NODES ? `A flow has at most ${MAX_NODES} steps` : undefined} onClick={addStep}>
            Add step
          </button>
        )}
        <span className="subtext">
          {graph.nodes.length} of {MAX_NODES} steps
        </span>
      </div>

      <div className={stageClass}>
        {graph.nodes.length === 0 ? (
          <div className="flow-editor__blank">
            <h3>Start with one step.</h3>
            <p>A step answers the caller. Give it instructions, a model and a voice, then add more steps and routes when you need them.</p>
            <button type="button" className="action-button" onClick={addStep}>
              Add first step
            </button>
          </div>
        ) : (
          <Canvas
            graph={graph}
            height={600}
            selected={panel?.kind === "step" ? { kind: "node", id: panel.id } : null}
            onSelect={select}
            onMove={(id, position) => setGraph((g) => updateNode(g, id, { position }))}
            onConnect={drawRoute}
            visual={call.live ? live.view.nodes : undefined}
            traversedEdges={call.live ? live.traversedEdges : undefined}
            justHandedEdge={live.justHanded}
          />
        )}

        {panel?.kind === "step" && (
          <StepPanel
            key={panel.id}
            graph={graph}
            stepId={panel.id}
            providers={providers}
            flowTools={flowTools}
            tab={tab}
            onTab={setTab}
            focusRoute={focusRoute}
            onFocusRoute={setFocusRoute}
            focusName={freshStep === panel.id}
            wide={wide}
            onToggleWide={() => setWide((w) => !w)}
            onChange={setGraph}
            onClose={close}
            returnFocusTo={opener.current}
          />
        )}
        {panel?.kind === "settings" && (
          <FlowSettingsPanel
            graph={graph}
            providers={providers}
            name={name}
            description={description}
            onName={setName}
            onDescription={setDescription}
            onChange={setGraph}
            onClose={close}
            returnFocusTo={opener.current}
          />
        )}
        {panel?.kind === "call" && agent && (
          <SidePanel title="Test call" onClose={close} returnFocusTo={opener.current}>
            {unpublished && (
              <p role="note" className="note">
                This call runs v{version}, the published version. Publish to hear your changes.
              </p>
            )}
            <AgentVoiceTest agent={agent} client={client} keyValue={keyValue} hideGraph onCall={setCall} />
          </SidePanel>
        )}
      </div>

      {confirming && (
        <PublishDialog
          flowName={name}
          version={version + 1}
          firstVersion={!published}
          changes={describeChanges(published, graph)}
          problems={problems}
          pending={pending}
          onConfirm={publish}
          onCancel={() => setConfirming(false)}
        />
      )}
    </div>
  );
}
```

Append to `styles.css`:

```css
.flow-editor {
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.flow-editor__head {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 10px 14px;
}
.flow-editor__head h2 {
  margin: 0;
}
.flow-editor__toolbar {
  display: flex;
  align-items: center;
  gap: 10px;
}
.flow-editor__stage {
  position: relative;
  min-height: 600px;
  border: 1px solid var(--rule);
  border-radius: var(--radius);
  overflow: hidden;
}
.flow-editor__stage--panel .agent-canvas {
  margin-right: min(440px, 100%);
}
.flow-editor__stage--wide .agent-canvas {
  margin-right: min(720px, 100%);
}
.flow-editor__blank {
  min-height: 600px;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 12px;
  padding: 24px;
  text-align: center;
}
.flow-editor__blank p {
  max-width: 44ch;
  margin: 0;
  color: var(--ink-2);
}
.chip {
  padding: 2px 8px;
  border: 1px solid var(--rule);
  border-radius: var(--radius);
  font-size: 13px;
  font-variant-numeric: tabular-nums;
}
```

- [ ] **Step 4: Run it to see it pass**

Run: `cd ui && npx vitest run src/routes/agents/graph/FlowEditor.test.tsx src/test/noHardcodedColors.test.ts`
Expected: PASS. (React Flow renders node text in jsdom; `ui/src/test/setup.ts` stubs `ResizeObserver` and `DOMMatrixReadOnly`. If clicking a node by text does not reach React Flow's `onNodeClick` in jsdom, select the step through the Routes/keyboard path instead and note it in the commit message; do not add a test-only prop.)

- [ ] **Step 5: Commit**

```bash
git add ui/src/routes/agents/graph/FlowEditor.tsx ui/src/routes/agents/graph/FlowEditor.test.tsx ui/src/styles.css ui/src/test/noHardcodedColors.test.ts
git commit -m "feat(ui): flow editor with step, settings and test-call panels over the graph

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 10: Wire the editor in, retire the old designer, rename the nav

**Files:**
- Modify: `ui/src/routes/agents/AgentsView.tsx`, `ui/src/routes/sections.tsx`, `ui/src/app/App.test.tsx`, `ui/src/routes/agents/AgentsView.test.tsx` (only expectations that name the old designer), `ui/src/test/noHardcodedColors.test.ts` (remove the two deleted files)
- Delete: `ui/src/routes/agents/graph/GraphDesigner.tsx`, `ui/src/routes/agents/graph/GraphDesigner.test.tsx`, `ui/src/routes/agents/graph/Inspector.tsx`

**Interfaces:**
- Consumes: `FlowEditor` (Task 9).
- Produces: the nav link `Flows` (path `/agents` unchanged until plan 2); the list page `h1` `Flows`; the button `New flow` (was `+ Create graph agent`); design mode renders `FlowEditor` inside `<section aria-labelledby="flow-editor-title">`.

- [ ] **Step 1: Update the failing expectations first**

In `ui/src/app/App.test.tsx`, change `"Agents"` in the nav-name list to `"Flows"` and `getByRole("link", { name: "Agents" })` to `getByRole("link", { name: "Flows" })`. In `AgentsView.test.tsx`, run `grep -n "Design a graph agent\|Create graph agent\|heading.*Agents" src/routes/agents/AgentsView.test.tsx` and change those expectations to `New flow` (button), `heading { name: "Flows" }` (h1) and `heading { name: "New flow" }` (editor). Port the two `GraphDesigner.test.tsx` behaviours that still apply by adding these cases to `FlowEditor.test.tsx` (its fixtures `agent`, `flat` and `setup` are from Task 9):

```tsx
  it("keeps the flow's tools on the start step when a step is added", async () => {
    const withTools: AgentDTO = {
      ...agent,
      current_version: { ...agent.current_version!, mcp_tools: [{ mcp_server_id: "crm", remote_name: "lookup", schema_hash: "h", required: false }] },
    };
    const { user, onSubmit } = setup({ agent: withTools });
    await user.click(screen.getByRole("button", { name: "Add step" }));
    await user.click(screen.getByRole("button", { name: "Publish" }));
    await user.click(screen.getByRole("button", { name: "Publish v2" }));
    const config = onSubmit.mock.calls[0][0].config as { entry: string; nodes: { id: string; tools?: string[] }[] };
    expect(config.nodes.find((n) => n.id === config.entry)!.tools).toEqual(["crm/lookup"]);
  });

  it("sends a plain one-step flow as a flat config", async () => {
    const { user, onSubmit } = setup({ agent });
    await user.click(screen.getByRole("button", { name: "Publish" }));
    await user.click(screen.getByRole("button", { name: "Publish v2" }));
    expect(onSubmit.mock.calls[0][0].config).toEqual(flat);
  });
```

- [ ] **Step 2: Run them to see them fail**

Run: `cd ui && npx vitest run src/app src/routes/agents`
Expected: FAIL on the renamed nav and headings.

- [ ] **Step 3: Implement**

`ui/src/routes/sections.tsx`: `{ path: "/agents", name: "Flows", icon: "Fl" },`

`ui/src/routes/agents/AgentsView.tsx`:
- `import { FlowEditor } from "./graph/FlowEditor";` replaces the `GraphDesigner` import.
- `<h1 tabIndex={-1}>Agents</h1>` → `<h1 tabIndex={-1}>Flows</h1>`.
- `<ActionButton onClick={() => startDesign(null)}>+ Create graph agent</ActionButton>` → `<ActionButton onClick={() => startDesign(null)}>New flow</ActionButton>`.
- Replace the whole `{mode === "design" && (…)}` block with:

```tsx
          {mode === "design" && (
            <section aria-labelledby="flow-editor-title">
              {formError && <ErrorState title="Publishing failed" error={formError} onRetry={() => setFormError(null)} />}
              {optionsError}
              <FlowEditor
                key={selectedAgent?.id ?? "new"}
                agent={selectedAgent ?? null}
                providers={providers}
                client={client}
                keyValue={keyValue}
                pending={createMutation.isPending || updateMutation.isPending}
                onSubmit={(input) => (selectedAgent ? updateMutation.mutate({ id: selectedAgent.id, payload: input }) : createMutation.mutate(input))}
                onCancel={() => setMode(selectedAgent ? "detail" : "list")}
              />
            </section>
          )}
```

Delete `GraphDesigner.tsx`, `GraphDesigner.test.tsx` and `Inspector.tsx`, and remove `"routes/agents/graph/GraphDesigner.tsx"` and `"routes/agents/graph/Inspector.tsx"` from `CONVERTED`. Run `cd ui && grep -rn "GraphDesigner\|Inspector" src` and expect no results.

- [ ] **Step 4: Run the whole UI gate**

Run: `cd ui && npx vitest run && npx tsc --noEmit`
Expected: PASS, no type errors.

- [ ] **Step 5: Commit**

```bash
git add -A ui/src
git commit -m "feat(ui): flows use the new editor; retire the graph designer and inspector

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 11: End-to-end graph suite on the new editor, and the review gate

**Files:**
- Modify: `e2e/tests-graph/10-agent-graph.spec.ts`

**Interfaces:**
- Consumes: the labels and names from Tasks 5–10.

- [ ] **Step 1: Update the helpers and the designer tests**

In `e2e/tests-graph/10-agent-graph.spec.ts`:

```ts
async function openAgentsList(page: Page) {
  await page.getByRole("link", { name: "Flows", exact: true }).first().click();
}

async function openDesigner(page: Page, stack: Stack) {
  await connectAdmin(page, stack);
  await openAgentsList(page);
  await page.getByRole("button", { name: "New flow" }).click();
  await expect(page.getByRole("heading", { name: "New flow" })).toBeVisible();
  await page.getByRole("button", { name: "Add first step" }).click();
  await page.getByLabel("Step name").fill("Main agent");
}
```

Replace the body of `designer: draw a two-agent graph with a handoff and publish it` up to and including the Publish click with:

```ts
  await openDesigner(page, stack);
  const panel = () => page.getByRole("complementary").last();

  // The start step: model, voice and instructions in Basics.
  await page.getByLabel("LLM provider").selectOption(PROVIDER);
  await pick(page, "LLM model", LLM_MODEL());
  await page.getByLabel("Instructions").fill(FRONT_PROMPT);
  await page.getByLabel("TTS provider").selectOption(PROVIDER);
  await pick(page, "Voice", LIVE ? catalogDefault("tts") : "frontvoice");

  // A second step, routed from the keyboard path (Routes tab).
  await page.getByRole("button", { name: "Add step" }).click();
  await page.getByLabel("Step name").fill("Sales");
  await page.getByLabel("Instructions").fill(SALES_PROMPT);
  if (!LIVE) await pick(page, "Voice", "salesvoice");
  await page.getByRole("button", { name: "Done" }).click();
  await page.getByText("Main agent", { exact: true }).click();
  await panel().getByRole("tab", { name: /Routes/ }).click();
  await page.getByLabel("Route to").selectOption({ label: "Sales" });
  await page.getByRole("button", { name: "Add route" }).click();
  await page.getByLabel("Condition for the route to Sales").fill("The caller wants to buy a car or asks about prices.");

  // Flow settings: name, speech-to-text, route sound.
  await page.getByRole("button", { name: "Flow settings" }).click();
  await page.getByLabel("Flow name").fill(AGENT);
  await page.getByLabel("STT provider").selectOption(PROVIDER);
  await pick(page, "STT model", catalogDefault("stt"));
  await page.getByLabel("Sound name").fill("chime");
  await page.getByLabel("Start after (ms)").fill("50");
  await step(page, "flow designed");

  await page.getByRole("button", { name: "Publish", exact: true }).click();
  await page.getByRole("button", { name: "Publish v1" }).click();
```

The `providers:` tests call `openDesigner` and read `STT provider` options. That select is now in Flow settings, so add `await page.getByRole("button", { name: "Flow settings" }).click();` after `openDesigner(page, stack)` in both tests.

In `designer: reopen the graph, edit it, republish…`, replace the old node and handoff steps:
- `getByRole("heading", { name: \`Design ${AGENT}\` })` → `getByRole("heading", { name: AGENT, level: 2 })`.
- `getByLabel("New agent node")` + `Add agent` → `getByRole("button", { name: "Add step" })` then `getByLabel("Step name").fill("Support")`.
- `Remove node` → `getByRole("button", { name: "Delete step" })`.
- `getByRole("button", { name: "Main agent → Sales" }).click()` + `When should it hand off?` → `getByText("Main agent", { exact: true }).click()`, open the Routes tab, then `getByLabel("Condition for the route to Sales").fill(…)`.
- `/unpublished edits are not in this test/i` → open `Test call` and expect `/This call runs v1, the published version/`.
- `Publish` → `Publish` then `Publish v2`.

In `layout: …390px`, the heading `Agents` → `Flows`.

- [ ] **Step 2: Run the graph suite with fake providers**

Run: `cd e2e && npx playwright test -c playwright.graph.config.ts`
Expected: all tests pass. If clicking a step by its text does not select it in React Flow, click `.react-flow__node` filtered by text instead (`page.locator(".react-flow__node", { hasText: "Main agent" }).click()`).

- [ ] **Step 3: Verification gate**

Run:

```bash
cd ui && npx vitest run && npx tsc --noEmit
cd ../e2e && npx playwright test -c playwright.graph.config.ts
cd .. && git status --short
```

Expected: everything passes; the working tree is clean apart from the spec file.

- [ ] **Step 4: Independent review (AGENTS.md "Mandatory Code Review Workflow")**

Dispatch a reviewer agent on the branch diff with this brief: YAGNI and bloat (is anything here unused by the editor?), correctness of draft-route validation (no empty condition reaches `onSubmit`), focus management (panel, drawn route, publish dialog), the five Review Focus items above, token-only colours, and test coverage of keyboard-only route creation. Fix confirmed findings before merging.

- [ ] **Step 5: Commit**

```bash
git add e2e/tests-graph/10-agent-graph.spec.ts
git commit -m "test(e2e): graph suite drives the flow editor

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```
