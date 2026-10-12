# UI polish for the internal demo: "Switchboard"

Status: draft for review. Date: 2026-10-12.

## Goal

An internal demo of go-voice where the memorable moment is a **live call moving between agents on the graph**. The admin UI today is a grey-teal console of identical cards; the call test and live graph are buried at the bottom of the agent detail page. This work gives the UI one visual identity ("Switchboard") and rebuilds the demo path around a **call stage**.

Decisions already made with the requester: hero moment = live call + agent graph; scope = demo path only; direction = A, Switchboard, light.

## Non-goals

- No behaviour, API or server change. Same routes, same data, same actions.
- No dark mode, no new features (single-node test and draft test stay parked).
- MCP servers, Server config and onboarding get the new tokens (so they are not visually broken) but no layout work.
- No new runtime dependency except the font package replacing IBM Plex Sans.

## Visual system

Concept: a handoff is a patch cable on a telephone exchange; the active agent is a lit lamp. This is used in one place (the graph), not as decoration elsewhere.

| Token | Value | Use |
|---|---|---|
| `--paper` | `#E8EDEF` | page base (mineral slate) |
| `--surface` | `#F5F8F9` | raised areas that need separation (forms, popovers) |
| `--ink` | `#12222B` | text, graph edges ("cables") |
| `--ink-2` | `#4A5B64` | secondary text (contrast on `--paper` >= 4.5:1, checked) |
| `--rule` | `#C5CFD4` | the single hairline |
| `--caller` | `#2F3FE0` | the caller: transcript lane, speaking indicator, primary actions |
| `--lamp` | `#F0A400` | the active agent and the handoff moment (only this) |
| `--ok` / `--bad` | `#1F8A5B` / `#C2412D` | status only |

- **Type:** Schibsted Grotesk (variable, via `@fontsource-variable`), weights 450 / 600 / 800. Scale 13 / 15 / 18 / 24 / 40 with 1.55 body line height, 1.15 headings. Display-size 800 weight is used for call state words ("Sales is answering"). Numbers use `font-variant-numeric: tabular-nums`. Monospace (IBM Plex Mono, already installed) stays only for ids, hashes and code; no mono labels, no all-caps eyebrows.
- **Structure:** no card boxes. Sections are separated by spacing and a single `--rule` hairline; borders only where they carry meaning (an input, a selected node, a status). One radius (4px) for controls, none elsewhere.
- **Layout:** 56px icon rail with tooltip/labels on hover and focus (labels visible on wide screens), content left-aligned, max line length 72ch for prose, tables full width.
- **Motion:** one orchestrated moment: when the call hands off, the old node's lamp fades out, the edge "cable" draws between nodes (stroke-dashoffset, 400ms) and the new node's lamp lights. Everything else responds only to input (focus, press, expand). `prefers-reduced-motion` swaps the draw for an instant state change.
- **Quality floor:** visible 3px focus ring in `--caller`; AA contrast for all text and for UI boundaries; layouts verified at 1440, 1024 and 390px wide; no horizontal page scroll.

## Screens on the demo path

1. **App shell:** the icon rail, a quiet top line (page name, key status), skip link and route focus kept.
2. **Dashboard:** replaces the stat-card grid with one readable sentence of state ("System healthy, 0 of 100 sessions active") plus two ledger-style rows (agents ready, providers detected) that link to their pages; sessions/errors shown as a compact inline figures row.
3. **Providers:** a status ledger: one row per provider, services as inline pills (available / name the missing variables), env var names in mono. Already read-only; this is layout and copy only.
4. **Agents list:** a table with a readiness lamp column (ok/warn/bad), version, graph size (nodes), actions. Primary action "New agent" and "Design graph agent".
5. **Agent detail = call stage.** The first screen of the demo:
   - header: name, version, readiness, Edit / Design;
   - **graph (large)** with lamp on the active node, cable animation on handoff;
   - call state in display type and the connection controls (call, mute, push-to-talk, disconnect) beside it;
   - transcript in two lanes (caller `--caller`, agent `--ink`) with the agent node name on each agent line;
   - configuration summary, tools, readiness reasons and the debug panel below, collapsed by default.
   The stale-version note and all existing test hooks stay.
6. **Graph designer:** same canvas styling as the stage; node inspector as a plain panel on the right (no card); the test panel below the form, now a compact version of the stage.
7. **Sessions list and session detail (trace/timeline):** table and turn timeline restyled to the tokens; speaker colours use `--caller` / `--ink` / `--lamp` for handoffs.

## Engineering approach

- Define the tokens once as CSS custom properties in `styles.css`; replace the ~160 hard-coded hex values in inline `style={{}}` props with token classes or `var(--token)`; on the demo path, inline styles move to named classes. Old palette hexes map to the nearest token on the untouched screens.
- Keep the component tree, props, `data-testid`s, accessible names and labels exactly (the vitest suites and the Playwright graph/full-stack suites depend on them). Layout changes are CSS plus markup reordering inside `AgentVoiceTest`/`AgentsView` detail, not behaviour.
- Demo seed: extend `scripts/dev-seed.mjs` to also create a two-agent graph ("Dealership desk": front desk to sales, with a handoff cue) when the catalog has an available provider, so the demo has a graph to show. It is data only, idempotent like the existing seed.
- Copy pass on the demo path: sentence case; one verb per action across the flow (Publish -> "Published v4"); empty and error states say what to do next.

## Verification

- `npx vitest run`, `npx tsc --noEmit`, `npm run build` stay green; update tests only for copy/markup I deliberately change.
- Playwright: fake graph suite and full-stack suite pass; live xAI graph suite passes once at the end.
- Visual: screenshots of each demo-path screen at 1440 / 1024 / 390 reviewed against this spec; a recorded run-through of a live call with a handoff.
- Independent review per AGENTS.md (YAGNI and correctness); an accessibility pass: contrast values for each token pair computed, keyboard path through the call stage, reduced motion.

## Risks

- Inline-style removal is wide (about 200 `style` props in 11 files): mitigated by doing it per screen with the existing tests as the net.
- The rail with icon-only labels must not hurt discoverability: labels stay visible at >= 1024px.
- Fonts: Schibsted Grotesk is OFL-licensed; bundled through the package, no CDN call at demo time.
