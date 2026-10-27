package dental_test

import (
	"slices"
	"strings"
	"testing"

	"govoice/demos/dental"
	"govoice/demos/dental/mcp/dentalmcp"
	"govoice/demos/spec"
	"govoice/server/agent"
)

const testServerID = "srv-dental"

// resolution is the fake seed result every test builds the agent from.
func resolution() spec.Resolution {
	return spec.Resolution{ServerIDs: map[string]string{"scheduling": testServerID}}
}

func build(t *testing.T, r spec.Resolution) (spec.AgentSpec, agent.Definition) {
	t.Helper()
	a, err := (dental.Demo{}).Agent(r)
	if err != nil {
		t.Fatalf("Agent: %v", err)
	}
	g, err := agent.Decode(a.Config)
	if err != nil {
		t.Fatalf("the demo config must decode as an agent: %v", err)
	}
	return a, g
}

func TestDemoMetadata(t *testing.T) {
	d := dental.Demo{}
	if got := d.Name(); got != dental.Name {
		t.Fatalf("Name() = %q, want %q", got, dental.Name)
	}
	servers := d.MCPServers()
	if len(servers) != 1 {
		t.Fatalf("MCPServers() returned %d servers, want 1", len(servers))
	}
	s := servers[0]
	if s.Logical != "scheduling" || s.Name != dental.ServerName {
		t.Errorf("server = logical %q name %q", s.Logical, s.Name)
	}
	if s.EndpointPath != "/mcp" || s.TokenEnv != "DENTAL_MCP_TOKEN" || s.SecretName != "demo-dental-mcp" {
		t.Errorf("server endpoint/token/secret = %q/%q/%q", s.EndpointPath, s.TokenEnv, s.SecretName)
	}
	if s.Description == "" {
		t.Error("the MCP server needs a description")
	}
	if !slices.Equal(s.Tools, dentalmcp.ToolNames) {
		t.Errorf("server tools = %v, want %v", s.Tools, dentalmcp.ToolNames)
	}
	if len(s.Tools) != 7 {
		t.Errorf("the demo pins %d MCP tools, want 7", len(s.Tools))
	}
	if tools := d.HTTPTools(); tools != nil {
		t.Errorf("HTTPTools() = %v, want none", tools)
	}
}

func TestAgentIsAValidThreeStepDefinition(t *testing.T) {
	a, g := build(t, resolution())
	if a.Name != dental.AgentName {
		t.Fatalf("agent name = %q, want %q", a.Name, dental.AgentName)
	}
	if a.Description == "" {
		t.Fatal("the agent needs a description")
	}
	if len(g.Steps) != 3 || len(g.Handoffs) != 2 {
		t.Fatalf("config has %d steps and %d handoffs, want 3 and 2", len(g.Steps), len(g.Handoffs))
	}
	if g.Start != "front" {
		t.Errorf("start = %q, want front", g.Start)
	}
	for _, s := range g.Steps {
		if !s.EndCall {
			t.Errorf("step %q must be able to end the call", s.Name)
		}
	}
	// The three provider stacks are part of the demo: keep them from drifting.
	front, _ := g.Step("front")
	if front.Config.STT.Provider != "google" || front.Config.LLM.Provider != "google" || front.Config.TTS.Provider != "elevenlabs" {
		t.Errorf("front stack = %+v", front.Config)
	}
	newPatient, _ := g.Step("new")
	returning, _ := g.Step("returning")
	if newPatient.Config.LLM.Provider != "xai" || newPatient.Config.TTS.Provider != "xai" {
		t.Errorf("new-patient stack = %+v", newPatient.Config)
	}
	if returning.Config.LLM.Provider != "google" || returning.Config.TTS.Provider != "google" {
		t.Errorf("returning-patient stack = %+v", returning.Config)
	}
	if newPatient.Config.STT.Provider != "" || returning.Config.STT.Provider != "" {
		t.Error("the STT stage is call-level: only the start step may set it")
	}
}

func TestStepToolsAreTheSevenSchedulingPins(t *testing.T) {
	_, g := build(t, resolution())
	expected := make(map[string]bool, len(dentalmcp.ToolNames))
	for _, ref := range spec.Refs(testServerID, dentalmcp.ToolNames...) {
		expected[ref] = true
	}
	seen := make(map[string]bool, len(expected))
	for _, s := range g.Steps {
		for _, ref := range s.Tools {
			if !expected[ref] {
				t.Errorf("step %q exposes %q, which is not a scheduling pin", s.ID, ref)
			}
			seen[ref] = true
		}
	}
	if len(seen) != len(expected) {
		t.Errorf("the steps expose %d of the %d scheduling pins: %v", len(seen), len(expected), expected)
	}

	// The split is part of the demo the prompts are tuned to.
	want := map[string][]string{
		"front":     nil,
		"new":       spec.Refs(testServerID, "register_patient", "find_slots", "book_appointment"),
		"returning": spec.Refs(testServerID, "find_patient", "list_appointments", "find_slots", "reschedule_appointment", "cancel_appointment"),
	}
	for _, s := range g.Steps {
		if !slices.Equal(s.Tools, want[s.ID]) {
			t.Errorf("step %q tools = %v, want %v", s.ID, s.Tools, want[s.ID])
		}
	}
}

func TestAgentRefusesAnUnresolvedServer(t *testing.T) {
	_, err := (dental.Demo{}).Agent(spec.Resolution{})
	if err == nil || !strings.Contains(err.Error(), "scheduling") {
		t.Fatalf("Agent without a scheduling server id = %v, want an error naming the server", err)
	}
}
