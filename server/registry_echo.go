package server

import (
	"errors"
	"strings"

	"govoice/core"
	"govoice/core/audio/dtmf"
	"govoice/providers/echo"
	"govoice/providers/google"
)

// registerEcho registers the simulated provider on every build. Its stages are the real Google
// processors (retries, breaker, usage, tracing, tool loop, unit splitting) running against the
// simulated backends of providers/echo, so a call through echo exercises the real pipeline code
// at no cost. It needs no secret, so spec.Secret is never read.
func registerEcho(r *Registry) {
	r.Register("echo.stt", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		sttCfg := googleSTTConfig(cfg, spec, r.echoSTTBreaker)
		sttCfg.Provider = "echo"
		sttCfg.StreamOpener = echo.NewSTTOpener(echo.Mimic(cfg.Providers.Echo.Mimic).STT(sttCfg.Model), cfg.Providers.Echo.STT == "audio")
		return google.NewSTTProcessor(sttCfg)
	})

	r.Register("echo.llm", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		llmCfg := googleLLMConfig(spec, r.echoLLMBreaker)
		llmCfg.Provider = "echo"
		llmCfg.Client = echo.NewLLMClient(echoLLMOptions(cfg, spec))
		return google.NewLLMProcessor(llmCfg)
	})

	r.Register("echo.tts", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		// A step whose voice is "brokenvoice" cannot be built: the e2e suites use it to make a route fail.
		if spec.Voice == "brokenvoice" {
			return nil, errors.New("echo tts: voice brokenvoice is unavailable")
		}
		ttsCfg := googleTTSConfig(cfg, spec, r.echoTTSBreaker)
		ttsCfg.Provider = "echo"
		ttsCfg.Client = echo.NewTTSClient(echo.Mimic(cfg.Providers.Echo.Mimic).TTS(googleTTSStreams(cfg, ttsCfg.Voice)), cfg.Providers.Echo.Audible)
		return googleTTSProcessor(cfg, ttsCfg)
	})
}

// echoLLMOptions turns the config and the stage into the simulated model's options: its timing, the
// reply label of a routed step, and the phrases that make it call a transfer, end_call or MCP tool.
func echoLLMOptions(cfg *Config, spec StageSpec) echo.LLMOptions {
	o := echo.LLMOptions{Figures: echo.Mimic(cfg.Providers.Echo.Mimic).LLM(), ExtraDelay: cfg.Providers.Echo.LLMExtraDelay,
		Phrases: map[string]string{}}
	if cfg.Providers.Echo.LabelReplies {
		o.Label = "[" + spec.SystemPrompt + "] "
	}
	for _, d := range spec.Tools {
		if step, ok := strings.CutPrefix(d.Name, "transfer_to_"); ok {
			o.Phrases["transfer to "+strings.ReplaceAll(step, "_", " ")] = d.Name
		} else if d.Name == endCallName {
			o.Phrases["end the call"] = endCallName // a step with the end_call tool hangs up after its goodbye
		}
	}
	phrases, spoken := mcpToolPhrases(spec.Tools)
	for p, name := range phrases {
		o.Phrases[p] = name
	}
	o.Spoken = spoken
	return o
}

// mcpToolPhrases maps "call tool <remote name>" to each MCP tool's model-facing name, and that name
// to the remote name the reply uses. A tool is an MCP tool when its definition carries a RemoteName
// (set where the runtime binds it). Two attached tools with the same remote name: the phrase goes
// to the first in the order given. send_dtmf gets the same phrase ("call tool send_dtmf {json}") so the
// e2e suites can send tones.
func mcpToolPhrases(defs []core.ToolDefinition) (map[string]string, map[string]string) {
	phrases, spoken := map[string]string{}, map[string]string{}
	for _, d := range defs {
		remote := d.RemoteName
		if d.Name == dtmf.ToolName {
			remote = d.Name
		}
		if remote == "" {
			continue
		}
		phrase := "call tool " + strings.ToLower(remote)
		if _, taken := phrases[phrase]; !taken {
			phrases[phrase] = d.Name
		}
		spoken[d.Name] = remote
	}
	return phrases, spoken
}
