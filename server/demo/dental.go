// Package demo installs the Smile Dental front-desk demo: the scheduling MCP server and a
// three-step flow (front desk -> new patient | returning patient) on three provider stacks.
// Installing is idempotent: it adds what is missing and replaces a flow left pointing at a gone MCP server.
package demo

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"govoice/server/flow"
	"govoice/server/mcp"
	"govoice/server/mcp/dentalmcp"
	"govoice/server/storage"
)

const (
	ServerName = "Smile Dental scheduling"
	FlowName   = "Smile Dental front desk"
	// AuthEnv is the env var holding the MCP server's bearer token (never stored).
	AuthEnv = "MCP_TOKEN_DENTAL"
)

// Servers and Flows are what the installer needs of the MCP and flow services.
type Servers interface {
	List(ctx context.Context) ([]storage.MCPServer, error)
	Create(ctx context.Context, cfg mcp.ServerConfig) (storage.MCPServer, error)
	Enable(ctx context.Context, id string) error
	Refresh(ctx context.Context, id string) (storage.MCPDiscovery, error)
}

type Flows interface {
	List(ctx context.Context, limit, offset int) ([]flow.Flow, error)
	Create(ctx context.Context, req flow.CreateRequest) (flow.Flow, error)
	Archive(ctx context.Context, id string) error
}

type Result struct {
	MCPServerID string `json:"mcp_server_id"`
	MCPCreated  bool   `json:"mcp_created"`
	Tools       int    `json:"tools"`
	FlowID      string `json:"flow_id"`
	FlowCreated bool   `json:"flow_created"`
}

// InstallDental adds the MCP server (endpoint is its /mcp URL) and the flow unless they exist.
func InstallDental(ctx context.Context, servers Servers, flows Flows, endpoint string) (Result, error) {
	var res Result
	endpoint, err := normaliseEndpoint(endpoint)
	if err != nil {
		return res, fmt.Errorf("DENTAL_MCP_URL: %w", err)
	}
	existing, err := servers.List(ctx)
	if err != nil {
		return res, fmt.Errorf("list MCP servers: %w", err)
	}
	var enabled bool
	for _, s := range existing {
		if s.Name == ServerName && !s.PendingDelete {
			if stored, err := normaliseEndpoint(s.Endpoint); err != nil || stored != endpoint {
				return res, fmt.Errorf("MCP server %q exists with endpoint %s; delete it or set DENTAL_MCP_URL to match", ServerName, s.Endpoint)
			}
			res.MCPServerID = s.ID
			enabled = s.Enabled
		}
	}
	if res.MCPServerID != "" && !enabled {
		// Refresh refuses disabled servers, so a previously disabled demo server is switched back on.
		if err := servers.Enable(ctx, res.MCPServerID); err != nil {
			return res, fmt.Errorf("enable MCP server: %w", err)
		}
	}
	if res.MCPServerID == "" {
		m, err := servers.Create(ctx, mcp.ServerConfig{Name: ServerName, Description: "Fake patients and appointments for the dentistry demo.", Endpoint: endpoint, AuthEnv: AuthEnv, Enabled: true})
		if err != nil {
			return res, fmt.Errorf("create MCP server: %w", err)
		}
		res.MCPServerID, res.MCPCreated = m.ID, true
	}
	disc, err := servers.Refresh(ctx, res.MCPServerID)
	if err != nil {
		return res, fmt.Errorf("discover dental tools: %w", err)
	}
	have := map[string]storage.MCPTool{}
	for _, t := range disc.Tools {
		have[t.RemoteName] = t
	}
	selections := make([]flow.ToolSelection, 0, len(dentalmcp.ToolNames))
	for _, name := range dentalmcp.ToolNames {
		t, ok := have[name]
		if !ok {
			return res, fmt.Errorf("MCP server %q does not offer the %s tool", ServerName, name)
		}
		selections = append(selections, flow.ToolSelection{ServerID: res.MCPServerID, RemoteName: name, SchemaHash: t.SchemaHash, Required: true})
	}
	res.Tools = len(selections)

	f, err := FindFlow(ctx, flows)
	if err != nil {
		return res, err
	}
	if f.ID != "" && !pointsAt(f, res.MCPServerID) {
		if err := flows.Archive(ctx, f.ID); err != nil { // a stale flow could never run its tools
			return res, fmt.Errorf("archive stale flow: %w", err)
		}
		f = flow.Flow{}
	}
	if f.ID != "" {
		res.FlowID = f.ID
		return res, nil
	}
	cfg, err := dentalConfig(res.MCPServerID)
	if err != nil {
		return res, err
	}
	created, err := flows.Create(ctx, flow.CreateRequest{Name: FlowName, Description: "Front desk takes the name, then hands new and returning patients to the right agent.", Config: cfg, MCPTools: selections})
	if err != nil {
		return res, fmt.Errorf("create flow: %w", err)
	}
	res.FlowID, res.FlowCreated = created.ID, true
	return res, nil
}

// FindFlow returns the live dental flow, or the zero Flow when it is not installed.
func FindFlow(ctx context.Context, flows Flows) (flow.Flow, error) {
	const page = 100 // the store's maximum page size
	for offset := 0; ; offset += page {
		batch, err := flows.List(ctx, page, offset)
		if err != nil {
			return flow.Flow{}, fmt.Errorf("list flows: %w", err)
		}
		for _, f := range batch {
			if f.Name == FlowName && !f.Archived {
				return f, nil
			}
		}
		if len(batch) < page {
			return flow.Flow{}, nil
		}
	}
}

// normaliseEndpoint puts a URL in the form the MCP service stores (it parses with
// url.ParseRequestURI), so the configured and stored endpoints compare equal.
func normaliseEndpoint(raw string) (string, error) {
	u, err := url.ParseRequestURI(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("invalid endpoint %q", raw)
	}
	return u.String(), nil
}

// pointsAt reports whether the flow selects any tool of serverID. Extra tools an admin
// added from other servers do not make it stale. A flow without version detail cannot be
// judged and is left alone.
func pointsAt(f flow.Flow, serverID string) bool {
	if f.Version == nil {
		return true
	}
	for _, t := range f.Version.MCPTools {
		if t.ServerID == serverID {
			return true
		}
	}
	return false
}

type stage = map[string]any

func step(id, name string, endCall bool, tools []string, config stage, x float64) stage {
	s := stage{"id": id, "name": name, "end_call": endCall, "config": config, "position": stage{"x": x, "y": 0}}
	if len(tools) > 0 {
		s["tools"] = tools
	}
	return s
}

func refs(serverID string, names ...string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = serverID + "/" + n
	}
	return out
}

func dentalConfig(serverID string) (json.RawMessage, error) {
	front := stage{
		"stt": stage{"provider": "google", "model": "chirp_2", "language": "en-US"},
		"llm": stage{"provider": "google", "model": "gemini-2.5-flash", "system_prompt": frontPrompt},
		"tts": stage{"provider": "elevenlabs", "voice": "21m00Tcm4TlvDq8ikWAM"},
	}
	newPatient := stage{
		"llm": stage{"provider": "xai", "model": "grok-4.20-0309-non-reasoning", "system_prompt": newPatientPrompt},
		"tts": stage{"provider": "xai", "voice": "eve"},
	}
	returning := stage{
		"llm": stage{"provider": "google", "model": "gemini-2.5-flash", "system_prompt": returningPrompt},
		"tts": stage{"provider": "google", "voice": "en-US-Chirp3-HD-Aoede"},
	}
	cfg := stage{
		"start": "front",
		"steps": []stage{
			step("front", "Front desk", true, nil, front, 0),
			step("new", "New patient", true, refs(serverID, "register_patient", "find_slots", "book_appointment"), newPatient, 320),
			step("returning", "Returning patient", true, refs(serverID, "find_patient", "list_appointments", "find_slots", "reschedule_appointment", "cancel_appointment"), returning, 320),
		},
		"routes": []flow.Route{
			{ID: "to-new", From: "front", To: "new", Condition: "The caller has never been a patient at Smile Dental and wants to register or book their first appointment."},
			{ID: "to-returning", From: "front", To: "returning", Condition: "The caller is already a patient and wants to change, move or cancel an appointment."},
		},
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("encode dental flow: %w", err)
	}
	return raw, nil
}

const frontPrompt = `You are the front desk voice assistant for Smile Dental, a dental practice, on a phone call. Keep every reply to one or two short sentences. No lists, no markdown.

Greet the caller warmly, say the practice name, and ask for their name. Once you have it, ask whether they have been a patient at Smile Dental before.

- If it is their first visit, or they want to register or book their first appointment: call transfer_to_new_patient with a note containing their name and what they want.
- If they are already a patient and want to change, move or cancel an appointment: call transfer_to_returning_patient with a note containing their name and what they want.

Opening hours, if asked: Monday to Friday, 9am to 5pm, closed at weekends. The practice is at 12 Elm Street. If you cannot help with something, apologise briefly, say a team member will call back, say goodbye and call end_call.

When you transfer, say one short sentence that you are connecting them, then stop talking.`

const newPatientPrompt = `You are the new-patient assistant at Smile Dental, on a phone call. Keep every reply to one or two short sentences. No lists, no markdown. Say dates the way a person would.

The front desk has already taken the caller's name and passed it to you in a note. Greet the caller by first name, welcome them to the practice and say you will get them registered. Do not ask for their name again.

1. Ask for a phone number, then call register_patient with their full name and phone number.
2. Ask what the visit is for (checkup, cleaning, filling or consultation) and roughly when suits them.
3. Call find_slots and offer at most three options out loud.
4. When they choose, call book_appointment with the patient id from register_patient, the slot id and the kind.
5. Confirm the day, time and kind in one sentence and ask if there is anything else. If not, say a warm goodbye and call end_call.

Use only what the tools return; never invent slots or ids. If a tool fails, apologise and offer another slot.`

const returningPrompt = `You are the returning-patient assistant at Smile Dental, on a phone call. Keep every reply to one or two short sentences. No lists, no markdown. Say dates the way a person would.

The front desk has already taken the caller's name and passed it to you in a note. Greet the caller with "Welcome back" and their first name. Do not ask for their name again.

1. Call find_patient with the name from the note. If they are not found, ask them to spell their name and try once more; if still not found, apologise, say a team member will call back, say goodbye and call end_call.
2. Call list_appointments with their patient id and tell them what they have booked.
3. To move an appointment: call find_slots, offer at most three options out loud, then call reschedule_appointment with the appointment id and the slot id they choose.
4. To cancel: ask them to confirm, then call cancel_appointment.
5. Confirm what changed in one sentence and ask if there is anything else. If not, say a warm goodbye and call end_call.

Use only what the tools return; never invent slots or ids. If a tool fails, apologise and offer another slot.`
