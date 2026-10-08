package server

import (
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

func TestNvidiaLLM(t *testing.T) {
	reg := NewRegistry()
	build := reg.factories["nvidia.llm"]
	// Bare key: NIM default URL and model, no private access.
	p, err := build(&Config{}, StageSpec{Provider: "nvidia", Secret: []byte("nvapi-0123456789")})
	if err != nil {
		t.Fatal(err)
	}
	_ = p.(core.LLMCounters)
	if _, ok := reg.nvidiaBreakers.Load("llm|" + nvidiaNIMBaseURL); !ok {
		t.Fatal("breaker not keyed by default base URL")
	}
	// Self-hosted NIM: refused until the operator opts in; breaker per base URL.
	nim := StageSpec{Secret: []byte(`{"api_key":"k","base_url":"http://10.0.0.5:8000/v1"}`)}
	if _, err := build(&Config{}, nim); err == nil {
		t.Fatal("private http base_url must be refused by default")
	}
	var opt Config
	opt.Providers.Nvidia.AllowPrivate = true
	if _, err := build(&opt, nim); err != nil {
		t.Fatal(err)
	}
	if reg.llmBreaker(StageSpec{Provider: "nvidia", Secret: nim.Secret}) == reg.llmBreaker(StageSpec{Provider: "nvidia", Secret: []byte("nvapi-0123456789")}) {
		t.Fatal("nvidia breakers must be scoped per base URL")
	}
	if _, err := build(&Config{}, StageSpec{Secret: []byte("two words")}); err == nil {
		t.Fatal("bad credential accepted")
	}
}

func TestNvidiaSTT(t *testing.T) {
	reg := NewRegistry()
	build := reg.factories["nvidia.stt"]
	// Hosted: needs function_id.
	if _, err := build(&Config{}, StageSpec{Secret: []byte("nvapi-0123456789")}); err == nil {
		t.Fatal("hosted endpoint without function_id accepted")
	}
	p, err := build(&Config{}, StageSpec{Language: "en-US", Secret: []byte(`{"api_key":"nvapi-0123456789","function_id":"abc"}`)})
	if err != nil {
		t.Fatal(err)
	}
	p.(*nvidia.STTProcessor).Close()
	// Self-hosted plaintext Riva needs no key, but needs the operator opt-in.
	self := StageSpec{Secret: []byte(`{"endpoint":"riva.internal:50051","use_tls":false}`)}
	if _, err := build(&Config{}, self); err == nil {
		t.Fatal("plaintext endpoint accepted without allow_private")
	}
	var opt Config
	opt.Providers.Nvidia.AllowPrivate = true
	p, err = build(&opt, self)
	if err != nil {
		t.Fatal(err)
	}
	p.(*nvidia.STTProcessor).Close()
	if br, ok := reg.nvidiaBreakers.Load("stt|riva.internal:50051"); !ok || br != reg.nvidiaSTTBreaker(self.Secret) {
		t.Fatal("stt breaker not scoped per endpoint")
	}
	if reg.nvidiaSTTBreaker(self.Secret) == reg.nvidiaSTTBreaker([]byte("nvapi-0123456789")) {
		t.Fatal("failover check must use the endpoint's breaker")
	}
	if reg.sttBreaker("nvidia") == nil {
		t.Fatal("no stt breaker for failover")
	}
}

func TestNvidiaTTS(t *testing.T) {
	reg := NewRegistry()
	build := reg.factories["nvidia.tts"]
	if _, err := build(&Config{}, StageSpec{Secret: []byte("nvapi-0123456789")}); err == nil {
		t.Fatal("hosted endpoint without function_id accepted")
	}
	if _, err := build(&Config{}, StageSpec{Secret: []byte("two words")}); err == nil {
		t.Fatal("bad credential accepted")
	}
	// Any allowed pipeline rate builds (Riva output is resampled); self-hosted needs no key.
	var cfg Config
	cfg.Providers.GoogleTTS.SampleRate = 16000
	cfg.Providers.Nvidia.AllowPrivate = true // plaintext Riva needs the opt-in
	p, err := build(&cfg, StageSpec{Voice: "English-US.Female-1", Secret: []byte(`{"endpoint":"riva.internal:50051","use_tls":false}`)})
	if err != nil {
		t.Fatal(err)
	}
	defer p.(*nvidia.TTSProcessor).Close()
	if _, ok := p.(core.TTSCounters); !ok {
		t.Fatal("nvidia TTS must export counters")
	}
	if _, ok := reg.nvidiaBreakers.Load("tts|riva.internal:50051"); !ok {
		t.Fatal("tts breaker not scoped per endpoint")
	}
	if reg.ttsBreaker("nvidia") == nil || reg.ttsBreaker("nvidia") == reg.sttBreaker("nvidia") {
		t.Fatal("nvidia TTS needs its own breaker")
	}
	self := []byte(`{"endpoint":"riva.internal:50051","use_tls":false}`)
	if br, _ := reg.nvidiaBreakers.Load("tts|riva.internal:50051"); br != reg.nvidiaRivaBreaker("tts", self) || br == reg.ttsBreaker("nvidia") {
		t.Fatal("failover check must use the endpoint's tts breaker")
	}
	// NVCF hosts ASR and TTS as separate functions: tts_function_id wins for TTS.
	if c, _ := nvidiaSTTCredential([]byte(`{"api_key":"nvapi-0123456789","function_id":"asr","tts_function_id":" tts "}`)); c.FunctionID != "asr" || c.TTSFuncID != "tts" {
		t.Fatalf("credential: %+v", c)
	}
	if c, _ := nvidiaSTTCredential([]byte(`{"api_key":"nvapi-0123456789","function_id":"asr"}`)); c.TTSFuncID != "asr" {
		t.Fatal("tts_function_id falls back to function_id")
	}
	if _, err := build(&Config{}, StageSpec{Secret: []byte(`{"endpoint":"riva.internal:50051","use_tls":false}`)}); err == nil {
		t.Fatal("plaintext without allow_private accepted")
	}
	hosted, err := build(&Config{}, StageSpec{Failover: true, Secret: []byte(`{"api_key":"nvapi-0123456789","function_id":"abc"}`)})
	if err != nil {
		t.Fatal(err)
	}
	hosted.(*nvidia.TTSProcessor).Close()
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
