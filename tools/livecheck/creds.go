package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// credSpec says how one vendor's stored credential is built from the environment, like
// scripts/dev-seed.mjs does for Google. role groups stages that share a stored credential
// (Google STT and TTS share the service account; the LLM prefers the Gemini API key).
type credSpec struct {
	role     func(stage string) string
	requires func(stage string) []string // hard env requirements; ids with defaults are checked by secret
	secret   func(stage string, env func(string) string) (string, error)
}

func oneKey(envName string) *credSpec {
	return &credSpec{
		role:     func(string) string { return "key" },
		requires: func(string) []string { return []string{envName} },
		secret: func(_ string, env func(string) string) (string, error) {
			if v := strings.TrimSpace(env(envName)); v != "" {
				return v, nil
			}
			return "", fmt.Errorf("%s is not set", envName)
		},
	}
}

// credSpecs is the only vendor-specific code in the harness: a new vendor adds an entry.
var credSpecs = map[string]*credSpec{
	"xai":        oneKey("XAI_API_KEY"),
	"deepgram":   oneKey("DEEPGRAM_API_KEY"),
	"cartesia":   oneKey("CARTESIA_API_KEY"),
	"elevenlabs": oneKey("ELEVENLABS_API_KEY"), // STT and TTS share it
	"openai": {
		role:     func(string) string { return "key" },
		requires: func(string) []string { return []string{"OPENAI_API_KEY"} },
		secret: func(_ string, env func(string) string) (string, error) {
			key := strings.TrimSpace(env("OPENAI_API_KEY"))
			if key == "" {
				return "", errors.New("OPENAI_API_KEY is not set")
			}
			if base := strings.TrimSpace(env("OPENAI_BASE_URL")); base != "" { // Groq, vLLM, ...
				b, _ := json.Marshal(map[string]string{"api_key": key, "base_url": base})
				return string(b), nil
			}
			return key, nil
		},
	},
	"google": {
		role: func(stage string) string {
			if stage == "llm" {
				return "llm"
			}
			return "speech"
		},
		requires: func(stage string) []string {
			if stage == "llm" {
				return []string{"GOOGLE_API_KEY"}
			}
			return []string{"GOOGLE_APPLICATION_CREDENTIALS"} // streaming STT rejects API keys
		},
		secret: func(stage string, env func(string) string) (string, error) {
			if stage == "llm" {
				if v := strings.TrimSpace(env("GOOGLE_API_KEY")); v != "" {
					return v, nil
				}
				return "", errors.New("GOOGLE_API_KEY is not set")
			}
			p := env("GOOGLE_APPLICATION_CREDENTIALS")
			if p == "" {
				return "", errors.New("GOOGLE_APPLICATION_CREDENTIALS is not set")
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return "", errors.New("GOOGLE_APPLICATION_CREDENTIALS file is not readable")
			}
			var j json.RawMessage
			if json.Unmarshal(b, &j) != nil {
				return "", errors.New("GOOGLE_APPLICATION_CREDENTIALS file is not JSON")
			}
			return string(compactJSON(j)), nil
		},
	},
	"nvidia": {
		role: func(stage string) string {
			if stage == "llm" {
				return "llm"
			}
			return "riva"
		},
		requires: func(string) []string { return []string{"NVIDIA_API_KEY"} },
		secret: func(stage string, env func(string) string) (string, error) {
			key := strings.TrimSpace(env("NVIDIA_API_KEY"))
			if key == "" {
				return "", errors.New("NVIDIA_API_KEY is not set")
			}
			if stage == "llm" {
				return key, nil
			}
			// NVCF hosts ASR and TTS as separate functions; the stage needs its own id (a profile
			// using Riva for TTS only does not need the ASR id).
			stt, tts := env("NVIDIA_STT_FUNCTION_ID"), env("NVIDIA_TTS_FUNCTION_ID")
			if (stage == "stt" && stt == "") || (stage == "tts" && tts == "") {
				return "", fmt.Errorf("NVIDIA_%s_FUNCTION_ID is not set", strings.ToUpper(stage))
			}
			c := map[string]string{"api_key": key}
			if stt != "" {
				c["function_id"] = stt
			}
			if tts != "" {
				c["tts_function_id"] = tts
			}
			b, _ := json.Marshal(c)
			return string(b), nil
		},
	},
}

func compactJSON(j json.RawMessage) []byte {
	var v any
	if json.Unmarshal(j, &v) != nil {
		return j
	}
	b, _ := json.Marshal(v)
	return b
}

// credPlan lists the distinct stored credentials a profile needs: key "provider/role" -> secret.
// The fake profile gets placeholder secrets (the fake server build never uses them).
func credPlan(p Profile, env func(string) string) (map[string]string, map[string]string, error) {
	lk := p.lookup(env)
	secrets, stageKey := map[string]string{}, map[string]string{}
	for stage, s := range p.stages() {
		cs := credSpecs[s.Provider]
		if cs == nil {
			return nil, nil, fmt.Errorf("no credential handling for provider %q", s.Provider)
		}
		k := s.Provider + "/" + cs.role(stage)
		stageKey[stage] = k
		if _, ok := secrets[k]; ok {
			continue
		}
		if p.Fake {
			secrets[k] = "selftest-placeholder-" + k
			continue
		}
		v, err := cs.secret(stage, lk)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", stage, err)
		}
		secrets[k] = v
	}
	return secrets, stageKey, nil
}
