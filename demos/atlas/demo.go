// Package atlas defines the Atlas Travel Concierge demo: a travel inventory MCP
// server, two HTTP endpoint tools and an eight-step agent with thirty-two
// handoffs. It is the stress case for the agent surface (the maximum steps and
// handoffs, mixed tool kinds, an async tool, parallel calls and a different
// voice per step).
package atlas

import (
	"encoding/json"
	"fmt"

	"govoice/demos/atlas/mcp/atlasmcp"
	"govoice/demos/spec"
)

const (
	// Name is the demo's registry name.
	Name = "atlas"
	// ServerName is the MCP server the demo registers.
	ServerName = "Atlas travel inventory"
	// AgentName is the demo agent.
	AgentName = "Atlas Travel Concierge"

	serverDescription = "Fake travellers, bookings and travel inventory for the Atlas demo."
	agentDescription  = "Travel concierge: eight steps and thirty-two handoffs across flights, stays, ground transport, payments, loyalty, disruptions and a human handoff."

	// The two HTTP endpoint tools the agent pins, as named by the fixture.
	weatherHTTPTool  = "weather_forecast"
	advisoryHTTPTool = "travel_advisory"
)

// Demo is the Atlas Travel Concierge demo definition.
type Demo struct{}

// Name implements spec.Demo.
func (Demo) Name() string { return Name }

// MCPServers implements spec.Demo.
func (Demo) MCPServers() []spec.MCPServerSpec {
	return []spec.MCPServerSpec{{
		Logical:      "inventory",
		Name:         ServerName,
		Description:  serverDescription,
		EndpointPath: "/mcp",
		TokenEnv:     "ATLAS_MCP_TOKEN",
		SecretName:   "demo-atlas-mcp",
		Tools:        atlasmcp.ToolNames,
	}}
}

// HTTPTools implements spec.Demo: the demo's two plain JSON endpoints, exposed
// to the agent as HTTP tools.
func (Demo) HTTPTools() []spec.HTTPToolSpec {
	out := make([]spec.HTTPToolSpec, 0, len(atlasmcp.HTTPTools))
	for _, t := range atlasmcp.HTTPTools {
		out = append(out, spec.HTTPToolSpec{
			Name:        t.Name,
			Description: t.Description,
			Method:      t.Method,
			Path:        t.Path,
			InputSchema: t.InputSchema,
		})
	}
	return out
}

// Agent implements spec.Demo. The config is exactly the one the demo has always
// published: eight steps, thirty-two handoffs, eighteen MCP tool references and
// the two HTTP tool references resolved from the seed's pins.
func (Demo) Agent(r spec.Resolution) (spec.AgentSpec, error) {
	serverID := r.ServerIDs["inventory"]
	if serverID == "" {
		return spec.AgentSpec{}, fmt.Errorf("%s: no server id resolved for the inventory MCP server", Name)
	}
	httpRefs := make(map[string]string, 2)
	for _, name := range []string{weatherHTTPTool, advisoryHTTPTool} {
		pin, ok := r.HTTPPins[name]
		if !ok || pin.ServerID == "" || pin.RemoteName == "" {
			return spec.AgentSpec{}, fmt.Errorf("%s: no pin resolved for HTTP tool %q", Name, name)
		}
		httpRefs[name] = spec.HTTPRef(pin)
	}
	cfg, err := config(serverID, httpRefs)
	if err != nil {
		return spec.AgentSpec{}, err
	}
	return spec.AgentSpec{Name: AgentName, Description: agentDescription, Config: cfg}, nil
}

// config builds the eight-step agent. serverID is the installed MCP server;
// httpRefs maps each HTTP tool name to its "<http id>/<name>" step reference.
func config(serverID string, httpRefs map[string]string) (json.RawMessage, error) {
	start := spec.Stage{
		"stt": spec.Stage{"provider": "google", "model": "chirp_2", "language": "en-US"},
		"llm": spec.Stage{"provider": "google", "model": "gemini-2.5-flash", "system_prompt": conciergePrompt},
		"tts": spec.Stage{"provider": "elevenlabs", "voice": "21m00Tcm4TlvDq8ikWAM"},
		// A long, tool-heavy call: keep more history than the default 20 messages.
		"context":                   spec.Stage{"max_messages": 30, "max_tokens": 6000},
		"max_call_duration_seconds": 600,
		// The disruption rebooking runs in the background: the model gets an ack and the result arrives later.
		"async_tools": spec.Refs(serverID, "rebook_disruption"),
	}
	flights := spec.Stage{
		"llm":      spec.Stage{"provider": "xai", "model": "grok-4.20-0309-non-reasoning", "system_prompt": flightsPrompt, "max_parallel": 3},
		"tts":      spec.Stage{"provider": "xai", "voice": "eve"},
		"greeting": "You're through to Atlas flights. How can I help with your trip?",
	}
	stays := spec.Stage{
		"llm": spec.Stage{"provider": "google", "model": "gemini-2.5-flash", "system_prompt": staysPrompt},
		"tts": spec.Stage{"provider": "google", "voice": "en-US-Chirp3-HD-Aoede"},
	}
	ground := spec.Stage{
		"llm":      spec.Stage{"provider": "google", "model": "gemini-2.5-flash", "system_prompt": groundPrompt},
		"tts":      spec.Stage{"provider": "elevenlabs", "voice": "EXAVITQu4vr4xnSDxMaL"},
		"greeting": "You're through to Atlas ground transport. Where are you heading?",
	}
	payments := spec.Stage{
		"llm": spec.Stage{"provider": "xai", "model": "grok-4.20-0309-non-reasoning", "system_prompt": paymentsPrompt},
		"tts": spec.Stage{"provider": "elevenlabs", "voice": "21m00Tcm4TlvDq8ikWAM"},
	}
	loyalty := spec.Stage{
		"llm": spec.Stage{"provider": "google", "model": "gemini-2.5-flash", "system_prompt": loyaltyPrompt},
		"tts": spec.Stage{"provider": "google", "voice": "en-US-Chirp3-HD-Aoede"},
	}
	disruptions := spec.Stage{
		"llm": spec.Stage{"provider": "xai", "model": "grok-4.20-0309-non-reasoning", "system_prompt": disruptionsPrompt, "max_parallel": 3},
		"tts": spec.Stage{"provider": "xai", "voice": "eve"},
	}
	human := spec.Stage{
		"llm":      spec.Stage{"provider": "google", "model": "gemini-2.5-flash", "system_prompt": humanPrompt},
		"tts":      spec.Stage{"provider": "elevenlabs", "voice": "21m00Tcm4TlvDq8ikWAM"},
		"greeting": "I'm sorry this needs a person. I'll take a few details and a teammate will call you back.",
	}

	cfg := spec.Stage{
		"start": "concierge",
		"steps": []spec.Stage{
			spec.Step("concierge", "Concierge", true, spec.Refs(serverID, "get_traveller", "list_bookings"), start, 0, 260),
			spec.Step("flights", "Flights", true, spec.Refs(serverID, "search_flights", "book_flight", "change_flight", "cancel_booking"), flights, 380, 0),
			spec.Step("stays", "Stays", true, append(spec.Refs(serverID, "search_stays", "book_stay", "cancel_booking"), httpRefs[weatherHTTPTool]), stays, 380, 180),
			spec.Step("ground", "Ground transport", true, spec.Refs(serverID, "search_transfers", "book_transfer"), ground, 380, 360),
			spec.Step("payments", "Payments", true, spec.Refs(serverID, "list_payment_methods", "charge_booking", "issue_refund"), payments, 760, 0),
			spec.Step("loyalty", "Loyalty", true, spec.Refs(serverID, "get_loyalty", "apply_points"), loyalty, 760, 180),
			spec.Step("disruptions", "Disruptions", true, append(spec.Refs(serverID, "list_bookings", "report_disruption", "rebook_disruption"), httpRefs[advisoryHTTPTool]), disruptions, 760, 360),
			spec.Step("human", "Human handoff", true, spec.Refs(serverID, "create_callback"), human, 1140, 260),
		},
		"handoffs": handoffs(),
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("encode Atlas agent: %w", err)
	}
	return raw, nil
}

// handoffs is the thirty-two-edge handoff graph: at the platform maximum, every
// ordered pair unique and every step reachable from the concierge.
func handoffs() []spec.Handoff {
	// The order groups each source step's outgoing handoffs, which is also the
	// order the model sees the transfer tools in.
	return []spec.Handoff{
		{ID: "concierge-flights", From: "concierge", To: "flights", Condition: "The caller wants to find, book, change or cancel a flight."},
		{ID: "concierge-stays", From: "concierge", To: "stays", Condition: "The caller wants a hotel, apartment or villa for their trip."},
		{ID: "concierge-ground", From: "concierge", To: "ground", Condition: "The caller needs an airport transfer, a hire car or a rail ticket."},
		{ID: "concierge-payments", From: "concierge", To: "payments", Condition: "The caller wants to pay, query a charge or ask for a refund."},
		{ID: "concierge-loyalty", From: "concierge", To: "loyalty", Condition: "The caller asks about points, tier status or an upgrade."},
		{ID: "concierge-disruptions", From: "concierge", To: "disruptions", Condition: "The caller's trip has been delayed, cancelled or changed by a supplier."},
		{ID: "concierge-human", From: "concierge", To: "human", Condition: "The caller asks for a person, or is angry or distressed."},
		{ID: "flights-concierge", From: "flights", To: "concierge", Condition: "The caller changes subject to something the concierge should triage."},
		{ID: "flights-stays", From: "flights", To: "stays", Condition: "The caller wants a hotel for the same trip."},
		{ID: "flights-ground", From: "flights", To: "ground", Condition: "The caller needs transport to or from the airport."},
		{ID: "flights-payments", From: "flights", To: "payments", Condition: "The caller wants to pay for or refund a flight."},
		{ID: "flights-disruptions", From: "flights", To: "disruptions", Condition: "A flight is delayed or cancelled and needs rebooking."},
		{ID: "flights-human", From: "flights", To: "human", Condition: "The caller asks for a person, or the issue is beyond a flight change."},
		{ID: "stays-concierge", From: "stays", To: "concierge", Condition: "The caller changes subject to something the concierge should triage."},
		{ID: "stays-flights", From: "stays", To: "flights", Condition: "The caller wants to add or change a flight."},
		{ID: "stays-ground", From: "stays", To: "ground", Condition: "The caller needs a transfer to or from the hotel."},
		{ID: "stays-payments", From: "stays", To: "payments", Condition: "The caller wants to pay for or refund a stay."},
		{ID: "stays-disruptions", From: "stays", To: "disruptions", Condition: "The hotel is overbooked, closed or must be rebooked."},
		{ID: "stays-human", From: "stays", To: "human", Condition: "The caller asks for a person, or the issue is beyond a stay change."},
		{ID: "ground-flights", From: "ground", To: "flights", Condition: "The caller wants to book or change a flight."},
		{ID: "ground-stays", From: "ground", To: "stays", Condition: "The caller wants to book or change a stay."},
		{ID: "ground-payments", From: "ground", To: "payments", Condition: "The caller wants to pay for or refund a transfer."},
		{ID: "ground-disruptions", From: "ground", To: "disruptions", Condition: "A missed connection or failed transfer needs help."},
		{ID: "ground-human", From: "ground", To: "human", Condition: "The caller asks for a person, or the issue is beyond a transfer."},
		{ID: "payments-flights", From: "payments", To: "flights", Condition: "The caller wants to add a flight after paying."},
		{ID: "payments-stays", From: "payments", To: "stays", Condition: "The caller wants to add a stay after paying."},
		{ID: "payments-loyalty", From: "payments", To: "loyalty", Condition: "The caller wants to use points or a tier benefit."},
		{ID: "payments-human", From: "payments", To: "human", Condition: "A payment dispute needs a person."},
		{ID: "loyalty-flights", From: "loyalty", To: "flights", Condition: "The caller wants to redeem points for a flight."},
		{ID: "loyalty-human", From: "loyalty", To: "human", Condition: "An account issue needs a person."},
		{ID: "disruptions-payments", From: "disruptions", To: "payments", Condition: "The caller wants a refund or compensation."},
		{ID: "disruptions-human", From: "disruptions", To: "human", Condition: "Rebooking cannot be resolved by the agent."},
	}
}

const conciergePrompt = `You are the Atlas Travel concierge on a phone call. Keep every reply to one or two short sentences; no lists, no markdown.

Greet the caller, say you are Atlas Travel, then ask for their name and what they need. When you have the name, call get_traveller and then list_bookings so you know their trips. Never invent a traveller id.

Hand the caller to the specialist who owns the request, and put their name and need in the transfer note:
- flights for anything about flying; stays for hotels; ground transport for transfers, cars or rail.
- payments for charges, receipts and refunds; loyalty for points, tier and upgrades.
- disruptions for a delayed, cancelled or changed trip; human handoff when they ask for a person or are upset.

Say one short sentence that you are connecting them, then stop talking. After a goodbye, call end_call.`

const flightsPrompt = `You are the Atlas flights specialist on a phone call. Keep every reply to one or two short sentences; no lists, no markdown.
The concierge has passed the caller's name and request in a note: greet them by first name and do not ask for it again.

1. Call search_flights with the two cities and the date, then offer at most three options out loud.
2. To book, call book_flight with the traveller id and the flight id from the results.
3. To move or cancel an existing flight, use the booking id from the earlier lookup; if you do not have one, transfer to the concierge rather than guessing. Confirm the change in one sentence.

Use only what the tools return; never invent flights, ids or prices. If a tool fails, apologise and offer another option. When the caller needs a hotel, transport, payment, a disruption or a person, use the matching transfer tool; for points or a tier benefit, transfer to payments. Say one short sentence that you are connecting them, then stop. After a goodbye, call end_call.`

const staysPrompt = `You are the Atlas stays specialist on a phone call. Keep every reply to one or two short sentences; no lists, no markdown.
The concierge has passed the caller's name and request in a note: greet them by first name and do not ask for it again.

1. Call search_stays with the city, a check-in date and a check-out date, then offer at most three options out loud.
2. To book, call book_stay with the traveller id and the stay id from the results.
3. To cancel an existing stay, use the booking id from the earlier lookup; if you do not have one, transfer to the concierge rather than guessing. Confirm the change in one sentence.
4. If the caller asks about the weather, call weather_forecast with the city and date and answer briefly.

Use only what the tools return; never invent hotels, ids or prices. If a tool fails, apologise and offer another option. When the caller needs a flight, transport, payment, a disruption or a person, use the matching transfer tool; for points or a tier benefit, transfer to payments. Say one short sentence that you are connecting them, then stop. After a goodbye, call end_call.`

const groundPrompt = `You are the Atlas ground transport specialist on a phone call. Keep every reply to one or two short sentences; no lists, no markdown.
The concierge has passed the caller's name and request in a note: greet them by first name and do not ask for it again.

1. Call search_transfers with the two places and the date, then offer at most three options out loud.
2. To book, call book_transfer with the traveller id and the transfer id from the results, and confirm in one sentence.

Use only what the tools return; never invent vehicles, ids or prices. If a tool fails, apologise and offer another option. When the caller needs a flight, hotel, payment, disruption help or a person, use the matching transfer tool. Say one short sentence that you are connecting them, then stop. After a goodbye, call end_call.`

const paymentsPrompt = `You are the Atlas payments specialist on a phone call. Keep every reply to one or two short sentences; no lists, no markdown.
The concierge has passed the caller's name and request in a note: greet them by first name and do not ask for it again.

1. Call list_payment_methods with the traveller id and read at most two options out loud, never a full card number.
2. To take payment, call charge_booking with the booking id and the chosen payment method id, then confirm the amount in one sentence.
3. To refund, call issue_refund with the charge id from the payment and a short reason; confirm it is on its way.

Use only what the tools return; never invent charges or refunds. For a payment dispute that needs a person, use the transfer tool to human handoff. For points or a tier benefit, transfer to loyalty. After a goodbye, call end_call.`

const loyaltyPrompt = `You are the Atlas loyalty specialist on a phone call. Keep every reply to one or two short sentences; no lists, no markdown.
The concierge has passed the caller's name and request in a note: greet them by first name and do not ask for it again.

1. Call get_loyalty with the traveller id and tell the caller their tier and points.
2. To use points, call apply_points with the traveller id and the number of points they agree to use (it deducts them from the balance), then confirm.

Use only what the tools return; never invent points or tiers. When the caller wants to redeem points for a flight, use the transfer tool to flights. When an account issue needs a person, transfer to human handoff. After a goodbye, call end_call.`

const disruptionsPrompt = `You are the Atlas disruptions specialist on a phone call. Keep every reply to one or two short sentences; no lists, no markdown.
The concierge has passed the caller's name and request in a note: greet them by first name and do not ask for it again.

1. Call list_bookings with the traveller id to find the affected booking. Ask for the booking id if it is not clear.
2. Call report_disruption with the booking id and a short reason.
3. Call rebook_disruption with the disruption id from the report. It returns at once and the result arrives later: tell the caller you are working on it and that you will confirm shortly.
4. If asked about travel advice, call travel_advisory with the country and summarise it in one sentence.

Use only what the tools return; never invent new flights or times. When the caller wants a refund or compensation, transfer to payments. When rebooking cannot be resolved, transfer to human handoff. After a goodbye, call end_call.`

const humanPrompt = `You are the Atlas human handoff on a phone call. Keep every reply to one or two short sentences; no lists, no markdown.
The concierge has passed the caller's name and request in a note: greet them by first name and do not ask for it again.

Be calm and apologetic. Ask for the best phone number if you do not have it, then call create_callback with their name, phone number and a short topic. Tell them a teammate will call back within one working day, then after a goodbye call end_call.`
