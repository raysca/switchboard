package server

import (
	"errors"
	"strings"

	"govoice/core"
	"govoice/providers/echo"
	"govoice/providers/google"
)

// registerEcho registers the simulated provider on every build. Its stages are the real Google
// processors (retries, breaker, usage, tracing, tool loop, unit splitting) running against the
// simulated backends of providers/echo, so a call through echo exercises the real pipeline code
// at no cost. It needs no secret, so spec.Secret is never read.
func registerEcho(r *Registry) {
	r.Register("echo.stt", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		sttCfg := google.DefaultSTTConfig()
		sttCfg.Provider = "echo"
		if spec.Model != "" {
			sttCfg.Model = spec.Model
		}
		if spec.Language != "" {
			sttCfg.LanguageCode = spec.Language
		}
		sttCfg.CircuitBreaker = r.echoSTTBreaker
		if spec.Failover {
			sttCfg.RetryPolicy = failoverPolicy("stt", sttCfg.RetryPolicy)
		}
		sttCfg.StreamOpener = echo.NewSTTOpener(echo.Mimic(cfg.Providers.Echo.Mimic).STT(sttCfg.Model), cfg.Providers.Echo.STT == "audio")
		return google.NewSTTProcessor(sttCfg)
	})

	r.Register("echo.llm", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		llmCfg := google.DefaultLLMConfig()
		llmCfg.Provider, llmCfg.Model, llmCfg.SystemPrompt = "echo", spec.Model, spec.SystemPrompt
		if len(spec.Tools) > 0 {
			llmCfg.ToolDefinitions, llmCfg.ToolExecutor, llmCfg.ToolHistory = spec.Tools, spec.ToolExecutor, spec.History
			llmCfg.ToolLoopConfig = spec.toolLoopConfig()
		}
		llmCfg.Client = echo.NewLLMClient(echoLLMOptions(cfg, spec))
		llmCfg.CircuitBreaker = r.echoLLMBreaker
		return google.NewLLMProcessor(llmCfg)
	})

	r.Register("echo.tts", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		// A step whose voice is "brokenvoice" cannot be built: the e2e suites use it to make a handoff fail.
		if spec.Voice == "brokenvoice" {
			return nil, errors.New("echo tts: voice brokenvoice is unavailable")
		}
		ttsCfg := google.DefaultTTSConfig()
		ttsCfg.Provider = "echo"
		if spec.Voice != "" {
			ttsCfg.Voice = spec.Voice
		}
		if cfg.Providers.GoogleTTS.SampleRate > 0 {
			ttsCfg.SampleRate = cfg.Providers.GoogleTTS.SampleRate
		}
		ttsCfg.Client = echo.NewTTSClient(echo.Mimic(cfg.Providers.Echo.Mimic).TTS(ttsCfg.Voice), cfg.Providers.Echo.Audible)
		ttsCfg.CircuitBreaker = r.echoTTSBreaker
		if spec.Failover {
			ttsCfg.RetryPolicy = failoverPolicy("tts", ttsCfg.RetryPolicy)
		}
		if google.IsChirp3HD(ttsCfg.Voice) && !cfg.Providers.GoogleTTS.DisableStreaming {
			return google.NewStreamingTTSProcessor(ttsCfg)
		}
		return google.NewTTSProcessor(ttsCfg)
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

// mcpToolPhrases maps "call tool <remote name>" to each MCP tool's model-facing name
// (runtime.ToolName: "mcp__" + server prefix + "_" + remote name + 8 hex), and that
// name to the remote name the reply uses. Two attached tools with the same remote
// name: the phrase goes to the first in the order given.
func mcpToolPhrases(defs []core.ToolDefinition) (map[string]string, map[string]string) {
	phrases, spoken := map[string]string{}, map[string]string{}
	for _, d := range defs {
		rest, ok := strings.CutPrefix(d.Name, "mcp__")
		if !ok {
			continue
		}
		_, remote, ok := strings.Cut(rest, "_")
		if !ok || len(remote) <= 8 {
			continue
		}
		remote = remote[:len(remote)-8]
		phrase := "call tool " + strings.ToLower(remote)
		if _, taken := phrases[phrase]; !taken {
			phrases[phrase] = d.Name
		}
		spoken[d.Name] = remote
	}
	return phrases, spoken
}
