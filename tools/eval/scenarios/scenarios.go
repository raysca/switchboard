// Package scenarios defines the contract specifications and invariants for the
// seven V2 vertical product scenarios (FW/SV/UI).
package scenarios

import (
	"fmt"
	"time"
)

// Scenario describes one vertical end-to-end product integration scenario.
type Scenario struct {
	ID          string        `json:"id"`
	Number      int           `json:"number"`
	Name        string        `json:"name"`
	Description string        `json:"description"`
	Invariants  []string      `json:"invariants"`
	Steps       []string      `json:"steps"`
	Timeout     time.Duration `json:"timeout"`
}

// SevenScenarios returns the canonical definitions for all seven vertical product scenarios.
func SevenScenarios() []Scenario {
	return []Scenario{
		{
			ID:          "fresh_install_onboarding",
			Number:      1,
			Name:        "Fresh Install & Onboarding",
			Description: "Clean startup on empty database with zero credentials and agents; onboarding configures provider credentials, creates first agent, evaluates readiness, and exposes immutable v1 version.",
			Invariants: []string{
				"Database migrations execute automatically on fresh start",
				"Initial state has exactly zero credentials and zero agents",
				"Provider credential creation succeeds and exposes masked secret",
				"Created agent receives UUIDv7 identity, contiguous version 1, and SHA-256 config hash",
				"Agent readiness evaluates to ready with zero blocking reasons",
			},
			Steps: []string{
				"initialize fresh SQLite database",
				"verify zero existing credentials and zero agents",
				"configure provider credentials via management service",
				"create first agent with STT/LLM/TTS configuration",
				"verify agent created with version 1, valid config hash, and ready state",
			},
			Timeout: 5 * time.Second,
		},
		{
			ID:          "mixed_providers",
			Number:      2,
			Name:        "Mixed Providers",
			Description: "Composite conversational pipeline combining simulated Google STT candidate, xAI LLM completion, and TTS output; session runs with immutable configuration hash pinning.",
			Invariants: []string{
				"Pipeline integrates heterogeneous providers across STT, LLM, and TTS boundaries",
				"Session pins to immutable agent snapshot and configuration hash",
				"User transcript from STT commits to conversation context",
				"LLM completion produces synthesis units and triggers TTS audio generation",
				"Audio output delivered to transport with matching turn identity and generation",
			},
			Steps: []string{
				"create agent configured with Google STT, xAI LLM, and mock TTS",
				"start session pinning immutable version and configuration hash",
				"ingest input audio frame and simulate Google STT candidate transcription",
				"route committed transcript to xAI LLM for completion",
				"synthesize assistant speech via TTS and verify transport output",
			},
			Timeout: 5 * time.Second,
		},
		{
			ID:          "mcp_tool_lifecycle",
			Number:      3,
			Name:        "MCP Tool Lifecycle",
			Description: "Remote streamable HTTP server discovery, immutable schema hashing, session startup runtime wrapper capture, ToolLoop execution, and turn-correlated trace spans.",
			Invariants: []string{
				"MCP discovery creates immutable tool snapshot with SHA-256 schema hash",
				"Session startup captures runtime MCP wrapper before persisting session",
				"ToolLoop execution succeeds through session executor",
				"Trace spans and events correlate by session ID, turn ID, and tool call ID",
			},
			Steps: []string{
				"register remote MCP server and perform discovery refresh",
				"verify discovery snapshot records tool with SHA-256 schema hash",
				"attach MCP tool selection to agent version",
				"start session and capture runtime tool wrapper",
				"execute ToolLoop with tool call and verify success result",
				"verify trace spans correlate to turn and session identity",
			},
			Timeout: 5 * time.Second,
		},
		{
			ID:          "immutable_agent_versioning",
			Number:      4,
			Name:        "Immutable Agent Versioning",
			Description: "Session starts on v1; agent config updated to v2 with different system prompt/voice; active session continues running on v1 snapshot while new sessions use v2.",
			Invariants: []string{
				"Agent configuration update publishes contiguous version v2 with distinct config hash",
				"Active session pinned to v1 remains completely unaffected by v2 publication",
				"Historical session query continues to resolve v1 configuration snapshot",
				"New session startup resolves to v2 configuration snapshot",
			},
			Steps: []string{
				"create agent v1 with initial prompt and voice configuration",
				"start active session pinned to agent v1",
				"update agent configuration to publish version 2 with new prompt/voice",
				"verify agent current version is 2 with distinct config hash",
				"verify active session continues running on v1 snapshot and reports v1 hash",
				"start second session and verify it resolves to v2 snapshot and v2 hash",
			},
			Timeout: 5 * time.Second,
		},
		{
			ID:          "mcp_deletion_safety",
			Number:      5,
			Name:        "MCP Deletion Safety",
			Description: "Deleting referenced MCP server is rejected with 409 conflict; confirmed detach-and-delete atomically publishes v2 agent without MCP tool, disables server, drains active sessions safely, and deletes server while preserving historical session snapshots.",
			Invariants: []string{
				"Unconfirmed delete of referenced MCP server is rejected with ErrMCPReferenced (HTTP 409)",
				"Confirmed detach-and-delete atomically creates new agent version without MCP tools",
				"Server is disabled while active sessions hold captured tool handles",
				"Server deletion completes only after active sessions finish draining",
				"Historical session records retain original tool snapshot and schema hash evidence",
			},
			Steps: []string{
				"create MCP server and attach tool to agent",
				"start active session capturing runtime tool wrapper",
				"attempt direct server deletion and verify 409 conflict rejection",
				"issue confirmed detach-and-delete to publish v2 agent without tool",
				"verify server remains disabled during active session drain",
				"close active session to complete drain and verify server record deletion",
				"verify historical session query preserves original MCP tool snapshot",
			},
			Timeout: 5 * time.Second,
		},
		{
			ID:          "repeated_barge_in",
			Number:      6,
			Name:        "Repeated Barge-in & Interruption",
			Description: "Rapid consecutive interruptions during assistant speech generation properly cancel output, flush audio pacer queues, advance cancellation generations, and preserve conversational turn order.",
			Invariants: []string{
				"Interruption cancels active generation output immediately",
				"Paced output writer flushes queued frames without leaking old generation audio",
				"Rapid consecutive interruptions advance generation fence monotonically",
				"Stale audio frames arriving after interruption are dropped before playback",
				"Conversational turn order remains strictly monotonic across interruptions",
			},
			Steps: []string{
				"start assistant speech generation for turn 1 (generation 1)",
				"enqueue audio frames into paced output writer",
				"trigger rapid barge-in interruption 1, advancing generation to 2",
				"verify paced writer flushes queued generation 1 frames",
				"start assistant speech generation for turn 2 (generation 2)",
				"trigger immediate rapid barge-in interruption 2, advancing generation to 3",
				"verify paced writer flushes queued generation 2 frames",
				"deliver uninterrupted turn 3 (generation 3) audio to completion",
				"verify zero stale audio leak and correct turn sequence",
			},
			Timeout: 5 * time.Second,
		},
		{
			ID:          "fault_isolation",
			Number:      7,
			Name:        "Fault Isolation",
			Description: "Injected trace repository write failure, remote MCP outage, or provider timeout DOES NOT crash the voice session, panic the runtime, or terminate the active voice pipeline.",
			Invariants: []string{
				"Trace store write failures/outages drop safely without blocking or failing the session",
				"Remote MCP server outage/timeout produces safe ToolFailed result without pipeline crash",
				"Provider timeout is handled cleanly and active session remains connected for next turn",
				"Concurrent sessions remain isolated: failure in one does not affect another",
			},
			Steps: []string{
				"inject trace storage write failure and verify session continues processing",
				"inject remote MCP outage and verify ToolFailed returned safely without session crash",
				"inject provider timeout and verify pipeline remains alive and caller connected",
				"execute subsequent turn on same session successfully",
				"verify concurrent session remains healthy and unaffected",
			},
			Timeout: 5 * time.Second,
		},
	}
}

// ExtendedParityScenarios returns the Phase 10 P2 extended parity scenarios (8, 9, 10).
func ExtendedParityScenarios() []Scenario {
	return []Scenario{
		{
			ID:          "audio_mixing_and_earcons",
			Number:      8,
			Name:        "Bounded Audio Mixing & Paced Earcons",
			Description: "Multi-source audio mixing combines speech output and ambient audio with saturation clipping prevention; earcon cues fire with interruptible and uninterruptible disposition handling.",
			Invariants: []string{
				"AudioMixer bounds concurrent streams to configured capacity",
				"Summed 16-bit PCM samples saturate without wraparound distortion",
				"Uninterruptible earcons complete playback during turn interruptions",
				"Interruptible earcons immediately cease upon interruption frames",
				"Background audio automatically ducks during active assistant speech",
			},
			Steps: []string{
				"configure AudioMixer with multiple input sources",
				"mix concurrent signals exceeding int16 bounds and verify clean saturation clipping",
				"play uninterruptible earcon cue across interruption and verify complete delivery",
				"start ambient background audio and verify automatic gain ducking upon TTS speech",
			},
			Timeout: 5 * time.Second,
		},
		{
			ID:          "runtime_introspection_and_readiness",
			Number:      9,
			Name:        "Runtime Introspection & Readiness Events",
			Description: "Pipeline dynamically emits structured readiness events and provides point-in-time diagnostic snapshots and processor capability discovery.",
			Invariants: []string{
				"Session.Snapshot provides immutable read-only view of runtime state and pipeline processors",
				"Processors advertise capability flags and side-effect metadata",
				"Readiness frames announce component health transitions without blocking active calls",
			},
			Steps: []string{
				"initialize pipeline with capable processors and start session",
				"query pipeline capabilities for speech_to_text, llm, and vad",
				"capture point-in-time session snapshot and verify diagnostics agreement",
				"emit readiness state transition and verify non-blocking observer delivery",
			},
			Timeout: 5 * time.Second,
		},
		{
			ID:          "deterministic_turn_replay",
			Number:      10,
			Name:        "Deterministic Turn Recording & Replay",
			Description: "TurnRecorder captures real-time conversation turns into portable JSON logs; TurnReplayer accurately simulates the recorded turn in headless regression testing.",
			Invariants: []string{
				"TurnRecorder serializes exact event timestamps, offsets, and directions",
				"JSON serialization round-trip maintains zero loss of metadata",
				"TurnReplayer faithfully reproduces event ordering and directionality",
			},
			Steps: []string{
				"record conversation turn with multiple input/output frames",
				"serialize recorded turn to JSON and deserialize back to struct",
				"replay recorded turn into mock recipient and verify exact event count and sequence",
			},
			Timeout: 5 * time.Second,
		},
	}
}

// AllScenarios returns both vertical scenarios (1-7) and extended P2 parity scenarios (8-10).
func AllScenarios() []Scenario {
	return append(SevenScenarios(), ExtendedParityScenarios()...)
}

// GetScenario returns a scenario definition by 1-based number or ID.
func GetScenario(numberOrID any) (Scenario, error) {
	all := AllScenarios()
	for _, sc := range all {
		switch v := numberOrID.(type) {
		case int:
			if sc.Number == v {
				return sc, nil
			}
		case string:
			if sc.ID == v || fmt.Sprintf("scenario_%d", sc.Number) == v {
				return sc, nil
			}
		}
	}
	return Scenario{}, fmt.Errorf("scenario %v not found", numberOrID)
}
