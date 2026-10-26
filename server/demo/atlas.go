package demo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"govoice/server/agent"
	"govoice/server/httptool"
	"govoice/server/mcp"
	"govoice/server/mcp/atlasmcp"
	"govoice/server/storage"
)

// Atlas is the large demo: a travel concierge with eight steps, thirty-two
// handoffs, eighteen MCP tools and two HTTP endpoint tools. It is the stress
// case for the agent surface (the maximum steps and handoffs, mixed tool kinds,
// an async tool, parallel calls and a different voice per step).
const (
	AtlasServerName = "Atlas travel inventory"
	AtlasAgentName  = "Atlas Travel Concierge"
	// AtlasAuthEnv is the env var holding the MCP server's bearer token (never stored).
	AtlasAuthEnv = "MCP_TOKEN_ATLAS"
)

// HTTPTools is what the Atlas installer needs of the HTTP tool service. It is
// separate from Servers so the dental installer stays unchanged.
type HTTPTools interface {
	List(ctx context.Context) ([]httptool.View, error)
	Create(ctx context.Context, d httptool.Definition) (httptool.View, error)
	SetEnabled(ctx context.Context, id string, enabled bool) (httptool.View, error)
}

// InstallAtlas adds the Atlas MCP server, its two HTTP tools and the eight-step
// agent unless they already exist. baseURL is ATLAS_DEMO_URL (an origin, with an
// optional path prefix); the MCP endpoint is <base>/mcp and the HTTP tools call
// <base>/api/weather and <base>/api/advisory.
func InstallAtlas(ctx context.Context, servers Servers, tools HTTPTools, agents Agents, baseURL string) (Result, error) {
	var res Result
	base, err := normaliseBase(baseURL)
	if err != nil {
		return res, fmt.Errorf("ATLAS_DEMO_URL: %w", err)
	}
	endpoint := base + "/mcp"

	existing, err := servers.List(ctx)
	if err != nil {
		return res, fmt.Errorf("list MCP servers: %w", err)
	}
	var enabled bool
	for _, s := range existing {
		if s.Name == AtlasServerName && !s.PendingDelete {
			if stored, err := normaliseEndpoint(s.Endpoint); err != nil || stored != endpoint {
				return res, fmt.Errorf("MCP server %q exists with endpoint %s; delete it or set ATLAS_DEMO_URL to match", AtlasServerName, s.Endpoint)
			}
			res.MCPServerID, enabled = s.ID, s.Enabled
		}
	}
	if res.MCPServerID != "" && !enabled {
		// Refresh refuses disabled servers, so a previously disabled demo server is switched back on.
		if err := servers.Enable(ctx, res.MCPServerID); err != nil {
			return res, fmt.Errorf("enable MCP server: %w", err)
		}
	}
	if res.MCPServerID == "" {
		m, err := servers.Create(ctx, mcp.ServerConfig{Name: AtlasServerName, Description: "Fake travellers, bookings and travel inventory for the Atlas demo.", Endpoint: endpoint, AuthEnv: AtlasAuthEnv, Enabled: true})
		if err != nil {
			return res, fmt.Errorf("create MCP server: %w", err)
		}
		res.MCPServerID, res.MCPCreated = m.ID, true
	}
	disc, err := servers.Refresh(ctx, res.MCPServerID)
	if err != nil {
		return res, fmt.Errorf("discover Atlas tools: %w", err)
	}
	have := make(map[string]storage.MCPTool, len(disc.Tools))
	for _, t := range disc.Tools {
		have[t.RemoteName] = t
	}
	selections := make([]agent.ToolSelection, 0, len(atlasmcp.ToolNames)+len(atlasmcp.HTTPTools))
	for _, name := range atlasmcp.ToolNames {
		t, ok := have[name]
		if !ok {
			return res, fmt.Errorf("MCP server %q does not offer the %s tool", AtlasServerName, name)
		}
		selections = append(selections, agent.ToolSelection{ServerID: res.MCPServerID, RemoteName: name, SchemaHash: t.SchemaHash, Required: true})
	}
	res.Tools = len(atlasmcp.ToolNames)

	httpSelections, httpCount, httpCreated, err := installAtlasHTTPTools(ctx, tools, base)
	if err != nil {
		return res, err
	}
	selections = append(selections, httpSelections...)
	res.HTTPTools, res.HTTPToolsCreated = httpCount, httpCreated

	f, err := FindAgentNamed(ctx, agents, AtlasAgentName)
	if err != nil {
		return res, err
	}
	httpRefs := make(map[string]string, len(httpSelections))
	for _, sel := range httpSelections {
		httpRefs[sel.RemoteName] = sel.ServerID + "/" + sel.RemoteName
	}
	cfg, err := atlasConfig(res.MCPServerID, httpRefs)
	if err != nil {
		return res, err
	}
	if f.ID != "" {
		// An agent without version detail cannot be judged: leave it.
		if f.Version == nil || (pinsMatch(f, selections) && configMatches(f, cfg)) {
			res.AgentID = f.ID
			return res, nil
		}
	}
	// A stale or drifted agent (another server, an edited HTTP tool, a missing pin,
	// an older prompt set) would not run this demo. Create the replacement first,
	// so a failed publish leaves the existing callable agent in place; archive the
	// old one only once the new one exists.
	created, err := agents.Create(ctx, agent.CreateRequest{Name: AtlasAgentName, Description: "Travel concierge: eight steps and thirty-two handoffs across flights, stays, ground transport, payments, loyalty, disruptions and a human handoff.", Config: cfg, MCPTools: selections})
	if err != nil {
		return res, fmt.Errorf("create agent: %w", err)
	}
	if f.ID != "" {
		if err := agents.Archive(ctx, f.ID); err != nil {
			return res, fmt.Errorf("archive replaced agent %s: %w", f.ID, err)
		}
	}
	res.AgentID, res.AgentCreated = created.ID, true
	return res, nil
}

// installAtlasHTTPTools creates or reuses the demo's two HTTP tools and returns
// their pins. A name already used with another definition is a conflict, not a
// silent edit: published agents pin a definition by hash.
func installAtlasHTTPTools(ctx context.Context, tools HTTPTools, base string) ([]agent.ToolSelection, int, int, error) {
	views, err := tools.List(ctx)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("list HTTP tools: %w", err)
	}
	have := make(map[string]httptool.View, len(views))
	for _, v := range views {
		have[v.Name] = v
	}
	out := make([]agent.ToolSelection, 0, len(atlasmcp.HTTPTools))
	created := 0
	for _, spec := range atlasmcp.HTTPTools {
		want, err := (httptool.Definition{Name: spec.Name, Description: spec.Description, Method: spec.Method, URL: base + spec.Path, InputSchema: spec.InputSchema}).Normalize(ctx, httptool.Options{AllowLocalHTTP: true})
		if err != nil {
			return nil, 0, 0, fmt.Errorf("HTTP tool %s: %w", spec.Name, err)
		}
		v, ok := have[spec.Name]
		switch {
		case !ok:
			v, err = tools.Create(ctx, want)
			if err != nil {
				// Two instances can create the same demo at once (the singleflight is
				// per process). The loser re-reads the winner's tool instead of 502ing.
				if !errors.Is(err, storage.ErrHTTPToolNameTaken) {
					return nil, 0, 0, fmt.Errorf("create HTTP tool %s: %w", spec.Name, err)
				}
				if v, ok, err = findHTTPTool(ctx, tools, spec.Name); err != nil {
					return nil, 0, 0, err
				}
				if !ok {
					return nil, 0, 0, fmt.Errorf("create HTTP tool %s: the name is taken but the tool cannot be read", spec.Name)
				}
				if !sameHTTPDefinition(v.Definition, want) {
					return nil, 0, 0, fmt.Errorf("HTTP tool %q exists with another definition; delete it or set ATLAS_DEMO_URL to match", spec.Name)
				}
			} else {
				created++
			}
		case !sameHTTPDefinition(v.Definition, want):
			return nil, 0, 0, fmt.Errorf("HTTP tool %q exists with another definition; delete it or set ATLAS_DEMO_URL to match", spec.Name)
		case !v.Enabled:
			v, err = tools.SetEnabled(ctx, v.ID, true)
			if err != nil {
				return nil, 0, 0, fmt.Errorf("enable HTTP tool %s: %w", spec.Name, err)
			}
		}
		out = append(out, agent.ToolSelection{ServerID: httptool.RefPrefix + v.ID, RemoteName: v.Name, SchemaHash: v.Hash, Required: true})
	}
	return out, len(out), created, nil
}

// configMatches reports whether the agent's stored config is the one this build
// would publish, so a reload also picks up prompt, handoff and prompt changes.
func configMatches(f agent.Agent, cfg json.RawMessage) bool {
	if f.Version == nil || len(f.Version.Config) == 0 {
		return false
	}
	stored, err := storage.CanonicalJSON(f.Version.Config)
	if err != nil {
		return false
	}
	want, err := storage.CanonicalJSON(cfg)
	if err != nil {
		return false
	}
	return string(stored) == string(want)
}

// sameHTTPDefinition compares the parts the installer sets, on the normalized
// forms the service stores, so a same-URL definition edited elsewhere is a
// conflict instead of being silently adopted.
func sameHTTPDefinition(a, b httptool.Definition) bool {
	schema := func(d httptool.Definition) string {
		out, err := storage.CanonicalJSON(d.InputSchema)
		if err != nil {
			return string(d.InputSchema)
		}
		return string(out)
	}
	return a.Name == b.Name && a.Description == b.Description && a.Method == b.Method &&
		a.URL == b.URL && a.TimeoutMS == b.TimeoutMS && a.MaxResponseBytes == b.MaxResponseBytes &&
		schema(a) == schema(b) && slices.Equal(a.Pick, b.Pick)
}

// findHTTPTool re-reads one HTTP tool by name after a lost create race.
func findHTTPTool(ctx context.Context, tools HTTPTools, name string) (httptool.View, bool, error) {
	views, err := tools.List(ctx)
	if err != nil {
		return httptool.View{}, false, fmt.Errorf("list HTTP tools: %w", err)
	}
	for _, v := range views {
		if v.Name == name {
			return v, true, nil
		}
	}
	return httptool.View{}, false, nil
}

// pinsMatch reports whether the agent's pins that belong to this demo are
// exactly the pins this install would publish. Pins of other servers are
// ignored: an admin may add a tool from elsewhere without making the demo
// stale. An agent without version detail cannot be judged, so it matches.
func pinsMatch(f agent.Agent, desired []agent.ToolSelection) bool {
	if f.Version == nil {
		return true
	}
	demo := make(map[string]bool, len(desired))
	for _, d := range desired {
		demo[d.ServerID] = true
	}
	got := make(map[string]agent.ToolSelection, len(desired))
	for _, t := range f.Version.MCPTools {
		if demo[t.ServerID] {
			got[t.ServerID+"\x00"+t.RemoteName] = t
		}
	}
	if len(got) != len(desired) {
		return false
	}
	for _, d := range desired {
		t, ok := got[d.ServerID+"\x00"+d.RemoteName]
		if !ok || t.SchemaHash != d.SchemaHash || t.Required != d.Required {
			return false
		}
	}
	return true
}

// normaliseBase returns the demo origin without a trailing slash. Only an
// origin (with an optional path prefix) is accepted: a query or fragment would
// make the derived MCP endpoint and HTTP tool URLs ambiguous.
func normaliseBase(raw string) (string, error) {
	u, err := url.ParseRequestURI(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("invalid base URL %q", raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("base URL %q must be http or https", raw)
	}
	if strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/mcp") {
		return "", fmt.Errorf("base URL %q is the MCP endpoint; use its origin (without /mcp)", raw)
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// atlasStep is a step with a canvas position, used by the eight-step layout.
func atlasStep(id, name string, endCall bool, tools []string, config stage, x, y float64) stage {
	s := step(id, name, endCall, tools, config, x)
	s["position"] = stage{"x": x, "y": y}
	return s
}

// atlasConfig builds the eight-step agent. serverID is the installed MCP
// server; httpRefs maps each HTTP tool name to its "<http id>/<name>" step
// reference.
func atlasConfig(serverID string, httpRefs map[string]string) (json.RawMessage, error) {
	start := stage{
		"stt": stage{"provider": "google", "model": "chirp_2", "language": "en-US"},
		"llm": stage{"provider": "google", "model": "gemini-2.5-flash", "system_prompt": conciergePrompt},
		"tts": stage{"provider": "elevenlabs", "voice": "21m00Tcm4TlvDq8ikWAM"},
		// A long, tool-heavy call: keep more history than the default 20 messages.
		"context":                   stage{"max_messages": 30, "max_tokens": 6000},
		"max_call_duration_seconds": 600,
		// The disruption rebooking runs in the background: the model gets an ack and the result arrives later.
		"async_tools": refs(serverID, "rebook_disruption"),
	}
	flights := stage{
		"llm":      stage{"provider": "xai", "model": "grok-4.20-0309-non-reasoning", "system_prompt": flightsPrompt, "max_parallel": 3},
		"tts":      stage{"provider": "xai", "voice": "eve"},
		"greeting": "You're through to Atlas flights. How can I help with your trip?",
	}
	stays := stage{
		"llm": stage{"provider": "google", "model": "gemini-2.5-flash", "system_prompt": staysPrompt},
		"tts": stage{"provider": "google", "voice": "en-US-Chirp3-HD-Aoede"},
	}
	ground := stage{
		"llm":      stage{"provider": "google", "model": "gemini-2.5-flash", "system_prompt": groundPrompt},
		"tts":      stage{"provider": "elevenlabs", "voice": "EXAVITQu4vr4xnSDxMaL"},
		"greeting": "You're through to Atlas ground transport. Where are you heading?",
	}
	payments := stage{
		"llm": stage{"provider": "xai", "model": "grok-4.20-0309-non-reasoning", "system_prompt": paymentsPrompt},
		"tts": stage{"provider": "elevenlabs", "voice": "21m00Tcm4TlvDq8ikWAM"},
	}
	loyalty := stage{
		"llm": stage{"provider": "google", "model": "gemini-2.5-flash", "system_prompt": loyaltyPrompt},
		"tts": stage{"provider": "google", "voice": "en-US-Chirp3-HD-Aoede"},
	}
	disruptions := stage{
		"llm": stage{"provider": "xai", "model": "grok-4.20-0309-non-reasoning", "system_prompt": disruptionsPrompt, "max_parallel": 3},
		"tts": stage{"provider": "xai", "voice": "eve"},
	}
	human := stage{
		"llm":      stage{"provider": "google", "model": "gemini-2.5-flash", "system_prompt": humanPrompt},
		"tts":      stage{"provider": "elevenlabs", "voice": "21m00Tcm4TlvDq8ikWAM"},
		"greeting": "I'm sorry this needs a person. I'll take a few details and a teammate will call you back.",
	}

	cfg := stage{
		"start": "concierge",
		"steps": []stage{
			atlasStep("concierge", "Concierge", true, refs(serverID, "get_traveller", "list_bookings"), start, 0, 260),
			atlasStep("flights", "Flights", true, refs(serverID, "search_flights", "book_flight", "change_flight", "cancel_booking"), flights, 380, 0),
			atlasStep("stays", "Stays", true, append(refs(serverID, "search_stays", "book_stay", "cancel_booking"), httpRefs["weather_forecast"]), stays, 380, 180),
			atlasStep("ground", "Ground transport", true, refs(serverID, "search_transfers", "book_transfer"), ground, 380, 360),
			atlasStep("payments", "Payments", true, refs(serverID, "list_payment_methods", "charge_booking", "issue_refund"), payments, 760, 0),
			atlasStep("loyalty", "Loyalty", true, refs(serverID, "get_loyalty", "apply_points"), loyalty, 760, 180),
			atlasStep("disruptions", "Disruptions", true, append(refs(serverID, "list_bookings", "report_disruption", "rebook_disruption"), httpRefs["travel_advisory"]), disruptions, 760, 360),
			atlasStep("human", "Human handoff", true, refs(serverID, "create_callback"), human, 1140, 260),
		},
		"handoffs": atlasHandoffs(),
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("encode Atlas agent: %w", err)
	}
	return raw, nil
}

// atlasHandoffs is the thirty-two-edge handoff graph: at the platform maximum,
// every ordered pair unique and every step reachable from the concierge.
func atlasHandoffs() []agent.Handoff {
	// The order groups each source step's outgoing handoffs, which is also the
	// order the model sees the transfer tools in.
	out := []agent.Handoff{
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
	return out
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
