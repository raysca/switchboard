package server

import (
	"fmt"

	"govoice/core"
)

// BuildStage builds one stage processor ("stt", "llm" or "tts") through the
// same factory a call uses (BuildCallPipeline builds every candidate through
// it), without the call pipeline, warm-up or failover wrapper. With
// spec.Failover false it is the single-provider policy (the factory's own
// retry policy); a failover primary gets the shorter failoverPolicy instead.
// The provider conformance suite (tools/provtest) tests providers through it.
func (r *Registry) BuildStage(cfg *Config, stage string, spec StageSpec) (core.Processor, error) {
	f, ok := r.factories[factoryKey(spec.Provider, stage)]
	if !ok {
		return nil, fmt.Errorf("%s provider %q is not available", stage, spec.Provider)
	}
	return f(cfg, spec)
}

// ClassifyStageError is the failover class ("client_error", "server_error",
// "timeout", "circuit_open") the call path assigns to err from a stage.
func ClassifyStageError(stage string, err error) string {
	switch stage {
	case "stt":
		return sttClassify(err)
	case "tts":
		return ttsClassify(err)
	}
	return llmClassify(err)
}
