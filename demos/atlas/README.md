# Atlas Travel Concierge demo

"Atlas Travel Concierge" is the large demo: a stored agent at the platform's current
maximum of **8 steps and 32 handoffs**, with 18 MCP tools, two HTTP endpoint tools, an
async tool, parallel tool calls and a different LLM and voice on most steps. A caller is
triaged by the concierge and handed between seven travel specialists; the voice changes
as the call moves, and the call ends by itself when the agent says goodbye.

It is the stress case beside the small [Smile Dental demo](../dental/README.md) (3 steps, 2
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

The MCP fixture (`demos/atlas/mcp/atlasmcp`, run as `demos/atlas/cmd/atlas-mcp`) holds fake travellers,
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
`weather_forecast` and `travel_advisory` call `<http-base>/api/weather` and
`<http-base>/api/advisory` (the seed defaults `--http-base` to the MCP base URL). HTTP tools cannot be async, so the
async tool is the MCP rebooking call.

The seeded names are stable, so a call can be tried without inventing data:

| Traveller | Tier | Points | Booking |
|---|---|---|---|
| `t1` Maria Lopez | Gold | 8,250 | `b1` flight London → Paris |
| `t2` James Okafor | Silver | 3,400 | `b2` flight Paris → Dubai |
| `t3` Priya Nair | Platinum | 21,700 | `b3` stay in Singapore (2 nights) |

## What is seeded

`demos/seed --demo atlas` wipes any previous Atlas demo and creates it fresh:

- MCP server **"Atlas travel inventory"** at `<mcp-base>/mcp`, with its bearer token stored
  as `demo-atlas-mcp`, plus the two HTTP endpoint tools `weather_forecast` and
  `travel_advisory` at `<http-base>/api/...`;
- the agent **"Atlas Travel Concierge"** with all 18 MCP tools and both HTTP tools pinned.

The fixture is `demos/atlas/mcp/atlasmcp`, run as `demos/atlas/cmd/atlas-mcp`. It holds fake
travellers, bookings and inventory in memory only and resets on restart.

A server deploy is clean-room and installs no demo. Seed the demo after the server is up (see
[the deployment docs](../README.md#deploy-a-demo)), or use the dev stack below.

## Run it locally

Provider keys for Google, xAI and ElevenLabs go in `examples/.env` (see
[providers.md](../../docs/providers.md)). The xAI TTS voice needs the `xaispeech` build tag,
which `make build` and `./dev up` use.

Terminal 1, the fixture server (port 18768, token `dev-atlas-token`):

```bash
./dev demo-start atlas
```

Terminal 2, the dev stack (no demo environment variables):

```bash
./dev up
```

Terminal 3, wipe and seed the demo:

```bash
./dev demo-seed atlas
```

Open the Admin UI printed by `./dev up`, open **Atlas Travel Concierge**, and use **Test
call** to talk to it. The eight-step canvas and the 32 handoffs are visible in the editor.

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
- The HTTP tools are loopback fixtures in development. In a deployment, the HTTP base
  must be an HTTPS address, because production refuses plain HTTP.
- The fixture keeps everything in memory: restarts reset travellers and bookings.
- Call history lives in the store. The demo never enables recording; keep retention short
  on a shared host, because transcripts hold what callers say.

## Non-goals

No DTMF, recording, handoff cue sound or background audio in this agent: each needs
server-level configuration outside an installable agent. No paid-provider verification is
part of the change; the browser proof runs on the free echo provider.
