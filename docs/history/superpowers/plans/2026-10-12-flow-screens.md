> **Historical** (moved to docs/history by Phase 8, 2026-10-21): written for the design at the time; it may name packages, files, endpoints and targets that no longer exist. Paths inside are as written then; see docs/history/README.md.

# Flow Screens Parity Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Bring canvas boards 3–12 (Flows list and empty state, New flow, the flow editor in all its states, the Flow settings panel, the Test call panel, Publish, "v8 is live") from "works" to "matches the canvas": a **full-bleed editor** with the canvas's header bar, toolbar row and full-height graph; **canvas-style step cards**; a **rebuilt Test call panel** (dark header with clock, state line and Mute / End call; transcript labelled by step); the Flows page, New flow dialog and Publish dialog in the canvas's shapes.

**Architecture:** Mostly markup and CSS over components that already exist (plans 1 and 2). New behaviour is small: the editor refits its graph when a panel opens, closes or widens (fixing a tiny-graph defect); the Test call panel learns the active step's name and a running clock; the Publish dialog gains a "Checks" list and the number of calls in progress; the Flows empty state names the detected providers. No server or API change.

**Tech Stack:** React 19, TypeScript 7, react-router 7, @tanstack/react-query 5, @xyflow/react 12, vitest 5 + Testing Library, Playwright (`e2e/tests-graph`).

**Spec:** `docs/superpowers/specs/2026-10-12-switchboard-console-design.md` (Sections 2, 3, 4). Visual rules: `docs/superpowers/specs/2026-10-12-ui-switchboard-design.md`. Canvas boards 3–12 (`AgentsEmpty`, `Agents`, `NewAgent`, `FlowBlank`, `NodeEdit`, `Designer`, `Definition`, `TestCall`, `Publish`, `Published`; the last six small boards are parameterised imports of `Designer` and `Agents`).

## What was compared (screenshots of the real screens against the canvas source)

1. **Flows (3, 4):** canvas is `h1` 28px with the summary inline and a filter beside it; ours is the big page title plus a paragraph. Empty state: canvas adds "Deepgram, OpenAI-compatible and Cartesia are ready to use."
2. **New flow (5):** canvas shows Blank and Copy as two bordered option cards; ours are plain radios.
3. **Editor (6–9):** canvas is a full-bleed screen: one compact bar (`Flows /`, 20px title, mono version chip, state), a toolbar row inside the graph (dashed "+ Step", "N of 8 steps", a hint to drag from a step's edge), the graph filling the rest of the screen, panels overlaid. Ours sits in the 1100px page column with a big title and a 600px bordered card. After a panel closes the graph stays zoomed out to the narrow width, so the steps are tiny (defect).
4. **Step cards:** canvas 210×104, name 15px, mono tag and mono model/voice line, a status row with a dot or square under a rule.
5. **Test call (10):** canvas has a dark header (`Test call · v7`, clock, state line, Mute and End call) over a step-labelled transcript. Ours is the old light "Live call" panel with the agent id and readiness line.
6. **Publish (11):** canvas has the calls-in-progress sentence, "What changes · N" rows and a Checks list. Ours has a one-line promise and a bullet list.
7. **Published (12):** canvas banner is tinted and offers "Watch new calls" for that flow.

## Global Constraints

- Keep every test id, label and text the suites rely on: `start-voice-test-btn`, `connection-state`, `mute-btn`, `disconnect-btn`, `send-text-btn`, `ptt-btn`, `transcript-user`, `transcript-bot`, `interim-transcript`, `bot-streaming-text`, `agent-readiness`, `session-summary`, `view-trace-link`, `toggle-debug-btn`; the button names `Flow settings`, `Test call`, `Publish`, `Add step`, `Add first step`, `Done`, `Start voice test`, `Send text`, the labels `Message`, `Push to talk`, `Step name`, `Flow name`; the connection words `READY` and `DISCONNECTED`; the text `Published v2`.
- UI copy says Flow / Step / Route. Colours only through tokens; no hex or `rgb(` in TSX or below `/* end tokens */`; radius is `var(--radius)`, `50%` or `999px`; **no drop shadows** (the canvas panel shadow and call halo are left out on purpose); no all-caps labels; sentence case; 44px touch targets; the global focus ring. Lamp amber stays reserved for the live call (the active step, the call state).
- Never offer or claim what the server cannot do. Left out on purpose: the canvas's waveform and "median reply" figures (S9), the Publish "note for the version history" and "roll back to vN" line (S2 and no field), "Saved to the v8 draft" (drafts are not saved on the server, S1: the panel footer says the changes are part of the draft until you publish), the Voice stack select in New flow (First run owns it), `Publish vN` as the button label (the suites find `Publish`; the version stays in the dialog).
- Gate for every task: `cd ui && npx vitest run && npx tsc --noEmit`; after Tasks 1, 4 and 7 also `make e2e-fake`.
- Commits are conventional (`feat(ui): …`, `fix(ui): …`) and end with `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`.

## Review Focus

1. **The graph is never left off-screen or tiny.** Opening or closing any panel, widening it, adding or removing a step must leave every step visible at a readable size, and must not lose a step's selection. Pinned in Task 1.
2. **A call in progress keeps the editor stable.** While a test call runs: panels that would replace the call panel stay off, the clock keeps counting between speech events, the transcript labels never rewrite earlier lines when the active step changes, and ending the call leaves the transcript and the trace link. Pinned in Task 4.
3. **No claim the data cannot back.** The Checks list shows only checks that the editor really performed; the calls-in-progress sentence only appears with a real count. Pinned in Task 6.
4. **Narrow screens.** At 390px nothing scrolls sideways: the editor header wraps, the toolbar wraps, panels become full width. Pinned in Tasks 1 and 7.
5. **Accessibility is not traded for looks.** The dark Test call header keeps readable contrast (guard test exists for tokens), controls keep accessible names, the cards in New flow are real radios, and `Escape` still closes panels and dialogs. Pinned in Tasks 4 and 5.

---

### Task 1: Full-bleed editor, canvas toolbar, blank state, and the refit fix

**Files:**
- Modify: `ui/src/routes/agents/graph/FlowEditor.tsx`, `ui/src/routes/agents/graph/Canvas.tsx`, `ui/src/routes/agents/FlowPage.tsx`, `ui/src/styles.css`
- Test: `ui/src/routes/agents/graph/FlowEditor.test.tsx` (add), `ui/src/routes/agents/graph/Canvas.test.tsx` (add), `ui/src/routes/agents/FlowPage.test.tsx` (adjust)

**Interfaces:**
- Produces: `Canvas` gains `fitKey?: string` (the graph refits when it changes, besides when the number of steps changes) and `height?: number | string` (default 420; `"100%"` fills its parent). The breadcrumb moves from `FlowPage` into the editor's header bar (`FlowEditor` renders `<nav aria-label="Breadcrumb"><Link to="/flows">Flows</Link> /</nav>`).

- [ ] **Step 1: Write the failing tests**

In `ui/src/routes/agents/graph/FlowEditor.test.tsx` add:

```tsx
describe("FlowEditor chrome", () => {
  it("has the breadcrumb in its header bar, the step count and the route hint in the toolbar", () => {
    setup({ agent });
    const head = screen.getByRole("heading", { name: "Front desk", level: 1 }).closest("header") as HTMLElement;
    expect(within(head).getByRole("link", { name: "Flows" })).toHaveAttribute("href", "/flows");
    expect(screen.getByText("1 of 8 steps")).toBeVisible();
    expect(screen.getByText(/Drag from the dot on a step's edge to add a route/)).toBeVisible();
  });

  it("explains the blank state with the canvas's words", () => {
    setup({ initialName: "After hours" });
    expect(screen.getByText(/that's enough to make a test call/)).toBeVisible();
    expect(screen.queryByText(/Drag from the dot/)).toBeNull();
  });

  it("refits the graph when a panel opens or closes, so the steps stay in view", async () => {
    const { user } = setup({ agent });
    const canvas = screen.getByTestId("agent-canvas");
    const before = canvas.getAttribute("data-fit");
    await user.click(screen.getByRole("button", { name: "Flow settings" }));
    expect(canvas.getAttribute("data-fit")).not.toBe(before);
    const open = canvas.getAttribute("data-fit");
    await user.click(screen.getByRole("button", { name: "Close Flow settings" }));
    expect(canvas.getAttribute("data-fit")).not.toBe(open);
  });
});
```

(add `within` to the file's `@testing-library/react` import if missing). In `Canvas.test.tsx` add a case that renders `<Canvas graph={…} fitKey="a" />`, re-renders with `fitKey="b"`, and expects the `data-testid="agent-canvas"` element's `data-fit` attribute to be `"1:b"` (one step, key `b`). In `FlowPage.test.tsx` the breadcrumb test must look inside the editor header: change `expect(screen.getByRole("link", { name: "Flows" }))…` to find it with `within(screen.getByRole("heading", { name: "Front desk", level: 1 }).closest("header") as HTMLElement)`.

- [ ] **Step 2: Run them to see them fail**

Run: `cd ui && npx vitest run src/routes/agents`
Expected: FAIL — no breadcrumb in the header, no hint, no `data-fit`.

- [ ] **Step 3: Implement**

`Canvas.tsx`: change the props and the wrapper:

```tsx
  readOnly?: boolean;
  height?: number | string;
  /** The graph refits when this changes (a panel opened, closed or widened) as well as when a step is added or removed. */
  fitKey?: string;
};
```

```tsx
export function Canvas({ graph, selected, onSelect, onMove, onConnect, visual, traversedEdges, justHandedEdge, readOnly, height = 420, fitKey = "" }: CanvasProps) {
```

```tsx
    <div className="agent-canvas" style={{ height }} data-testid="agent-canvas" data-fit={`${graph.nodes.length}:${fitKey}`}>
      <ReactFlow
        // Re-fit when a step is added or removed, or the room for the graph changes, so no step is left off-screen or tiny.
        key={`${graph.nodes.length}:${fitKey}`}
```

`FlowEditor.tsx`: import `Link` (`import { Link } from "react-router";`), and in the returned markup:

1. Header bar — replace `<header className="flow-editor__head"> <h1 …>…</h1>` start with:

```tsx
      <header className="flow-editor__head">
        <nav aria-label="Breadcrumb" className="flow-editor__crumb">
          <Link to="/flows">Flows</Link> /
        </nav>
        <h1 id="flow-editor-title" tabIndex={-1}>
          {name.trim() || "New flow"}
        </h1>
```

2. Toolbar — replace the block `<div className="flow-editor__toolbar"> … </div>` with:

```tsx
      <div className="flow-editor__toolbar">
        {graph.nodes.length > 0 && (
          <button
            type="button"
            className="action-button-small flow-editor__add"
            disabled={graph.nodes.length >= MAX_NODES || call.live}
            title={graph.nodes.length >= MAX_NODES ? `A flow has at most ${MAX_NODES} steps` : undefined}
            onClick={addStep}
          >
            Add step
          </button>
        )}
        <span className="subtext">
          {graph.nodes.length} of {MAX_NODES} steps
        </span>
        <span className="grow" />
        {graph.nodes.length > 0 && !call.live && <span className="subtext">Drag from the dot on a step's edge to add a route</span>}
      </div>
```

3. Blank state — replace the `flow-editor__blank` block with:

```tsx
          <div className="flow-editor__blank">
            <div className="flow-editor__blank-card">
              <span className="flow-editor__plus" aria-hidden="true">
                +
              </span>
              <h3>Start with one step.</h3>
              <p>A step answers the caller. Give it instructions, a model and a voice: that's enough to make a test call. Add more steps and routes when you need them.</p>
              <button type="button" className="action-button" onClick={addStep}>
                Add first step
              </button>
            </div>
          </div>
```

4. Canvas: `height={600}` → `height="auto"` and add `fitKey={panel ? `${panel.kind}${wide ? "-wide" : ""}` : "none"}`.

`FlowPage.tsx`: delete the `<nav aria-label="Breadcrumb" className="flow-page__crumb"> … </nav>` block, and wrap the banner and error states so they take the page padding: replace the `<div className="flow-page">` opening tag with `<div className="flow-page flow-page--editor">` (the banner and errors keep their place above the editor). Remove the now-unused `.flow-page__crumb` rule if nothing else uses it (`CallView` uses it: keep it).

`styles.css`: replace the existing `.flow-editor`, `.flow-editor__head`, `.flow-editor__toolbar`, `.flow-editor__stage*`, `.flow-editor__blank*` rules (lines near `.flow-editor {`) and `.flow-editor__head h1 {` with:

```css
/* The editor uses the whole workspace, like the canvas: a compact bar, a toolbar row, and the graph filling the rest. */
main:has(.flow-editor) {
  max-width: none;
  padding: 0;
}
.flow-page--editor > .published-banner,
.flow-page--editor > .message-panel {
  margin: 12px 24px 0;
}
.flow-editor {
  display: flex;
  flex-direction: column;
  min-height: calc(100vh - 56px);
}
.flow-editor__head {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 10px 16px;
  padding: 12px 24px;
  border-bottom: 1px solid var(--rule);
}
.flow-editor__crumb {
  color: var(--ink-2);
  font-size: 0.875rem;
}
.flow-editor__crumb a {
  color: var(--ink-2);
}
.flow-editor__head h1 {
  margin: 0;
  font-size: 1.25rem;
  font-weight: 600;
  letter-spacing: -0.01em;
  line-height: 1.2;
}
.flow-editor__head .chip {
  font-family: "IBM Plex Mono", monospace;
  font-size: 0.75rem;
  background: var(--surface);
}
.flow-editor__toolbar {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px 12px;
  padding: 10px 20px;
  border-bottom: 1px solid var(--rule);
}
.flow-editor__add {
  border-style: dashed;
  border-color: var(--ink-2);
  background: transparent;
  color: var(--ink);
}
.flow-editor__stage {
  position: relative;
  flex: 1;
  display: flex;
  min-height: 560px;
  overflow: hidden;
}
.flow-editor__stage > .agent-canvas {
  flex: 1;
  min-width: 0;
}
.flow-editor__stage--panel > .agent-canvas {
  margin-right: min(440px, 100%);
}
.flow-editor__stage--wide > .agent-canvas {
  margin-right: min(720px, 100%);
}
.flow-editor__blank {
  flex: 1;
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 24px;
  background-image: radial-gradient(var(--rule) 1px, transparent 1px);
  background-size: 20px 20px;
}
.flow-editor__blank-card {
  max-width: 440px;
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 14px;
  padding: 36px 32px;
  border: 1.5px dashed var(--ink-2);
  border-radius: var(--radius);
  background: color-mix(in srgb, var(--surface) 85%, transparent);
  text-align: center;
}
.flow-editor__blank-card h3,
.flow-editor__blank-card p {
  margin: 0;
}
.flow-editor__blank-card h3 {
  font-size: 1.375rem;
  font-weight: 600;
}
.flow-editor__blank-card p {
  color: var(--ink-2);
  font-size: 0.875rem;
}
.flow-editor__plus {
  display: flex;
  align-items: center;
  justify-content: center;
  width: 44px;
  height: 44px;
  border: 1.5px solid var(--ink);
  border-radius: var(--radius);
  font-size: 1.375rem;
}
```

Delete the old `.flow-editor__head h1 { margin: 0; font-size: 24px; … }` rule near the end of the file (it is replaced above), and make `.agent-canvas { height: … }` not fight the inline style (it already uses an inline `height`).

- [ ] **Step 4: Run them to see them pass**

Run: `cd ui && npx vitest run && npx tsc --noEmit`
Expected: PASS. If an older `FlowEditor` test asserts the blank-state sentence "then add more steps and routes", update it to the new copy ("that's enough to make a test call").

- [ ] **Step 5: Run the browser suite and look**

Run: `make e2e-fake` (the layout check at 390px must still pass: the header wraps, the stage keeps `min-height`). Fix any e2e locator that relied on the breadcrumb being outside the editor.
Expected: all graph specs pass.

- [ ] **Step 6: Commit**

```bash
git add ui/src e2e
git commit -m "feat(ui): full-bleed flow editor — canvas header bar, toolbar row, blank state, and the graph refits when a panel changes

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 2: Step cards in the canvas's shape

**Files:**
- Modify: `ui/src/routes/agents/graph/Canvas.tsx`, `ui/src/styles.css`
- Test: `ui/src/routes/agents/graph/Canvas.test.tsx` (add)

**Interfaces:**
- Produces: the step card is 210px wide with a title row (name, mono tag), a mono model·voice line, and a status row with a dot under a rule; the status dot is a square for "Needs setup" and "No route leads here", a circle otherwise. Test hooks: `.agent-node__dot[data-shape="square"]`.

- [ ] **Step 1: Write the failing test.** In `Canvas.test.tsx` add:

```tsx
  it("draws a step as a card: title and tag, the model and voice line, and a status row with a dot", () => {
    render(<Canvas graph={twoStepGraphWithOneIncomplete} />);
    const card = screen.getByText("Reception").closest(".agent-node") as HTMLElement;
    expect(card.querySelector(".agent-node__tag")).toHaveTextContent("Start");
    expect(card.querySelector(".agent-node__status .agent-node__dot")).not.toBeNull();
    const needs = screen.getByText("Needs setup").closest(".agent-node__status") as HTMLElement;
    expect(needs.querySelector('.agent-node__dot[data-shape="square"]')).not.toBeNull();
    const ready = screen.getAllByText("Ready")[0].closest(".agent-node__status") as HTMLElement;
    expect(ready.querySelector('.agent-node__dot[data-shape="square"]')).toBeNull();
  });
```

where `twoStepGraphWithOneIncomplete` is a fixture the file already has an equivalent of (a graph with a start step named `Reception` that is ready and a second step with no model); build it from `fromAgentConfig` like the file's other cases, so that one step reads "Ready" and one "Needs setup".

- [ ] **Step 2: Run it to see it fail**

Run: `cd ui && npx vitest run src/routes/agents/graph/Canvas.test.tsx`
Expected: FAIL — no `.agent-node__tag`, no `.agent-node__dot`.

- [ ] **Step 3: Implement.** In `Canvas.tsx` replace the body of `AgentNode`'s returned card:

```tsx
      <Handle type="target" position={Position.Left} />
      <div className="agent-node__title">
        <b>{data.name || "Untitled step"}</b>
        {data.entry && <span className="agent-node__tag">Start</span>}
      </div>
      <div className="agent-node__meta">{data.model && data.voice ? `${data.model} · ${data.voice}` : data.model || "No model or voice yet"}</div>
      {status && (
        <div className="agent-node__status">
          <span className="agent-node__dot" data-shape={data.setup === "incomplete" || data.setup === "unreachable" ? (state === "idle" ? "square" : undefined) : undefined} aria-hidden="true" />
          {status}
        </div>
      )}
      <Handle type="source" position={Position.Right} />
```

In `styles.css`, replace the `.agent-node`, `.agent-node__title`, `.agent-node__badge`, `.agent-node__meta`, `.agent-node__status` rules with (keep the `--selected`, `--active`, `--preparing`, `--visited`, `--incomplete` variants and the keyframes as they are, then adjust `--incomplete .agent-node__status` colour to `var(--bad)`):

```css
.agent-node {
  width: 210px;
  min-height: 104px;
  display: flex;
  flex-direction: column;
  gap: 4px;
  padding: 11px 14px 9px;
  border: 1px solid var(--rule);
  border-radius: var(--radius);
  background: var(--surface);
  color: var(--ink);
}
.agent-node__title {
  display: flex;
  align-items: center;
  gap: 8px;
}
.agent-node__title b {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-size: 0.9375rem;
  font-weight: 600;
}
.agent-node__tag {
  font-family: "IBM Plex Mono", monospace;
  font-size: 0.6875rem;
  color: var(--ink-2);
}
.agent-node__meta {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-family: "IBM Plex Mono", monospace;
  font-size: 0.6875rem;
  color: var(--ink-2);
}
.agent-node__status {
  display: flex;
  align-items: center;
  gap: 6px;
  margin-top: auto;
  padding-top: 6px;
  border-top: 1px solid var(--rule);
  font-size: 0.75rem;
  color: var(--ink-2);
}
.agent-node__dot {
  width: 7px;
  height: 7px;
  border-radius: 50%;
  background: var(--ok);
}
.agent-node__dot[data-shape="square"] {
  border-radius: 0;
  background: var(--bad);
}
.agent-node--active .agent-node__dot {
  background: var(--lamp);
}
.agent-node--visited .agent-node__dot {
  background: var(--ink-2);
}
```

(If the existing `.agent-node--active` rule fills the whole card with `--lamp`, as the live screenshot shows, keep that behaviour: the guard that amber is for the live call only still holds. Check `system.css.test.ts` and `noHardcodedColors.test.ts` pass.)

- [ ] **Step 4: Run it to see it pass**

Run: `cd ui && npx vitest run && npx tsc --noEmit`
Expected: PASS. Any older test that looked for `.agent-node__badge` or the pill "Start" by class must use `.agent-node__tag`.

- [ ] **Step 5: Commit**

```bash
git add ui/src
git commit -m "feat(ui): step cards in the canvas's shape — tag, mono model line, status row with a dot

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 3: Flow settings footer and panel head

**Files:**
- Modify: `ui/src/routes/agents/graph/FlowSettingsPanel.tsx`, `ui/src/styles.css`
- Test: `ui/src/routes/agents/graph/FlowSettingsPanel.test.tsx` (add)

**Interfaces:**
- Produces: the Flow settings panel has a footer with the sentence `Part of the draft until you publish.` and a **Done** button that closes the panel (`onClose`).

- [ ] **Step 1: Write the failing test.** In `FlowSettingsPanel.test.tsx` inside `describe("FlowSettingsPanel", …)`, using the file's `Harness` (it passes `onClose={vi.fn()}`; change `Harness` to accept an `onClose` prop defaulting to `vi.fn()` and forward it):

```tsx
  it("ends with a footer: the changes are part of the draft, and Done closes the panel", async () => {
    const onClose = vi.fn();
    render(<Harness onClose={onClose} />);
    expect(screen.getByText("Part of the draft until you publish.")).toBeVisible();
    await userEvent.setup().click(screen.getByRole("button", { name: "Done" }));
    expect(onClose).toHaveBeenCalledOnce();
  });
```

- [ ] **Step 2: Run it to see it fail**

Run: `cd ui && npx vitest run src/routes/agents/graph/FlowSettingsPanel.test.tsx`
Expected: FAIL — no footer.

- [ ] **Step 3: Implement.** Find the `<SidePanel` element in `FlowSettingsPanel.tsx` and add the `footer` prop:

```tsx
      footer={
        <>
          <span className="subtext grow">Part of the draft until you publish.</span>
          <button type="button" className="action-button" onClick={onClose}>
            Done
          </button>
        </>
      }
```

(`.grow` already exists: it is used by the Flows toolbar.)

- [ ] **Step 4: Run it to see it pass**

Run: `cd ui && npx vitest run src/routes/agents && npx tsc --noEmit`
Expected: PASS. If `getByRole("button", { name: "Done" })` is ambiguous in an editor test that has a step panel open too, scope it with `within(screen.getByRole("complementary", { name: "Flow settings" }))`.

- [ ] **Step 5: Commit**

```bash
git add ui/src
git commit -m "feat(ui): Flow settings panel ends with a footer and Done

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 4: Rebuild the Test call panel

**Files:**
- Modify: `ui/src/routes/agents/AgentVoiceTest.tsx`, `ui/src/routes/agents/graph/FlowEditor.tsx`, `ui/src/components/SidePanel.tsx`, `ui/src/styles.css`, `e2e/tests-graph/10-agent-graph.spec.ts` (only if a locator breaks)
- Test: `ui/src/routes/agents/AgentVoiceTest.test.tsx` (adjust + add), `ui/src/routes/agents/AgentVoiceTest.graph.test.tsx` (add), `ui/src/routes/agents/graph/FlowEditor.call.test.tsx` (adjust)

**Interfaces:**
- Consumes: `useNow` (`ui/src/routes/sessions/useNow.ts`), `mmss` (`ui/src/routes/sessions/callStats.ts`).
- Produces:
  - `AgentVoiceTestProps` gains `activeStep?: string` (the name of the step that holds the call now, from the editor's live view). A bot line is labelled with the step that was active **when the line first appeared**, and never relabelled.
  - The panel's layout: a dark header (`Test call · v<n>` and a running clock `mm:ss` while connected, then the state line with a lamp, a short row of the controls: Push to talk, **Mute** / **Unmute**, **End call** or **Start voice test**), then the transcript labelled by step, then the message form, the session summary and the debug toggle.
  - Test ids and states unchanged (see Global Constraints). Visible changes: the old "Live call" heading, the "Agent: <id>" line and the connection badge row become the header; the `agent-readiness` code stays (small text `ready` next to the version); the button `Disconnect` reads **End call**; `Mute mic`/`Unmute mic` read **Mute**/**Unmute**.
  - `SidePanel` gains `tone?: "dark"` which renders the head with the dark token pair; the Test call panel uses it with `title="Test call"`.

- [ ] **Step 1: Write/adjust the failing tests**

In `AgentVoiceTest.test.tsx` change only the assertions that name text the new header replaces:
- `expect(screen.getByText(/Live call active/i)).toBeVisible();` → keep (the state line keeps "Live call active — speak into your microphone").
- Any `getByRole("button", { name: /disconnect/i })`: none (uses test id). Keep test-id usage.
- In the first-render test (the one reading `agent-readiness`), nothing changes.

Add to `AgentVoiceTest.test.tsx`, reusing the file's harness (`renderPanel` or the equivalent it already uses to mount the component with its mocked WebSocket):

```tsx
  it("shows the version and a clock in a header, and counts while the call is live", async () => {
    // mount as the file's other tests do, with agent version 3
    const { user } = await mountReady({ versionNumber: 3 });
    expect(screen.getByText("Test call · v3")).toBeVisible();
    expect(screen.getByTestId("call-clock")).toHaveTextContent("00:00");
    await waitFor(() => expect(screen.getByTestId("call-clock").textContent).not.toBe("00:00"), { timeout: 3000 });
    void user;
  });

  it("offers End call and Mute with their new words, keeping the test ids", async () => {
    await mountReady({});
    expect(screen.getByTestId("disconnect-btn")).toHaveTextContent("End call");
    expect(screen.getByTestId("mute-btn")).toHaveTextContent("Mute");
    await userEvent.click(screen.getByTestId("mute-btn"));
    expect(screen.getByTestId("mute-btn")).toHaveTextContent("Unmute");
  });

  it("labels each reply with the step that held the call when it arrived, and never relabels it", async () => {
    const h = await mountReady({ activeStep: "Reception" });
    await h.botSays("Hello, you're through to reception.");
    h.setActiveStep("Billing");
    await h.botSays("I can see your invoice.");
    const lines = screen.getAllByTestId("transcript-bot");
    expect(lines[0]).toHaveTextContent("Reception");
    expect(lines[1]).toHaveTextContent("Billing");
    expect(lines[0]).not.toHaveTextContent("Billing");
  });

  it("falls back to the flow's name when no step is known", async () => {
    const h = await mountReady({});
    await h.botSays("Hi.");
    expect(screen.getByTestId("transcript-bot")).toHaveTextContent("Front desk");
  });
```

`mountReady`, `botSays` and `setActiveStep` are small helpers to add at the top of the file out of the existing pieces: `mountReady` is what the "executes normal conversational flow" test already does (render, click `start-voice-test-btn`, drive the mocked socket to `bot-ready`) returning `{ user }` plus `rerender`-based `setActiveStep(name)` (re-render with a new `activeStep` prop) and `botSays(text)` (push the mocked `bot-llm-text`/`bot-transcript` sequence the existing test uses for a bot reply). Use the existing test's own steps; do not invent new socket messages. `versionNumber` sets `agent.current_version.number`.

Add the same prop to `AgentVoiceTest.graph.test.tsx`'s `renderTest` (`activeStep` pass-through) so it type-checks.

In `FlowEditor.call.test.tsx` the case "keeps the call…" must keep passing unchanged.

- [ ] **Step 2: Run them to see them fail**

Run: `cd ui && npx vitest run src/routes/agents`
Expected: FAIL — no header text, no `call-clock`, labels say `You (User)` and the agent's name.

- [ ] **Step 3: Implement**

`SidePanel.tsx`: add `tone?: "dark"` to `SidePanelProps`, destructure it, and set the head's class: `` className={`side-panel__head${tone === "dark" ? " side-panel__head--dark" : ""}`} ``. Add the CSS (Step 3 end).

`FlowEditor.tsx`:
- compute the active step name next to `live`:

```tsx
  const activeStep = graph.nodes.find((n) => live.view.nodes[n.id]?.state === "active")?.name;
```

- pass it: `<AgentVoiceTest agent={agent} client={client} keyValue={keyValue} onCall={setCall} activeStep={activeStep} />`, and use the dark head: `<SidePanel title="Test call" tone="dark" …>` (leave the panel's other props).

`AgentVoiceTest.tsx`:
1. Imports: `import { useNow } from "../sessions/useNow"; import { mmss } from "../sessions/callStats";`.
2. Props: add `activeStep?: string;` to `AgentVoiceTestProps` and destructure it.
3. State/refs after the existing `useState` hooks:

```tsx
  // The clock counts from the moment the call connects.
  const startedAt = useRef<number | null>(null);
  // Which step held the call when each reply first appeared. Written once per line, so earlier lines never change.
  const stepOf = useRef(new Map<string, string>());
```

4. After `const inCall = …`:

```tsx
  const now = useNow(inCall);
  if (inCall && startedAt.current === null) startedAt.current = Date.now();
  if (!inCall) startedAt.current = null;
  const clock = mmss(inCall && startedAt.current !== null ? (now - startedAt.current) / 1000 : 0);
  for (const item of state.transcripts) if (item.speaker !== "user" && !stepOf.current.has(item.id)) stepOf.current.set(item.id, activeStep || agent.name);
```

(a ref write during render is idempotent: the same item id always gets the first value it was given.)

5. Replace the whole `return ( <section …> … </section> );` with:

```tsx
  return (
    <section aria-label="Voice test" className="call-panel">
      <div className="call-panel__top">
        <div className="call-panel__title">
          <span id="voice-test-heading">
            Test call · v{agent.current_version?.number ?? 0}
          </span>
          <code data-testid="agent-readiness" className="call-panel__ready">
            {agent.readiness.state}
          </code>
          <span className="grow" />
          <span className="call-panel__clock" data-testid="call-clock">
            {clock}
          </span>
        </div>
        <p className="call-panel__state">
          <span className="lamp" data-tone={isConnectedOrReady ? (statusTone === "idle" ? "warn" : statusTone) : undefined} aria-hidden="true" />
          <span data-testid="connection-state">
            <Status tone={state.sessionState === "ready" ? "good" : state.sessionState === "error" ? "bad" : "neutral"}>{state.sessionState.toUpperCase()}</Status>
          </span>
          {isConnectedOrReady && <span aria-live="polite">{speechStatusDescription}</span>}
          {state.isMuted && <span className="flag flag--bad">[Mic Muted]</span>}
          {state.metrics.bargeInCount > 0 && <span className="flag">Interrupted: {state.metrics.bargeInCount}x</span>}
        </p>
        <div className="call-panel__controls">
          <label className="stage__ptt">
            <input type="checkbox" checked={pushToTalk} disabled={isConnectedOrReady || isConnecting} onChange={(e) => setPushToTalk(e.target.checked)} />
            Push to talk
          </label>
          {!isConnectedOrReady && !isConnecting && (
            <ActionButton type="button" onClick={handleConnect} disabled={!keyValue || !callable} data-testid="start-voice-test-btn">
              Start voice test
            </ActionButton>
          )}
          {isConnecting && (
            <ActionButton type="button" disabled>
              Connecting…
            </ActionButton>
          )}
          {isConnectedOrReady && (
            <>
              <button type="button" className="action-button-small" onClick={handleToggleMute} data-testid="mute-btn" aria-pressed={state.isMuted}>
                {state.isMuted ? "Unmute" : "Mute"}
              </button>
              {state.pushToTalk && (
                <button
                  type="button"
                  className="action-button-small"
                  data-testid="ptt-btn"
                  aria-pressed={state.isTalking}
                  onPointerDown={(e) => {
                    // Keep the press while the pointer drifts off the button.
                    e.currentTarget.setPointerCapture?.(e.pointerId);
                    voiceClient.setTalking(true);
                  }}
                  onPointerUp={() => voiceClient.setTalking(false)}
                  onPointerCancel={() => voiceClient.setTalking(false)}
                  onBlur={() => voiceClient.setTalking(false)}
                  onKeyDown={(e) => {
                    if ((e.key === " " || e.key === "Enter") && !e.repeat) {
                      e.preventDefault();
                      voiceClient.setTalking(true);
                    }
                  }}
                  onKeyUp={(e) => {
                    if (e.key === " " || e.key === "Enter") {
                      e.preventDefault();
                      voiceClient.setTalking(false);
                    }
                  }}
                >
                  {state.isTalking ? "Talking… release to send" : "Hold to talk"}
                </button>
              )}
              <span className="grow" />
              <button type="button" className="action-button-small action-button-danger" onClick={handleDisconnect} data-testid="disconnect-btn">
                End call
              </button>
            </>
          )}
          {!keyValue && <span className="flag flag--bad">Enter management key to run voice tests.</span>}
        </div>
      </div>

      {!callable && !isConnectedOrReady && (
        <div role="note" className="note">
          <strong>Notice:</strong> This agent is marked as <code>{agent.readiness.state}</code> ({agent.readiness.reasons.join(", ") || "configuration incomplete"}). Fix these before starting a voice test; the server will not accept calls to this agent.
        </div>
      )}

      {state.error && (
        <div role="alert" className="note note--bad">
          <strong>
            {state.error.category === "mic" ? "Microphone Access Denied" : state.error.category === "token" ? "Token Creation Failed" : "Voice Session Error"}
          </strong>
          <p>{state.error.message}</p>
          <button type="button" className="action-button-small" onClick={handleRetry} disabled={!keyValue || !callable}>
            Retry Connection
          </button>
        </div>
      )}

      {callIsStale && (
        <p role="status" className="note">
          This call is on v{callVersion?.number}. Disconnect and reconnect to test v{agent.current_version?.number}.
        </p>
      )}

      <div className="transcript" tabIndex={0} aria-label="Conversation transcripts">
        {state.transcripts.length === 0 && !state.currentInterimTranscript && !state.currentBotStreamingText ? (
          <p className="transcript__empty">
            {isConnectedOrReady ? "Listening… Speak to the agent to start the transcript." : "No voice session active. Click \"Start voice test\" to test audio."}
          </p>
        ) : (
          <div className="transcript__lines">
            {state.transcripts.map((item) => (
              <div key={item.id} data-testid={item.speaker === "user" ? "transcript-user" : "transcript-bot"} className={`line line--${item.speaker === "user" ? "caller" : "agent"}`}>
                <div className="line__who">{item.speaker === "user" ? "You" : (stepOf.current.get(item.id) ?? agent.name)}</div>
                <div>{item.text}</div>
              </div>
            ))}
            {state.currentInterimTranscript && (
              <div className="line line--caller line--live" data-testid="interim-transcript">
                <div className="line__who">Listening…</div>
                <div>{state.currentInterimTranscript}</div>
              </div>
            )}
            {state.currentBotStreamingText && (
              <div className="line line--agent line--live" data-testid="bot-streaming-text">
                <div className="line__who">{activeStep || agent.name}</div>
                <div>{state.currentBotStreamingText}</div>
              </div>
            )}
          </div>
        )}
      </div>

      {/* Typed user turns: sent as RTVI send-text; the server echoes a final user transcription. */}
      <form onSubmit={handleSendText} aria-label="Send text message" className="stage__send">
        <label htmlFor="voice-test-text">Message</label>
        <input
          id="voice-test-text"
          type="text"
          value={textMessage}
          onChange={(e) => setTextMessage(e.target.value)}
          disabled={!isConnectedOrReady}
          placeholder={isConnectedOrReady ? "Type a message to the agent" : "Connect to send text"}
        />
        <button type="submit" className="action-button-small" disabled={!isConnectedOrReady || !textMessage.trim()} data-testid="send-text-btn">
          Send text
        </button>
      </form>

      {state.sessionId && (
        <div className="session-summary-card" data-testid="session-summary">
          <div>
            <div className="subtext">{inCall ? "Voice session" : "Call ended"}</div>
            <div className="session-summary-card__id">
              <code>{state.sessionId}</code>
              <span className="subtext">
                ({state.transcripts.length} turns, {state.metrics.bargeInCount} barge-ins)
              </span>
            </div>
          </div>
          <Link to={`/sessions/${state.sessionId}`} className="action-button-small" data-testid="view-trace-link">
            View session trace
          </Link>
        </div>
      )}

      <div className="stage__debug">
        <button type="button" className="action-button-small" onClick={() => setShowDebugPanel(!showDebugPanel)} data-testid="toggle-debug-btn">
          {showDebugPanel ? "Hide Pipecat Debug Panel" : "Show Pipecat Debug Panel"}
        </button>
      </div>

      {showDebugPanel && (
        <PipecatDebugPanel
          events={state.events}
          metrics={state.metrics}
          speakingState={{ isUserSpeaking: state.isUserSpeaking, isBotSpeaking: state.isBotSpeaking, isBargeInActive: state.isBargeInActive }}
          connectionStatus={state.sessionState}
          onClearEvents={() => voiceClient.clearLogs()}
        />
      )}
    </section>
  );
```

If the existing tests read the speaker label `You (User)` or `<agent name> (streaming…)`, update them to `You` and the step/flow name (the lines' text and test ids are unchanged). If one test reads the heading `Live call` (`voice-test-heading`), update it to `Test call · v1`.

`styles.css`: append

```css
.side-panel__head--dark {
  background: var(--ink);
  color: var(--paper);
  border-bottom-color: var(--ink);
}
.side-panel__head--dark .icon-button {
  color: var(--paper);
}
.call-panel {
  display: flex;
  flex-direction: column;
  gap: 16px;
  margin: -16px;
}
.call-panel > :not(.call-panel__top) {
  margin-left: 16px;
  margin-right: 16px;
}
.call-panel__top {
  display: flex;
  flex-direction: column;
  gap: 12px;
  padding: 16px;
  background: var(--ink);
  color: var(--paper);
}
.call-panel__title {
  display: flex;
  align-items: center;
  gap: 10px;
  font-weight: 600;
}
.call-panel__ready {
  font-size: 0.75rem;
  font-weight: 450;
  color: var(--rule);
}
.call-panel__clock {
  font-family: "IBM Plex Mono", monospace;
  font-size: 0.8125rem;
  font-weight: 450;
  color: var(--rule);
}
.call-panel__state {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 6px 12px;
  margin: 0;
}
.call-panel__state .status {
  color: var(--paper);
}
.call-panel__controls {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
}
.call-panel__top .action-button-small {
  border-color: var(--rule);
  background: transparent;
  color: var(--paper);
}
.call-panel__top .action-button-danger {
  border-color: var(--bad);
  background: var(--bad);
  color: var(--paper);
}
.call-panel__top .stage__ptt {
  color: var(--paper);
}
```

(Check the contrast guard in the existing tests: `--paper` on `--ink` must pass; `--rule` on `--ink` is used only for secondary text: if the contrast test flags it, use `var(--paper)` with `opacity: 0.8`. Do not introduce hex.)

- [ ] **Step 4: Run it to see it pass**

Run: `cd ui && npx vitest run && npx tsc --noEmit`
Expected: PASS. Run `make e2e-fake` too: `e2e/tests-graph/10-agent-graph.spec.ts`'s `startCall` and `disconnect-btn` locators use test ids and keep working; the `openFlowTest` readiness wait uses `agent-readiness` (kept).

- [ ] **Step 5: Commit**

```bash
git add ui/src e2e
git commit -m "feat(ui): rebuilt Test call panel — dark header with version and clock, controls, transcript labelled by step

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 5: Flows page head, empty state, and New flow cards

**Files:**
- Modify: `ui/src/routes/agents/FlowsView.tsx`, `ui/src/routes/agents/NewFlowDialog.tsx`, `ui/src/styles.css`
- Test: `ui/src/routes/agents/FlowsView.test.tsx` (add/adjust), `ui/src/routes/agents/NewFlowDialog.test.tsx` (add)

**Interfaces:**
- Consumes: `client.getProviders`, `useSearchParams` is not needed.
- Produces:
  - Flows head: `<header className="page-head"><h1>Flows</h1><span class="subtext" role="status">N enabled · M not ready[ · K calls in progress]</span><span class="grow"/><filter/><New flow/></header>`; the old `section-heading` paragraph and toolbar go away.
  - Empty state gains a line: `<Names> is ready to use.` / `<A>, <B> and <C> are ready to use.` from detected providers, or `No provider is set up yet.` with a link to `/providers`; the failure to load providers leaves the line out.
  - New flow options are two bordered cards (`.option-card`), each a real radio inside a label; the selected card has the `--caller` outline.

- [ ] **Step 1: Write the failing tests**

`FlowsView.test.tsx` (the file's `setup` default client gains `getProviders: vi.fn().mockResolvedValue([])`):

```tsx
  it("puts the summary beside the heading, with the filter and New flow in the same header", async () => {
    setup({ getSessions: vi.fn().mockResolvedValue([]) });
    const head = (await screen.findByRole("heading", { name: "Flows", level: 1 })).closest("header") as HTMLElement;
    expect(within(head).getByText("1 enabled · 0 not ready")).toBeVisible();
    expect(within(head).getByLabelText("Filter flows")).toBeVisible();
    expect(within(head).getByRole("button", { name: "New flow" })).toBeVisible();
  });

  it("names the providers that are ready in the empty state", async () => {
    const google = { id: "google", display_name: "Google", capabilities: [], detected: true, env: [], services: {} };
    const xai = { id: "xai", display_name: "xAI", capabilities: [], detected: false, env: [], services: {} };
    const cartesia = { id: "cartesia", display_name: "Cartesia", capabilities: [], detected: true, env: [], services: {} };
    setup({ getAgents: vi.fn().mockResolvedValue([]), getProviders: vi.fn().mockResolvedValue([google, xai, cartesia]) });
    expect(await screen.findByText("Google and Cartesia are ready to use.")).toBeVisible();
  });

  it("says when no provider is set up, with a way to see them", async () => {
    setup({ getAgents: vi.fn().mockResolvedValue([]), getProviders: vi.fn().mockResolvedValue([]) });
    expect(await screen.findByText("No provider is set up yet.")).toBeVisible();
    expect(screen.getByRole("link", { name: "See providers" })).toHaveAttribute("href", "/providers");
  });

  it("leaves the provider line out when providers cannot be loaded", async () => {
    setup({ getAgents: vi.fn().mockResolvedValue([]), getProviders: vi.fn().mockRejectedValue(new ApiError("down", "internal", 500, "r")) });
    await screen.findByRole("heading", { name: "No flows yet." });
    expect(screen.queryByText(/ready to use|No provider is set up/)).toBeNull();
  });
```

Check the file's existing test "teaches the vocabulary when there are no flows yet" still passes (it does not read the head). In `NewFlowDialog.test.tsx` add:

```tsx
  it("shows Blank and Copy as option cards that are real radios, with the chosen one marked", async () => {
    // render as the file's other cases do, with one flow to copy
    const blank = screen.getByRole("radio", { name: /Blank/ });
    expect(blank.closest(".option-card")).toHaveAttribute("data-selected", "true");
    await userEvent.setup().click(screen.getByRole("radio", { name: /Copy an existing flow/ }));
    expect(screen.getByRole("radio", { name: /Copy an existing flow/ }).closest(".option-card")).toHaveAttribute("data-selected", "true");
    expect(blank.closest(".option-card")).not.toHaveAttribute("data-selected");
  });
```

- [ ] **Step 2: Run them to see them fail**

Run: `cd ui && npx vitest run src/routes/agents/FlowsView.test.tsx src/routes/agents/NewFlowDialog.test.tsx`
Expected: FAIL.

- [ ] **Step 3: Implement**

`FlowsView.tsx`:
1. Add `const providersQuery = useQuery({ queryKey: ["providers"], queryFn: client.getProviders, enabled: !!keyValue, retry: false });` next to the other queries, and compute the ready line:

```tsx
  const ready = (providersQuery.data ?? []).filter((p) => p.detected).map((p) => p.display_name);
  const readyLine = !providersQuery.isSuccess
    ? null
    : ready.length === 0
      ? null
      : `${ready.length === 1 ? ready[0] : `${ready.slice(0, -1).join(", ")} and ${ready[ready.length - 1]}`} ${ready.length === 1 ? "is" : "are"} ready to use.`;
```

2. Replace the `<header className="section-heading">…</header>` with a head that carries the summary, filter and New flow when flows exist (the summary/filter/button render only in the list state; in other states show only the `h1`):

```tsx
      <header className="page-head">
        <h1 tabIndex={-1}>Flows</h1>
        {flows.length > 0 && (
          <span className="subtext" role="status">
            {enabledCount} enabled · {notReady} not ready{onCalls > 0 ? ` · ${plural(onCalls, "call", "calls")} in progress` : ""}
          </span>
        )}
        <span className="grow" />
        {flows.length > 0 && (
          <>
            <label className="form-field flows-filter" htmlFor="flows-filter">
              <span className="visually-hidden">Filter flows</span>
              <input id="flows-filter" type="search" placeholder="Filter by name" value={filter} onChange={(e) => setFilter(e.target.value)} />
            </label>
            <button type="button" className="action-button" onClick={() => openDialog({ kind: "new" })}>
              New flow
            </button>
          </>
        )}
      </header>
```

and delete the old toolbar block (`<div className="flows-toolbar"> … </div>`) inside the list state (keep the action-error, cap note and "No flow matches" paragraph under the head).

3. In the empty state, under the lead paragraph add:

```tsx
            {providersQuery.isSuccess &&
              (readyLine ? (
                <p className="subtext flows-empty__ready">
                  <span className="lamp" data-tone="ok" aria-hidden="true" /> {readyLine}
                </p>
              ) : (
                <p className="subtext">
                  No provider is set up yet. <Link to="/providers">See providers</Link>
                </p>
              ))}
```

(`Link` is already imported.)

`NewFlowDialog.tsx`: replace the two radio `<label className="check-row" …>` blocks with cards:

```tsx
          <label className="option-card" htmlFor={`${uid}-blank`} data-selected={mode === "blank" ? "true" : undefined}>
            <input id={`${uid}-blank`} type="radio" name={`${uid}-mode`} checked={mode === "blank"} onChange={() => setMode("blank")} />
            <span>
              <b>Blank</b>
              <span className="subtext">An empty canvas: add the first step yourself.</span>
            </span>
          </label>
          <label className="option-card" htmlFor={`${uid}-copy`} data-selected={mode === "copy" ? "true" : undefined}>
            <input id={`${uid}-copy`} type="radio" name={`${uid}-mode`} checked={mode === "copy"} disabled={flows.length === 0} onChange={() => setMode("copy")} />
            <span>
              <b>Copy an existing flow</b>
              <span className="subtext">Its steps, routes and settings. Call history is not copied.</span>
            </span>
          </label>
```

`styles.css` append:

```css
.option-card {
  display: grid;
  grid-template-columns: 22px 1fr;
  gap: 12px;
  padding: 14px 16px;
  border: 1px solid var(--rule);
  border-radius: var(--radius);
  background: transparent;
  cursor: pointer;
}
.option-card[data-selected="true"] {
  border-color: var(--caller);
  background: var(--surface);
  outline: 1px solid var(--caller);
}
.option-card b {
  display: block;
  font-weight: 600;
}
.flows-empty__ready {
  display: flex;
  align-items: center;
  gap: 8px;
}
```

- [ ] **Step 4: Run it to see it pass**

Run: `cd ui && npx vitest run && npx tsc --noEmit`
Expected: PASS. Older Flows tests that looked for the toolbar summary by role still find it (it moved into the header). If an e2e test locates the summary text, `getByText("1 enabled · …")` still works.

- [ ] **Step 5: Commit**

```bash
git add ui/src
git commit -m "feat(ui): Flows header with the summary and filter, ready providers in the empty state, New flow option cards

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 6: Publish dialog and the published banner

**Files:**
- Modify: `ui/src/routes/agents/graph/PublishDialog.tsx`, `ui/src/routes/agents/graph/FlowEditor.tsx`, `ui/src/routes/agents/FlowPage.tsx`, `ui/src/routes/agents/PublishedBanner.tsx`, `ui/src/styles.css`
- Test: `ui/src/routes/agents/graph/PublishDialog.test.tsx` (add), `ui/src/routes/agents/PublishedBanner.test.tsx` (adjust)

**Interfaces:**
- Produces:
  - `PublishDialogProps` gains `callsInProgress?: number` (calls running on the current version; the dialog says so only when it is above 0) and `currentVersion?: number`.
  - The dialog shows `What changes · N` as ruled rows and, when nothing blocks publishing, a **Checks** list with the two checks the editor has really made: `Every step is set up` and `Every step is reachable from the start step` (a flow that passes `validate` has both), each with a check mark.
  - `FlowEditor` gains `callsInProgress?: number`, passed through; `FlowPage` computes it as `liveCalls(active.data ?? [], flow).live` for a stored flow.
  - `PublishedBanner` gains `flowId: string`; its link reads `Watch calls` and goes to `/sessions?flow=<id>&outcome=live`; the banner is tinted (`--ok` mix) and the flow name is bold.

- [ ] **Step 1: Write the failing tests**

`PublishDialog.test.tsx` add (use the file's existing render helper/props factory):

```tsx
  it("says how many calls are in progress on the current version, and only when there are some", () => {
    renderDialog({ version: 8, currentVersion: 7, callsInProgress: 9, changes: ["x"], firstVersion: false });
    expect(screen.getByText(/The 9 calls on v7 right now finish on v7/)).toBeVisible();
  });
  it("leaves that sentence out when no call is running", () => {
    renderDialog({ version: 8, currentVersion: 7, callsInProgress: 0, changes: ["x"], firstVersion: false });
    expect(screen.queryByText(/right now finish on/)).toBeNull();
  });
  it("counts the changes in the heading, and lists the checks when nothing blocks publishing", () => {
    renderDialog({ changes: ["Renamed the flow to X", "Description changed"], firstVersion: false, problems: [] });
    expect(screen.getByRole("heading", { name: "What changes · 2", level: 3 })).toBeVisible();
    expect(screen.getByText("Every step is set up")).toBeVisible();
    expect(screen.getByText("Every step is reachable from the start step")).toBeVisible();
  });
  it("shows no checks when something blocks publishing", () => {
    renderDialog({ problems: ["Fix this."] });
    expect(screen.queryByText("Every step is set up")).toBeNull();
  });
```

`PublishedBanner.test.tsx`: `show()` passes `flowId="f1"`; the link assertion becomes `getByRole("link", { name: "Watch calls" })` with `href` `/sessions?flow=f1&outcome=live`; add `expect(banner.querySelector("b")).toHaveTextContent("Published v8.")` (it already is bold).

- [ ] **Step 2: Run them to see them fail**

Run: `cd ui && npx vitest run src/routes/agents`
Expected: FAIL.

- [ ] **Step 3: Implement**

`PublishDialog.tsx`: add the props and replace the body:

```tsx
export type PublishDialogProps = {
  flowName: string;
  version: number;
  /** The version in use now, and how many calls are running on it. */
  currentVersion?: number;
  callsInProgress?: number;
  firstVersion: boolean;
  changes: string[];
  problems: string[];
  pending: boolean;
  onConfirm: () => void;
  onCancel: () => void;
};
```

```tsx
export function PublishDialog({ flowName, version, currentVersion, callsInProgress = 0, firstVersion, changes, problems, pending, onConfirm, onCancel }: PublishDialogProps) {
  const changesId = useId();
  const checksId = useId();
  const blocked = problems.length > 0;
  return (
    <Modal title={`Publish ${flowName.trim() || "this flow"} v${version}?`} onCancel={onCancel}>
      <p>
        New calls get v{version} straight away.{" "}
        {callsInProgress > 0 && currentVersion !== undefined ? (
          <>
            <b>
              The {callsInProgress} {callsInProgress === 1 ? "call" : "calls"} on v{currentVersion} right now
            </b>{" "}
            finish on v{currentVersion}: publishing never changes a call in progress.
          </>
        ) : (
          "Calls already in progress finish on the version they started with."
        )}
      </p>
      {blocked ? (
        <div role="alert" className="note note--bad">
          <p>Fix these before publishing:</p>
          <ul className="plain-list">
            {problems.map((p) => (
              <li key={p}>{p}</li>
            ))}
          </ul>
        </div>
      ) : (
        <>
          {firstVersion ? (
            <p className="subtext">This is the first version of this flow.</p>
          ) : changes.length ? (
            <section aria-labelledby={changesId}>
              <h3 id={changesId}>What changes · {changes.length}</h3>
              <ul className="change-rows">
                {changes.map((c, i) => (
                  <li key={i}>{c}</li>
                ))}
              </ul>
            </section>
          ) : (
            <p className="subtext">No changes since v{version - 1}.</p>
          )}
          <section aria-labelledby={checksId}>
            <h3 id={checksId}>Checks</h3>
            <ul className="change-rows">
              <li>
                <span aria-hidden="true" className="check-mark">
                  ✓
                </span>
                Every step is set up
              </li>
              <li>
                <span aria-hidden="true" className="check-mark">
                  ✓
                </span>
                Every step is reachable from the start step
              </li>
            </ul>
          </section>
        </>
      )}
      <div className="row row--end">
        <button type="button" className="action-button-small" onClick={onCancel}>
          Keep editing
        </button>
        <button type="button" className="action-button" disabled={blocked || pending} onClick={onConfirm}>
          {pending ? "Publishing…" : `Publish v${version}`}
        </button>
      </div>
    </Modal>
  );
}
```

`FlowEditor.tsx`: add `callsInProgress?: number;` to `FlowEditorProps`, destructure it, and pass `currentVersion={agent ? version : undefined}` and `callsInProgress={callsInProgress}` to `PublishDialog`. `FlowPage.tsx`: pass `callsInProgress={flow ? liveCalls(active.data ?? [], flow).live : 0}` to `FlowEditor`, and `flowId={flow.id}` to `PublishedBanner`.

`PublishedBanner.tsx`: add `flowId` to the props, the link becomes `<Link to={`/sessions?flow=${encodeURIComponent(flowId)}&outcome=live`} …>Watch calls</Link>`. (The text, `Published v<n>.` in a `<b>`, is unchanged.)

`styles.css`: `.published-banner` background becomes `color-mix(in srgb, var(--ok) 10%, var(--surface))` and its border `color-mix(in srgb, var(--ok) 35%, var(--rule))`; append

```css
.change-rows {
  list-style: none;
  margin: 0;
  padding: 0;
  border-top: 1px solid var(--ink);
}
.change-rows li {
  display: flex;
  gap: 10px;
  padding: 9px 0;
  border-bottom: 1px solid var(--rule);
}
.check-mark {
  width: 14px;
  color: var(--ok);
}
```

- [ ] **Step 4: Run it to see it pass**

Run: `cd ui && npx vitest run && npx tsc --noEmit`
Expected: PASS. The `FlowPage` publish test and e2e `Published v2` lookups are unchanged.

- [ ] **Step 5: Commit**

```bash
git add ui/src
git commit -m "feat(ui): Publish dialog with calls in progress and checks; published banner scoped to its flow

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 7: A permanent screens spec, and the suites follow

**Files:**
- Create: `e2e/tests-graph/20-screens.spec.ts`
- Modify: `e2e/tests-graph/10-agent-graph.spec.ts` (only where a locator broke)

**Interfaces:**
- Produces: one Playwright spec that walks boards 3–12 and leaves a numbered screenshot of each in `e2e/evidence/` (via `step`), asserting the structure that matters (the editor header holds the breadcrumb; the stage is wider than 1000px at a 1440px viewport; the call panel's header shows the version; the published banner names the version).

- [ ] **Step 1: Write the spec.** Create `e2e/tests-graph/20-screens.spec.ts` from the temporary parity spec (the file `e2e/tests-graph/99-parity.spec.ts` from the comparison: a serial test that connects, shows the empty Flows page, seeds two flows through the API, opens the list, the New flow dialog, the blank editor, the first step panel, a stored flow with an added unfinished step, Flow settings, a live test call, the Publish dialog and the published banner), with these assertions added next to the screenshots:

```ts
  // board 8: the editor uses the width of the screen, and the steps are readable (not zoomed out to specks)
  const stage = page.locator(".flow-editor__stage");
  expect((await stage.boundingBox())!.width).toBeGreaterThan(1000);
  const card = page.locator(".react-flow__node").first();
  expect((await card.boundingBox())!.width).toBeGreaterThan(150);
  // board 10: the call panel says which version it is testing
  await expect(page.getByText(/^Test call · v\d+$/)).toBeVisible();
  // board 12: the banner names the version
  await expect(page.getByRole("status", { name: "Publish confirmation" })).toContainText("Published v2.");
```

(The card-width check is the guard for the zoomed-out graph defect: a step card renders at 210px at zoom 1; anything under 150px means the graph was fitted too small.) Keep `test.use({ viewport: { width: 1440, height: 1040 } })`. Delete `99-parity.spec.ts`.

- [ ] **Step 2: Run the suite**

Run: `make e2e-fake`
Expected: every graph spec passes, including this one, with screenshots numbered into `e2e/evidence/`. Fix locators in `10-agent-graph.spec.ts` that the new chrome broke (the breadcrumb now lives in the editor header; "Add step" keeps its name).

- [ ] **Step 3: Commit**

```bash
git add e2e
git commit -m "test(e2e): a screens spec that walks boards 3 to 12 and guards the editor's width and card size

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 8: Gate, spec update, a look beside the canvas, and independent review

**Files:**
- Modify: `docs/superpowers/specs/2026-10-12-switchboard-console-design.md`

**Interfaces:**
- Consumes: everything above.

- [ ] **Step 1: Update the spec.** In Section 3's table, in the rows for boards 3–12, add at the end of the notes: `Brought to the canvas's layout in plan 6 (flow screens parity).` In Section 4 add:

```
- The Test call panel follows the canvas (dark header with version and clock, controls, step-labelled transcript) without the waveform and the "median reply" figures (S9), and without a halo or shadow. The Publish dialog follows the canvas without the history note and the "roll back to vN" line (S2), and lists only the checks the editor really makes. The Flow settings footer says the changes are part of the draft until published (S1); it does not say "saved".
```

and in Section 6 add `7. **Done:** flow screens parity (boards 3–12): full-bleed editor, step cards, rebuilt Test call panel, Flows header and empty state, New flow cards, Publish checks.` (renumber if the list differs; keep First run as the remaining item).

- [ ] **Step 2: Full gate**

Run:

```bash
cd ui && npx vitest run && npx tsc --noEmit
cd .. && make e2e-fake
git status --short
```

Expected: all green; the tree is clean apart from the spec file.

- [ ] **Step 3: Look at it, next to the canvas.** Open the numbered screenshots from `e2e/evidence/` (boards 3–12) beside the canvas boards in the artifact. Fix any difference that does not need data the server lacks, each as a commit naming the difference.

- [ ] **Step 4: Independent review (AGENTS.md "Mandatory Code Review Workflow").** Dispatch a fresh reviewer on `main..HEAD` with this brief: YAGNI and bloat (CSS no longer used, props no longer read); correctness (the refit key and what it does to selection and dragging, `Canvas` `height="auto"` inside a flex stage and at 390px, the Test call panel's clock and its ref-based step labels under StrictMode and re-renders, `activeStep` flicker when the call hands off, the readiness test id kept, push-to-talk and mute unchanged, the Publish dialog's claims, the banner link); the five Review Focus items; accessibility (dark header contrast, headings, the new option cards, `Escape`); token-only colours and no shadows; tests that pass for the wrong reason. Fix confirmed findings, each with a test that fails first.

- [ ] **Step 5: Commit**

```bash
git add docs/superpowers/specs/2026-10-12-switchboard-console-design.md
git commit -m "docs: mark flow screens parity (boards 3-12) done and record what it leaves out

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```
