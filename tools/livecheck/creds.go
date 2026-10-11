package main

import (
	"slices"
	"strings"
)

// credSpecs lists, per vendor and stage, the environment variables the server needs to run
// that stage: the catalog's first credential kind's required fields plus the service's needs
// (server/catalog/data). TestCredSpecsMatchCatalog fails when the two drift apart.
// ponytail: a copy of the catalog on purpose (spec: moving livecheck onto the catalog is a follow-up).
var credSpecs = map[string]func(stage string) []string{
	"xai":        oneKey("XAI_API_KEY"),
	"deepgram":   oneKey("DEEPGRAM_API_KEY"),
	"cartesia":   oneKey("CARTESIA_API_KEY"),
	"elevenlabs": oneKey("ELEVENLABS_API_KEY"), // STT and TTS share it
	"openai":     oneKey("OPENAI_API_KEY"),     // OPENAI_BASE_URL is optional (Groq, vLLM, ...)
	"google": func(stage string) []string {
		if stage == "llm" {
			return []string{"GOOGLE_API_KEY"}
		}
		return []string{"GOOGLE_APPLICATION_CREDENTIALS"} // streaming STT rejects API keys
	},
	"nvidia": func(stage string) []string {
		switch stage { // NVCF hosts ASR and TTS as separate functions
		case "stt":
			return []string{"NVIDIA_API_KEY", "NVIDIA_STT_FUNCTION_ID"}
		case "tts":
			return []string{"NVIDIA_API_KEY", "NVIDIA_TTS_FUNCTION_ID"}
		}
		return []string{"NVIDIA_API_KEY"}
	},
}

func oneKey(envName string) func(string) []string {
	return func(string) []string { return []string{envName} }
}

// providerEnv is the provider part of the server child's environment: the profile's requires,
// optional and defaulted variables (validate makes these cover every variable a stage reads),
// valued from env, then the defaults. Nothing else from the harness environment reaches the child.
func providerEnv(p Profile, env func(string) string) []string {
	names := slices.Concat(p.Requires, p.Optional)
	for k := range p.Defaults {
		names = append(names, k)
	}
	slices.Sort(names)
	lk := p.lookup(env)
	out := make([]string, 0, len(names))
	for _, n := range slices.Compact(names) {
		if v := lk(n); v != "" {
			out = append(out, n+"="+v)
		}
	}
	return out
}

func normProvider(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
