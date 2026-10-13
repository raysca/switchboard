# Flows List Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the old "Configured agents" table and its create/edit/detail screens with the Switchboard Flows list, a New flow dialog, an empty state, `/flows` routes, and the flow editor as its own page that confirms a publish.

**Architecture:** A `FlowsView` page lists flows as rows (status lamp, version, steps/routes/tools, live calls) with enable, test, copy and archive actions. A `FlowPage` route hosts the existing `FlowEditor` (plan 1) with its own breadcrumb and a "Published vN" banner. Dialogs share one native-`<dialog>` `Modal`. The flat `AgentForm`, the agent detail screen and the embedded `LiveGraph` are deleted. No server or API change: every figure comes from `GET /agents` and `GET /sessions?status=active`.

**Tech Stack:** React 19, TypeScript 7, react-router 7, @tanstack/react-query 5, vitest 5 + Testing Library, Playwright (`e2e/tests-graph`).

**Spec:** `docs/superpowers/specs/2026-10-12-switchboard-console-design.md` (Sections 1, 3, 4, 5; this is "plan 2"). Visual rules: `docs/superpowers/specs/2026-10-12-ui-switchboard-design.md`.

## Global Constraints

- UI copy says Flow / Step / Route / Start step / Flow settings. Never "agent", "node" or "handoff" in new copy ("agent" may still mean a human taking over). API names stay (`AgentDTO`, `client.getAgents`, `/api/v1/agents`, `agent_id`, the query key `["agents"]`) until the code rename (spec S6).
- Colours only through tokens (`var(--paper)`, `--surface`, `--ink`, `--ink-2`, `--rule`, `--caller`, `--lamp`, `--ok`, `--bad`). No hex or `rgb(` in TSX or below `/* end tokens */` in `ui/src/styles.css`. Add every new TSX file to `CONVERTED` in `ui/src/test/noHardcodedColors.test.ts`. Corner radius is `var(--radius)` or `50%` only (`system.css.test.ts`).
- Lamp amber (`--lamp`, `.lamp[data-tone="warn"]`) is for the live call only. Flow status uses `.lamp[data-tone="ok" | "bad"]` or the neutral default.
- Sentence-case labels; no all-caps or monospace labels; no drop shadows; focus ring stays the global 3px `--caller` outline; touch targets at least 44px; every icon-only control has an accessible name.
- Never offer a control the server cannot honour. These are omitted on purpose (spec Section 5): draft badges (S1), version rollback (S2), "calls today" and typical reply time per flow (S7), restoring archived flows (S8). Use these ids in code comments only where a reader would otherwise wonder.
- Enabling or disabling a flow sends `updateAgent(id, { enabled })` with no `config`. The server publishes a version only when a config is sent (`MutateAndPublish`: `publish := len(config) > 0`), so toggling never creates a version.
- `GET /agents` returns at most 100 flows and `GET /sessions` at most 100 rows; code must say so rather than silently truncate.
- Keep these test ids and names working: `start-voice-test-btn`, `connection-state`, `mute-btn`, `disconnect-btn`, `send-text-btn`, `transcript-user`, `transcript-bot`, `bot-streaming-text`, `agent-readiness`, `session-summary`; labels `Management key`, `Flow name`, `STT provider`, `LLM provider`, `TTS provider`, `Step name`, `Instructions`; buttons `Flow settings`, `Test call`, `Publish`, `Add step`, `Add first step`; the text `Published v2`.
- Gate for every task: `cd ui && npx vitest run && npx tsc --noEmit`.
- Commits are conventional (`feat(ui): …`) and end with `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`.

## Review Focus

1. **Toggling Enabled must not publish a version.** Expect: `updateAgent` is called with exactly `{ enabled }`, the row's version chip stays the same, and a failed toggle shows an error and leaves the checkbox at the server's value. Pinned in Task 4.
2. **A failing live-calls query must not break the list.** `GET /sessions` can fail or be slow while `GET /agents` works. Expect: rows still render, the live figure is omitted, no error banner. Pinned in Task 4.
3. **Unknown, archived-elsewhere or malformed flow id.** `/flows/nope`, or a flow archived in another tab while its editor is open. Expect: "That flow was not found." with a link back to the list, never a blank editor or a crash. Pinned in Task 6.
4. **Names with spaces and `&` survive `New flow`.** The blank flow's name travels in `?name=`. Expect: the editor header shows exactly `R&D line ñ`, and a second `?name=` edit never double-encodes. Pinned in Tasks 5 and 6.
5. **More flows than the API returns.** With exactly 100 flows the list is probably truncated. Expect: a note "Showing the first 100 flows." Pinned in Task 4.

---

### Task 1: Flow figures — counts, readiness and live calls

**Files:**
- Create: `ui/src/routes/agents/flowStats.ts`, `ui/src/routes/agents/useActiveSessions.ts`
- Test: `ui/src/routes/agents/flowStats.test.ts`, `ui/src/routes/agents/useActiveSessions.test.tsx`

**Interfaces:**
- Produces:
  - `type Tone = "ok" | "bad" | "idle"`
  - `stepCount(a: AgentDTO): number` (1 for a flat config), `routeCount(a): number`, `toolCount(a): number`
  - `readiness(a: AgentDTO): { tone: Tone; label: string }`
  - `canTest(a: AgentDTO): boolean`
  - `type LiveCalls = { live: number; onOlderVersion: number }`
  - `liveCalls(sessions: SessionDTO[], a: AgentDTO): LiveCalls`
  - `useActiveSessions(client: ApiClient, keyValue: string | null)`: a react-query result of `SessionDTO[]`, key `["sessions", "active"]`, polled every 10 s, `retry: false`.

- [ ] **Step 1: Write the failing tests**

`ui/src/routes/agents/flowStats.test.ts`:

```ts
import { describe, expect, it } from "vitest";
import type { AgentDTO, SessionDTO } from "../../api/client";
import { canTest, liveCalls, readiness, routeCount, stepCount, toolCount } from "./flowStats";

const agent = (over: Partial<AgentDTO> = {}, config: Record<string, unknown> = {}): AgentDTO => ({
  id: "a1",
  name: "Front desk",
  description: "",
  enabled: true,
  archived: false,
  current_version: { id: "v2", number: 2, schema_version: 1, config, config_hash: "h", created_at: "", mcp_tools: [] },
  readiness: { state: "ready", reasons: [] },
  created_at: "",
  updated_at: "",
  ...over,
});
const session = (over: Partial<SessionDTO>): SessionDTO => ({
  id: "s",
  agent_id: "a1",
  agent_version_id: "v2",
  config_hash: "h",
  started_at: "",
  status: "active",
  turn_count: 0,
  safe_summary: "",
  ...over,
});

describe("flow figures", () => {
  it("counts steps, routes and tools; a flat config is one step", () => {
    expect(stepCount(agent())).toBe(1);
    const graph = agent({}, { entry: "a", nodes: [{ id: "a" }, { id: "b" }, { id: "c" }], edges: [{ id: "e1" }, { id: "e2" }] });
    expect([stepCount(graph), routeCount(graph)]).toEqual([3, 2]);
    expect(routeCount(agent())).toBe(0);
    const withTools = agent();
    withTools.current_version!.mcp_tools = [{ mcp_server_id: "crm", remote_name: "lookup", schema_hash: "h", required: false }];
    expect(toolCount(withTools)).toBe(1);
    expect(toolCount(agent({ current_version: null }))).toBe(0);
  });

  it("maps readiness to a lamp tone and a plain label; disabled wins", () => {
    expect(readiness(agent())).toEqual({ tone: "ok", label: "Ready" });
    expect(readiness(agent({ readiness: { state: "ready_with_tool_warning", reasons: [] } }))).toEqual({ tone: "ok", label: "Ready (tool warning)" });
    expect(readiness(agent({ readiness: { state: "needs_tool_review", reasons: [] } }))).toEqual({ tone: "bad", label: "Needs tool review" });
    expect(readiness(agent({ readiness: { state: "tool_dependency_unavailable", reasons: [] } }))).toEqual({ tone: "bad", label: "Tool unavailable" });
    expect(readiness(agent({ readiness: { state: "not_ready", reasons: ["no_provider"] } }))).toEqual({ tone: "bad", label: "Not ready" });
    expect(readiness(agent({ enabled: false, readiness: { state: "not_ready", reasons: [] } }))).toEqual({ tone: "idle", label: "Disabled" });
  });

  it("only offers a test call for an enabled, ready flow", () => {
    expect(canTest(agent())).toBe(true);
    expect(canTest(agent({ enabled: false }))).toBe(false);
    expect(canTest(agent({ readiness: { state: "not_ready", reasons: [] } }))).toBe(false);
  });

  it("counts a flow's active calls and the ones still on an earlier version", () => {
    const sessions = [
      session({ id: "1" }),
      session({ id: "2", agent_version_id: "v1" }),
      session({ id: "3", agent_version_id: "v1" }),
      session({ id: "4", agent_id: "other", agent_version_id: "v1" }),
      session({ id: "5", status: "completed", agent_version_id: "v1" }),
    ];
    expect(liveCalls(sessions, agent())).toEqual({ live: 3, onOlderVersion: 2 });
    expect(liveCalls([], agent())).toEqual({ live: 0, onOlderVersion: 0 });
  });
});
```

`ui/src/routes/agents/useActiveSessions.test.tsx`:

```tsx
import { describe, expect, it, vi } from "vitest";
import { renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import type { ApiClient } from "../../api/client";
import { useActiveSessions } from "./useActiveSessions";

const wrapper = ({ children }: { children: ReactNode }) => (
  <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>{children}</QueryClientProvider>
);

describe("useActiveSessions", () => {
  it("asks for the active sessions, at the API's page size", async () => {
    const getSessions = vi.fn().mockResolvedValue([{ id: "s1" }]);
    const { result } = renderHook(() => useActiveSessions({ getSessions } as unknown as ApiClient, "key"), { wrapper });
    await waitFor(() => expect(result.current.data).toEqual([{ id: "s1" }]));
    expect(getSessions).toHaveBeenCalledWith({ status: "active", limit: 100 });
  });

  it("does nothing without a management key", () => {
    const getSessions = vi.fn();
    renderHook(() => useActiveSessions({ getSessions } as unknown as ApiClient, null), { wrapper });
    expect(getSessions).not.toHaveBeenCalled();
  });
});
```

- [ ] **Step 2: Run them to see them fail**

Run: `cd ui && npx vitest run src/routes/agents/flowStats.test.ts src/routes/agents/useActiveSessions.test.tsx`
Expected: FAIL — `Failed to resolve import "./flowStats"` / `"./useActiveSessions"`.

- [ ] **Step 3: Implement**

`ui/src/routes/agents/flowStats.ts`:

```ts
import type { AgentDTO, SessionDTO } from "../../api/client";

export type Tone = "ok" | "bad" | "idle";

export function stepCount(a: AgentDTO): number {
  const nodes = a.current_version?.config?.nodes;
  return Array.isArray(nodes) ? nodes.length : 1;
}

export function routeCount(a: AgentDTO): number {
  const edges = a.current_version?.config?.edges;
  return Array.isArray(edges) ? edges.length : 0;
}

export function toolCount(a: AgentDTO): number {
  return a.current_version?.mcp_tools?.length ?? 0;
}

/** The status lamp and its words. A disabled flow reads as Disabled whatever the server says. */
export function readiness(a: AgentDTO): { tone: Tone; label: string } {
  if (!a.enabled) return { tone: "idle", label: "Disabled" };
  switch (a.readiness.state) {
    case "ready":
      return { tone: "ok", label: "Ready" };
    case "ready_with_tool_warning":
      return { tone: "ok", label: "Ready (tool warning)" };
    case "needs_tool_review":
      return { tone: "bad", label: "Needs tool review" };
    case "tool_dependency_unavailable":
      return { tone: "bad", label: "Tool unavailable" };
    default:
      return { tone: "bad", label: "Not ready" };
  }
}

/** Mirrors the server: only these readiness states may take calls. */
export function canTest(a: AgentDTO): boolean {
  return a.enabled && (a.readiness.state === "ready" || a.readiness.state === "ready_with_tool_warning");
}

export type LiveCalls = { live: number; onOlderVersion: number };

/** A flow's calls in progress, and how many started on a version that is no longer current. */
export function liveCalls(sessions: SessionDTO[], a: AgentDTO): LiveCalls {
  const mine = sessions.filter((s) => s.status === "active" && s.agent_id === a.id);
  const current = a.current_version?.id;
  return { live: mine.length, onOlderVersion: mine.filter((s) => s.agent_version_id !== current).length };
}
```

`ui/src/routes/agents/useActiveSessions.ts`:

```ts
import { useQuery } from "@tanstack/react-query";
import type { ApiClient } from "../../api/client";

/** The calls in progress (the API returns at most 100), refreshed every 10 seconds. */
export function useActiveSessions(client: ApiClient, keyValue: string | null) {
  return useQuery({
    queryKey: ["sessions", "active"],
    queryFn: () => client.getSessions({ status: "active", limit: 100 }),
    enabled: !!keyValue,
    refetchInterval: 10_000,
    retry: false,
  });
}
```

- [ ] **Step 4: Run them to see them pass**

Run: `cd ui && npx vitest run src/routes/agents/flowStats.test.ts src/routes/agents/useActiveSessions.test.tsx && npx tsc --noEmit`
Expected: PASS (4 + 2 tests), no type errors.

- [ ] **Step 5: Commit**

```bash
git add ui/src/routes/agents/flowStats.ts ui/src/routes/agents/flowStats.test.ts ui/src/routes/agents/useActiveSessions.ts ui/src/routes/agents/useActiveSessions.test.tsx
git commit -m "feat(ui): flow figures: counts, readiness and live calls

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 2: One `Modal` for every dialog

**Files:**
- Create: `ui/src/components/Modal.tsx`, `ui/src/components/Modal.test.tsx`
- Modify: `ui/src/routes/agents/graph/PublishDialog.tsx`, `ui/src/test/noHardcodedColors.test.ts` (add `"components/Modal.tsx"`)

**Interfaces:**
- Produces: `Modal(props: { title: string; onCancel: () => void; children: ReactNode })`: a native `<dialog>` opened with `showModal()`, named by its `h2` title (the heading takes focus), cancelled by Escape (the browser's `cancel` event), returning focus to the element that opened it, also under StrictMode. Class `dialog` (already styled).
- `PublishDialog`'s props and behaviour do not change; its existing tests are the regression guard.

- [ ] **Step 1: Write the failing test** — `ui/src/components/Modal.test.tsx`:

```tsx
import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { StrictMode, useState } from "react";
import { Modal } from "./Modal";

describe("Modal", () => {
  it("is a modal dialog named by its title, with focus on the title", () => {
    render(
      <Modal title="Archive Front desk?" onCancel={vi.fn()}>
        <button>Keep</button>
      </Modal>,
    );
    const dialog = screen.getByRole("dialog", { name: "Archive Front desk?" });
    expect(dialog.tagName).toBe("DIALOG");
    expect(dialog).toHaveAttribute("open");
    expect(screen.getByRole("heading", { name: "Archive Front desk?" })).toHaveFocus();
    expect(dialog).toContainElement(screen.getByRole("button", { name: "Keep" }));
  });

  it("cancels on the browser's Escape (the cancel event)", () => {
    const onCancel = vi.fn();
    render(
      <Modal title="T" onCancel={onCancel}>
        <p>body</p>
      </Modal>,
    );
    fireEvent(screen.getByRole("dialog"), new Event("cancel", { cancelable: true }));
    expect(onCancel).toHaveBeenCalledOnce();
  });

  it("gives focus back to what opened it, also under StrictMode", async () => {
    const user = userEvent.setup();
    function Harness() {
      const [open, setOpen] = useState(false);
      return (
        <>
          <button onClick={() => setOpen(true)}>Open</button>
          {open && (
            <Modal title="T" onCancel={() => setOpen(false)}>
              <button onClick={() => setOpen(false)}>Close</button>
            </Modal>
          )}
        </>
      );
    }
    render(
      <StrictMode>
        <Harness />
      </StrictMode>,
    );
    await user.click(screen.getByRole("button", { name: "Open" }));
    await user.click(screen.getByRole("button", { name: "Close" }));
    expect(screen.getByRole("button", { name: "Open" })).toHaveFocus();
  });
});
```

- [ ] **Step 2: Run it to see it fail**

Run: `cd ui && npx vitest run src/components/Modal.test.tsx`
Expected: FAIL — `Failed to resolve import "./Modal"`.

- [ ] **Step 3: Implement** — `ui/src/components/Modal.tsx`:

```tsx
import { useEffect, useId, useRef, useState, type ReactNode } from "react";

export type ModalProps = { title: string; onCancel: () => void; children: ReactNode };

/** A native modal dialog: the page behind is inert, Tab stays inside, Escape cancels. */
export function Modal({ title, onCancel, children }: ModalProps) {
  const id = useId();
  const dialog = useRef<HTMLDialogElement>(null);
  const heading = useRef<HTMLHeadingElement>(null);
  // Read during the first render, before any effect: under StrictMode the effect runs twice, and by
  // the second run focus is already inside the dialog.
  const [opener] = useState(() => document.activeElement as HTMLElement | null);
  useEffect(() => {
    dialog.current?.showModal();
    heading.current?.focus();
    return () => {
      if (opener?.isConnected) opener.focus();
    };
  }, [opener]);
  return (
    <dialog
      ref={dialog}
      aria-labelledby={id}
      className="dialog"
      onCancel={(e) => {
        e.preventDefault();
        onCancel();
      }}
    >
      <h2 id={id} ref={heading} tabIndex={-1}>
        {title}
      </h2>
      {children}
    </dialog>
  );
}
```

Replace the whole of `ui/src/routes/agents/graph/PublishDialog.tsx` with:

```tsx
import { useId } from "react";
import { Modal } from "../../../components/Modal";

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
  const changesId = useId();
  const blocked = problems.length > 0;
  return (
    <Modal title={`Publish ${flowName.trim() || "this flow"} v${version}?`} onCancel={onCancel}>
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
        <section aria-labelledby={changesId}>
          <h3 id={changesId}>What changes</h3>
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
    </Modal>
  );
}
```

Add `"components/Modal.tsx"` to the `CONVERTED` array in `ui/src/test/noHardcodedColors.test.ts` (append before the closing `]`).

- [ ] **Step 4: Run the new test, the publish dialog's tests, the editor's, and the colour guard**

Run: `cd ui && npx vitest run src/components/Modal.test.tsx src/routes/agents/graph/PublishDialog.test.tsx src/routes/agents/graph/FlowEditor.test.tsx src/test && npx tsc --noEmit`
Expected: PASS everywhere (the unchanged `PublishDialog.test.tsx` is the proof the refactor kept behaviour).

- [ ] **Step 5: Commit**

```bash
git add ui/src/components/Modal.tsx ui/src/components/Modal.test.tsx ui/src/routes/agents/graph/PublishDialog.tsx ui/src/test/noHardcodedColors.test.ts
git commit -m "refactor(ui): one native Modal for the publish dialog and the new dialogs

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 3: New flow and Archive dialogs

**Files:**
- Create: `ui/src/routes/agents/NewFlowDialog.tsx`, `ui/src/routes/agents/ArchiveFlowDialog.tsx`
- Test: `ui/src/routes/agents/NewFlowDialog.test.tsx`, `ui/src/routes/agents/ArchiveFlowDialog.test.tsx`
- Modify: `ui/src/test/noHardcodedColors.test.ts` (add both TSX paths)

**Interfaces:**
- Consumes: `Modal` (Task 2).
- Produces:
  - `type FlowChoice = { id: string; name: string }`
  - `NewFlowDialog(props: { flows: FlowChoice[]; pending: boolean; error: string | null; onBlank: (name: string) => void; onCopy: (sourceId: string, name: string) => void; onCancel: () => void })`. Title `New flow`. Name is required in both modes. "Copy an existing flow" is disabled when `flows` is empty.
  - `ArchiveFlowDialog(props: { name: string; pending: boolean; error: string | null; onConfirm: () => void; onCancel: () => void })`. Title `Archive <name>?`.

- [ ] **Step 1: Write the failing tests**

`ui/src/routes/agents/NewFlowDialog.test.tsx`:

```tsx
import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { NewFlowDialog, type NewFlowDialogProps } from "./NewFlowDialog";

const flows = [
  { id: "a1", name: "Front desk" },
  { id: "a2", name: "Appointments" },
];
function setup(over: Partial<NewFlowDialogProps> = {}) {
  const props: NewFlowDialogProps = { flows, pending: false, error: null, onBlank: vi.fn(), onCopy: vi.fn(), onCancel: vi.fn(), ...over };
  render(<NewFlowDialog {...props} />);
  return { props, user: userEvent.setup() };
}

describe("NewFlowDialog", () => {
  it("creates a blank flow with the typed name", async () => {
    const { props, user } = setup();
    const create = screen.getByRole("button", { name: "Create and open editor" });
    expect(create).toBeDisabled();
    await user.type(screen.getByLabelText("Name"), "After hours");
    await user.click(create);
    expect(props.onBlank).toHaveBeenCalledWith("After hours");
    expect(props.onCopy).not.toHaveBeenCalled();
  });

  it("copies the chosen flow under a new name", async () => {
    const { props, user } = setup();
    await user.click(screen.getByRole("radio", { name: /Copy an existing flow/ }));
    await user.selectOptions(screen.getByLabelText("Flow to copy"), "a2");
    await user.type(screen.getByLabelText("Name"), "Night line");
    await user.click(screen.getByRole("button", { name: "Copy and open editor" }));
    expect(props.onCopy).toHaveBeenCalledWith("a2", "Night line");
    expect(props.onBlank).not.toHaveBeenCalled();
  });

  it("submits with Enter, and not while the name is blank or a request is running", async () => {
    const { props, user } = setup();
    await user.type(screen.getByLabelText("Name"), "   {Enter}");
    expect(props.onBlank).not.toHaveBeenCalled();
    await user.clear(screen.getByLabelText("Name"));
    await user.type(screen.getByLabelText("Name"), "X{Enter}");
    expect(props.onBlank).toHaveBeenCalledWith("X");
  });

  it("cannot copy when there is nothing to copy", () => {
    setup({ flows: [] });
    expect(screen.getByRole("radio", { name: /Copy an existing flow/ })).toBeDisabled();
  });

  it("shows a failed copy in the dialog and locks the button while it works", () => {
    setup({ error: "Copy failed" });
    expect(screen.getByRole("alert")).toHaveTextContent("Copy failed");
    setup({ pending: true });
    expect(screen.getAllByRole("button", { name: "Working…" })[0]).toBeDisabled();
  });

  it("cancels from the button and from Escape", async () => {
    const { props, user } = setup();
    await user.click(screen.getByRole("button", { name: "Cancel" }));
    fireEvent(screen.getByRole("dialog"), new Event("cancel", { cancelable: true }));
    expect(props.onCancel).toHaveBeenCalledTimes(2);
  });
});
```

`ui/src/routes/agents/ArchiveFlowDialog.test.tsx`:

```tsx
import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ArchiveFlowDialog } from "./ArchiveFlowDialog";

describe("ArchiveFlowDialog", () => {
  it("says what archiving does and confirms or keeps the flow", async () => {
    const user = userEvent.setup();
    const onConfirm = vi.fn();
    const onCancel = vi.fn();
    render(<ArchiveFlowDialog name="Front desk" pending={false} error={null} onConfirm={onConfirm} onCancel={onCancel} />);
    const dialog = screen.getByRole("dialog", { name: "Archive Front desk?" });
    expect(dialog).toHaveTextContent("stops taking calls");
    expect(dialog).toHaveTextContent("version history and past calls are kept");
    await user.click(screen.getByRole("button", { name: "Keep flow" }));
    await user.click(screen.getByRole("button", { name: "Archive flow" }));
    expect(onCancel).toHaveBeenCalledOnce();
    expect(onConfirm).toHaveBeenCalledOnce();
  });

  it("shows a failure and locks the button while archiving", () => {
    render(<ArchiveFlowDialog name="X" pending error="Archive failed" onConfirm={vi.fn()} onCancel={vi.fn()} />);
    expect(screen.getByRole("alert")).toHaveTextContent("Archive failed");
    expect(screen.getByRole("button", { name: "Archiving…" })).toBeDisabled();
  });
});
```

- [ ] **Step 2: Run them to see them fail**

Run: `cd ui && npx vitest run src/routes/agents/NewFlowDialog.test.tsx src/routes/agents/ArchiveFlowDialog.test.tsx`
Expected: FAIL — imports not found.

- [ ] **Step 3: Implement**

`ui/src/routes/agents/NewFlowDialog.tsx`:

```tsx
import { useId, useState, type FormEvent } from "react";
import { Modal } from "../../components/Modal";

export type FlowChoice = { id: string; name: string };

export type NewFlowDialogProps = {
  flows: FlowChoice[];
  pending: boolean;
  error: string | null;
  onBlank: (name: string) => void;
  onCopy: (sourceId: string, name: string) => void;
  onCancel: () => void;
};

/** Name a flow, then start from nothing or from a copy of an existing one. */
export function NewFlowDialog({ flows, pending, error, onBlank, onCopy, onCancel }: NewFlowDialogProps) {
  const uid = useId();
  const [name, setName] = useState("");
  const [mode, setMode] = useState<"blank" | "copy">("blank");
  const [source, setSource] = useState(flows[0]?.id ?? "");
  const copying = mode === "copy" && !!source;
  const ready = name.trim() !== "" && !pending;

  function submit(e: FormEvent) {
    e.preventDefault();
    if (!ready) return;
    if (copying) onCopy(source, name.trim());
    else onBlank(name.trim());
  }

  return (
    <Modal title="New flow" onCancel={onCancel}>
      <form onSubmit={submit} className="dialog__form" noValidate>
        <label className="form-field" htmlFor={`${uid}-name`}>
          <span>Name</span>
          <input id={`${uid}-name`} type="text" value={name} maxLength={128} autoComplete="off" onChange={(e) => setName(e.target.value)} />
        </label>
        <fieldset className="stage-box">
          <legend>Start from</legend>
          <label className="check-row" htmlFor={`${uid}-blank`}>
            <input id={`${uid}-blank`} type="radio" name={`${uid}-mode`} checked={mode === "blank"} onChange={() => setMode("blank")} />
            <span>
              <b>Blank</b> An empty canvas: add the first step yourself.
            </span>
          </label>
          <label className="check-row" htmlFor={`${uid}-copy`}>
            <input id={`${uid}-copy`} type="radio" name={`${uid}-mode`} checked={mode === "copy"} disabled={flows.length === 0} onChange={() => setMode("copy")} />
            <span>
              <b>Copy an existing flow</b> Its steps, routes and settings. Call history is not copied.
            </span>
          </label>
          {mode === "copy" && flows.length > 0 && (
            <label className="form-field" htmlFor={`${uid}-source`}>
              <span>Flow to copy</span>
              <select id={`${uid}-source`} value={source} onChange={(e) => setSource(e.target.value)}>
                {flows.map((f) => (
                  <option key={f.id} value={f.id}>
                    {f.name}
                  </option>
                ))}
              </select>
            </label>
          )}
        </fieldset>
        {error && (
          <p role="alert" className="note note--bad">
            {error}
          </p>
        )}
        <p className="subtext">A new flow is a draft in this editor until you publish it. Nothing takes calls before that.</p>
        <div className="row row--end">
          <button type="button" className="action-button-small" onClick={onCancel}>
            Cancel
          </button>
          <button type="submit" className="action-button" disabled={!ready}>
            {pending ? "Working…" : copying ? "Copy and open editor" : "Create and open editor"}
          </button>
        </div>
      </form>
    </Modal>
  );
}
```

`ui/src/routes/agents/ArchiveFlowDialog.tsx`:

```tsx
import { Modal } from "../../components/Modal";

export type ArchiveFlowDialogProps = {
  name: string;
  pending: boolean;
  error: string | null;
  onConfirm: () => void;
  onCancel: () => void;
};

/** Archiving hides a flow and stops it taking calls; its history stays. */
export function ArchiveFlowDialog({ name, pending, error, onConfirm, onCancel }: ArchiveFlowDialogProps) {
  return (
    <Modal title={`Archive ${name}?`} onCancel={onCancel}>
      <p>
        It stops taking calls and leaves this list. Its version history and past calls are kept. Calls in progress are not cut off.
      </p>
      {error && (
        <p role="alert" className="note note--bad">
          {error}
        </p>
      )}
      <div className="row row--end">
        <button type="button" className="action-button-small" onClick={onCancel}>
          Keep flow
        </button>
        <button type="button" className="action-button action-button-danger" disabled={pending} onClick={onConfirm}>
          {pending ? "Archiving…" : "Archive flow"}
        </button>
      </div>
    </Modal>
  );
}
```

Append to `ui/src/styles.css`:

```css
.dialog__form {
  display: flex;
  flex-direction: column;
  gap: 14px;
}
```

Add `"routes/agents/NewFlowDialog.tsx"` and `"routes/agents/ArchiveFlowDialog.tsx"` to `CONVERTED` in `noHardcodedColors.test.ts`.

- [ ] **Step 4: Run them to see them pass**

Run: `cd ui && npx vitest run src/routes/agents/NewFlowDialog.test.tsx src/routes/agents/ArchiveFlowDialog.test.tsx src/test && npx tsc --noEmit`
Expected: PASS. (The Enter-key case relies on jsdom submitting a form when Enter is pressed in a text input that has a submit button in its form.)

- [ ] **Step 5: Commit**

```bash
git add ui/src/routes/agents/NewFlowDialog.tsx ui/src/routes/agents/NewFlowDialog.test.tsx ui/src/routes/agents/ArchiveFlowDialog.tsx ui/src/routes/agents/ArchiveFlowDialog.test.tsx ui/src/styles.css ui/src/test/noHardcodedColors.test.ts
git commit -m "feat(ui): New flow dialog (blank or copy) and Archive flow dialog

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 4: `FlowsView`, the list page

**Files:**
- Create: `ui/src/routes/agents/FlowsView.tsx`, `ui/src/routes/agents/FlowsView.test.tsx`
- Modify: `ui/src/styles.css` (append), `ui/src/test/noHardcodedColors.test.ts` (add `"routes/agents/FlowsView.tsx"`)

**Interfaces:**
- Consumes: `flowStats` and `useActiveSessions` (Task 1), `NewFlowDialog`, `ArchiveFlowDialog` (Task 3), `client.getAgents`, `client.updateAgent`, `client.duplicateAgent`, `client.archiveAgent`.
- Produces: `FlowsView(props: { client: ApiClient; keyValue: string | null })`. Navigation it performs: Blank → `/flows/new?name=<encodeURIComponent(name)>`; Copy → `/flows/<new id>`; row title → `/flows/<id>`; row Test → `/flows/<id>?panel=test`; empty state → `/onboarding`.

- [ ] **Step 1: Write the failing test** — `ui/src/routes/agents/FlowsView.test.tsx`:

```tsx
import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes, useLocation } from "react-router";
import type { AgentDTO, ApiClient, SessionDTO } from "../../api/client";
import { ApiError } from "../../api/client";
import { FlowsView } from "./FlowsView";

const flow = (over: Partial<AgentDTO> = {}, config: Record<string, unknown> = {}): AgentDTO => ({
  id: "a1",
  name: "Front desk",
  description: "Answers the main line.",
  enabled: true,
  archived: false,
  current_version: { id: "v2", number: 2, schema_version: 1, config, config_hash: "h", created_at: "", mcp_tools: [] },
  readiness: { state: "ready", reasons: [] },
  created_at: "",
  updated_at: "",
  ...over,
});
const graph = { entry: "a", nodes: [{ id: "a" }, { id: "b" }, { id: "c" }, { id: "d" }], edges: [{ id: "e1" }, { id: "e2" }, { id: "e3" }] };
const session = (over: Partial<SessionDTO>): SessionDTO => ({
  id: "s",
  agent_id: "a1",
  agent_version_id: "v2",
  config_hash: "h",
  started_at: "",
  status: "active",
  turn_count: 0,
  safe_summary: "",
  ...over,
});

function Where() {
  const l = useLocation();
  return <p data-testid="where">{l.pathname + l.search}</p>;
}
function setup(client: Partial<ApiClient>, key: string | null = "k") {
  const full = { getAgents: vi.fn().mockResolvedValue([flow({}, graph)]), getSessions: vi.fn().mockResolvedValue([]), ...client } as unknown as ApiClient;
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <MemoryRouter initialEntries={["/flows"]}>
        <Routes>
          <Route path="/flows" element={<FlowsView client={full} keyValue={key} />} />
          <Route path="*" element={<Where />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
  return { client: full, user: userEvent.setup() };
}

describe("FlowsView", () => {
  it("asks for a management key first", () => {
    setup({}, null);
    expect(screen.getByText(/Enter a management key/)).toBeVisible();
  });

  it("lists flows with status, version, figures and live calls", async () => {
    setup({
      getSessions: vi.fn().mockResolvedValue([session({ id: "1" }), session({ id: "2", agent_version_id: "v1" })]),
      getAgents: vi.fn().mockResolvedValue([
        flow({}, graph),
        flow({ id: "a2", name: "Appointments", enabled: false, readiness: { state: "not_ready", reasons: ["no_provider"] } }),
      ]),
    });
    const row = (await screen.findByRole("link", { name: "Front desk" })).closest("li") as HTMLElement;
    expect(within(row).getByText("v2")).toBeVisible();
    expect(within(row).getByText("Ready")).toBeVisible();
    expect(within(row).getByText("4 steps · 3 routes · 0 tools")).toBeVisible();
    await waitFor(() => expect(within(row).getByText("2 calls live, 1 on an earlier version")).toBeVisible());
    const second = screen.getByRole("link", { name: "Appointments" }).closest("li") as HTMLElement;
    expect(within(second).getByText("Disabled")).toBeVisible();
    expect(within(second).getByText("1 step · 0 routes · 0 tools")).toBeVisible();
    expect(screen.getByRole("heading", { name: "Flows", level: 1 })).toBeVisible();
    // A disabled flow is not "not ready": it is switched off.
    expect(screen.getByText("1 enabled · 0 not ready")).toBeVisible();
  });

  it("keeps listing flows when the live-calls query fails", async () => {
    setup({ getSessions: vi.fn().mockRejectedValue(new ApiError("boom", "internal", 500)) });
    const row = (await screen.findByRole("link", { name: "Front desk" })).closest("li") as HTMLElement;
    expect(within(row).getByText("Ready")).toBeVisible();
    expect(within(row).queryByText(/calls? live/)).toBeNull();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("shows why a flow is not ready, and offers Test only to a ready one", async () => {
    setup({
      getAgents: vi.fn().mockResolvedValue([
        flow(),
        flow({ id: "a2", name: "Broken", readiness: { state: "not_ready", reasons: ["missing_configuration"] } }),
      ]),
    });
    const ready = (await screen.findByRole("link", { name: "Front desk" })).closest("li") as HTMLElement;
    expect(within(ready).getByRole("link", { name: "Test Front desk" })).toHaveAttribute("href", "/flows/a1?panel=test");
    const broken = screen.getByRole("link", { name: "Broken" }).closest("li") as HTMLElement;
    expect(within(broken).getByText("missing_configuration")).toBeVisible();
    expect(within(broken).queryByRole("link", { name: /^Test/ })).toBeNull();
    expect(screen.getByText("2 enabled · 1 not ready")).toBeVisible();
  });

  it("toggles Enabled by sending only enabled, never a config", async () => {
    const updateAgent = vi.fn().mockResolvedValue(flow({ enabled: false }));
    const { user } = setup({ updateAgent });
    await user.click(await screen.findByRole("checkbox", { name: "Front desk enabled" }));
    expect(updateAgent).toHaveBeenCalledWith("a1", { enabled: false });
    expect(Object.keys(updateAgent.mock.calls[0][1])).toEqual(["enabled"]);
  });

  it("shows a failed toggle instead of swallowing it", async () => {
    const { user } = setup({ updateAgent: vi.fn().mockRejectedValue(new ApiError("nope", "conflict", 409)) });
    await user.click(await screen.findByRole("checkbox", { name: "Front desk enabled" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("nope");
  });

  it("filters by name", async () => {
    const { user } = setup({ getAgents: vi.fn().mockResolvedValue([flow(), flow({ id: "a2", name: "Appointments" })]) });
    await screen.findByRole("link", { name: "Front desk" });
    await user.type(screen.getByLabelText("Filter flows"), "appoint");
    expect(screen.queryByRole("link", { name: "Front desk" })).toBeNull();
    expect(screen.getByRole("link", { name: "Appointments" })).toBeVisible();
    await user.clear(screen.getByLabelText("Filter flows"));
    await user.type(screen.getByLabelText("Filter flows"), "zzz");
    expect(screen.getByText("No flow matches “zzz”.")).toBeVisible();
  });

  it("copies a flow from its row and stays on the list", async () => {
    const duplicateAgent = vi.fn().mockResolvedValue(flow({ id: "a9", name: "Front desk copy" }));
    const { user } = setup({ duplicateAgent });
    await user.click(await screen.findByRole("button", { name: "Duplicate Front desk" }));
    expect(duplicateAgent).toHaveBeenCalledWith("a1", undefined);
    expect(screen.queryByTestId("where")).toBeNull();
  });

  it("archives only after confirmation, and a failure stays in the dialog", async () => {
    const archiveAgent = vi.fn().mockRejectedValueOnce(new ApiError("busy", "conflict", 409)).mockResolvedValue({ archived: true });
    const { user } = setup({ archiveAgent });
    await user.click(await screen.findByRole("button", { name: "Archive Front desk" }));
    expect(archiveAgent).not.toHaveBeenCalled();
    await user.click(screen.getByRole("button", { name: "Archive flow" }));
    expect(await within(screen.getByRole("dialog")).findByRole("alert")).toHaveTextContent("busy");
    await user.click(screen.getByRole("button", { name: "Archive flow" }));
    await waitFor(() => expect(archiveAgent).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  it("opens the editor on a blank flow, carrying a name with spaces and &", async () => {
    const { user } = setup({});
    await user.click(await screen.findByRole("button", { name: "New flow" }));
    await user.type(screen.getByLabelText("Name"), "R&D line ñ");
    await user.click(screen.getByRole("button", { name: "Create and open editor" }));
    expect(screen.getByTestId("where")).toHaveTextContent("/flows/new?name=R%26D%20line%20%C3%B1");
  });

  it("copies from the dialog and opens the copy", async () => {
    const duplicateAgent = vi.fn().mockResolvedValue(flow({ id: "a9", name: "Night line" }));
    const { user } = setup({ duplicateAgent });
    await user.click(await screen.findByRole("button", { name: "New flow" }));
    await user.click(screen.getByRole("radio", { name: /Copy an existing flow/ }));
    await user.type(screen.getByLabelText("Name"), "Night line");
    await user.click(screen.getByRole("button", { name: "Copy and open editor" }));
    expect(duplicateAgent).toHaveBeenCalledWith("a1", { name: "Night line" });
    expect(await screen.findByTestId("where")).toHaveTextContent("/flows/a9");
  });

  it("teaches the vocabulary when there are no flows yet", async () => {
    setup({ getAgents: vi.fn().mockResolvedValue([]) });
    expect(await screen.findByRole("heading", { name: "No flows yet." })).toBeVisible();
    for (const term of ["Flow", "Steps", "Routes", "Flow settings"]) expect(screen.getByText(term, { selector: "dt" })).toBeVisible();
    expect(screen.getByRole("link", { name: "Use guided setup" })).toHaveAttribute("href", "/onboarding");
    // One label for the action in both states, so it is always "New flow".
    expect(screen.getByRole("button", { name: "New flow" })).toBeVisible();
  });

  it("hides archived flows, and says when the list was cut at the API's page size", async () => {
    const many = Array.from({ length: 100 }, (_, i) => flow({ id: `f${i}`, name: `Flow ${i}` }));
    many[0] = flow({ id: "f0", name: "Gone", archived: true });
    setup({ getAgents: vi.fn().mockResolvedValue(many) });
    expect(await screen.findByText("Showing the first 100 flows.")).toBeVisible();
    expect(screen.queryByRole("link", { name: "Gone" })).toBeNull();
  });

  it("shows a load failure with a retry", async () => {
    setup({ getAgents: vi.fn().mockRejectedValue(new ApiError("down", "internal", 500)) });
    expect(await screen.findByRole("alert")).toHaveTextContent("down");
    expect(screen.getByRole("button", { name: "Try again" })).toBeVisible();
  });
});
```

- [ ] **Step 2: Run it to see it fail**

Run: `cd ui && npx vitest run src/routes/agents/FlowsView.test.tsx`
Expected: FAIL — `Failed to resolve import "./FlowsView"`.

- [ ] **Step 3: Implement**

`ui/src/routes/agents/FlowsView.tsx`:

```tsx
import { useState } from "react";
import { Link, useNavigate } from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { asApiError, type AgentDTO, type ApiClient, type ApiError } from "../../api/client";
import { EmptyState, ErrorState, LoadingState } from "../../components/primitives";
import { ArchiveFlowDialog } from "./ArchiveFlowDialog";
import { NewFlowDialog } from "./NewFlowDialog";
import { canTest, liveCalls, readiness, routeCount, stepCount, toolCount } from "./flowStats";
import { useActiveSessions } from "./useActiveSessions";

/** The management API returns at most this many flows (GET /agents?limit=100). */
const LIST_LIMIT = 100;
const plural = (n: number, one: string, many: string) => `${n} ${n === 1 ? one : many}`;

export type FlowsViewProps = { client: ApiClient; keyValue: string | null };
type Dialog = { kind: "new" } | { kind: "archive"; flow: AgentDTO } | null;

export function FlowsView({ client, keyValue }: FlowsViewProps) {
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const [filter, setFilter] = useState("");
  const [dialog, setDialog] = useState<Dialog>(null);
  const [actionError, setActionError] = useState<ApiError | null>(null);

  const flowsQuery = useQuery({ queryKey: ["agents"], queryFn: () => client.getAgents(), enabled: !!keyValue, retry: false });
  const active = useActiveSessions(client, keyValue);
  const sessions = active.data ?? [];

  const refresh = () => void queryClient.invalidateQueries({ queryKey: ["agents"] });
  const toggleMutation = useMutation({
    // Only `enabled`, never a config: the server publishes a version only when a config is sent.
    mutationFn: ({ id, enabled }: { id: string; enabled: boolean }) => client.updateAgent(id, { enabled }),
    onSuccess: () => {
      setActionError(null);
      refresh();
    },
    onError: (err) => setActionError(asApiError(err, "Could not change whether the flow is enabled")),
  });
  const copyMutation = useMutation({
    mutationFn: ({ id, name }: { id: string; name?: string; open?: boolean }) => client.duplicateAgent(id, name ? { name } : undefined),
    onSuccess: (copy, vars) => {
      setActionError(null);
      setDialog(null);
      refresh();
      if (vars.open) navigate(`/flows/${encodeURIComponent(copy.id)}`);
    },
    onError: (err) => setActionError(asApiError(err, "Copy failed")),
  });
  const archiveMutation = useMutation({
    mutationFn: (id: string) => client.archiveAgent(id),
    onSuccess: () => {
      setActionError(null);
      setDialog(null);
      refresh();
    },
    onError: (err) => setActionError(asApiError(err, "Archive failed")),
  });

  const all = flowsQuery.data ?? [];
  const flows = all.filter((f) => !f.archived);
  const q = filter.trim().toLowerCase();
  const shown = q ? flows.filter((f) => f.name.toLowerCase().includes(q)) : flows;
  const enabledCount = flows.filter((f) => f.enabled).length;
  const notReady = flows.filter((f) => f.enabled && readiness(f).tone === "bad").length;
  const onCalls = flows.reduce((n, f) => n + liveCalls(sessions, f).live, 0);
  const openDialog = (d: Dialog) => {
    setActionError(null);
    setDialog(d);
  };

  return (
    <>
      <header className="section-heading">
        <h1 tabIndex={-1}>Flows</h1>
        <p>What callers reach: a flow is a set of steps, with routes between them.</p>
      </header>

      {!keyValue ? (
        <EmptyState title="Connect to manage flows">Enter a management key above to configure voice flows.</EmptyState>
      ) : flowsQuery.isPending ? (
        <LoadingState label="Loading flows" />
      ) : flowsQuery.isError ? (
        <ErrorState title="Failed to load flows" error={flowsQuery.error as ApiError} onRetry={() => void flowsQuery.refetch()} />
      ) : flows.length === 0 ? (
        <section aria-labelledby="no-flows" className="flows-empty">
          <div className="flows-empty__lead">
            <h2 id="no-flows">No flows yet.</h2>
            <p>A flow is one voice line. Start with a single step that answers the phone; add more steps and routes when you need them.</p>
            <div className="row">
              <button type="button" className="action-button" onClick={() => openDialog({ kind: "new" })}>
                New flow
              </button>
              <Link to="/onboarding" className="action-button-small">
                Use guided setup
              </Link>
            </div>
          </div>
          <dl className="flows-empty__terms">
            <div><dt>Flow</dt><dd>What callers reach. You create it, test it and publish it in versions.</dd></div>
            <div><dt>Steps</dt><dd>Parts of one flow, like Reception or Billing. Each has its own instructions, model, voice and tools. Up to 8.</dd></div>
            <div><dt>Routes</dt><dd>Plain-language conditions that move the caller from one step to another.</dd></div>
            <div><dt>Flow settings</dt><dd>Shared by the whole call: listening, turn-taking, memory, recording.</dd></div>
          </dl>
        </section>
      ) : (
        <div className="flows-view">
          <div className="flows-toolbar">
            <p className="subtext" role="status">
              {enabledCount} enabled · {notReady} not ready{onCalls > 0 ? ` · ${plural(onCalls, "call", "calls")} in progress` : ""}
            </p>
            <span className="grow" />
            <label className="form-field flows-filter" htmlFor="flows-filter">
              <span className="visually-hidden">Filter flows</span>
              <input id="flows-filter" type="search" placeholder="Filter by name" value={filter} onChange={(e) => setFilter(e.target.value)} />
            </label>
            <button type="button" className="action-button" onClick={() => openDialog({ kind: "new" })}>
              New flow
            </button>
          </div>

          {actionError && !dialog && <ErrorState title="Flow action failed" error={actionError} onRetry={() => setActionError(null)} />}
          {all.length >= LIST_LIMIT && <p className="subtext" role="note">Showing the first {LIST_LIMIT} flows.</p>}
          {shown.length === 0 && <p className="subtext">No flow matches “{filter.trim()}”.</p>}

          <ul className="flow-list">
            {shown.map((f) => {
              const r = readiness(f);
              const live = liveCalls(sessions, f);
              return (
                <li key={f.id} className="flow-row">
                  <div className="flow-row__main">
                    <div className="flow-row__title">
                      <span className="lamp" data-tone={r.tone === "idle" ? undefined : r.tone} aria-hidden="true" />
                      <Link to={`/flows/${encodeURIComponent(f.id)}`}>{f.name}</Link>
                      <span className="chip">v{f.current_version?.number ?? 1}</span>
                      <span className="flow-row__state">{r.label}</span>
                    </div>
                    {f.description && <p className="subtext">{f.description}</p>}
                    {f.readiness.reasons.length > 0 && (
                      <div className="reasons-list">
                        {f.readiness.reasons.map((reason) => (
                          <span key={reason} className="reason">
                            {reason}
                          </span>
                        ))}
                      </div>
                    )}
                  </div>
                  <div className="flow-row__figures">
                    <p>
                      {plural(stepCount(f), "step", "steps")} · {plural(routeCount(f), "route", "routes")} · {plural(toolCount(f), "tool", "tools")}
                    </p>
                    {live.live > 0 && (
                      <p>
                        {plural(live.live, "call", "calls")} live{live.onOlderVersion > 0 ? `, ${live.onOlderVersion} on an earlier version` : ""}
                      </p>
                    )}
                  </div>
                  <div className="flow-row__actions">
                    {canTest(f) && (
                      <Link to={`/flows/${encodeURIComponent(f.id)}?panel=test`} className="action-button-small" aria-label={`Test ${f.name}`}>
                        Test
                      </Link>
                    )}
                    <label className="check-row">
                      <input
                        type="checkbox"
                        aria-label={`${f.name} enabled`}
                        checked={f.enabled}
                        disabled={toggleMutation.isPending}
                        onChange={(e) => toggleMutation.mutate({ id: f.id, enabled: e.target.checked })}
                      />
                      Enabled
                    </label>
                    <button type="button" className="action-button-small" aria-label={`Duplicate ${f.name}`} disabled={copyMutation.isPending} onClick={() => copyMutation.mutate({ id: f.id })}>
                      Duplicate
                    </button>
                    <button type="button" className="action-button-small action-button-danger" aria-label={`Archive ${f.name}`} onClick={() => openDialog({ kind: "archive", flow: f })}>
                      Archive
                    </button>
                  </div>
                </li>
              );
            })}
          </ul>
        </div>
      )}

      {dialog?.kind === "new" && (
        <NewFlowDialog
          flows={flows.map((f) => ({ id: f.id, name: f.name }))}
          pending={copyMutation.isPending}
          error={actionError?.message ?? null}
          onBlank={(name) => navigate(`/flows/new?name=${encodeURIComponent(name)}`)}
          onCopy={(id, name) => copyMutation.mutate({ id, name, open: true })}
          onCancel={() => setDialog(null)}
        />
      )}
      {dialog?.kind === "archive" && (
        <ArchiveFlowDialog
          name={dialog.flow.name}
          pending={archiveMutation.isPending}
          error={actionError?.message ?? null}
          onConfirm={() => archiveMutation.mutate(dialog.flow.id)}
          onCancel={() => setDialog(null)}
        />
      )}
    </>
  );
}
```

Append to `ui/src/styles.css`:

```css
.visually-hidden {
  position: absolute;
  width: 1px;
  height: 1px;
  overflow: hidden;
  clip: rect(0 0 0 0);
  white-space: nowrap;
}
.flows-toolbar {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 10px 14px;
  margin-bottom: 12px;
}
.flows-filter {
  width: 240px;
  max-width: 100%;
}
.flow-list {
  list-style: none;
  margin: 0;
  padding: 0;
  border-top: 1px solid var(--ink);
}
.flow-row {
  display: flex;
  flex-wrap: wrap;
  align-items: flex-start;
  gap: 12px 32px;
  padding: 20px 0;
  border-bottom: 1px solid var(--rule);
}
.flow-row__main {
  flex: 3 1 320px;
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.flow-row__title {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 10px;
}
.flow-row__title a {
  font-size: 19px;
  font-weight: 600;
}
.flow-row__state,
.flow-row__figures {
  color: var(--ink-2);
  font-size: 14px;
}
.flow-row__figures {
  flex: 2 1 220px;
  min-width: 0;
  font-variant-numeric: tabular-nums;
}
.flow-row__figures p {
  margin: 0;
}
.flow-row__actions {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
}
.flows-empty {
  display: flex;
  flex-wrap: wrap;
  gap: 40px;
  padding: 32px 0;
  border-top: 1px solid var(--ink);
}
.flows-empty__lead {
  flex: 1 1 360px;
  max-width: 520px;
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.flows-empty__lead h2 {
  margin: 0;
}
.flows-empty__terms {
  flex: 1 1 400px;
  margin: 0;
  border: 1px solid var(--rule);
  border-radius: var(--radius);
  background: var(--surface);
  padding: 4px 20px;
}
.flows-empty__terms div {
  display: grid;
  grid-template-columns: 120px 1fr;
  gap: 16px;
  padding: 14px 0;
  border-bottom: 1px solid var(--rule);
}
.flows-empty__terms div:last-child {
  border-bottom: 0;
}
.flows-empty__terms dt {
  font-weight: 600;
}
.flows-empty__terms dd {
  margin: 0;
  color: var(--ink-2);
  font-size: 14px;
}
```

Add `"routes/agents/FlowsView.tsx"` to `CONVERTED` in `noHardcodedColors.test.ts`.

- [ ] **Step 4: Run it to see it pass**

Run: `cd ui && npx vitest run src/routes/agents/FlowsView.test.tsx src/test && npx tsc --noEmit`
Expected: PASS (13 tests). If `getByText("2 calls live, 1 on an earlier version")` fails, the two `<p>` lines must render exactly `2 calls live, 1 on an earlier version`; check the template literal, not the test.

- [ ] **Step 5: Commit**

```bash
git add ui/src/routes/agents/FlowsView.tsx ui/src/routes/agents/FlowsView.test.tsx ui/src/styles.css ui/src/test/noHardcodedColors.test.ts
git commit -m "feat(ui): Flows list with status, live calls, enable, test, copy and archive

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 5: Editor additions — start name, test panel on arrival, page title, published version, banner

**Files:**
- Create: `ui/src/routes/agents/PublishedBanner.tsx`, `ui/src/routes/agents/PublishedBanner.test.tsx`
- Modify: `ui/src/routes/agents/graph/FlowEditor.tsx`, `ui/src/routes/agents/graph/FlowSettingsPanel.tsx`, `ui/src/styles.css`, `ui/src/test/noHardcodedColors.test.ts` (add `"routes/agents/PublishedBanner.tsx"`)
- Test: `ui/src/routes/agents/graph/FlowEditor.test.tsx` (add cases), `ui/src/routes/agents/graph/FlowSettingsPanel.test.tsx` (add a case)

**Interfaces:**
- Produces:
  - `FlowEditorProps` gains `initialName?: string` (the flow's starting name; ignored when `agent` is given) and `initialPanel?: "call"` (opens the Test call panel on arrival when the flow can be called).
  - The editor title is an `h1` (id `flow-editor-title`), so the page has one `h1`.
  - `FlowSettingsPanelProps` gains `published?: { number: number; createdAt: string; hash: string }`, rendered as a read-only "Published version" section.
  - `PublishedBanner(props: { flowName: string; version: number; olderCalls: number; onDismiss: () => void })`: a `role="status"` banner whose text starts `Published v<version>.`

- [ ] **Step 1: Write the failing tests**

`ui/src/routes/agents/PublishedBanner.test.tsx`:

```tsx
import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import { PublishedBanner } from "./PublishedBanner";

const show = (olderCalls: number, onDismiss = vi.fn()) =>
  render(
    <MemoryRouter>
      <PublishedBanner flowName="Front desk" version={8} olderCalls={olderCalls} onDismiss={onDismiss} />
    </MemoryRouter>,
  );

describe("PublishedBanner", () => {
  it("says the version is live and that new calls get it", () => {
    show(0);
    const banner = screen.getByRole("status", { name: "Publish confirmation" });
    expect(banner).toHaveTextContent("Published v8. Front desk now answers new calls with it.");
    expect(banner).not.toHaveTextContent("finishing");
    expect(screen.getByRole("link", { name: "Watch calls" })).toHaveAttribute("href", "/sessions");
  });

  it("counts the calls still finishing on an earlier version", () => {
    show(1);
    expect(screen.getByRole("status", { name: "Publish confirmation" })).toHaveTextContent("1 call is finishing on an earlier version");
    show(3);
    expect(screen.getAllByRole("status", { name: "Publish confirmation" })[1]).toHaveTextContent("3 calls are finishing on an earlier version");
  });

  it("dismisses", async () => {
    const onDismiss = vi.fn();
    show(0, onDismiss);
    await userEvent.click(screen.getByRole("button", { name: "Dismiss" }));
    expect(onDismiss).toHaveBeenCalledOnce();
  });
});
```

Append to `ui/src/routes/agents/graph/FlowSettingsPanel.test.tsx` (inside the file, after the existing `describe`; reuse its `Harness`, but it does not pass `published`, so render the panel directly):

```tsx
describe("FlowSettingsPanel published version", () => {
  it("shows the published version and its config hash, read-only", () => {
    render(
      <div style={{ position: "relative" }}>
        <FlowSettingsPanel
          graph={two}
          providers={providers}
          name="Front desk"
          description=""
          onName={vi.fn()}
          onDescription={vi.fn()}
          onChange={vi.fn()}
          onClose={vi.fn()}
          published={{ number: 3, createdAt: "2026-10-05T12:00:00Z", hash: "abc123" }}
        />
      </div>,
    );
    const section = screen.getByRole("group", { name: "Published version" });
    expect(section).toHaveTextContent("v3");
    expect(section).toHaveTextContent("abc123");
  });
});
```

Append to `ui/src/routes/agents/graph/FlowEditor.test.tsx`:

```tsx
describe("FlowEditor start state", () => {
  it("starts a new flow with the name it was given, exactly", () => {
    setup({ initialName: "R&D line ñ" });
    expect(screen.getByRole("heading", { name: "R&D line ñ", level: 1 })).toBeVisible();
  });

  it("ignores the starting name for a stored flow", () => {
    setup({ agent, initialName: "Other" });
    expect(screen.getByRole("heading", { name: "Front desk", level: 1 })).toBeVisible();
  });

  it("opens the test call panel on arrival for a flow that can take calls", () => {
    setup({ agent, initialPanel: "call" });
    expect(screen.getByRole("complementary", { name: "Test call" })).toBeVisible();
  });

  it("does not open the test call panel for a flow that cannot take calls", () => {
    setup({ agent: { ...agent, readiness: { state: "not_ready", reasons: ["missing_configuration"] } }, initialPanel: "call" });
    expect(screen.queryByRole("complementary", { name: "Test call" })).toBeNull();
  });

  it("shows the published version in Flow settings", async () => {
    const { user } = setup({ agent });
    await user.click(screen.getByRole("button", { name: "Flow settings" }));
    expect(screen.getByRole("group", { name: "Published version" })).toHaveTextContent("v1");
  });
});
```

(The existing assertions `getByRole("heading", { name: "New flow" })` and `{ name: "Front desk", level: 2 }` in `FlowEditor.test.tsx` and `AgentsView`-era tests must change to level 1: run `grep -rn 'level: 2' ui/src e2e` and change only the headings that refer to the flow editor title; the e2e ones are handled in Task 8.)

- [ ] **Step 2: Run them to see them fail**

Run: `cd ui && npx vitest run src/routes/agents/PublishedBanner.test.tsx src/routes/agents/graph/FlowSettingsPanel.test.tsx src/routes/agents/graph/FlowEditor.test.tsx`
Expected: FAIL — `PublishedBanner` import missing; the new editor cases fail on `initialName` / level 1 / published group.

- [ ] **Step 3: Implement**

`ui/src/routes/agents/PublishedBanner.tsx`:

```tsx
import { Link } from "react-router";

export type PublishedBannerProps = { flowName: string; version: number; olderCalls: number; onDismiss: () => void };

/** Said once after a publish: what is live now, and what is still finishing on the old version. */
export function PublishedBanner({ flowName, version, olderCalls, onDismiss }: PublishedBannerProps) {
  return (
    <div role="status" aria-label="Publish confirmation" className="published-banner">
      <span className="lamp" data-tone="ok" aria-hidden="true" />
      <p>
        <b>Published v{version}.</b> {flowName.trim() || "This flow"} now answers new calls with it.
        {olderCalls > 0 && ` ${olderCalls} ${olderCalls === 1 ? "call is" : "calls are"} finishing on an earlier version and will end on their own.`}
      </p>
      <Link to="/sessions" className="action-button-small">
        Watch calls
      </Link>
      <button type="button" className="icon-button" aria-label="Dismiss" onClick={onDismiss}>
        ×
      </button>
    </div>
  );
}
```

The text content renders as `Published v8. Front desk now answers new calls with it.`, which the test expects exactly: keep the single space between `</b>` and the flow name.

Append to `ui/src/styles.css`:

```css
.published-banner {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 10px 14px;
  padding: 10px 16px;
  border: 1px solid var(--rule);
  border-radius: var(--radius);
  background: var(--surface);
}
.published-banner p {
  flex: 1 1 280px;
  margin: 0;
}
.flow-editor__head h1 {
  margin: 0;
  font-size: 24px;
  line-height: 1.15;
}
```

Remove the now-unused rule `.flow-editor__head h2 { margin: 0; }` from `styles.css`.

`FlowEditor.tsx` edits:

1. Props and a shared helper. Replace

```tsx
export type FlowEditorProps = {
  agent: AgentDTO | null;
```

with

```tsx
const canCall = (a: AgentDTO | null) => !!a && (a.readiness.state === "ready" || a.readiness.state === "ready_with_tool_warning");

export type FlowEditorProps = {
  /** The name a new flow starts with (from the New flow dialog). Ignored for a stored flow. */
  initialName?: string;
  /** Open the Test call panel on arrival, when the flow can take calls. */
  initialPanel?: "call";
  agent: AgentDTO | null;
```

2. The component signature: add `initialName, initialPanel` to the destructured props:
`export function FlowEditor({ agent, providers, client, keyValue, pending, onSubmit, onCancel, initialName, initialPanel }: FlowEditorProps) {`

3. State. Replace `useState(agent?.name ?? "")` with `useState(agent?.name ?? initialName ?? "")`, and `useState<Panel>(null)` with `useState<Panel>(initialPanel === "call" && canCall(agent) ? { kind: "call" } : null)`.

4. Replace `const callable = !!agent && (agent.readiness.state === "ready" || agent.readiness.state === "ready_with_tool_warning");` with `const callable = canCall(agent);`.

5. Title: replace `<h2 id="flow-editor-title" tabIndex={-1}>` … `</h2>` with `<h1 id="flow-editor-title" tabIndex={-1}>` … `</h1>`.

6. Pass the published version to Flow settings. In the `<FlowSettingsPanel` element add:

```tsx
            published={
              agent?.current_version
                ? { number: agent.current_version.number, createdAt: agent.current_version.created_at, hash: agent.current_version.config_hash }
                : undefined
            }
```

`FlowSettingsPanel.tsx` edits: add to `FlowSettingsPanelProps`

```ts
  /** The published version, shown read-only: what is live, and its content hash. */
  published?: { number: number; createdAt: string; hash: string };
```

destructure `published` in the component signature, and render, just before the closing `</SidePanel>`:

```tsx
      {published && (
        <fieldset className="stage-box">
          <legend>Published version</legend>
          <p>
            v{published.number}
            {published.createdAt && <span className="subtext"> · {new Date(published.createdAt).toLocaleString()}</span>}
          </p>
          <p className="subtext">
            Config hash <code>{published.hash}</code>
          </p>
        </fieldset>
      )}
```

Add `"routes/agents/PublishedBanner.tsx"` to `CONVERTED`.

- [ ] **Step 4: Run them to see them pass**

Run: `cd ui && npx vitest run src/routes/agents src/components src/test && npx tsc --noEmit`
Expected: PASS. (`AgentsView.test.tsx` still exists at this point and still passes: it never reads the editor title level except `{ name: "Two Node Bot", level: 2 }`; change that one assertion to `level: 1` in this step.)

- [ ] **Step 5: Commit**

```bash
git add ui/src/routes/agents ui/src/styles.css ui/src/test/noHardcodedColors.test.ts
git commit -m "feat(ui): editor takes a starting name, opens on the test panel, shows the published version

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 6: `FlowPage` and the `/flows` routes

**Files:**
- Create: `ui/src/routes/agents/FlowPage.tsx`, `ui/src/routes/agents/FlowPage.test.tsx`
- Modify: `ui/src/app/App.tsx`, `ui/src/app/App.test.tsx`, `ui/src/routes/sections.tsx`, `ui/src/routes/dashboard/DashboardView.tsx`, `ui/src/routes/onboarding/OnboardingView.tsx`, `ui/src/test/noHardcodedColors.test.ts` (add `"routes/agents/FlowPage.tsx"`)

**Interfaces:**
- Consumes: `FlowEditor` (+ `initialName`, `initialPanel`), `PublishedBanner`, `useActiveSessions`, `liveCalls`, `client.getAgents/createAgent/updateAgent/getProviders`.
- Produces: `FlowPage(props: { client: ApiClient; keyValue: string | null })`, mounted at `/flows/new` and `/flows/:flowId`. Query params it reads: `name` (starting name for `/flows/new`), `panel=test`. After a publish it navigates to `/flows/<id>` with `{ replace: true, state: { published: <version number> } }`; the banner reads `location.state.published`. Routes: `/flows`, `/flows/new`, `/flows/:flowId`; `/agents` → `/flows`; `/agents/:agentId` → `/flows/:agentId`.

- [ ] **Step 1: Write the failing tests**

`ui/src/routes/agents/FlowPage.test.tsx`:

```tsx
import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ReactFlowProvider } from "@xyflow/react";
import { MemoryRouter, Route, Routes } from "react-router";
import type { AgentDTO, ApiClient, ProviderStatus } from "../../api/client";
import { ApiError } from "../../api/client";
import { FlowPage } from "./FlowPage";

const svc = { available: true, custom: true, models: [{ id: "gemini", label: "Gemini", default: true }], voices: [{ id: "aoede", label: "Aoede", default: true }], languages: [{ id: "en", label: "English", default: true }] };
const providers: ProviderStatus[] = [{ id: "google", display_name: "Google", capabilities: ["llm", "stt", "tts"], detected: true, env: [], services: { stt: svc, llm: svc, tts: svc } }];
const flat = { stt: { provider: "google" }, llm: { provider: "google", model: "gemini" }, tts: { provider: "google", voice: "aoede" } };
const flow = (over: Partial<AgentDTO> = {}): AgentDTO => ({
  id: "a1",
  name: "Front desk",
  description: "",
  enabled: true,
  archived: false,
  current_version: { id: "v1", number: 1, schema_version: 1, config: flat, config_hash: "h1", created_at: "", mcp_tools: [] },
  readiness: { state: "ready", reasons: [] },
  created_at: "",
  updated_at: "",
  ...over,
});

function setup(client: Partial<ApiClient>, path: string, key: string | null = "k") {
  const full = {
    getAgents: vi.fn().mockResolvedValue([flow()]),
    getProviders: vi.fn().mockResolvedValue(providers),
    getSessions: vi.fn().mockResolvedValue([]),
    getSessionTraces: vi.fn(),
    issueVoiceToken: vi.fn(),
    ...client,
  } as unknown as ApiClient;
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <MemoryRouter initialEntries={[path]}>
        <ReactFlowProvider>
          <Routes>
            <Route path="/flows" element={<p>the list</p>} />
            <Route path="/flows/new" element={<FlowPage client={full} keyValue={key} />} />
            <Route path="/flows/:flowId" element={<FlowPage client={full} keyValue={key} />} />
          </Routes>
        </ReactFlowProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  );
  return { client: full, user: userEvent.setup() };
}

describe("FlowPage", () => {
  it("asks for a management key first", () => {
    setup({}, "/flows/a1", null);
    expect(screen.getByText(/Enter a management key/)).toBeVisible();
  });

  it("opens a stored flow in the editor under a breadcrumb back to the list", async () => {
    setup({}, "/flows/a1");
    expect(await screen.findByRole("heading", { name: "Front desk", level: 1 })).toBeVisible();
    expect(screen.getByRole("link", { name: "Flows" })).toHaveAttribute("href", "/flows");
  });

  it("opens a new flow under the name it was given, exactly", async () => {
    setup({}, "/flows/new?name=R%26D%20line%20%C3%B1");
    expect(await screen.findByRole("heading", { name: "R&D line ñ", level: 1 })).toBeVisible();
    expect(screen.getByRole("button", { name: "Add first step" })).toBeVisible();
  });

  it("says so, with a way back, for a flow that does not exist", async () => {
    setup({}, "/flows/nope");
    expect(await screen.findByText("That flow was not found.")).toBeVisible();
    expect(screen.getByRole("link", { name: "Back to flows" })).toHaveAttribute("href", "/flows");
    expect(screen.queryByRole("button", { name: "Publish" })).toBeNull();
  });

  it("treats an archived flow as not found", async () => {
    setup({ getAgents: vi.fn().mockResolvedValue([flow({ archived: true })]) }, "/flows/a1");
    expect(await screen.findByText("That flow was not found.")).toBeVisible();
  });

  it("opens on the test call panel for ?panel=test", async () => {
    setup({}, "/flows/a1?panel=test");
    expect(await screen.findByRole("complementary", { name: "Test call" })).toBeVisible();
  });

  it("publishes an edit, stays in the editor, and confirms the version with calls still on the old one", async () => {
    const updated = flow({ current_version: { id: "v2", number: 2, schema_version: 1, config: flat, config_hash: "h2", created_at: "", mcp_tools: [] } });
    const updateAgent = vi.fn().mockResolvedValue(updated);
    const { user } = setup(
      {
        updateAgent,
        getSessions: vi.fn().mockResolvedValue([
          { id: "s1", agent_id: "a1", agent_version_id: "v1", config_hash: "h1", started_at: "", status: "active", turn_count: 0, safe_summary: "" },
        ]),
      },
      "/flows/a1",
    );
    await screen.findByRole("heading", { name: "Front desk", level: 1 });
    // A rename is a publishable change on its own.
    await user.click(screen.getByRole("button", { name: "Flow settings" }));
    await user.type(screen.getByLabelText("Flow name"), " line");
    await user.click(screen.getByRole("button", { name: "Publish" }));
    await user.click(screen.getByRole("button", { name: "Publish v2" }));
    expect(updateAgent).toHaveBeenCalledWith("a1", expect.objectContaining({ name: "Front desk line" }));
    const banner = await screen.findByRole("status", { name: "Publish confirmation" });
    expect(banner).toHaveTextContent("Published v2.");
    await waitFor(() => expect(banner).toHaveTextContent("1 call is finishing on an earlier version"));
    expect(screen.getByRole("heading", { level: 1 })).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Dismiss" }));
    expect(screen.queryByText(/Published v2/)).toBeNull();
  });

  it("creates a new flow on first publish and moves to its own address without losing the editor", async () => {
    const created = flow({ id: "n1", name: "After hours" });
    const createAgent = vi.fn().mockResolvedValue(created);
    // The refetch after publishing never answers: the page must already know the new flow.
    const getAgents = vi.fn().mockResolvedValueOnce([]).mockImplementation(() => new Promise(() => {}));
    const { user } = setup({ createAgent, getAgents }, "/flows/new?name=After%20hours");
    await user.click(await screen.findByRole("button", { name: "Add first step" }));
    await user.selectOptions(screen.getByLabelText("LLM provider"), "google");
    await user.selectOptions(screen.getByLabelText("TTS provider"), "google");
    await user.click(screen.getByRole("button", { name: "Flow settings" }));
    await user.selectOptions(screen.getByLabelText("STT provider"), "google");
    await user.click(screen.getByRole("button", { name: "Publish" }));
    await user.click(screen.getByRole("button", { name: "Publish v1" }));
    await waitFor(() => expect(createAgent).toHaveBeenCalledWith(expect.objectContaining({ name: "After hours" })));
    expect(await screen.findByRole("status", { name: "Publish confirmation" })).toHaveTextContent("Published v1.");
    expect(screen.queryByText("That flow was not found.")).toBeNull();
    expect(screen.getByRole("heading", { name: "After hours", level: 1 })).toBeVisible();
  });

  it("shows a failed publish with a way to dismiss it", async () => {
    const { user } = setup({ updateAgent: vi.fn().mockRejectedValue(new ApiError("version conflict", "version_conflict", 409)) }, "/flows/a1");
    await screen.findByRole("heading", { name: "Front desk", level: 1 });
    await user.click(screen.getByRole("button", { name: "Flow settings" }));
    await user.type(screen.getByLabelText("Flow name"), "!");
    await user.click(screen.getByRole("button", { name: "Publish" }));
    await user.click(screen.getByRole("button", { name: "Publish v2" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("version conflict");
  });

  it("closing the editor goes back to the list", async () => {
    const { user } = setup({}, "/flows/a1");
    await user.click(await screen.findByRole("button", { name: "Close" }));
    expect(screen.getByText("the list")).toBeVisible();
  });
});
```

Add to `ui/src/app/App.test.tsx` (inside `describe("admin shell")`):

```tsx
  it("keeps old /agents bookmarks working", async () => {
    render(<App initialPath="/agents/abc" />);
    expect(await screen.findByRole("link", { name: "Flows" })).toHaveAttribute("aria-current", "page");
    expect(screen.queryByText("Area not found")).toBeNull();
  });
```

and change the existing `render(<App initialPath="/agents" />)` to `initialPath="/flows"`.

- [ ] **Step 2: Run them to see them fail**

Run: `cd ui && npx vitest run src/routes/agents/FlowPage.test.tsx src/app/App.test.tsx`
Expected: FAIL — `Failed to resolve import "./FlowPage"`; App test: no `/flows` route.

- [ ] **Step 3: Implement**

`ui/src/routes/agents/FlowPage.tsx`:

```tsx
import { useState } from "react";
import { Link, useLocation, useNavigate, useParams, useSearchParams } from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { asApiError, type AgentDTO, type ApiClient, type ApiError, type CreateAgentInput } from "../../api/client";
import { EmptyState, ErrorState, LoadingState } from "../../components/primitives";
import { FlowEditor } from "./graph/FlowEditor";
import { PublishedBanner } from "./PublishedBanner";
import { liveCalls } from "./flowStats";
import { useActiveSessions } from "./useActiveSessions";

export type FlowPageProps = { client: ApiClient; keyValue: string | null };

/** /flows/new and /flows/:flowId: the flow editor as a page of its own. */
export function FlowPage({ client, keyValue }: FlowPageProps) {
  const { flowId } = useParams();
  const [search] = useSearchParams();
  const location = useLocation();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [formError, setFormError] = useState<ApiError | null>(null);
  const [dismissedAt, setDismissedAt] = useState<string | null>(null);

  const flowsQuery = useQuery({ queryKey: ["agents"], queryFn: () => client.getAgents(), enabled: !!keyValue, retry: false });
  const providersQuery = useQuery({ queryKey: ["providers"], queryFn: () => client.getProviders(), enabled: !!keyValue, retry: false });
  const active = useActiveSessions(client, keyValue);

  const flow: AgentDTO | undefined = flowId ? (flowsQuery.data ?? []).find((a) => a.id === flowId && !a.archived) : undefined;

  const published = (saved: AgentDTO) => {
    setFormError(null);
    // Put the saved flow in the cache before moving to its address: a refetch is still in flight, and
    // without this the new address would flash "That flow was not found." and remount the editor.
    queryClient.setQueryData<AgentDTO[]>(["agents"], (old) => [...(old ?? []).filter((a) => a.id !== saved.id), saved]);
    void queryClient.invalidateQueries({ queryKey: ["agents"] });
    // Same address for an update: the state change is what raises the banner.
    navigate(`/flows/${encodeURIComponent(saved.id)}`, { replace: true, state: { published: saved.current_version?.number ?? 1 } });
  };
  const createMutation = useMutation({
    mutationFn: (payload: CreateAgentInput) => client.createAgent(payload),
    onSuccess: published,
    onError: (err) => setFormError(asApiError(err, "Publishing failed")),
  });
  const updateMutation = useMutation({
    mutationFn: ({ id, payload }: { id: string; payload: CreateAgentInput }) => client.updateAgent(id, payload),
    onSuccess: published,
    onError: (err) => setFormError(asApiError(err, "Publishing failed")),
  });

  if (!keyValue) return <EmptyState title="Connect to edit flows">Enter a management key above to configure voice flows.</EmptyState>;
  if (flowsQuery.isPending) return <LoadingState label="Loading flow" />;
  if (flowsQuery.isError) return <ErrorState title="Failed to load the flow" error={flowsQuery.error as ApiError} onRetry={() => void flowsQuery.refetch()} />;
  if (flowId && !flow) {
    return (
      <section className="flow-missing">
        <p role="status">That flow was not found.</p>
        <Link to="/flows" className="action-button-small">
          Back to flows
        </Link>
      </section>
    );
  }

  const state = location.state as { published?: number } | null;
  const publishedVersion = typeof state?.published === "number" ? state.published : null;
  const providers = providersQuery.data ?? [];

  return (
    <div className="flow-page">
      <nav aria-label="Breadcrumb" className="flow-page__crumb">
        <Link to="/flows">Flows</Link>
      </nav>
      {flow && publishedVersion !== null && dismissedAt !== location.key && (
        <PublishedBanner
          flowName={flow.name}
          version={publishedVersion}
          olderCalls={liveCalls(active.data ?? [], flow).onOlderVersion}
          onDismiss={() => setDismissedAt(location.key)}
        />
      )}
      {formError && <ErrorState title="Publishing failed" error={formError} onRetry={() => setFormError(null)} />}
      {providersQuery.isError && (
        <ErrorState title="Failed to load providers" error={providersQuery.error as ApiError} onRetry={() => void providersQuery.refetch()} />
      )}
      <FlowEditor
        key={flow?.id ?? "new"}
        agent={flow ?? null}
        initialName={search.get("name") ?? undefined}
        initialPanel={search.get("panel") === "test" ? "call" : undefined}
        providers={providers}
        client={client}
        keyValue={keyValue}
        pending={createMutation.isPending || updateMutation.isPending}
        onSubmit={(input) => (flow ? updateMutation.mutate({ id: flow.id, payload: input }) : createMutation.mutate(input))}
        onCancel={() => navigate("/flows")}
      />
    </div>
  );
}
```

Routes in `ui/src/app/App.tsx`:

1. Imports: replace `import { AgentsView } from "../routes/agents/AgentsView";` with

```tsx
import { FlowsView } from "../routes/agents/FlowsView";
import { FlowPage } from "../routes/agents/FlowPage";
```

and add `useParams` to the `react-router` import list.

2. Above `export function App`, add:

```tsx
/** An old /agents/:id bookmark opens the same flow. */
function LegacyAgentRedirect() {
  const { agentId = "" } = useParams();
  return <Navigate to={`/flows/${encodeURIComponent(agentId)}`} replace />;
}
```

3. Replace the two `<Route path="/agents" …>` and `<Route path="/agents/:agentId" …>` elements with:

```tsx
                <Route path="/flows" element={<FlowsView client={client} keyValue={keyValue} />} />
                <Route path="/flows/new" element={<FlowPage client={client} keyValue={keyValue} />} />
                <Route path="/flows/:flowId" element={<FlowPage client={client} keyValue={keyValue} />} />
                <Route path="/agents" element={<Navigate to="/flows" replace />} />
                <Route path="/agents/:agentId" element={<LegacyAgentRedirect />} />
```

`ui/src/routes/sections.tsx`: `{ path: "/flows", name: "Flows", icon: "Fl" },`

`ui/src/routes/dashboard/DashboardView.tsx`: change `<Link to="/agents" className="ledger__link">` to `<Link to="/flows" className="ledger__link">` and its text `View agents` to `View flows`; change the ledger name `Agents` to `Flows` and `No agents created yet.` to `No flows created yet.` (update `DashboardView.test.tsx` expectations that name these strings).

`ui/src/routes/onboarding/OnboardingView.tsx`: change `<Link to="/agents"` to `<Link to="/flows"`; keep its visible text unless it says "agents" in a link (then say "flows") and update `OnboardingView.test.tsx` expectations accordingly.

Add `"routes/agents/FlowPage.tsx"` to `CONVERTED`.

- [ ] **Step 4: Run the suite**

Run: `cd ui && npx vitest run && npx tsc --noEmit`
Expected: PASS. `AgentsView.test.tsx` still passes (the file still exists, unrouted). Dashboard and Onboarding tests pass after their expectation updates.

- [ ] **Step 5: Commit**

```bash
git add ui/src
git commit -m "feat(ui): /flows routes, the editor as a page, and a published banner

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 7: Retire the old agent pages

**Files:**
- Delete: `ui/src/routes/agents/AgentsView.tsx`, `ui/src/routes/agents/AgentsView.test.tsx`, `ui/src/routes/agents/graph/LiveGraph.tsx`, `ui/src/routes/agents/graph/LiveGraph.test.tsx`, `ui/src/routes/agents/graph/LiveGraph.cable.test.tsx`
- Modify: `ui/src/routes/agents/AgentVoiceTest.tsx`, `ui/src/routes/agents/AgentVoiceTest.graph.test.tsx`, `ui/src/routes/agents/graph/FlowEditor.tsx` (stop passing `hideGraph`), `ui/src/test/noHardcodedColors.test.ts`

**Interfaces:**
- Consumes: nothing new.
- Produces: `AgentVoiceTest` loses the `hideGraph` prop and the embedded `LiveGraph`; everything else it did (`onCall`, readiness, transcript, session summary) stays.

- [ ] **Step 1: Write the failing check** — the compiler is the test. First delete the importers' targets so the build names every leftover:

```bash
git rm ui/src/routes/agents/AgentsView.tsx ui/src/routes/agents/AgentsView.test.tsx \
  ui/src/routes/agents/graph/LiveGraph.tsx ui/src/routes/agents/graph/LiveGraph.test.tsx ui/src/routes/agents/graph/LiveGraph.cable.test.tsx
```

- [ ] **Step 2: Run it to see it fail**

Run: `cd ui && npx tsc --noEmit`
Expected: FAIL — `AgentVoiceTest.tsx` cannot find `./graph/LiveGraph`; possibly the colour guard lists missing files.

- [ ] **Step 3: Implement**

In `ui/src/routes/agents/AgentVoiceTest.tsx`:
- remove `import { LiveGraph } from "./graph/LiveGraph";`
- remove the `hideGraph` prop (the line `hideGraph?: boolean;` with its comment in `AgentVoiceTestProps`, and `hideGraph,` in the destructuring);
- remove the `const graph = useMemo(…)` block with its comment, and the whole `{graph && !hideGraph && ( <div className="stage__graph"> <LiveGraph … /> </div> )}` element;
- then delete what the compiler now reports unused: the `useMemo` import, the `fromAgentConfig` import, and `shown` if only `graph` used it. Keep `callVersion`, `callIsStale` and the "This call is on vN" note: they still protect a call from a publish mid-call.

In `ui/src/routes/agents/graph/FlowEditor.tsx` change `<AgentVoiceTest agent={agent} client={client} keyValue={keyValue} hideGraph onCall={setCall} />` to `<AgentVoiceTest agent={agent} client={client} keyValue={keyValue} onCall={setCall} />`.

In `ui/src/routes/agents/AgentVoiceTest.graph.test.tsx` delete the three cases that drove the embedded graph: `leaves the graph to the page when asked`, `shows the path through the graph for a multi-node agent`, `shows no graph for a plain agent`. Keep `reports the idle call to the page, and NO_CALL when it goes away`, and the shared helpers it uses (`agent`, `node`, `stage`, `graphAgent`, `renderTest`). Rename the `describe` to `"AgentVoiceTest call reporting"`.

In `ui/src/test/noHardcodedColors.test.ts` remove `"routes/agents/AgentsView.tsx"` and `"routes/agents/graph/LiveGraph.tsx"` from `CONVERTED`.

Then: `cd ui && grep -rn "stage__graph\|live-graph\|agents-view\|detail-head\|\.fold\b" src/styles.css` and delete CSS rules for selectors that no TSX uses any more (`live-graph*`, `stage__graph`, `agents-view`, `detail-head*`, `meta-grid`/`meta-card` if unused). `cable.css.test.ts` guards the cable CSS that `Canvas` still uses: leave those rules.

- [ ] **Step 4: Run the suite**

Run: `cd ui && npx tsc --noEmit && npx vitest run`
Expected: PASS, no unused-symbol errors.

- [ ] **Step 5: Commit**

```bash
git add -A ui/src
git commit -m "refactor(ui): retire the agent list, create, edit and detail screens and the embedded live graph

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 8: End-to-end suites follow the new pages

**Files:**
- Modify: `e2e/tests-graph/10-agent-graph.spec.ts`, `e2e/tests/10-full-stack.spec.ts`

**Interfaces:**
- Consumes: the page names and labels from Tasks 4 to 6.
- Produces: `openFlow(page, name)` and `openFlowTest(page, name)` helpers in the graph suite; the paid full-stack suite follows the same pages.

- [ ] **Step 1: Update the graph suite helpers** in `e2e/tests-graph/10-agent-graph.spec.ts`. Replace `openAgentsList` and `openAgentDetail` with:

```ts
async function openFlowsList(page: Page) {
  await page.getByRole("link", { name: "Flows", exact: true }).first().click();
  await expect(page.getByRole("heading", { name: "Flows", level: 1 })).toBeVisible();
}

// Opens a flow in the editor from the list (or leaves it open if it already is).
async function openFlow(page: Page, name: string) {
  const heading = page.getByRole("heading", { name, level: 1 });
  if (await heading.isVisible()) return;
  await openFlowsList(page);
  await page.getByRole("link", { name, exact: true }).click();
  await expect(heading).toBeVisible();
}

// The editor's Test call panel, where the voice test lives.
async function openFlowTest(page: Page, name: string) {
  await openFlow(page, name);
  await page.getByRole("button", { name: "Test call" }).click();
  await expect(page.getByTestId("agent-readiness")).toHaveText(/^ready/);
}
```

Update every call site:
- `openDesigner`: `await openAgentsList(page);` → `await openFlowsList(page);`; the heading check after `New flow` becomes the dialog: after `getByRole("button", { name: "New flow" }).click()` add
  `await page.getByLabel("Name").fill("Main flow"); await page.getByRole("button", { name: "Create and open editor" }).click(); await expect(page.getByRole("heading", { name: "Main flow", level: 1 })).toBeVisible();` and replace the old `expect(...heading "New flow"...)` line. Keep `Add first step` and the `Step name` fill.
- The "designer: draw…" test: replace `await openAgentDetail(page, AGENT); await expect(page.getByTestId("agent-readiness")).toHaveText(/^ready/); await expect(pathTaken(page)).toContainText("Main agent");` with `await openFlowTest(page, AGENT);` and the line `await step(page, "graph agent published and ready");` stays. (After Publish v1 the editor is already open on the new flow; `openFlow` finds the heading and does not navigate.)
- The "call:" test: `await openAgentDetail(page, AGENT); await startCall(page);` → `await openFlowTest(page, AGENT); await startCall(page);`. Replace `liveStatus(page)` assertions with the editor's own graph: `await expect(stepCard(page, "Main agent").locator(".agent-node")).toHaveAttribute("data-state", "active");` and `await expect(stepCard(page, "Sales").locator(".agent-node")).toHaveAttribute("data-state", "active", { timeout: LIVE ? 90_000 : 15_000 });`; replace the two `pathTaken` assertions with `await expect(stepCard(page, "Main agent").locator(".agent-node")).toHaveAttribute("data-state", "visited");`. Delete the now-unused `liveStatus` and `pathTaken` helpers.
- The "reopen the graph…" test: `await openAgentsList(page); await page.getByRole("row").filter({ hasText: AGENT }).getByRole("button", { name: "Edit" }).click();` → `await openFlow(page, AGENT);`; `getByRole("heading", { name: AGENT, level: 2 })` → `level: 1` (three places); `getByText(/Published v2/)` stays (it is the banner now).
- The "failure:" test: `await openAgentDetail(page, "E2E Broken Sales");` → `await openFlowTest(page, "E2E Broken Sales");`; replace `await expect(liveStatus(page)).toContainText("Front is answering");` with `await expect(stepCard(page, "Front").locator(".agent-node")).toHaveAttribute("data-state", "active");`.
- The "a plain agent still works unchanged" test: `openAgentDetail(page, …)` → `openFlowTest(page, …)`.
- The 390px layout test: `await go("/agents");` → `await go("/flows");` (heading stays `Flows`, level 1); `await openAgentDetail(page, AGENT); await noSideways("agent detail");` → `await openFlow(page, AGENT); await noSideways("flow editor");`.

Add a list test after the layout test's helpers, before the layout test:

```ts
test("flows list: status, enable toggle without a new version, copy and archive", async ({ page, stack }) => {
  await connectAdmin(page, stack);
  await openFlowsList(page);
  const row = page.getByRole("link", { name: AGENT, exact: true }).locator("xpath=ancestor::li[1]");
  await expect(row).toContainText("Ready");
  await expect(row).toContainText("2 steps · 1 route");

  const before = (await api<{ data: { name: string; current_version: { number: number } }[] }>(stack, "/agents")).data.find((a) => a.name === AGENT)!.current_version.number;
  await row.getByRole("checkbox", { name: `${AGENT} enabled` }).uncheck();
  await expect(row).toContainText("Disabled");
  await row.getByRole("checkbox", { name: `${AGENT} enabled` }).check();
  await expect(row).toContainText("Ready");
  const after = (await api<{ data: { name: string; current_version: { number: number } }[] }>(stack, "/agents")).data.find((a) => a.name === AGENT)!.current_version.number;
  expect(after, "toggling Enabled must not publish a version").toBe(before);

  await row.getByRole("button", { name: `Duplicate ${AGENT}` }).click();
  // The server names a copy "<name> copy" (server/agent/service.go, Duplicate).
  const copyName = `${AGENT} copy`;
  const copy = page.getByRole("link", { name: copyName, exact: true });
  await expect(copy).toBeVisible();
  await copy.locator("xpath=ancestor::li[1]").getByRole("button", { name: `Archive ${copyName}` }).click();
  await page.getByRole("button", { name: "Archive flow" }).click();
  await expect(page.getByRole("link", { name: copyName, exact: true })).toHaveCount(0);
});
```

- [ ] **Step 2: Run the fake-provider suite**

Run: `make e2e-fake`
Expected: all graph specs pass, including the new list test. `ONNX` runtime libraries must exist in the checkout (`make ort`, or symlink the git-ignored `third_party/onnxruntime-*` files from another checkout) or the server will not start.

- [ ] **Step 3: Update the paid full-stack suite** in `e2e/tests/10-full-stack.spec.ts` (it runs against real Google with `GOVOICE_LIVE=1 make e2e` and costs money: do not run it as part of this task; it is edited to match, and the first paid run is the person's decision).

- Replace `openAgentDetail` with:

```ts
async function openFlowTest(page: Page) {
  const heading = page.getByRole("heading", { name: AGENT, level: 1 });
  if (!(await heading.isVisible())) {
    await page.getByRole("link", { name: "Flows", exact: true }).first().click();
    await page.getByRole("link", { name: AGENT, exact: true }).click();
    await expect(heading).toBeVisible();
  }
  if (!(await page.getByTestId("agent-readiness").isVisible())) await page.getByRole("button", { name: "Test call" }).click();
}
```

and replace its three call sites (`openAgentDetail(page)`) with `openFlowTest(page)`.
- Replace the creation steps (from `await page.getByRole("link", { name: "Agents", exact: true }).first().click();` through `await page.getByRole("button", { name: "Save agent" }).click();`) with:

```ts
  await page.getByRole("link", { name: "Flows", exact: true }).first().click();
  await page.getByRole("button", { name: "New flow" }).click();
  await page.getByLabel("Name").fill(AGENT);
  await page.getByRole("button", { name: "Create and open editor" }).click();
  await page.getByRole("button", { name: "Add first step" }).click();
  await page.getByLabel("Step name").fill("Assistant");
  await page.getByLabel("Instructions").fill("You are a concise voice assistant. Answer in one short sentence.");
  await page.getByLabel("LLM provider").selectOption("google");
  await page.getByRole("combobox", { name: "LLM model", exact: true }).selectOption("gemini-2.5-flash");
  await page.getByLabel("TTS provider").selectOption("google");
  await page.getByRole("combobox", { name: "Voice", exact: true }).selectOption("en-US-Chirp3-HD-Aoede");
  await page.getByRole("button", { name: "Flow settings" }).click();
  await page.getByLabel("STT provider").selectOption("google");
  await page.getByRole("combobox", { name: "STT model", exact: true }).selectOption("chirp_2");
  await page.getByLabel("STT language").selectOption("en-US");
  await step(page, "flow filled");
  await page.getByRole("button", { name: "Publish", exact: true }).click();
  await page.getByRole("button", { name: "Publish v1" }).click();
  await expect(page.getByText(/Published v1/)).toBeVisible();
```

- The stored-config assertion reads a graph now (a named step is published as a graph): replace the `cfg.stt…` lines with

```ts
  const node = (saved.data.current_version.config as unknown as { nodes: { config: Record<string, Record<string, unknown>> }[] }).nodes[0].config;
  expect([node.stt.provider, node.stt.model, node.llm.model, node.tts.voice]).toEqual(["google", "chirp_2", "gemini-2.5-flash", "en-US-Chirp3-HD-Aoede"]);
  expect(JSON.stringify(node)).not.toContain("credential");
```

  and in the `api<…>` generic on the line above, widen the type to `{ data: { current_version: { config: Record<string, unknown> } } }`.
- The delete test: `await page.getByRole("button", { name: "Delete agent" }).click(); … await page.getByRole("button", { name: "Confirm delete" }).click(); await expect(page.getByRole("row").filter({ hasText: AGENT })).toHaveCount(0);` becomes

```ts
  await page.getByRole("link", { name: "Flows", exact: true }).first().click();
  await page.getByRole("button", { name: `Archive ${AGENT}` }).click();
  await step(page, "archive confirmation");
  await page.getByRole("button", { name: "Archive flow" }).click();
  await expect(page.getByRole("link", { name: AGENT, exact: true })).toHaveCount(0);
```

  and the first line of that test (`await openAgentDetail(page);`) is deleted.
- Test names: `"admin UI: delete the agent"` → `"admin UI: archive the flow"`.

- [ ] **Step 4: Type-check the e2e project**

Run: `cd e2e && npx tsc --noEmit -p .`
Expected: no errors (if the e2e package has no tsconfig, run `npx playwright test --list -c playwright.config.ts` and expect both configs to list their specs without a compile error).

- [ ] **Step 5: Commit**

```bash
git add e2e/tests-graph/10-agent-graph.spec.ts e2e/tests/10-full-stack.spec.ts
git commit -m "test(e2e): suites follow the Flows list and the editor page

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 9: Gate, spec update and independent review

**Files:**
- Modify: `docs/superpowers/specs/2026-10-12-switchboard-console-design.md`

**Interfaces:**
- Consumes: everything above.

- [ ] **Step 1: Record what plan 2 left out, in the spec.** In Section 5's table add:

```
| S7 | Per-flow call statistics (calls today, median reply time) | Those two figures on the Flows list rows |
| S8 | Unarchive a flow (`POST /agents/{id}/restore`) | The canvas's "Archived · 1 / Restore" section |
```

In Section 3's table, replace the rows for boards 3–5 and 12 with:

```
| 3–5, 12 | Flows list, empty state, New flow dialog, published banner | `/flows`, `/flows/new`, `/flows/:id` | **Done (plan 2).** Rows show status, version, steps/routes/tools and live calls; Enabled, Test, Duplicate, Archive. New flow asks for a name, then Blank or Copy (the canvas's voice-stack choice is left to First run). The banner appears in the editor after a publish. Draft badges, rollback, calls-today and Restore wait on S1, S2, S7, S8. |
```

and in Section 6 change plan item 2 to `2. **Done:** Flows list, New flow dialog, `/flows` routes, retire the agent screens, published banner.` Remove the bound-MCP-tools table and config JSON dump from the retired detail screen from the list of things the UI shows: add one line to Section 4: `- The old agent detail screen's raw config JSON and MCP tools table are gone; the version and hash are in Flow settings and the tools are chosen per step.`

- [ ] **Step 2: Full gate**

Run:

```bash
cd ui && npx vitest run && npx tsc --noEmit
cd .. && make e2e-fake
git status --short
```

Expected: all green; the tree is clean apart from the spec file.

- [ ] **Step 3: Look at it in a browser.** Run `make dev`, open the printed Admin UI address, connect with the printed management key, and check by eye: the Flows list (a ready flow, a disabled one, live-call figures if a call is open), New flow → Blank (the editor opens with the typed name), New flow → Copy, Archive, `/agents` redirecting, and a publish showing the banner. Note anything that looks wrong in the commit message of a fix; stop the stack with Ctrl-C.

- [ ] **Step 4: Independent review (AGENTS.md "Mandatory Code Review Workflow").** Dispatch a fresh reviewer on `main..HEAD` with this brief: YAGNI and bloat (is any new file or prop unused? is anything the old AgentsView did silently lost that a user needs?); correctness (query invalidation, `location.state` banner logic, `replace` navigation, stale `["agents"]` data after archive in another tab); the five Review Focus items; accessibility of the list rows, dialogs and breadcrumb; token-only colours; tests that pass for the wrong reason. Fix confirmed findings, each with a test that fails first.

- [ ] **Step 5: Commit**

```bash
git add docs/superpowers/specs/2026-10-12-switchboard-console-design.md
git commit -m "docs: record what the Flows list leaves for the server (S7, S8) and mark plan 2 done

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```
