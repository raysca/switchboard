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
	// nvidiaNIMBaseURL is build.nvidia.com's OpenAI-compatible endpoint;
	// self-hosted NIM sets base_url in the stored credential.
	nvidiaNIMBaseURL = "https://integrate.api.nvidia.com/v1"
	// Live-checked 2026-10 (docs/benchmarks/nvidia-live.md): streams, usage, one tool call,
	// first token ~0.4-0.9 s (one 1.5 s). Pipecat's default nemotron-3-super reasons for
	// 0.7-4.6 s first token, even with /no_think. The catalog lists models that 404/410:
	// a retired default surfaces as "model ... not found or deprecated: set the model explicitly".
	nvidiaNIMModel = "openai/gpt-oss-20b"
)

// nvidiaBreaker returns the breaker for key ("llm|<base url>", "stt|<endpoint>", "tts|<endpoint>"):
// one tenant's broken self-hosted NIM or Riva must not open it for the others.
// ponytail: never evicted; bounded by the distinct endpoints stored.
func (r *Registry) nvidiaBreaker(key string) *resilience.Breaker {
	br, loaded := r.nvidiaBreakers.LoadOrStore(key, resilience.NewBreaker(resilience.DefaultConfig()))
	b := br.(*resilience.Breaker)
	if !loaded && r.metrics != nil { // first use: export the gauge (voice_<stage>_circuit_state, /ready)
		stage, endpoint, _ := strings.Cut(key, "|")
		endpoint = endpointLabel(endpoint)
		r.metrics.circuits.set(stage, "nvidia", endpoint, 0)
		b.SetObserver(func(st resilience.State) {
			r.metrics.circuits.set(stage, "nvidia", endpoint, circuitCode(string(st)))
		})
	}
	return b
}

// endpointLabel reduces a base URL or host:port to host[:port] for a metric label
// (no path, userinfo or query).
func endpointLabel(ep string) string {
	if u, err := url.Parse(ep); err == nil && u.Host != "" {
		return u.Host
	}
	return ep
}

// nvidiaLLMTarget resolves the stored credential to key and base URL.
func nvidiaLLMTarget(secret []byte) (key, baseURL string, err error) {
	if key, baseURL, err = openAICredential(secret); err != nil {
		return "", "", err
	}
	if baseURL == "" {
		baseURL = nvidiaNIMBaseURL
	}
	return key, baseURL, nil
}

// nvidiaSTTCredential accepts a bare NVCF API key (then the hosted endpoint
// still needs a function id, so it fails there) or JSON
// {"api_key","function_id","tts_function_id","endpoint","use_tls"}. NVCF hosts
// ASR and TTS as different functions, so nvidia.tts takes tts_function_id when
// set (else function_id). use_tls defaults to true;
// self-hosted Riva sets {"endpoint":"host:50051","use_tls":false} (no key needed).
func nvidiaSTTCredential(secret []byte) (c struct {
	APIKey     string `json:"api_key"`
	FunctionID string `json:"function_id"`
	TTSFuncID  string `json:"tts_function_id"`
	Endpoint   string `json:"endpoint"`
	UseTLS     *bool  `json:"use_tls"`
}, err error) {
	trimmed := strings.TrimSpace(string(secret))
	if !strings.HasPrefix(trimmed, "{") {
		c.APIKey, err = apiKey(secret)
		return c, err
	}
	if json.Unmarshal([]byte(trimmed), &c) != nil {
		return c, errors.New("stored credential is not valid JSON")
	}
	if c.APIKey = strings.TrimSpace(c.APIKey); c.APIKey != "" {
		if c.APIKey, err = apiKey([]byte(c.APIKey)); err != nil {
			return c, err
		}
	}
	c.FunctionID, c.Endpoint = strings.TrimSpace(c.FunctionID), strings.TrimSpace(c.Endpoint)
	if c.TTSFuncID = strings.TrimSpace(c.TTSFuncID); c.TTSFuncID == "" {
		c.TTSFuncID = c.FunctionID
	}
	return c, nil
}

// nvidiaSTTBreaker is the breaker of the Riva endpoint the credential names
// (the hosted one by default), shared by the factory and the failover check.
func (r *Registry) nvidiaSTTBreaker(secret []byte) *resilience.Breaker {
	return r.nvidiaRivaBreaker("stt", secret)
}

func (r *Registry) nvidiaRivaBreaker(stage string, secret []byte) *resilience.Breaker {
	ep := nvidia.DefaultEndpoint
	if c, err := nvidiaSTTCredential(secret); err == nil && c.Endpoint != "" {
		ep = c.Endpoint
	}
	return r.nvidiaBreaker(stage + "|" + ep)
}

// registerNvidia adds nvidia.llm (NIM chat completions through providers/openai)
// nvidia.stt (Riva streaming ASR) and nvidia.tts (Riva streaming TTS). All honour providers.nvidia.allow_private.
func (r *Registry) registerNvidia() {
	r.Register("nvidia.llm", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		key, baseURL, err := nvidiaLLMTarget(spec.Secret)
		if err != nil {
			return nil, fmt.Errorf("nvidia.llm: %w", err)
		}
		model := spec.Model
		if model == "" {
			model = nvidiaNIMModel
		}
		oaCfg := openai.Config{Provider: "nvidia", Model: model, BaseURL: baseURL, SystemPrompt: spec.SystemPrompt, APIKey: key,
			CircuitBreaker: r.nvidiaBreaker("llm|" + baseURL), AllowPrivate: cfg.Providers.Nvidia.AllowPrivate}
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
		if c.Endpoint != "" {
			sttCfg.Endpoint = c.Endpoint
		}
		sttCfg.APIKey, sttCfg.FunctionID = c.APIKey, c.FunctionID
		sttCfg.Insecure = c.UseTLS != nil && !*c.UseTLS
		sttCfg.AllowPrivate = cfg.Providers.Nvidia.AllowPrivate
		if spec.Model != "" {
			sttCfg.Model = spec.Model
		}
		if spec.Language != "" {
			sttCfg.Language = spec.Language
		}
		sttCfg.CircuitBreaker = r.nvidiaSTTBreaker(spec.Secret)
		if spec.Failover {
			sttCfg.RetryPolicy = failoverPolicy("stt", sttCfg.RetryPolicy)
		}
		return nvidia.NewSTTProcessor(sttCfg)
	})

	// The credential is the STT one (plus tts_function_id on NVCF). Riva is asked
	// for the pipeline rate and resamples server side (see nvidia.TTSConfig).
	r.Register("nvidia.tts", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		c, err := nvidiaSTTCredential(spec.Secret)
		if err != nil {
			return nil, fmt.Errorf("nvidia.tts: %w", err)
		}
		ttsCfg := nvidia.DefaultTTSConfig()
		if c.Endpoint != "" {
			ttsCfg.Endpoint = c.Endpoint
		}
		ttsCfg.APIKey, ttsCfg.FunctionID = c.APIKey, c.TTSFuncID
		ttsCfg.Insecure = c.UseTLS != nil && !*c.UseTLS
		ttsCfg.AllowPrivate = cfg.Providers.Nvidia.AllowPrivate
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
		ttsCfg.CircuitBreaker = r.nvidiaRivaBreaker("tts", spec.Secret)
		if spec.Failover {
			ttsCfg.RetryPolicy = failoverPolicy("tts", ttsCfg.RetryPolicy)
		}
		return nvidia.NewTTSProcessor(ttsCfg)
	})
}
