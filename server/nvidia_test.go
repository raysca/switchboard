package server

import (
	"strings"
	"testing"

	"govoice/core"
	"govoice/providers/nvidia"
	"govoice/server/agent"
)

func TestNvidiaRegistration(t *testing.T) {
	reg := NewRegistry()
	if !reg.Supports("nvidia", "llm") || !reg.Supports("nvidia", "stt") || !reg.Supports("nvidia", "tts") {
		t.Fatal("nvidia offers llm, stt and tts")
	}
}

// Hosted endpoints only: a secret naming a base URL or a Riva endpoint (from an
// old stored credential) is not honoured, and every nvidia stage uses the one
// breaker of its hosted endpoint.
func TestNvidiaLLM(t *testing.T) {
	reg := NewRegistry()
	build := reg.factories["nvidia.llm"]
	p, err := build(&Config{}, StageSpec{Provider: "nvidia", Secret: []byte("nvapi-0123456789")})
	if err != nil {
		t.Fatal(err)
	}
	_ = p.(core.LLMCounters)
	if _, ok := reg.breakers.Load(circuitKey{"llm", "nvidia", "integrate.api.nvidia.com"}); !ok {
		t.Fatal("breaker not keyed by the NIM base URL")
	}
	self := []byte(`{"api_key":"k","base_url":"http://10.0.0.5:8000/v1"}`)
	if reg.llmBreaker(StageSpec{Provider: "nvidia", Secret: self}) != reg.llmBreaker(StageSpec{Provider: "nvidia", Secret: []byte("nvapi-0123456789")}) {
		t.Fatal("a base_url in the secret must not select a self-hosted NIM")
	}
	if _, err := build(&Config{}, StageSpec{Secret: []byte("two words")}); err == nil {
		t.Fatal("bad credential accepted")
	}
}

func TestNvidiaSTT(t *testing.T) {
	reg := NewRegistry()
	build := reg.factories["nvidia.stt"]
	if _, err := build(&Config{}, StageSpec{Secret: []byte(`{"api_key":"nvapi-0123456789"}`)}); err == nil {
		t.Fatal("hosted endpoint without function_id accepted")
	}
	p, err := build(&Config{}, StageSpec{Language: "en-US", Secret: []byte(`{"api_key":"nvapi-0123456789","function_id":"abc"}`)})
	if err != nil {
		t.Fatal(err)
	}
	p.(*nvidia.STTProcessor).Close()
	// An endpoint in the secret is ignored: no key, so the hosted endpoint refuses it.
	if _, err := build(&Config{}, StageSpec{Secret: []byte(`{"endpoint":"riva.internal:50051","use_tls":false}`)}); err == nil {
		t.Fatal("self-hosted Riva endpoint honoured")
	}
	reg.breakers.Range(func(k, _ any) bool {
		if strings.Contains(k.(circuitKey).endpoint, "riva.internal") {
			t.Fatal("breaker created for a self-hosted endpoint")
		}
		return true
	})
	if reg.breaker("stt", "nvidia") == nil {
		t.Fatal("no stt breaker for failover")
	}
}

func TestNvidiaTTS(t *testing.T) {
	reg := NewRegistry()
	build := reg.factories["nvidia.tts"]
	if _, err := build(&Config{}, StageSpec{Secret: []byte(`{"api_key":"nvapi-0123456789"}`)}); err == nil {
		t.Fatal("hosted endpoint without function_id accepted")
	}
	if _, err := build(&Config{}, StageSpec{Secret: []byte("two words")}); err == nil {
		t.Fatal("bad credential accepted")
	}
	if _, err := build(&Config{}, StageSpec{Secret: []byte(`{"endpoint":"riva.internal:50051","use_tls":false}`)}); err == nil {
		t.Fatal("self-hosted Riva endpoint honoured")
	}
	var cfg Config
	cfg.Providers.GoogleTTS.SampleRate = 16000 // any allowed pipeline rate builds (Riva resamples)
	p, err := build(&cfg, StageSpec{Voice: "English-US.Female-1", Failover: true, Secret: []byte(`{"api_key":"nvapi-0123456789","function_id":"abc"}`)})
	if err != nil {
		t.Fatal(err)
	}
	defer p.(*nvidia.TTSProcessor).Close()
	if _, ok := p.(core.TTSCounters); !ok {
		t.Fatal("nvidia TTS must export counters")
	}
	if reg.breaker("tts", "nvidia") == nil || reg.breaker("tts", "nvidia") == reg.breaker("stt", "nvidia") {
		t.Fatal("nvidia TTS needs its own breaker")
	}
	// NVCF hosts ASR and TTS as separate functions: tts_function_id wins for TTS.
	if c, _ := nvidiaSTTCredential([]byte(`{"api_key":"nvapi-0123456789","function_id":"asr","tts_function_id":" tts "}`)); c.FunctionID != "asr" || c.TTSFuncID != "tts" {
		t.Fatalf("credential: %+v", c)
	}
	if c, _ := nvidiaSTTCredential([]byte(`{"api_key":"nvapi-0123456789","function_id":"asr"}`)); c.TTSFuncID != "asr" {
		t.Fatal("tts_function_id falls back to function_id")
	}
	// ClientError rides the failover classification.
	if got := ttsClassify(&nvidia.ProviderError{Capability: "tts", Code: "Unauthenticated", ClientError: true}); got != agent.ClassClient {
		t.Fatalf("class=%q", got)
	}
}

// The default NIM model was live-checked (2026-10); the previous default
// (nemotron-70b) is listed by /v1/models yet answers 404, so changing this
// needs a live re-check (providers/openai -tags integration TestLiveNIM).
func TestNvidiaDefaultModelPinned(t *testing.T) {
	if nvidiaNIMModel != "openai/gpt-oss-20b" {
		t.Fatalf("default NIM model changed to %q: re-run the live NIM check", nvidiaNIMModel)
	}
}
