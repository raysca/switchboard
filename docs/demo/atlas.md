# Atlas Travel Concierge demo

"Atlas Travel Concierge" is the large demo: a stored agent at the platform's current
maximum of **8 steps and 32 handoffs**, with 18 MCP tools, two HTTP endpoint tools, an
async tool, parallel tool calls and a different LLM and voice on most steps. A caller is
triaged by the concierge and handed between seven travel specialists; the voice changes
as the call moves, and the call ends by itself when the agent says goodbye.

It is the stress case beside the small [Smile Dental demo](dentistry.md) (3 steps, 2
handoffs, one MCP server). Use Atlas to see a dense agent in the designer and a long call
path in the Calls page.

| Step | LLM | TTS | Tools | Job |
|---|---|---|---|---|
| **Concierge** (start) | Gemini 2.5 Flash | ElevenLabs | `get_traveller`, `list_bookings` | Greet, take the name, look up the traveller and their trips, hand off. End the call. |
| **Flights** | xAI Grok | xAI | `search_flights`, `book_flight`, `change_flight`, `cancel_booking` | Find, book, change or cancel a flight. Parallel tool calls enabled. |
| **Stays** | Gemini 2.5 Flash | Google Chirp 3 HD | `search_stays`, `book_stay`, `cancel_booking`, `weather_forecast` (HTTP) | Book or cancel a hotel; answer a weather question from the HTTP tool. |
| **Ground transport** | Gemini 2.5 Flash | ElevenLabs | `search_transfers`, `book_transfer` | Airport transfers, hire cars and rail. |
| **Payments** | xAI Grok | ElevenLabs | `list_payment_methods`, `charge_booking`, `issue_refund` | Take a payment, read a receipt, issue a refund. |
| **Loyalty** | Gemini 2.5 Flash | Google Chirp 3 HD | `get_loyalty`, `apply_points` | Read tier and points, deduct points from the balance. |
| **Disruptions** | xAI Grok | xAI | `list_bookings`, `report_disruption`, `rebook_disruption` (async), `travel_advisory` (HTTP) | Handle a delayed, cancelled or changed trip; rebook in the background. |
| **Human handoff** | Gemini 2.5 Flash | ElevenLabs | `create_callback` | Take a callback for a person; end the call. |

STT is Google Chirp 2 for the whole call. The handoffs are plain-language conditions the
model uses to call `transfer_to_<step>`; each takes an optional `note` (at most 500 bytes)
with the caller's name and need, which the next step gets as context in its greeting. The
start step sets the call-level settings: a 10-minute cap, a larger history budget (30
messages / 6,000 tokens) and `async_tools` naming `rebook_disruption`. Flights and
Disruptions set `llm.max_parallel: 3`, so one model turn may run up to three tools at
once. Every step can end the call.

## Handoffs

The graph is deliberately at the 32-handoff limit: each ordered pair appears at most once
and every step is reachable from the concierge. A single call can take at most 10 transfers.

| From | To | When |
|---|---|---|
| Concierge | Flights | Find, book, change or cancel a flight |
| Concierge | Stays | A hotel, apartment or villa for the trip |
| Concierge | Ground transport | Airport transfer, hire car or rail |
| Concierge | Payments | Pay, query a charge or request a refund |
| Concierge | Loyalty | Points, tier status or an upgrade |
| Concierge | Disruptions | The trip was delayed, cancelled or changed by a supplier |
| Concierge | Human handoff | Asks for a person, or is angry or distressed |
| Flights | Concierge | Changes subject to something the concierge triages |
| Flights | Stays | Wants a hotel for the same trip |
| Flights | Ground transport | Needs transport to or from the airport |
| Flights | Payments | Wants to pay for or refund a flight |
| Flights | Disruptions | A flight is delayed or cancelled and needs rebooking |
| Flights | Human handoff | Asks for a person, or the issue is beyond a flight change |
| Stays | Concierge | Changes subject to something the concierge triages |
| Stays | Flights | Wants to add or change a flight |
| Stays | Ground transport | Needs a transfer to or from the hotel |
| Stays | Payments | Wants to pay for or refund a stay |
| Stays | Disruptions | The hotel is overbooked, closed or must be rebooked |
| Stays | Human handoff | Asks for a person, or the issue is beyond a stay change |
| Ground transport | Flights | Wants to book or change a flight |
| Ground transport | Stays | Wants to book or change a stay |
| Ground transport | Payments | Wants to pay for or refund a transfer |
| Ground transport | Disruptions | A missed connection or failed transfer needs help |
| Ground transport | Human handoff | Asks for a person, or the issue is beyond a transfer |
| Payments | Flights | Wants to add a flight after paying |
| Payments | Stays | Wants to add a stay after paying |
| Payments | Loyalty | Wants to use points or a tier benefit |
| Payments | Human handoff | A payment dispute needs a person |
| Loyalty | Flights | Wants to redeem points for a flight |
| Loyalty | Human handoff | An account issue needs a person |
| Disruptions | Payments | Wants a refund or compensation |
| Disruptions | Human handoff | Rebooking cannot be resolved by the agent |

## Tools

The MCP fixture (`server/mcp/atlasmcp`, run as `cmd/atlas-mcp`) holds fake travellers,
bookings and inventory only:

- Travellers: `get_traveller`, `list_bookings`
- Flights: `search_flights`, `book_flight`, `change_flight`, `cancel_booking`
- Stays: `search_stays`, `book_stay`
- Ground: `search_transfers`, `book_transfer`
- Payments: `list_payment_methods`, `charge_booking`, `issue_refund`
- Loyalty: `get_loyalty`, `apply_points`
- Disruptions: `report_disruption`, `rebook_disruption` (two seconds, marked async)
- Human handoff: `create_callback`

Two **HTTP endpoint tools** show the other tool kind, without an MCP server:
`weather_forecast` and `travel_advisory` call `<ATLAS_DEMO_URL>/api/weather` and
`<ATLAS_DEMO_URL>/api/advisory` on the same process. HTTP tools cannot be async, so the
async tool is the MCP rebooking call.

The seeded names are stable, so a call can be tried without inventing data:

| Traveller | Tier | Points | Booking |
|---|---|---|---|
| `t1` Maria Lopez | Gold | 8,250 | `b1` flight London → Paris |
| `t2` James Okafor | Silver | 3,400 | `b2` flight Paris → Dubai |
| `t3` Priya Nair | Platinum | 21,700 | `b3` stay in Singapore (2 nights) |

## What is installed

The installer (`server/demo`) is idempotent. It creates or reuses:

- the MCP server **"Atlas travel inventory"** at `<ATLAS_DEMO_URL>/mcp`, with its bearer
  token named by `MCP_TOKEN_ATLAS` (the value is read from the environment and never
  stored);
- the HTTP tools **"weather_forecast"** and **"travel_advisory"** at
  `<ATLAS_DEMO_URL>/api/...`;
- the agent **"Atlas Travel Concierge"** with all 18 MCP tools and both HTTP tools
  selected, pinned to the agent version.

A partially finished install is completed by the next run: matching servers and tools are
reused, missing ones created. A name that already exists with another endpoint or URL is
reported as a conflict instead of being edited.

## Run it locally

Provider keys for Google, xAI and ElevenLabs go in `examples/.env` (see
[providers.md](../providers.md)). The xAI TTS voice needs the `xaispeech` build tag, which
`make build` uses.

Terminal 1, the fixture server (port 18768, token `dev-atlas-token`):

```bash
make atlas-mcp
```

Terminal 2, set the two variables the server reads, then start the dev stack. `make dev`
runs the server as a child process, so it inherits them from this shell. The development
profile already allows loopback HTTP for MCP and HTTP tools
(`mcp.allow_local_http: true`).

```bash
export ATLAS_DEMO_URL=http://127.0.0.1:18768
export MCP_TOKEN_ATLAS=dev-atlas-token
make dev
```

Open the Admin UI printed by `make dev`, enter the management key, and click **Load Atlas demo**
under **Atlas Travel demo** on the Dashboard. Open the agent to see the eight-step canvas
and the 32 handoffs, or use **Test call** to talk to it.

Without keys the free echo provider can still run the whole call shape: the browser suite
(`make e2e-fake`) installs the demo and drives an echo variant of the agent through four
handoffs, MCP and HTTP tools, and `end_call`.

## Acceptance script

1. "I need to move my flight." The concierge takes the name, looks up the traveller,
   and hands off to Flights. The voice changes. Flights offers options and changes `b1`.
2. "Add a hotel in Paris." From Flights, ask for a hotel: the call moves to Stays, which
   offers three hotels and books one. Ask about the weather and it calls the HTTP tool.
3. "Can I use some points?" From Stays, transfer to Payments, then to Loyalty.
4. "My flight was cancelled." From any travel step, transfer to Disruptions. It reports
   the disruption, starts the background rebooking (the model acknowledges at once), then
   confirms when the result arrives.
5. "I want to speak to a person." The call ends at Human handoff: it takes a callback and
   hangs up.
6. Calls page: the call shows the full path (for example
   Concierge → Flights → Stays → Payments → Human handoff), a transcript labelled by step,
   the end reason "By the agent (…)", and the tool calls in the traces.

## Limits and notes

- 8 steps, 32 handoffs, at most one handoff per ordered pair, at most 10 transfers per call,
  500-byte transfer notes, at most 64 selected tools per agent version.
- At a step change the call keeps its history but swaps the LLM and TTS; a fixed greeting
  on Flights, Ground transport and Human handoff is spoken verbatim (the transfer note is
  still added as context). The other steps let the model greet.
- Tool arguments and results are never stored or traced, same as every MCP or HTTP tool.
- The HTTP tools are loopback fixtures in development. In a deployment,
  `ATLAS_DEMO_URL` must be an HTTPS address, because production refuses plain HTTP.
- The fixture keeps everything in memory: restarts reset travellers and bookings.
- Call history lives in the store. The demo never enables recording; keep retention short
  on a shared host, because transcripts hold what callers say.

## Non-goals

No DTMF, recording, handoff cue sound or background audio in this agent: each needs
server-level configuration outside an installable agent. No paid-provider verification is
part of the change; the browser proof runs on the free echo provider.
