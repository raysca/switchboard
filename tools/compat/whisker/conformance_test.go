package whisker

import (
	"testing"

	"govoice/core"
)

func TestRuntimeSemanticConformanceScenarios(t *testing.T) {
	want := []string{"session_pipeline_start", "audio_frame_flow", "user_speech", "stt_interim_final", "llm_output", "tts_lifecycle_audio", "barge_in", "flush", "tool_call_result", "provider_pipeline_errors", "session_teardown"}
	results := RunScenarios()
	if len(results) != len(want) {
		t.Fatalf("got %d scenarios, want %d", len(results), len(want))
	}
	for i, result := range results {
		if result.Name != want[i] {
			t.Fatalf("scenario %d = %q, want %q", i, result.Name, want[i])
		}
		if result.Err != nil {
			t.Errorf("%s: %v", result.Name, result.Err)
			continue
		}
		if len(result.Events) == 0 {
			t.Errorf("%s has no runtime events", result.Name)
			continue
		}
		for j, e := range result.Events {
			if e.SessionID == "" || e.PipelineID == "" || e.Processor == "" || e.ProcessorIndex < 0 || (e.Direction != core.Downstream.String() && e.Direction != core.Upstream.String()) || e.Sequence == 0 {
				t.Errorf("%s event %d lacks correlation/position: %#v", result.Name, j, e)
			}
			if j > 0 && e.Sequence <= result.Events[j-1].Sequence {
				t.Errorf("%s nonmonotonic sequence", result.Name)
			}
		}
		byName := map[string]Event{}
		for _, e := range result.Events {
			byName[e.Name] = e
		}
		switch result.Name {
		case "audio_frame_flow":
			if byName["audio.input"].FrameKind != uint16(core.FrameInputAudio) || byName["audio.input"].Attributes()["sample_rate"] != 16000 || byName["audio.output"].FrameKind != uint16(core.FrameOutputAudio) || byName["audio.output"].Attributes()["sample_rate"] != 24000 {
				t.Error("audio direction/format/position evidence missing")
			}
		case "user_speech", "stt_interim_final", "llm_output", "tts_lifecycle_audio":
			for _, e := range result.Events {
				if e.TurnID != "" && e.TurnID != "turn-1" {
					t.Errorf("turn correlation drifted: %#v", e)
				}
			}
		case "barge_in":
			if byName["turn.interrupted"].Generation != 1 {
				t.Error("interruption generation missing")
			}
			if byName["turn.interrupted"].Sequence >= byName["output.flush.started"].Sequence || byName["output.flush.started"].Sequence >= byName["output.flush.completed"].Sequence {
				t.Error("runtime interruption/flush order missing")
			}
			for _, e := range result.Events {
				if e.Name == "audio.output" && e.Generation == 1 {
					t.Error("stale output observed as current")
				}
			}
		case "flush":
			if byName["output.flush.started"].Sequence >= byName["output.flush.completed"].Sequence {
				t.Error("flush completion did not follow start")
			}
			if byName["output.flush.completed"].Attributes()["count"] == nil {
				t.Error("pacer drop count not observed")
			}
		case "tool_call_result":
			if byName["tool.completed"].ToolCallID != "call-1" || byName["tool.requested"].ToolCallID != "call-2" {
				t.Error("tool call correlation missing")
			}
			if byName["tool.failed"].ToolCallID != "call-2" || byName["tool.failed"].Attributes()["status"] != "failed" {
				t.Error("failed tool outcome missing")
			}
		case "provider_pipeline_errors":
			if byName["provider.error"].Attributes()["error.category"] != "provider_error" || byName["session.error"].Attributes()["error.category"] != "internal_error" {
				t.Error("safe error categories missing")
			}
		case "session_teardown":
			if byName["session.cancelled"].Sequence >= byName["session.ended"].Sequence {
				t.Error("cancel did not precede end")
			}
			if byName["session.ended"].Sequence >= byName["processor.shutdown"].Sequence || byName["processor.shutdown"].Sequence >= byName["session.resources.completed"].Sequence {
				t.Error("processor/resource shutdown not observed after end")
			}
		}
	}
}
