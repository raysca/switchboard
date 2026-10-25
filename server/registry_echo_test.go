package server

import (
	"strings"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/audio/dtmf"
	"govoice/providers/echo"
	"govoice/providers/google"
	"govoice/server/runtime"
)

func TestEchoIsRegisteredOnEveryBuild(t *testing.T) {
	reg := NewRegistry()
	for _, stage := range []string{"stt", "llm", "tts"} {
		if !reg.Supports("echo", stage) {
			t.Errorf("echo.%s is not registered", stage)
		}
	}
}

func TestGoogleFactoriesAreUntouched(t *testing.T) {
	reg := NewRegistry()
	// The google factories are the real ones: they still read a credential from the secret, and say
	// so under their own name. (Echo's factories need none and never say "google".)
	for _, stage := range []string{"stt", "llm", "tts"} {
		_, err := reg.factories["google."+stage](DefaultConfig(ProfileDevelopment), StageSpec{Voice: "x"})
		if err == nil || !strings.HasPrefix(err.Error(), "google."+stage+":") {
			t.Errorf("google.%s with no secret: %v, want an error naming google.%s", stage, err, stage)
		}
	}
}

func TestEchoTTSChoosesStreamingLikeGoogle(t *testing.T) {
	f := NewRegistry().factories["echo.tts"]
	for _, tc := range []struct {
		voice     string
		disable   bool
		streaming bool
	}{
		{"en-US-Chirp3-HD-Aoede", false, true},
		{"en-US-Chirp3-HD-Aoede", true, false},
		{"en-US-Journey-F", false, false},
		{"", false, true},
	} {
		cfg := DefaultConfig(ProfileDevelopment)
		cfg.Providers.GoogleTTS.DisableStreaming = tc.disable
		p, err := f(cfg, StageSpec{Voice: tc.voice})
		if err != nil {
			t.Fatal(err)
		}
		_, streaming := p.(*google.StreamingTTSProcessor)
		_, unary := p.(*google.TTSProcessor)
		if streaming != tc.streaming || unary == tc.streaming {
			t.Errorf("voice %q disable=%v built %T", tc.voice, tc.disable, p)
		}
	}
}

func TestEchoBrokenVoiceFailsToBuild(t *testing.T) {
	if _, err := NewRegistry().factories["echo.tts"](DefaultConfig(ProfileDevelopment), StageSpec{Voice: "brokenvoice"}); err == nil {
		t.Fatal("brokenvoice must not build: the e2e suites use it to fail a handoff")
	}
}

func TestEchoStagesBuildWithoutAnySecret(t *testing.T) {
	reg := NewRegistry()
	cfg := DefaultConfig(ProfileDevelopment)
	for _, stage := range []string{"stt", "llm", "tts"} {
		p, err := reg.factories["echo."+stage](cfg, StageSpec{Model: "m", Voice: "v"})
		if err != nil || p == nil {
			t.Fatalf("echo.%s: %v", stage, err)
		}
	}
}

func TestEchoLLMWiresTransferEndCallAndMCPPhrases(t *testing.T) {
	name := runtime.ToolName("01a12f27-7f90-792f-85e5-6fd6c949fe6b", "lookup_order")
	phrases, spoken := mcpToolPhrases([]core.ToolDefinition{{Name: name, RemoteName: "lookup_order"}, {Name: "transfer_to_sales"}})
	if phrases["call tool lookup_order"] != name || spoken[name] != "lookup_order" || len(phrases) != 1 {
		t.Fatalf("phrases=%v spoken=%v", phrases, spoken)
	}
	opts := echoLLMOptions(DefaultConfig(ProfileDevelopment), StageSpec{
		SystemPrompt: "front desk",
		Tools:        []core.ToolDefinition{{Name: name, RemoteName: "lookup_order"}, {Name: "transfer_to_sales"}, {Name: endCallName}, {Name: dtmf.ToolName}},
	})
	for phrase, tool := range map[string]string{"transfer to sales": "transfer_to_sales", "end the call": endCallName, "call tool lookup_order": name, "call tool send_dtmf": dtmf.ToolName} {
		if opts.Phrases[phrase] != tool {
			t.Errorf("phrase %q -> %q, want %q (all: %v)", phrase, opts.Phrases[phrase], tool, opts.Phrases)
		}
	}
	if opts.Label != "" {
		t.Fatalf("label %q without label_replies", opts.Label)
	}
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Providers.Echo.LabelReplies = true
	if got := echoLLMOptions(cfg, StageSpec{SystemPrompt: "front desk"}).Label; got != "[front desk] " {
		t.Fatalf("label %q", got)
	}
}

func TestEchoMimicSelectsFiguresAndExtraDelay(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	if got := echoLLMOptions(cfg, StageSpec{}).Figures.FirstToken; got != 700*time.Millisecond {
		t.Fatalf("default mimic is google, first token %v", got)
	}
	cfg.Providers.Echo.Mimic = "instant"
	cfg.Providers.Echo.LLMExtraDelay = 100 * time.Millisecond
	o := echoLLMOptions(cfg, StageSpec{})
	if o.Figures != (echo.LLMFigures{}) || o.ExtraDelay != 100*time.Millisecond {
		t.Fatalf("instant options %+v", o)
	}
}

func TestEchoConfigDefaultsAndValidation(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	e := cfg.Providers.Echo
	if e.Mimic != "google" || e.STT != "text" || e.Audible || e.LabelReplies || e.LLMExtraDelay != 0 {
		t.Fatalf("defaults %+v", e)
	}
	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}
	for field, mutate := range map[string]func(*Config){
		"providers.echo.mimic":           func(c *Config) { c.Providers.Echo.Mimic = "x" },
		"providers.echo.stt":             func(c *Config) { c.Providers.Echo.STT = "y" },
		"providers.echo.llm_extra_delay": func(c *Config) { c.Providers.Echo.LLMExtraDelay = -time.Second },
	} {
		c := DefaultConfig(ProfileDevelopment)
		mutate(c)
		if err := ValidateConfig(c); err == nil || !strings.Contains(err.Error(), field) {
			t.Errorf("%s: error %v", field, err)
		}
	}
}

// Failover must treat echo like Google: skip a stage whose breaker is open, record a first-token hang.
func TestEchoStagesShareTheirBreakersWithFailover(t *testing.T) {
	r := NewRegistry()
	if r.breaker("stt", "echo") == nil || r.breaker("tts", "echo") == nil || r.llmBreaker(StageSpec{Provider: "echo"}) != r.breaker("llm", "echo") {
		t.Fatal("failover must find echo's own breakers")
	}
	if r.breaker("stt", "echo") == r.breaker("stt", "google") || r.breaker("tts", "echo") == r.breaker("tts", "google") || r.breaker("llm", "echo") == r.breaker("llm", "google") {
		t.Fatal("echo must not share Google's breakers")
	}
}

func TestSTTConfigHonoursRotationAndSpec(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Providers.GoogleSTT.RotationThreshold = 90 * time.Second
	got := googleSTTConfig(cfg, StageSpec{Model: "latest_long", Language: "en-GB"}, nil)
	if got.StreamRotationThreshold != 90*time.Second || got.Model != "latest_long" || got.LanguageCode != "en-GB" {
		t.Fatalf("config %+v", got)
	}
}

// The phrase comes from the tool's own remote name, wherever it was bound, not from parsing the
// model-facing name (whose format is the runtime's business).
func TestMCPToolPhrasesUseTheRemoteNameNotTheToolNameFormat(t *testing.T) {
	phrases, spoken := mcpToolPhrases([]core.ToolDefinition{
		{Name: "anything_at_all", RemoteName: "Find_Slots"},
		{Name: "mcp__abcd1234_looks_like_one00000000"}, // no RemoteName: not an MCP tool, whatever it looks like
	})
	if phrases["call tool find_slots"] != "anything_at_all" || spoken["anything_at_all"] != "Find_Slots" || len(phrases) != 1 || len(spoken) != 1 {
		t.Fatalf("phrases=%v spoken=%v", phrases, spoken)
	}
}
