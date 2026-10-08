package provtest

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"

	"govoice/core"
	"govoice/server"
)

// Env reads configuration (os.Getenv in live runs, a map in tests).
type Env func(string) string

// Provider is one row of the conformance table. Adding a provider to the
// suite means adding its stage(s) here and nothing else, provided the server
// registry has a factory for "<id>.<stage>".
type Provider struct {
	ID    string
	Stage Stage
	// Tag is the Go build tag the server needs for this factory ("" = none).
	Tag string
	// Need lists required env names; "A|B" means either. Missing -> skipped.
	Need []string
	// Optional lists env names the row also reads (besides ModelEnv).
	Optional []string
	// Secret builds the stored credential the server factory expects.
	Secret func(Env) ([]byte, error)
	// BadKey is the invalid API key substituted for the real one ("" = generic).
	BadKey string
	// Model/Voice/Language go into the StageSpec; ModelEnv overrides Model.
	Model, ModelEnv, Voice, Language string
}

// NVCF function ids from docs/benchmarks/nvidia-live.md (public ids; they can change).
const (
	nvidiaSTTFunction = "1598d209-5e27-4d3c-8079-4751568b1081" // ai-parakeet-ctc-1_1b-asr
	nvidiaTTSFunction = "877104f7-e885-42b9-8de8-f6e4c6303969" // ai-magpie-tts-multilingual
	googleBadKey      = "AIzaSyBadBadBadBadBadBadBadBadBadBad0"
	nvidiaBadKey      = "nvapi-BadBadBadBadBadBadBadBadBadBadBadBadBadBadBadBad00"
)

func bare(name string) func(Env) ([]byte, error) {
	return func(e Env) ([]byte, error) { return []byte(strings.TrimSpace(e(name))), nil }
}

// googleSpeech prefers the service account (streaming STT rejects API keys).
func googleSpeech(e Env) ([]byte, error) {
	if p := e("GOOGLE_APPLICATION_CREDENTIALS"); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("GOOGLE_APPLICATION_CREDENTIALS unreadable")
		}
		var j json.RawMessage
		if json.Unmarshal(b, &j) != nil {
			return nil, fmt.Errorf("GOOGLE_APPLICATION_CREDENTIALS is not JSON")
		}
		return json.Marshal(j)
	}
	return []byte(e("GOOGLE_API_KEY")), nil
}

func nvidiaRiva(e Env) ([]byte, error) {
	or := func(v, d string) string {
		if v != "" {
			return v
		}
		return d
	}
	return json.Marshal(map[string]string{"api_key": strings.TrimSpace(e("NVIDIA_API_KEY")),
		"function_id": or(e("NVIDIA_STT_FUNCTION_ID"), nvidiaSTTFunction), "tts_function_id": or(e("NVIDIA_TTS_FUNCTION_ID"), nvidiaTTSFunction)})
}

func openAISecret(e Env) ([]byte, error) {
	if u := e("OPENAI_BASE_URL"); u != "" {
		return json.Marshal(map[string]string{"api_key": e("OPENAI_API_KEY"), "base_url": u})
	}
	return []byte(strings.TrimSpace(e("OPENAI_API_KEY"))), nil
}

// Table is every provider stage the suite knows about.
var Table = []Provider{
	{ID: "google", Stage: STT, Need: []string{"GOOGLE_APPLICATION_CREDENTIALS"}, Secret: googleSpeech, BadKey: googleBadKey, Language: "en-US"},
	{ID: "xai", Stage: STT, Tag: "xaispeech", Need: []string{"XAI_API_KEY"}, Secret: bare("XAI_API_KEY"), Language: "en"},
	{ID: "deepgram", Stage: STT, Need: []string{"DEEPGRAM_API_KEY"}, Secret: bare("DEEPGRAM_API_KEY"), Language: "en"},
	{ID: "cartesia", Stage: STT, Need: []string{"CARTESIA_API_KEY"}, Secret: bare("CARTESIA_API_KEY"), Language: "en"},
	{ID: "elevenlabs", Stage: STT, Need: []string{"ELEVENLABS_API_KEY"}, Secret: bare("ELEVENLABS_API_KEY"), Language: "en"},
	{ID: "nvidia", Stage: STT, Need: []string{"NVIDIA_API_KEY"}, Optional: []string{"NVIDIA_STT_FUNCTION_ID"}, Secret: nvidiaRiva, BadKey: nvidiaBadKey, Language: "en-US"},

	{ID: "google", Stage: TTS, Need: []string{"GOOGLE_APPLICATION_CREDENTIALS|GOOGLE_API_KEY"}, Secret: googleSpeech, BadKey: googleBadKey},
	{ID: "xai", Stage: TTS, Tag: "xaispeech", Need: []string{"XAI_API_KEY"}, Secret: bare("XAI_API_KEY")},
	{ID: "cartesia", Stage: TTS, Need: []string{"CARTESIA_API_KEY"}, Secret: bare("CARTESIA_API_KEY")},
	{ID: "elevenlabs", Stage: TTS, Need: []string{"ELEVENLABS_API_KEY"}, Secret: bare("ELEVENLABS_API_KEY")},
	{ID: "nvidia", Stage: TTS, Need: []string{"NVIDIA_API_KEY"}, Optional: []string{"NVIDIA_TTS_FUNCTION_ID"}, Secret: nvidiaRiva, BadKey: nvidiaBadKey},

	{ID: "google", Stage: LLM, Need: []string{"GOOGLE_API_KEY"}, Secret: bare("GOOGLE_API_KEY"), BadKey: googleBadKey, Model: "gemini-2.5-flash"},
	// xai.llm uses the server's providers.xai_llm.endpoint default (the Responses API, which tools
	// need). XAI_ENDPOINT is an examples/ override production never reads; it is deliberately ignored.
	{ID: "xai", Stage: LLM, Need: []string{"XAI_API_KEY"}, Secret: bare("XAI_API_KEY"), ModelEnv: "XAI_MODEL"},
	{ID: "openai", Stage: LLM, Need: []string{"OPENAI_API_KEY"}, Optional: []string{"OPENAI_BASE_URL"}, Secret: openAISecret, Model: "gpt-4o-mini", ModelEnv: "OPENAI_MODEL"},
	{ID: "nvidia", Stage: LLM, Need: []string{"NVIDIA_API_KEY"}, Secret: bare("NVIDIA_API_KEY"), BadKey: nvidiaBadKey, ModelEnv: "NVIDIA_LLM_MODEL"}, // empty model = server default (NIM)
}

// Filter keeps the table rows whose provider and stage are selected (empty = all).
func Filter(table []Provider, ids []string, stages []Stage) []Provider {
	var out []Provider
	for _, p := range table {
		if (len(ids) == 0 || slices.Contains(ids, p.ID)) && (len(stages) == 0 || slices.Contains(stages, p.Stage)) {
			out = append(out, p)
		}
	}
	return out
}

// EnvMarkdown lists each row's env names (never values); with env non-nil it
// also says whether the row would run.
func EnvMarkdown(table []Provider, env Env) string {
	var b strings.Builder
	b.WriteString("| provider | stage | build tag | needs | optional |")
	if env != nil {
		b.WriteString(" status |")
	}
	b.WriteString("\n|---|---|---|---|---|")
	if env != nil {
		b.WriteString("---|")
	}
	b.WriteString("\n")
	for _, p := range table {
		opt := slices.Clone(p.Optional)
		if p.ModelEnv != "" {
			opt = append(opt, p.ModelEnv)
		}
		tag := p.Tag
		if tag == "" {
			tag = "-"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |", p.ID, p.Stage, tag, strings.ReplaceAll(strings.Join(p.Need, ", "), "|", " or "), strings.Join(opt, ", "))
		if env != nil {
			st := "ready"
			if _, why := Live(p, env); why != "" { // no network: registry lookup and env only
				st = why
			}
			fmt.Fprintf(&b, " %s |", st)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// missing returns the first unmet env requirement ("" = all set).
func (p Provider) missing(env Env) string {
	for _, n := range p.Need {
		ok := false
		for _, alt := range strings.Split(n, "|") {
			ok = ok || env(alt) != ""
		}
		if !ok {
			return strings.ReplaceAll(n, "|", " or ")
		}
	}
	return ""
}

const (
	llmPrompt  = "You are a concise assistant. Answer in one short sentence."
	toolPrompt = "You must call get_weather to answer weather questions, then reply in one short sentence."
	badModel   = "provtest-no-such-model"
)

// Live resolves p against env into a Target built through the server's own
// registry, or says why it cannot run (skip reason).
func Live(p Provider, env Env) (Target, string) {
	if !server.NewRegistry().Supports(p.ID, string(p.Stage)) {
		if p.Tag != "" {
			return Target{}, "not in this build (needs -tags " + p.Tag + ")"
		}
		return Target{}, "no server factory " + p.ID + "." + string(p.Stage)
	}
	if m := p.missing(env); m != "" {
		return Target{}, "needs " + m
	}
	secret, err := p.Secret(env)
	if err != nil || len(secret) == 0 {
		return Target{}, "credential unavailable"
	}
	AddSecret(badKeyOf(secret))
	bad := badSecret(secret, p.BadKey)
	AddSecret(badKeyOf(bad))
	model := p.Model
	if p.ModelEnv != "" && env(p.ModelEnv) != "" {
		model = env(p.ModelEnv)
	}
	cfg := server.DefaultConfig(server.ProfileDevelopment)
	t := Target{Provider: p.ID, Stage: p.Stage, BadSecret: badKeyOf(bad),
		Classify: func(err error) string { return server.ClassifyStageError(string(p.Stage), err) }}
	t.Build = func(v Variant) (core.Processor, error) {
		spec := server.StageSpec{Provider: p.ID, Model: model, Language: p.Language, Voice: p.Voice, Secret: secret, SystemPrompt: llmPrompt}
		if v.BadCredential {
			spec.Secret = bad
		}
		if v.BadModel {
			spec.Model = badModel
		}
		if v.Tools != nil {
			spec.Tools, spec.ToolExecutor, spec.History, spec.SystemPrompt = v.Tools.Defs, v.Tools.Exec, v.Tools.History, toolPrompt
		}
		// A fresh registry per build: a bad-credential run must not open the breaker a later good build shares.
		return server.NewRegistry().BuildStage(cfg, string(p.Stage), spec)
	}
	return t, ""
}

// badSecret keeps the shape of the real credential (bare key or JSON) with an invalid key.
func badSecret(good []byte, key string) []byte {
	if key == "" {
		b := make([]byte, 12)
		_, _ = rand.Read(b)
		key = "bad-credential-" + hex.EncodeToString(b)
	}
	if t := strings.TrimSpace(string(good)); strings.HasPrefix(t, "{") {
		var m map[string]any
		if json.Unmarshal([]byte(t), &m) == nil {
			if _, ok := m["api_key"]; ok {
				m["api_key"] = key
				out, _ := json.Marshal(m)
				return out
			}
		}
	}
	return []byte(key)
}

// badKeyOf is the bare invalid key inside a (possibly JSON) bad secret, the
// literal an error must not echo.
func badKeyOf(bad []byte) string {
	var m map[string]any
	if json.Unmarshal(bad, &m) == nil {
		if k, ok := m["api_key"].(string); ok {
			return k
		}
	}
	return string(bad)
}
