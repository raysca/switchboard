package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"govoice/core"
	"govoice/core/resilience"
	coretools "govoice/core/tools"
	"govoice/providers/nvidia"
	"govoice/providers/openai"
)

const (
	// nvidiaNIMBaseURL is build.nvidia.com's OpenAI-compatible endpoint. Only the
	// hosted NIM and NVCF Riva endpoints are supported: self-hosted ones would need
	// catalog env fields (base URL, endpoint, TLS) and an opt-in, added when needed.
	nvidiaNIMBaseURL = "https://integrate.api.nvidia.com/v1"
	// Live-checked 2026-10 (docs/benchmarks/nvidia-live.md): streams, usage, one tool call,
	// first token ~0.4-0.9 s (one 1.5 s). Pipecat's default nemotron-3-super reasons for
	// 0.7-4.6 s first token, even with /no_think. The catalog lists models that 404/410:
	// a retired default surfaces as "model ... not found or deprecated: set the model explicitly".
	nvidiaNIMModel = "openai/gpt-oss-20b"
)

// nvidiaEndpoint is the endpoint label of an nvidia stage's breaker. Only the hosted endpoints
// exist, so a secret never names a breaker.
func nvidiaEndpoint(stage string) string {
	if stage == "llm" {
		return endpointLabel(nvidiaNIMBaseURL)
	}
	return endpointLabel(nvidia.DefaultEndpoint)
}

// endpointLabel reduces a base URL or host:port to host[:port] for a metric label
// (no path, userinfo or query).
func endpointLabel(ep string) string {
	if u, err := url.Parse(ep); err == nil && u.Host != "" {
		return u.Host
	}
	return ep
}

// nvidiaSTTCredential parses the assembled Riva secret
// {"api_key","function_id","tts_function_id"}. NVCF hosts ASR and TTS as
// different functions, so nvidia.tts takes tts_function_id when set (else function_id).
func nvidiaSTTCredential(secret []byte) (c struct {
	APIKey     string `json:"api_key"`
	FunctionID string `json:"function_id"`
	TTSFuncID  string `json:"tts_function_id"`
}, err error) {
	if json.Unmarshal(secret, &c) != nil {
		return c, errors.New("credential is not valid JSON")
	}
	if c.APIKey, err = apiKey([]byte(c.APIKey)); err != nil {
		return c, err
	}
	c.FunctionID = strings.TrimSpace(c.FunctionID)
	if c.TTSFuncID = strings.TrimSpace(c.TTSFuncID); c.TTSFuncID == "" {
		c.TTSFuncID = c.FunctionID
	}
	return c, nil
}

func nvidiaLLMConfig(spec StageSpec, model, key string, br *resilience.Breaker) openai.Config {
	c := openai.Config{Provider: "nvidia", Model: model, BaseURL: nvidiaNIMBaseURL, SystemPrompt: spec.SystemPrompt, APIKey: key, CircuitBreaker: br}
	c.RetryPolicy.OnRetry = spec.OnRetry
	c.Temperature, c.TopP, c.MaxOutputTokens = spec.sampling()
	return c
}

// registerNvidia adds nvidia.llm (NIM chat completions through providers/openai)
// nvidia.stt (Riva streaming ASR) and nvidia.tts (Riva streaming TTS), all on the hosted endpoints.
func (r *Registry) registerNvidia() {
	r.Register("nvidia.llm", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		key, err := apiKey(spec.Secret)
		if err != nil {
			return nil, fmt.Errorf("nvidia.llm: %w", err)
		}
		model := spec.Model
		if model == "" {
			model = nvidiaNIMModel
		}
		oaCfg := nvidiaLLMConfig(spec, model, key, r.breaker("llm", "nvidia"))
		if len(spec.Tools) > 0 {
			loop, err := coretools.NewToolLoop(spec.Tools, spec.ToolExecutor, spec.History, spec.toolLoopConfig())
			if err != nil {
				return nil, fmt.Errorf("nvidia.llm tools: %w", err)
			}
			oaCfg.ToolDefinitions, oaCfg.ToolLoop = spec.Tools, loop
		}
		return openai.NewProcessor(oaCfg)
	})

	r.Register("nvidia.stt", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		c, err := nvidiaSTTCredential(spec.Secret)
		if err != nil {
			return nil, fmt.Errorf("nvidia.stt: %w", err)
		}
		sttCfg := nvidia.DefaultSTTConfig()
		sttCfg.APIKey, sttCfg.FunctionID = c.APIKey, c.FunctionID
		if spec.Model != "" {
			sttCfg.Model = spec.Model
		}
		if spec.Language != "" {
			sttCfg.Language = spec.Language
		}
		sttCfg.CircuitBreaker = r.breaker("stt", "nvidia")
		if spec.Failover {
			sttCfg.RetryPolicy = failoverPolicy("stt", sttCfg.RetryPolicy)
		}
		return nvidia.NewSTTProcessor(sttCfg)
	})

	// The credential is the STT one (plus tts_function_id). Riva is asked
	// for the pipeline rate and resamples server side (see nvidia.TTSConfig).
	r.Register("nvidia.tts", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		c, err := nvidiaSTTCredential(spec.Secret)
		if err != nil {
			return nil, fmt.Errorf("nvidia.tts: %w", err)
		}
		ttsCfg := nvidia.DefaultTTSConfig()
		ttsCfg.APIKey, ttsCfg.FunctionID = c.APIKey, c.TTSFuncID
		if spec.Voice != "" {
			ttsCfg.Voice = spec.Voice
		}
		if spec.Language != "" {
			ttsCfg.Language = spec.Language
		}
		if spec.Model != "" {
			ttsCfg.Model = spec.Model
		}
		ttsCfg.SampleRate = ttsRate(cfg)
		ttsCfg.CircuitBreaker = r.breaker("tts", "nvidia")
		if spec.Failover {
			ttsCfg.RetryPolicy = failoverPolicy("tts", ttsCfg.RetryPolicy)
		}
		ttsCfg.RetryPolicy.OnRetry = spec.OnRetry
		return nvidia.NewTTSProcessor(ttsCfg)
	})
}
