// Package dental defines the Smile Dental front-desk demo: the scheduling MCP
// server and a three-step agent (front desk -> new patient | returning patient)
// on three provider stacks.
package dental

import (
	"encoding/json"
	"fmt"

	"govoice/demos/dental/mcp/dentalmcp"
	"govoice/demos/spec"
)

const (
	// Name is the demo's registry name.
	Name = "dental"
	// ServerName is the MCP server the demo registers.
	ServerName = "Smile Dental scheduling"
	// AgentName is the demo agent.
	AgentName = "Smile Dental front desk"

	serverDescription = "Fake patients and appointments for the dentistry demo."
	agentDescription  = "Front desk takes the name, then hands new and returning patients to the right step."
)

// Demo is the Smile Dental demo definition.
type Demo struct{}

// Name implements spec.Demo.
func (Demo) Name() string { return Name }

// MCPServers implements spec.Demo.
func (Demo) MCPServers() []spec.MCPServerSpec {
	return []spec.MCPServerSpec{{
		Logical:      "scheduling",
		Name:         ServerName,
		Description:  serverDescription,
		EndpointPath: "/mcp",
		TokenEnv:     "DENTAL_MCP_TOKEN",
		SecretName:   "demo-dental-mcp",
		Tools:        dentalmcp.ToolNames,
	}}
}

// HTTPTools implements spec.Demo: the dental demo has none.
func (Demo) HTTPTools() []spec.HTTPToolSpec { return nil }

// Agent implements spec.Demo. The config is exactly the one the demo has always
// published: three steps, two handoffs, and the scheduling server's seven tools
// split over the two patient steps.
func (Demo) Agent(r spec.Resolution) (spec.AgentSpec, error) {
	serverID := r.ServerIDs["scheduling"]
	if serverID == "" {
		return spec.AgentSpec{}, fmt.Errorf("%s: no server id resolved for the scheduling MCP server", Name)
	}
	cfg, err := config(serverID)
	if err != nil {
		return spec.AgentSpec{}, err
	}
	return spec.AgentSpec{Name: AgentName, Description: agentDescription, Config: cfg}, nil
}

func config(serverID string) (json.RawMessage, error) {
	front := spec.Stage{
		"stt": spec.Stage{"provider": "google", "model": "chirp_2", "language": "en-US"},
		"llm": spec.Stage{"provider": "google", "model": "gemini-2.5-flash", "system_prompt": frontPrompt},
		"tts": spec.Stage{"provider": "elevenlabs", "voice": "21m00Tcm4TlvDq8ikWAM"},
	}
	newPatient := spec.Stage{
		"llm": spec.Stage{"provider": "xai", "model": "grok-4.20-0309-non-reasoning", "system_prompt": newPatientPrompt},
		"tts": spec.Stage{"provider": "xai", "voice": "eve"},
	}
	returning := spec.Stage{
		"llm": spec.Stage{"provider": "google", "model": "gemini-2.5-flash", "system_prompt": returningPrompt},
		"tts": spec.Stage{"provider": "google", "voice": "en-US-Chirp3-HD-Aoede"},
	}
	cfg := spec.Stage{
		"start": "front",
		"steps": []spec.Stage{
			spec.Step("front", "Front desk", true, nil, front, 0, 0),
			spec.Step("new", "New patient", true, spec.Refs(serverID, "register_patient", "find_slots", "book_appointment"), newPatient, 320, 0),
			spec.Step("returning", "Returning patient", true, spec.Refs(serverID, "find_patient", "list_appointments", "find_slots", "reschedule_appointment", "cancel_appointment"), returning, 320, 0),
		},
		"handoffs": []spec.Handoff{
			{ID: "to-new", From: "front", To: "new", Condition: "The caller has never been a patient at Smile Dental and wants to register or book their first appointment."},
			{ID: "to-returning", From: "front", To: "returning", Condition: "The caller is already a patient and wants to change, move or cancel an appointment."},
		},
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("encode dental agent: %w", err)
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
