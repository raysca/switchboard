package core

// ProcessorCapability represents a recognized capability flag for pipeline processors.
type ProcessorCapability string

const (
	CapabilityAudioInput    ProcessorCapability = "audio_input"
	CapabilityAudioOutput   ProcessorCapability = "audio_output"
	CapabilitySpeechToText  ProcessorCapability = "speech_to_text"
	CapabilityTextToSpeech  ProcessorCapability = "text_to_speech"
	CapabilityLLM           ProcessorCapability = "llm"
	CapabilityVAD           ProcessorCapability = "vad"
	CapabilityTurnDetector  ProcessorCapability = "turn_detector"
	CapabilityFilter        ProcessorCapability = "filter"
	CapabilityToolExecutor  ProcessorCapability = "tool_executor"
	CapabilityUrgentControl ProcessorCapability = "urgent_control"
	CapabilityMetrics       ProcessorCapability = "metrics"
)

// ProcessorCapabilities describes the runtime features and contract behavior of a processor.
type ProcessorCapabilities struct {
	Name         string                `json:"name"`
	Version      string                `json:"version,omitempty"`
	Description  string                `json:"description,omitempty"`
	Capabilities []ProcessorCapability `json:"capabilities"`
	InputFrames  []FrameType           `json:"input_frames,omitempty"`
	OutputFrames []FrameType           `json:"output_frames,omitempty"`
	SideEffects  bool                  `json:"side_effects"` // true if processor triggers external RPCs/writes
}

// CapableProcessor is an optional interface implemented by processors that declare metadata and capabilities.
type CapableProcessor interface {
	Processor
	Capabilities() ProcessorCapabilities
}

// GetProcessorCapabilities queries a processor for capabilities, returning a default fallback if not implemented.
func GetProcessorCapabilities(p Processor) ProcessorCapabilities {
	if cp, ok := p.(CapableProcessor); ok {
		return cp.Capabilities()
	}
	return ProcessorCapabilities{
		Name:         "unspecified",
		Capabilities: []ProcessorCapability{},
	}
}

// HasCapability checks whether a processor advertises a specific capability.
func HasCapability(p Processor, cap ProcessorCapability) bool {
	caps := GetProcessorCapabilities(p)
	for _, c := range caps.Capabilities {
		if c == cap {
			return true
		}
	}
	return false
}

// FindProcessorsWithCapability returns all processors in a pipeline matching the capability.
func (p *Pipeline) FindProcessorsWithCapability(cap ProcessorCapability) []Processor {
	var matches []Processor
	for _, proc := range p.Processors() {
		if HasCapability(proc, cap) {
			matches = append(matches, proc)
		}
	}
	return matches
}
