# Switchboard UI Polish Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give the admin UI one visual identity ("Switchboard") and rebuild the demo path around a call stage where a live call moves across the agent graph.

**Architecture:** CSS custom properties are the single source of colour; every hard-coded hex in the demo-path TSX moves to token classes. Two guard tests (contrast of token pairs, no hard-coded colours in listed files) are written first and turn green task by task. Component props, `data-testid`s, accessible names and labels do not change, so the existing vitest and Playwright suites are the safety net.

**Tech Stack:** React 19, Vite, vitest, Playwright (`e2e/`), `@xyflow/react` (graph canvas), `@fontsource-variable/schibsted-grotesk`.

**Spec:** `docs/superpowers/specs/2026-10-12-ui-switchboard-design.md`

## Global Constraints

- No behaviour, API or server change; no new runtime dependency except `@fontsource-variable/schibsted-grotesk` (5.3.0) replacing `@fontsource/ibm-plex-sans`. `@fontsource/ibm-plex-mono` stays, used only for ids, hashes and code.
- Keep every `data-testid`, accessible name, label text and role the tests use (`start-voice-test-btn`, `connection-state`, `mute-btn`, `ptt-btn`, `disconnect-btn`, `send-text-btn`, `transcript-user`, `transcript-bot`, `bot-streaming-text`, `agent-readiness`, `session-summary`, `toggle-debug-btn`, labels `Management key`, `Agent name`, `STT provider`…). Run `grep -rn "getByTestId\|getByLabel\|getByRole" ui/src e2e` before renaming anything; the answer is "do not rename".
- Tokens (exact values): `--paper #E8EDEF`, `--surface #F5F8F9`, `--ink #12222B`, `--ink-2 #4A5B64`, `--rule #C5CFD4`, `--caller #2F3FE0`, `--lamp #F0A400`, `--ok #1F8A5B`, `--bad #C2412D`. Lamp amber is used only for the active agent and the handoff moment; ok/bad only for status.
- Type: Schibsted Grotesk weights 450 / 600 / 800; scale 13 / 15 / 18 / 24 / 40px; body line-height 1.55, headings 1.15; `font-variant-numeric: tabular-nums` on numbers; no monospace labels, no all-caps eyebrows. One radius, 4px, on controls only; no card boxes, no drop shadows.
- Motion: one orchestrated moment (cable draw + lamp light at a handoff, 400ms); `prefers-reduced-motion: reduce` makes it instant. No entrance animations.
- Quality floor: 3px `--caller` focus ring; WCAG AA contrast for text (4.5:1) and UI boundaries (3:1); no horizontal page scroll at 1440, 1024 and 390px.
- Copy: sentence case; one verb per action across the flow ("Publish" -> "Published v4"); empty and error states say what to do next.
- Commit messages are conventional (`feat(ui): ...`) and end with `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`.
- Gate for every task: `cd ui && npx vitest run && npx tsc --noEmit`.

## Review Focus

1. **Test hooks survive** (most likely): a restyle that renames or removes a `data-testid`, label or role breaks the Playwright graph and full-stack suites. Pin: the e2e fake graph suite passes unchanged except where this plan names a test.
2. **Rail collapsed state keeps accessible names:** an icon-only link with no text is unreachable by name. Pin: every nav link keeps its text (visually hidden below 1024px) and `aria-label`-equivalent via text; vitest asserts `getByRole("link", { name: "Agents" })`.
3. **Management key flow:** the key field/Connect/Clear key must stay reachable on every page and never persisted or echoed. Pin: existing `App.test.tsx` cases pass; no `localStorage`/URL use added.
4. **Reduced motion:** the cable/lamp transition must not run under `prefers-reduced-motion: reduce`. Pin: a CSS test asserts the media query disables the animation/transition on the graph classes.
5. **390px layout:** the call stage, tables and designer must not scroll the page horizontally. Pin: Playwright assertion `document.documentElement.scrollWidth <= innerWidth` on each demo-path screen at 390px.
6. **Contrast:** every text/background token pair used meets AA; amber text on slate does not (amber is a fill/border, never text on `--paper`). Pin: `tokens.contrast.test.ts` (Task 1).
7. **No leftover hard-coded colours** on the demo path (they would ignore the system and break on a future token change). Pin: `noHardcodedColors.test.ts` (Task 1), whose file list shrinks to empty by Task 9.

---

### Task 1: Tokens, font, and the two guard tests

**Files:**
- Modify: `ui/package.json`, `ui/package-lock.json`, `ui/src/main.tsx`, `ui/src/styles.css` (top of file and legacy-colour mapping)
- Create: `ui/src/test/tokens.contrast.test.ts`, `ui/src/test/noHardcodedColors.test.ts`

**Interfaces:**
- Produces: CSS custom properties above on `:root`; the guard tests other tasks turn green; a `LEGACY` colour map used in Task 9 (documented in the css as comments, not code).

- [ ] **Step 1: Write the failing guard tests.**

`ui/src/test/tokens.contrast.test.ts`:

```ts
import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

const css = readFileSync(resolve(__dirname, "../styles.css"), "utf8");
const token = (name: string) => {
  const m = css.match(new RegExp(`--${name}:\\s*(#[0-9a-fA-F]{6})`));
  if (!m) throw new Error(`token --${name} not defined in styles.css`);
  return m[1];
};
const lum = (hex: string) => {
  const c = [1, 3, 5].map((i) => parseInt(hex.slice(i, i + 2), 16) / 255).map((v) => (v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4));
  return 0.2126 * c[0] + 0.7152 * c[1] + 0.0722 * c[2];
};
const ratio = (a: string, b: string) => {
  const [x, y] = [lum(a), lum(b)].sort((p, q) => q - p);
  return (x + 0.05) / (y + 0.05);
};

describe("Switchboard tokens", () => {
  const pairs: [string, string, number][] = [
    ["ink", "paper", 4.5],
    ["ink", "surface", 4.5],
    ["ink-2", "paper", 4.5],
    ["ink-2", "surface", 4.5],
    ["caller", "paper", 4.5],
    ["caller", "surface", 4.5],
    ["ok", "paper", 4.5],
    ["bad", "paper", 4.5],
    ["ink", "lamp", 4.5], // text on an amber fill
    ["rule", "paper", 1.2], // hairline: decorative, must still be visible
  ];
  for (const [fg, bg, min] of pairs) {
    it(`--${fg} on --${bg} is at least ${min}:1`, () => {
      expect(ratio(token(fg), token(bg))).toBeGreaterThanOrEqual(min);
    });
  }
  it("amber is never used as text on the paper (3:1 boundary only)", () => {
    expect(ratio(token("lamp"), token("paper"))).toBeLessThan(4.5); // documents why: fill/border only
    expect(ratio(token("lamp"), token("ink"))).toBeGreaterThanOrEqual(4.5);
  });
});
```

`ui/src/test/noHardcodedColors.test.ts`:

```ts
import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

// Files already moved to tokens. Each later task appends the files it converted.
export const CONVERTED: string[] = [];

const hex = /#[0-9a-fA-F]{3,8}\b|rgba?\(/;

describe("no hard-coded colours outside the token block", () => {
  for (const f of CONVERTED) {
    it(f, () => {
      const lines = readFileSync(resolve(__dirname, "..", f), "utf8").split("\n");
      const bad = lines.map((l, i) => [i + 1, l] as const).filter(([, l]) => hex.test(l) && !l.includes("--"));
      expect(bad).toEqual([]);
    });
  }
  it("styles.css defines colours only in the :root token block and the legacy block", () => {
    const css = readFileSync(resolve(__dirname, "../styles.css"), "utf8");
    const after = css.split("/* end tokens */")[1] ?? css;
    const leaks = after.split("\n").filter((l) => hex.test(l) && !l.trim().startsWith("/*") && !l.includes("var("));
    expect(leaks.length).toBe(0);
  });
});
```

- [ ] **Step 2: Run, expect FAIL** (`token --ink not defined`, and the css leak test fails): `cd ui && npx vitest run src/test`.
- [ ] **Step 3: Install the font and define the tokens.** `cd ui && npm i @fontsource-variable/schibsted-grotesk@5.3.0 && npm rm @fontsource/ibm-plex-sans`. In `main.tsx` replace the three `ibm-plex-sans` imports with `import "@fontsource-variable/schibsted-grotesk/wght.css";` (keep the mono import). Replace the top of `styles.css` (`:root`, `body`, focus rules) with:

```css
:root {
  --paper: #e8edef;
  --surface: #f5f8f9;
  --ink: #12222b;
  --ink-2: #4a5b64;
  --rule: #c5cfd4;
  --caller: #2f3fe0;
  --lamp: #f0a400;
  --ok: #1f8a5b;
  --bad: #c2412d;
  --radius: 4px;
  --font: "Schibsted Grotesk Variable", system-ui, sans-serif;
  font-family: var(--font);
  font-size: 15px;
  line-height: 1.55;
  color: var(--ink);
  background: var(--paper);
  font-synthesis: none;
  font-variant-numeric: tabular-nums;
}
/* end tokens */
a:focus-visible, button:focus-visible, input:focus-visible, select:focus-visible, textarea:focus-visible {
  outline: 3px solid var(--caller);
  outline-offset: 3px;
}
```

Then replace every remaining hex/rgba in `styles.css` with the nearest token (`#15262a`/`#1b2a2f` -> `var(--ink)`, `#53666b` -> `var(--ink-2)`, `#c9d3d2` -> `var(--rule)`, `#f3f5f4`/`#e7edeb` -> `var(--paper)`, `#fff`/`#f7fafa` -> `var(--surface)`, `#007e82` -> `var(--caller)`, `#b7791f` -> `var(--lamp)`, greens -> `var(--ok)`, reds -> `var(--bad)`); weights 400/500/600 become 450/600/800 where used for display. Remove `font-family: "IBM Plex Sans"` rules; keep IBM Plex Mono only on `.masked-value`, `.request-id`, `code`.
- [ ] **Step 4: Run the guard tests, expect PASS;** `npx vitest run && npx tsc --noEmit && npm run build`.
- [ ] **Step 5: Commit** `feat(ui): Switchboard tokens, Schibsted Grotesk, contrast and colour guard tests`.

### Task 2: App shell, rail and the management key

**Files:**
- Modify: `ui/src/app/App.tsx`, `ui/src/styles.css` (`.app-layout`, `.sidebar`, `.brand`, `.navigation`, `.nav-link`, `.topbar`, `.access-panel`, `.capability-line`), `ui/src/routes/sections.tsx` (add an `icon` glyph per section if needed), `ui/src/app/App.test.tsx`
- Modify: `ui/src/test/noHardcodedColors.test.ts` (append `app/App.tsx`, `routes/sections.tsx` to `CONVERTED`)

**Interfaces:**
- Consumes: Task 1 tokens.
- Produces: shell classes `.rail`, `.rail__link`, `.topline`, `.key-bar` used by later tasks' layout assumptions (`main` has max width 1100px, left aligned).

- [ ] **Step 1: Write failing tests** in `App.test.tsx`: (a) every section link is found by role+name (`Dashboard`, `Agents`, `Providers`, `MCP Servers`, `Sessions`, `Server`) and has non-empty visible-or-visually-hidden text; (b) with no key, the `Management key` field and `Connect` button are in the document and visible on `/agents` (the key bar is not only at the page bottom: assert it appears before `main`'s route content in DOM order); (c) after connecting, `Clear key` is present and the status text "Key in memory" is present; (d) the input stays `type="password"`. Run: expect (b) to fail on ordering.
- [ ] **Step 2: Implement.** Rail: 56px at < 1024px (link text visually hidden with a `.sr-only` class, glyph shown), 200px with text at >= 1024px; active link marked with a 3px `--caller` bar on the inner edge and `aria-current="page"` (NavLink already sets it). Brand mark keeps `aria-hidden`. Top line: page name left, key status right. Move the access panel to a single-row `.key-bar` rendered above `main`'s routes when no key is set (field + Connect inline) and collapses to the existing status plus a `Clear key` button in the top line once connected; keep `id`s, labels, `autoComplete="off"`, and `ErrorState` for `authError`. The browser capability line stays at the bottom of the page in `--ink-2` 13px.
- [ ] **Step 3: Run** `npx vitest run src/app src/test && npx tsc --noEmit`; PASS.
- [ ] **Step 4: Visual check:** with `make dev`'s UI (`PUBLIC_PORT=18080 ADMIN_PORT=19090 UI_PORT=15173 make dev`), screenshot `/dashboard` at 1440, 1024 and 390 wide; the rail never overlaps content and nothing scrolls horizontally.
- [ ] **Step 5: Commit** `feat(ui): Switchboard shell: icon rail, top line, key bar`.

### Task 3: Shared primitives and page rhythm

**Files:**
- Modify: `ui/src/components/primitives.tsx`, `ui/src/styles.css` (`.section-heading`, `.message-panel*`, `.status*`, `.badge*`, `.action-button*`, `.form-field`, `.form-card`, table styles in `.table-scroll`, `.subtext`, `.alert-box`, `.filter-bar`, `.pagination-*`)
- Test: `ui/src/components/primitives.test.tsx` (create if absent)
- Modify: `noHardcodedColors.test.ts` (append `components/primitives.tsx`)

**Interfaces:**
- Produces: `Status` renders a lamp dot (`::before`) coloured by tone (`good`->`--ok`, `bad`->`--bad`, `neutral`->`--ink-2`) plus the same text and `role="status"`; `.message-panel` is a left rule (3px, tone colour) with no box; `.action-button` solid `--caller` with `--surface` text, `.action-button-small` outlined; tables use hairline row dividers, no zebra, no outer box. These class names stay unchanged.

- [ ] **Step 1: Write failing tests:** `Status` keeps `role="status"` and its text for each tone; `ErrorState` keeps `role="alert"`, the retry button text `Try again` and the request id line; `EmptyState` renders `h2` + paragraph. (They pass today for markup; the failing part is the guard test after appending `components/primitives.tsx` to `CONVERTED`, which flags any inline colour.)
- [ ] **Step 2: Implement** the CSS above; sentence-case copy in primitives where capitalised ("Request failed" stays). Headings: `h1` 40px/800, section `h2` 24px/600; `p` max-width 72ch.
- [ ] **Step 3: Run gate; PASS.** Screenshot a table page (Agents) for rhythm.
- [ ] **Step 4: Commit** `feat(ui): Switchboard primitives: lamp status, rule panels, hairline tables`.

### Task 4: Dashboard

**Files:**
- Modify: `ui/src/routes/dashboard/DashboardView.tsx`, `ui/src/routes/dashboard/DashboardView.test.tsx`, `ui/src/styles.css` (replace `.health-banner*`, `.meta-grid`, `.meta-card`, `.summary-*`, `.uptime-pill` with `.state-line`, `.figures`, `.ledger`, `.ledger__row`)
- Modify: `noHardcodedColors.test.ts` (append the dashboard TSX)

**Interfaces:**
- Consumes: the same queries and data as today (operations, agents, providers, sessions); no new fetch.

- [ ] **Step 1: Update tests first** to the new reading while keeping the facts: the health state is one sentence ("System healthy. 0 of 100 sessions active." / degraded and unhealthy variants keep their explanatory text), still `role="status"` with `aria-label="System health status"`; figures row lists Total sessions, Errors and Turns with their values; ledger rows "Agents: 1 of 1 ready" and "Providers: 5 of 7 detected" each contain a link named `View agents` / `View providers`; the capacity alert keeps `role="alert"`, `aria-label="Capacity warning"` and its text. Expected: FAIL on the new strings.
- [ ] **Step 2: Implement.** State line in display type with the lamp dot (ok/bad); figures inline (tabular numbers) separated by spacing, not boxes; the two ledger rows under one hairline each; the existing empty/loading/error branches stay but use the sentence-case copy "Connect with a management key to see operations." and a link to onboarding.
- [ ] **Step 3: Run gate; PASS; screenshot at 1440/390.**
- [ ] **Step 4: Commit** `feat(ui): dashboard as a state line and ledgers`.

### Task 5: Providers and Agents list

**Files:**
- Modify: `ui/src/routes/providers/ProvidersView.tsx`, `ProvidersView.test.tsx`, `ui/src/routes/agents/AgentsView.tsx` (list mode only: lines of the `mode === "list"` block), `AgentsView.test.tsx`, `ui/src/styles.css` (`.ledger*` reuse; `.pill`, `.lamp`)
- Modify: `noHardcodedColors.test.ts` (append `routes/providers/ProvidersView.tsx`)

**Interfaces:**
- Consumes: `ProviderStatus` and `AgentDTO` unchanged. `.lamp` is a 10px dot using `currentColor`, coloured through `data-tone` (`ok|warn|bad`); `.pill` is the inline service chip.

- [ ] **Step 1: Write failing tests:** Providers page keeps the text the existing tests assert (the `env` variable names, "not in this build" naming, detected/undetected state) and each service pill reads `STT available` / `TTS needs GOOGLE_APPLICATION_CREDENTIALS`; agents list shows a lamp with accessible text (`Ready`, `Needs setup`, `Not ready`) per row, and still exposes the row actions `View detail`, `Edit`, `Duplicate`, `Delete agent`.
- [ ] **Step 2: Implement.** Providers: one ledger row per provider (name, detected lamp, services as pills, variables in mono). Agents list: table with name + description, readiness lamp, version, node count (use `fromAgentConfig(...).nodes.length` already imported elsewhere), actions; primary buttons "New agent" and "Design graph agent" keep their existing accessible names (read them from the current test and keep verbatim).
- [ ] **Step 4: Run gate; PASS.** `npx playwright test -c e2e/playwright.graph.config.ts` is not needed yet.
- [ ] **Step 5: Commit** `feat(ui): providers ledger and agents list with readiness lamps`.

### Task 6: The call stage (agent detail), live graph and handoff moment

**Files:**
- Modify: `ui/src/routes/agents/AgentsView.tsx` (detail mode), `ui/src/routes/agents/AgentVoiceTest.tsx`, `ui/src/routes/agents/graph/LiveGraph.tsx`, `ui/src/routes/agents/graph/Canvas.tsx`, `ui/src/styles.css` (`.stage`, `.stage__graph`, `.stage__state`, `.stage__controls`, `.lane`, `.lane--caller`, `.lane--agent`, `.agent-node*`, `.agent-edge*`), tests `AgentVoiceTest.test.tsx`, `AgentVoiceTest.graph.test.tsx`, `LiveGraph.test.tsx`, `AgentsView.test.tsx`
- Create: `ui/src/routes/agents/graph/cable.css.test.ts` (reduced-motion guard)
- Modify: `noHardcodedColors.test.ts` (append `routes/agents/AgentVoiceTest.tsx`, `routes/agents/AgentsView.tsx`, `routes/agents/graph/LiveGraph.tsx`, `routes/agents/graph/Canvas.tsx`)

**Interfaces:**
- Consumes: everything `AgentVoiceTest` consumes today; props unchanged. `Canvas` keeps `graph`, `visual`, `traversedEdges`, `readOnly`, `height`.
- Produces: the new detail layout (below) with all existing `data-testid`s on the same elements.

Layout of detail mode, top to bottom: (1) header: name (h2), `v{n}` and readiness lamp, `Edit` / `Design` / `Duplicate` / `Delete agent` / `Back to list` actions (names unchanged); (2) stage: left large graph (height 360, full width of the main column up to 1100px), with call state in display type beside/under it (`{Agent} is answering`, derived from the existing live status text, which keeps `role="status"` and the "is answering" wording the e2e asserts) and the controls row; (3) transcript in two lanes: caller lines `--caller`, agent lines `--ink` with the node name; typed-turn form; (4) collapsed `<details>` sections for configuration summary, tools, readiness reasons, session summary and the debug panel (`toggle-debug-btn` keeps working: the debug panel stays mounted behind its existing toggle). The immutable-snapshot explainer paragraph becomes one line under the header's version.

- [ ] **Step 1: Write failing tests.** (a) `AgentVoiceTest.graph.test.tsx`: for a graph agent the stage shows the graph and the controls without scrolling past a configuration section (assert the DOM order: the element with `data-testid="start-voice-test-btn"` precedes the config summary `details`); (b) detail mode shows readiness text in `agent-readiness` and starts collapsed sections closed (`details` without `open`); (c) a handoff test: `LiveGraph` given a trace with a handoff marks the target node `agent-node--active` and the edge `agent-edge--traversed` (existing behaviour, keep green) and sets `data-just-handed="true"` on the edge for 450ms after the change (new: used to run the cable draw); (d) `cable.css.test.ts`: read `styles.css`, assert the `.agent-edge--traversed` / `.agent-node--active` transition rules appear only inside `@media (prefers-reduced-motion: no-preference)` and that a `reduce` block sets `animation: none; transition: none` for them. Expected: (a), (c), (d) fail.
- [ ] **Step 2: Implement the layout and node/edge styling.** Active node: fill `--lamp`, text `--ink` (contrast 4.5:1 verified in Task 1), a 2px `--ink` border; visited nodes `--surface` with `--ink-2` text; preparing node dashed `--ink` border with the existing pulse (disabled under reduce); edges `--ink` 2px, traversed edges 3px `--ink` drawn with `stroke-dasharray`/`stroke-dashoffset` over 400ms when `data-just-handed`. Caller activity indicator: a 10px `--caller` dot that pulses while `userSpeaking`; bot speaking uses the lamp on the node only (no second indicator).
- [ ] **Step 3: Implement `data-just-handed`** in `LiveGraph.tsx` (compare previous `view.traversed.length` in a `useRef`; set a state flag, clear with `setTimeout` 450ms, cleared on unmount) and pass it to `Canvas` as `justHandedEdge?: string`; `Canvas` applies the attribute to that edge's className/`data-` prop. No new dependency.
- [ ] **Step 4: Run** `npx vitest run src/routes/agents && npx tsc --noEmit`; PASS. Then `cd e2e && npx playwright test -c playwright.graph.config.ts`: all 7 pass unchanged (this is Review Focus 1).
- [ ] **Step 5: Visual check:** with a live dev stack start a call (typed text works without microphone: use `Message` field), screenshot the stage idle, mid-call and after a handoff at 1440 and 390; record a short screen capture of the handoff if the browser tool supports it.
- [ ] **Step 6: Commit** `feat(ui): call stage with live graph, lamp and cable handoff`.

### Task 7: Graph designer

**Files:**
- Modify: `ui/src/routes/agents/graph/GraphDesigner.tsx`, `Inspector.tsx`, `ui/src/routes/agents/StageFields.tsx`, `ui/src/routes/agents/AgentsView.tsx` (create/edit/design form areas), `ui/src/styles.css` (`.designer*`, `.form-card`, `.stage-box`), tests `GraphDesigner.test.tsx`, `StageFields.test.tsx`, `AgentsView.test.tsx`
- Modify: `noHardcodedColors.test.ts` (append these TSX files)

**Interfaces:**
- Consumes: Task 6 stage styles for the test panel below the form. Props unchanged; labels unchanged (`Agent name`, `New agent node`, `STT provider`, `Voice`, `Sound name`, `Start after (ms)`, `Publish`, `Add agent`, `Add handoff`, `Hand off when`, `Node name`, `System prompt`).

- [ ] **Step 1: Failing tests:** the guard (appended files contain no inline colours) plus a test that the unpublished-edits note renders in a `role="status"` region and that the designer inspector has no box class (`.form-card` is replaced by `.inspector`). 
- [ ] **Step 2: Implement:** canvas on `--surface` with a hairline; selected node 2px `--caller` border; inspector as a right-hand column separated by a hairline (collapses under the canvas below 800px); form fields underline-style with 4px radius on focus ring; the "Unpublished edits are not in this test" note uses the lamp rule (3px `--lamp` left rule, `--ink` text). Copy: "Publish" -> after success the existing success message becomes "Published v{n}." (update the test strings and the e2e regex `/published immutable version v2/i` in `e2e/tests-graph/10-agent-graph.spec.ts` in the same commit: the e2e expectation changes to `/published v2/i`).
- [ ] **Step 3: Run gate and the fake graph e2e; PASS.**
- [ ] **Step 4: Commit** `feat(ui): graph designer in Switchboard style`.

### Task 8: Sessions, trace timeline and debug panel

**Files:**
- Modify: `ui/src/routes/sessions/SessionsView.tsx`, `SessionDetailView.tsx`, `ui/src/components/timeline/TurnTimeline.tsx`, `ui/src/components/debug/PipecatDebugPanel.tsx`, `ui/src/styles.css` (`.timeline`, `.turn-*`, `.latency-*`, `.trace-*`, `.console-*`), their tests
- Modify: `noHardcodedColors.test.ts` (append the four TSX files)

**Interfaces:**
- Consumes: speaker colours: caller `--caller`, agent `--ink`, handoff `--lamp` (fill/border only); everything else tokens.

- [ ] **Step 1: Failing tests:** the guard for the four files; session detail keeps `Turn` structure text the existing tests assert; the handoff marker in the trace/timeline has accessible text "Handed off to {node}" (check what the timeline already renders for `handoff` spans and keep its text; add the lamp marker only as styling).
- [ ] **Step 2: Implement:** sessions list as a hairline table with a lamp for outcome; turn timeline as two lanes (caller/agent) with latency pills in tabular numbers; debug panel and console boxes lose their inline hex and use `--surface`/`--rule`/`--ink`; keep the debug panel's content and testids.
- [ ] **Step 3: Run gate; PASS. Screenshot a session with a handoff.**
- [ ] **Step 4: Commit** `feat(ui): sessions and timeline in Switchboard style`.

### Task 9: MCP, Server and Onboarding inherit the tokens

**Files:**
- Modify: `ui/src/routes/mcp/MCPView.tsx`, `ui/src/routes/server/ServerView.tsx`, `ui/src/routes/onboarding/OnboardingView.tsx`, `ui/src/styles.css` if classes are needed
- Modify: `noHardcodedColors.test.ts` (append these three; `CONVERTED` now lists every TSX under `routes/` and `components/` plus `app/App.tsx`)

- [ ] **Step 1:** append the three files; run, expect FAIL listing each hex line.
- [ ] **Step 2:** mechanical mapping only (no layout work): `#53666b`->`var(--ink-2)`, `#c9d3d2`->`var(--rule)`, `#007e82`->`var(--caller)`, whites->`var(--surface)`, status greens/reds->`var(--ok)`/`var(--bad)`; move repeated inline style objects into one class each where it removes duplication, otherwise swap the value in place.
- [ ] **Step 3:** run gate; PASS; screenshot each of the three at 1440 and 390 to confirm nothing is broken or unreadable.
- [ ] **Step 4: Commit** `refactor(ui): MCP, server and onboarding use the Switchboard tokens`.

### Task 10: Demo seed graph

**Files:**
- Modify: `scripts/dev-seed.mjs` (and `scripts/dev.sh` only if it lists what the seed makes); create nothing else.

**Interfaces:**
- Consumes: `GET /api/v1/providers` (catalog defaults) and the agent create API; the existing "Dev Assistant" creation code and its idempotence check.

- [ ] **Step 1: Failing check:** run `node scripts/dev-seed.mjs` against the fake e2e stack (see how Task 11 of the credentials plan did it: `e2e/global-setup-graph.ts` starts one) and observe that no "Dealership desk" agent exists.
- [ ] **Step 2: Implement:** after "Dev Assistant", if an LLM, STT and TTS provider are available, create "Dealership desk" once (look up by name first): entry node "Front desk" (STT, LLM with a prompt that greets, answers simply and calls the transfer when the caller wants to buy, TTS) and node "Sales" (LLM prompt with a short catalogue, TTS), edge front->sales "The caller wants to buy a car or asks about prices.", `handoff_cue: { name: "chime", after_ms: 50 }`; defaults come from the catalog (`default: true` entries), no hard-coded model or voice ids.
- [ ] **Step 3: Verify:** the seed run twice creates one agent; its readiness is `ready`; the graph appears in the UI. Leave the existing Dev Assistant behaviour untouched.
- [ ] **Step 4: Commit** `feat(dev): seed a two-agent Dealership desk graph for demos`.

### Task 11: Verification gate, accessibility pass and review

- [ ] **Step 1: Automated:** `cd ui && npx vitest run && npx tsc --noEmit && npm run build`; `make e2e-fake`; `make test-graph-e2e`; `GOVOICE_LIVE=1 make e2e-xai` once (spends xAI credit; the keys are in `examples/.env`; ask before running if unsure).
- [ ] **Step 2: Layout:** a Playwright check (add to `e2e/tests-graph/` as a fake-mode test `layout: demo-path screens do not scroll sideways at 390px`) visiting dashboard, providers, agents, an agent detail and a session detail at width 390 and asserting `scrollWidth <= innerWidth`.
- [ ] **Step 3: Accessibility pass:** keyboard-only run through the call stage (Tab order: header actions, call, mute, message field, send), visible focus on every control, `prefers-reduced-motion` emulated in the browser tool shows no cable animation, the contrast test green.
- [ ] **Step 4: Visual review:** screenshot every demo-path screen at 1440, 1024, 390 and review against the spec's visual system; fix defects found in-place with a test or check.
- [ ] **Step 5: Independent adversarial review** of `git diff 6d1c392..HEAD` (YAGNI/bloat, test hooks preserved, a11y, leftover inline styles); fix findings with a failing test first; re-run Step 1.
- [ ] **Step 6: Commit** any fixes: `fix(ui): review fixes for the Switchboard polish`.
