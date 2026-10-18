package core

import "testing"

// TestFrameTypeNumbersAreStable pins every frame type number. The values reach
// traces (frameTraceLabel) and Whisker events (frame_kind), so deleting a
// constant must leave a blank placeholder rather than shift later values.
func TestFrameTypeNumbersAreStable(t *testing.T) {
	want := map[FrameType]FrameType{
		FrameInputAudio: 100, FrameOutputAudio: 101,
		FrameUserStartedSpeaking: 202, FrameUserStoppedSpeaking: 203,
		FrameInterimTranscription: 304, FrameFinalTranscription: 305,
		FrameLLMTextDelta: 406, FrameLLMText: 407, FrameSynthesisUnit: 408,
		FrameTTSStarted: 509, FrameTTSStopped: 511,
		FrameUserMessage: 612, FrameAssistantMessage: 613, FrameToolCall: 615, FrameToolResult: 616,
		FrameEnd: 718, FrameError: 720, FrameInterruption: 721, FrameFlush: 722,
		FrameFlushProcessed: 723, FrameInputMute: 724, FrameInputUnmute: 725, FrameInputMutedState: 726,
		FrameInternal: 821, FrameWriteCompletion: 822, FrameTurnEvent: 823, FrameSpeakingState: 824,
		FrameOutputTurnFinal: 825, FrameTranscriptCandidate: 826, FrameTranscriptAdvance: 827,
		FrameTranscriptInvalidation: 828, FrameUsageMetrics: 829, FramePushToTalk: 830,
		FrameInputDTMF: 840, FrameOutputDTMF: 841,
	}
	for got, w := range want {
		if got != w {
			t.Errorf("frame type = %d, want %d", got, w)
		}
	}
}
