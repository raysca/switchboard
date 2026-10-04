# V2 admin shell design

The audience is the operator responsible for voice agents, their provider connections, and live sessions. The first job is to find the right operational area and understand what is available at the current API boundary. Task 5 is a navigable shell; the later product tasks supply collection and detail views.

## Visual direction

Use a quiet control-room palette: **paper** `#F3F5F4`, **ink** `#15262A`, **slate** `#53666B`, **line** `#C9D3D2`, **signal** `#007E82`, and **alert** `#A43E30`. Signal marks selected navigation, focus, and the trace motif; alert is reserved for actionable failure. Bundle IBM Plex Sans for interface prose and IBM Plex Mono for technical identifiers and measurements. The compact typographic scale gives the current route one clear heading, then plain sentence-case labels and explanatory copy.

Desktop: a narrow fixed navigation rail and a wide left-aligned workspace. The top line carries the product name and an explicit connection state. The main content is an editorial column with one route heading, one short purpose statement, then the relevant empty, loading, or error state.

```text
┌───────────────┬────────────────────────────────────────────┐
│ go-voice      │ Area heading             connection status │
│ Dashboard     │ Purpose / next action                      │
│ Agents        │                                            │
│ Providers     │   route content or API result              │
│ MCP Servers   │                                            │
│ Sessions      │                                            │
│ Server        │                                            │
└───────────────┴────────────────────────────────────────────┘
```

Mobile: header first, then a horizontally scrollable, keyboard reachable route strip above the same left-aligned content. No cramped multi-column cards.

```text
┌─────────────────────────────┐
│ go-voice    connection      │
│ Dashboard  Agents  …        │
├─────────────────────────────┤
│ Area heading                │
│ Purpose / next action       │
│ route content               │
└─────────────────────────────┘
```

The one memorable element is a signal glyph at the rail title, continued by the selected route rule. Its shape refers to turn and trace inspection without presenting fake live data. All other divisions are functional rules. Motion is limited to a short status change; reduced-motion disables it. Keyboard focus is high contrast and never depends on color alone. Nav uses native links and visible current-page state; form controls have labels; errors include request IDs and recovery text; status is announced politely; no browser permission is requested on load.

## Self-critique and revision

The initial sketch used six equal rounded cards for the six destinations. That would hide navigation inside a generic SaaS dashboard and imply data the API does not yet provide. The revised layout keeps destinations in a rail/strip and gives each route a purpose-led empty state. A bright oscillating waveform was also removed: the only trace is a static line, because animated audio would misleadingly imply a live session.

## Architecture and limits

React Router owns navigation, TanStack Query owns API loading, and a typed API client owns the `/api/v1` envelope, request ID, and authorization behavior. The management bearer lives only in React memory; each request captures its auth generation, so a late 401 cannot clear a later key. A current 401 clears the key, preserves a safe rejection with request ID outside the query cache, and focuses the key input. Reloading requires re-entry. The browser cannot securely persist this deployment key, so the admin listener must remain protected as specified in `docs/api/v1.md`. Task 5 calls only the existing read-only server-config route. Agent, provider, MCP, session, and dashboard data stay explicit placeholders until their APIs arrive. A browser capability readout checks secure context, microphone API presence, and media support without asking permission.
