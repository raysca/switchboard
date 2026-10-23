package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/coretest"
	coreturn "govoice/core/turn"
	"govoice/server/agent"
	"govoice/smartturn"
	"govoice/vad"
)

func ptr[T any](v T) *T { return &v }

func TestTurnForCall(t *testing.T) {
	server := func() TurnConfig {
		return TurnConfig{Manager: true, MinWords: 4, Mute: []string{"first_speech"}, MaxMute: 20 * time.Second}
	}
	withCompletion := func() TurnConfig {
		return TurnConfig{Manager: true, Stop: []string{"llm-completion", "speech-timeout"}}
	}
	cases := []struct {
		name  string
		srv   func() TurnConfig
		agent *agent.TurnSettings
		want  func(TurnConfig) TurnConfig
	}{
		{"absent is the server's", server, nil, func(c TurnConfig) TurnConfig { return c }},
		{"min words alone", server, &agent.TurnSettings{MinWords: ptr(2)}, func(c TurnConfig) TurnConfig { c.MinWords = 2; return c }},
		{"min words 0 turns the filter off", server, &agent.TurnSettings{MinWords: ptr(0)}, func(c TurnConfig) TurnConfig { c.MinWords = 0; return c }},
		{"mute replaces the list", server, &agent.TurnSettings{Mute: &[]string{"always"}}, func(c TurnConfig) TurnConfig { c.Mute = []string{"always"}; return c }},
		{"empty mute means none", server, &agent.TurnSettings{Mute: &[]string{}}, func(c TurnConfig) TurnConfig { c.Mute = []string{}; return c }},
		{"max mute tightens", server, &agent.TurnSettings{MaxMuteMS: ptr(5000)}, func(c TurnConfig) TurnConfig { c.MaxMute = 5 * time.Second; return c }},
		{"max mute above the server's is clamped", server, &agent.TurnSettings{MaxMuteMS: ptr(40000)}, func(c TurnConfig) TurnConfig { return c }},
		{"max mute against the default 30 s when the server sets none", func() TurnConfig { return TurnConfig{Manager: true} }, &agent.TurnSettings{MaxMuteMS: ptr(40000)},
			func(c TurnConfig) TurnConfig { c.MaxMute = 30 * time.Second; return c }},
		{"max mute under the default when the server sets none", func() TurnConfig { return TurnConfig{Manager: true} }, &agent.TurnSettings{MaxMuteMS: ptr(9000)},
			func(c TurnConfig) TurnConfig { c.MaxMute = 9 * time.Second; return c }},
		{"members are independent", server, &agent.TurnSettings{MinWords: ptr(1), Mute: &[]string{"function_call"}, MaxMuteMS: ptr(1000)},
			func(c TurnConfig) TurnConfig {
				c.MinWords, c.Mute, c.MaxMute = 1, []string{"function_call"}, time.Second
				return c
			}},
		{"speech timeout", server, &agent.TurnSettings{SpeechTimeoutMS: ptr(900)}, func(c TurnConfig) TurnConfig { c.SpeechTimeout = 900 * time.Millisecond; return c }},
		{"default on a server with no chain stays empty", server, &agent.TurnSettings{EndOfTurn: "default"}, func(c TurnConfig) TurnConfig { return c }},
		{"smart", server, &agent.TurnSettings{EndOfTurn: "smart"}, func(c TurnConfig) TurnConfig { c.Stop = []string{"turn-analyzer", "speech-timeout"}; return c }},
		{"eager", server, &agent.TurnSettings{EndOfTurn: "eager"}, func(c TurnConfig) TurnConfig { c.Stop = []string{"eager", "speech-timeout"}; return c }},
		{"smart hold", server, &agent.TurnSettings{EndOfTurn: "smart", SmartHoldMS: ptr(2000)}, func(c TurnConfig) TurnConfig {
			c.Stop, c.Analyzer.Hold = []string{"turn-analyzer", "speech-timeout"}, 2*time.Second
			return c
		}},
		{"eager keeps the server's llm-completion", withCompletion, &agent.TurnSettings{EndOfTurn: "eager"}, func(c TurnConfig) TurnConfig {
			c.Stop = []string{"eager", "speech-timeout", "llm-completion"}
			return c
		}},
		{"smart keeps the server's llm-completion", withCompletion, &agent.TurnSettings{EndOfTurn: "smart"}, func(c TurnConfig) TurnConfig {
			c.Stop = []string{"turn-analyzer", "speech-timeout", "llm-completion"}
			return c
		}},
		{"default keeps llm-completion and the speech-timeout it needs", withCompletion, &agent.TurnSettings{EndOfTurn: "default"}, func(c TurnConfig) TurnConfig {
			c.Stop = []string{"speech-timeout", "llm-completion"}
			return c
		}},
		{"timeout alone keeps the server's chain", withCompletion, &agent.TurnSettings{SpeechTimeoutMS: ptr(900)}, func(c TurnConfig) TurnConfig { c.SpeechTimeout = 900 * time.Millisecond; return c }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := c.srv()
			got := srv.forCall(c.agent)
			want := c.want(c.srv())
			if got.MinWords != want.MinWords || got.MaxMute != want.MaxMute || got.Manager != want.Manager || !slices.Equal(got.Mute, want.Mute) ||
				got.SpeechTimeout != want.SpeechTimeout || !slices.Equal(got.Stop, want.Stop) || got.Analyzer.Hold != want.Analyzer.Hold {
				t.Fatalf("got %+v, want %+v", got, want)
			}
			if !slices.Equal(srv.Mute, c.srv().Mute) || srv.MinWords != c.srv().MinWords || srv.MaxMute != c.srv().MaxMute || !slices.Equal(srv.Stop, c.srv().Stop) {
				t.Fatalf("the server's config changed: %+v", srv)
			}
		})
	}
}

// Every chain forCall produces must build, with or without the server's llm-completion entry: a chain of
// only [llm-completion] is the trap (turn.stop must include speech-timeout).
func TestTurnForCallChainsAreAcceptedByTheStopDetector(t *testing.T) {
	model := func(time.Duration) coreturn.UserTurnStopStrategy { return nil }
	for _, srvStop := range [][]string{nil, {"speech-timeout"}, {"llm-completion", "speech-timeout"}, {"eager", "speech-timeout", "llm-completion"}} {
		for _, eot := range []string{"", "default", "smart", "eager"} {
			got := TurnConfig{Manager: true, Stop: srvStop}.forCall(&agent.TurnSettings{EndOfTurn: eot})
			if _, err := turnStopDetector(got.Stop, coreturn.SpeechTimeoutConfig{}, 0, model); err != nil {
				t.Errorf("server %v + %q -> %v: %v", srvStop, eot, got.Stop, err)
			}
		}
	}
	if _, err := turnStopDetector([]string{"llm-completion"}, coreturn.SpeechTimeoutConfig{}, 0, nil); err == nil || !strings.Contains(err.Error(), "must include speech-timeout") {
		t.Fatalf("the trap this guards against is gone: %v", err)
	}
}

func TestLoadsSmartTurn(t *testing.T) {
	var c Config
	if c.LoadsSmartTurn() {
		t.Fatal("an empty config loads the model")
	}
	c.Turn.Stop = []string{"turn-analyzer", "speech-timeout"}
	if !c.LoadsSmartTurn() {
		t.Fatal("a smart server default must load the model")
	}
	c.Turn.Stop, c.Turn.Analyzer.ForAgents = nil, true
	if !c.LoadsSmartTurn() {
		t.Fatal("for_agents must load the model")
	}
	if !policyFrom(&c).SmartTurn() {
		t.Fatal("the policy must say smart is available when the model is loaded")
	}
}

func TestForAgentsIsValidatedLikeASmartDefault(t *testing.T) {
	for name, mut := range map[string]func(*Config){
		"bad mode":        func(c *Config) { c.Turn.Analyzer.Mode = "thread" },
		"negative budget": func(c *Config) { c.Turn.Analyzer.Budget = -1 },
		"manager off":     func(c *Config) { c.Turn.Manager = false },
	} {
		cfg := DefaultConfig(ProfileDevelopment)
		cfg.Turn.Analyzer.ForAgents = true
		mut(cfg)
		if err := validateTurn(cfg); err == nil {
			t.Errorf("%s: for_agents accepted", name)
		}
	}
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Turn.Analyzer.ForAgents = true
	if err := validateTurn(cfg); err != nil {
		t.Fatalf("a valid for_agents config: %v", err)
	}
}

func TestTurnForCallDoesNotAliasOrMutateTheServersConfig(t *testing.T) {
	srv := TurnConfig{Manager: true, Mute: []string{"always", "function_call"}}
	for _, f := range []*agent.TurnSettings{nil, {MinWords: ptr(1)}, {Mute: &[]string{"first_speech"}}} {
		got := srv.forCall(f)
		if len(got.Mute) > 0 {
			got.Mute[0] = "scribbled"
		}
	}
	if !slices.Equal(srv.Mute, []string{"always", "function_call"}) {
		t.Fatalf("the shared server slice was written through: %v", srv.Mute)
	}
	agentMute := []string{"always"}
	got := srv.forCall(&agent.TurnSettings{Mute: &agentMute})
	got.Mute[0] = "scribbled"
	if agentMute[0] != "always" {
		t.Fatal("the result aliases the flow's slice")
	}
}

func TestTurnForCallIsRaceFreeOnOneSharedConfig(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Turn.Mute = []string{"first_speech"}
	f := &agent.TurnSettings{MinWords: ptr(2), Mute: &[]string{"always"}, MaxMuteMS: ptr(5000)}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				tc := cfg.Turn.forCall(f)
				tc.Mute[0] = "x"
				tc.MinWords = i
				_ = cfg.Turn.forCall(nil)
			}
		}()
	}
	wg.Wait()
}

func TestServerConfigSendsTheEffectiveTurnSettings(t *testing.T) {
	get := func(s *Server) (out struct {
		Manager         bool     `json:"manager"`
		MinWords        int      `json:"min_words"`
		Mute            []string `json:"mute"`
		MaxMuteMS       int      `json:"max_mute_ms"`
		SpeechTimeoutMS int      `json:"speech_timeout_ms"`
		EndOfTurn       string   `json:"end_of_turn"`
		SmartAvailable  bool     `json:"smart_turn_available"`
		SmartHoldMS     int      `json:"smart_hold_ms"`
	}) {
		t.Helper()
		w := apiRequest(s, http.MethodGet, "/api/v1/server-config", "management-test-key-32-bytes-long-and-separate", "")
		var env struct {
			Data struct {
				Turn json.RawMessage `json:"turn"`
			} `json:"data"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &env) != nil || json.Unmarshal(env.Data.Turn, &out) != nil {
			t.Fatalf("status %d body %s", w.Code, w.Body.String())
		}
		if string(env.Data.Turn) == "" || !json.Valid(env.Data.Turn) {
			t.Fatalf("no turn block: %s", w.Body.String())
		}
		return out
	}
	got := get(apiTestServer(t))
	if !got.Manager || got.SpeechTimeoutMS != 600 || got.MaxMuteMS != 30000 || got.MinWords != 0 || got.Mute == nil || len(got.Mute) != 0 ||
		got.EndOfTurn != "default" || got.SmartAvailable || got.SmartHoldMS != 3000 {
		t.Fatalf("defaults must be the effective values, never a raw 0: %+v", got)
	}
	t.Setenv("GOVOICE_MANAGEMENT_API_KEY", "management-test-key-32-bytes-long-and-separate")
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Token.Secret = "voice-test-secret-32-bytes-long-and-separate"
	cfg.Turn.MinWords, cfg.Turn.Mute, cfg.Turn.MaxMute, cfg.Turn.SpeechTimeout = 3, []string{"always"}, 12*time.Second, 900*time.Millisecond
	cfg.Turn.Stop, cfg.Turn.Analyzer.Hold = []string{"eager", "speech-timeout"}, 2*time.Second
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.env = testProviderEnv(t)
	got = get(s)
	if got.MinWords != 3 || !slices.Equal(got.Mute, []string{"always"}) || got.MaxMuteMS != 12000 || got.SpeechTimeoutMS != 900 || got.EndOfTurn != "eager" || got.SmartHoldMS != 2000 {
		t.Fatalf("configured values: %+v", got)
	}
}

func TestServerConfigSaysWhetherSmartTurnIsAvailable(t *testing.T) {
	t.Setenv("GOVOICE_MANAGEMENT_API_KEY", "management-test-key-32-bytes-long-and-separate")
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Token.Secret = "voice-test-secret-32-bytes-long-and-separate"
	cfg.Turn.Analyzer.ForAgents = true
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.env = testProviderEnv(t)
	w := apiRequest(s, http.MethodGet, "/api/v1/server-config", "management-test-key-32-bytes-long-and-separate", "")
	var env struct {
		Data struct {
			Turn struct {
				EndOfTurn      string `json:"end_of_turn"`
				SmartAvailable bool   `json:"smart_turn_available"`
			}
		} `json:"data"`
	}
	if json.Unmarshal(w.Body.Bytes(), &env) != nil || !env.Data.Turn.SmartAvailable || env.Data.Turn.EndOfTurn != "default" {
		t.Fatalf("for_agents keeps the default chain but offers smart: %s", w.Body.String())
	}
}

type markedExecutor struct{}

func (*markedExecutor) Execute(context.Context, core.ToolCall) (core.ToolResult, error) {
	return core.ToolResult{}, nil
}

func turnTestBuild(t *testing.T, cfg *Config, call CallSpec, model ...smartturn.Backend) (*core.Pipeline, StageSpec, error) {
	t.Helper()
	var llm StageSpec
	reg := NewRegistry()
	for _, b := range model {
		reg.SetTurnBackend(b)
	}
	for _, k := range []string{"google.stt", "google.tts"} {
		reg.Register(k, func(*Config, StageSpec) (core.Processor, error) { return &coretest.PassthroughProcessor{}, nil })
	}
	reg.Register("google.llm", func(_ *Config, s StageSpec) (core.Processor, error) {
		llm = s
		return &coretest.PassthroughProcessor{}, nil
	})
	vadProc := vad.NewProcessor(&mockVADExecutor{ready: true, available: 1, total: 1}, vad.DefaultProcessorConfig())
	call.STT, call.TTS = StageSpec{Provider: "google"}, StageSpec{Provider: "google"}
	call.LLM = StageSpec{Provider: "google", ToolExecutor: &markedExecutor{}}
	pipe, err := reg.BuildCallPipeline(cfg, call, vadProc, &fakeTransportSink{})
	return pipe, llm, err
}

func TestBuildCallPipelineUsesTheAgentsTurnSettings(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment) // the server has no mute list
	cfg.Idle.UserTimeout = 0                 // idle also wraps the tools: keep it out of the way
	if _, llm, err := turnTestBuild(t, cfg, CallSpec{}); err != nil || llm.ToolExecutor == nil {
		t.Fatalf("baseline: %v", err)
	} else if _, wrapped := llm.ToolExecutor.(*markedExecutor); !wrapped {
		t.Fatal("tools are wrapped although nothing asks for function_call mute")
	}
	// The flow's list is the one parsed: an unknown name only fails the build if it is the flow's.
	if _, _, err := turnTestBuild(t, cfg, CallSpec{Turn: &agent.TurnSettings{Mute: &[]string{"nope"}}}); err == nil {
		t.Fatal("the flow's mute list was not used")
	}
	if _, llm, err := turnTestBuild(t, cfg, CallSpec{Turn: &agent.TurnSettings{Mute: &[]string{"function_call"}}}); err != nil {
		t.Fatal(err)
	} else if _, wrapped := llm.ToolExecutor.(*markedExecutor); wrapped {
		t.Fatal("function_call from the flow did not create the tool activity")
	}
	// Push-to-talk keeps the flow's mute; a server with the manager off ignores the rest.
	if _, _, err := turnTestBuild(t, cfg, CallSpec{PushToTalk: true, Turn: &agent.TurnSettings{MinWords: ptr(3), Mute: &[]string{"nope"}}}); err == nil {
		t.Fatal("push-to-talk dropped the flow's mute list")
	}
	// The flow's chain is the one built: smart needs the model, and fails the build without it (readiness
	// keeps such a flow off the line); eager and a timeout need nothing.
	smart := CallSpec{Turn: &agent.TurnSettings{EndOfTurn: "smart", SmartHoldMS: ptr(1500)}}
	if _, _, err := turnTestBuild(t, cfg, smart); err == nil || !strings.Contains(err.Error(), "smart-turn model is not loaded") {
		t.Fatalf("smart without the model: %v", err)
	}
	if _, _, err := turnTestBuild(t, cfg, smart, smartBackend{p: 0.9}); err != nil {
		t.Fatalf("smart with the model: %v", err)
	}
	if _, _, err := turnTestBuild(t, cfg, CallSpec{Turn: &agent.TurnSettings{EndOfTurn: "eager", SpeechTimeoutMS: ptr(900)}}); err != nil {
		t.Fatalf("eager: %v", err)
	}
	off := DefaultConfig(ProfileDevelopment)
	off.Idle.UserTimeout, off.Turn.Manager = 0, false
	if _, _, err := turnTestBuild(t, off, CallSpec{Turn: &agent.TurnSettings{MinWords: ptr(3)}}); err != nil {
		t.Fatalf("manager off must ignore the flow's turn settings: %v", err)
	}
}

func TestTurnSettingsIgnoredWarningIsLoggedOncePerAgentVersion(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	reg := NewRegistry()
	for _, k := range []string{"google.stt", "google.tts", "google.llm"} {
		reg.Register(k, func(*Config, StageSpec) (core.Processor, error) { return &coretest.PassthroughProcessor{}, nil })
	}
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Idle.UserTimeout, cfg.Turn.Manager = 0, false
	build := func(version string) {
		t.Helper()
		call := CallSpec{AgentVersionID: version, Turn: &agent.TurnSettings{MinWords: ptr(3)},
			STT: StageSpec{Provider: "google"}, TTS: StageSpec{Provider: "google"}, LLM: StageSpec{Provider: "google"}}
		vadProc := vad.NewProcessor(&mockVADExecutor{ready: true, available: 1, total: 1}, vad.DefaultProcessorConfig())
		if _, err := reg.BuildCallPipeline(cfg, call, vadProc, &fakeTransportSink{}); err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		build("v1")
	}
	if n := strings.Count(buf.String(), "turn.manager is off"); n != 1 {
		t.Fatalf("warned %d times for one flow version, want 1: %s", n, buf.String())
	}
	build("v2")
	if n := strings.Count(buf.String(), "turn.manager is off"); n != 2 {
		t.Fatalf("a new flow version must warn once more: %d", n)
	}
}

func TestCallStopWaitsForTheAgentsSpeechTimeout(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Turn.SpeechTimeout = 600 * time.Millisecond // the server's own; the flow's 900 ms must win
	vadStop := cfg.vadStop(0)
	ends := func(call CallSpec, silence time.Duration) bool {
		t.Helper()
		call.STT = StageSpec{Provider: "google"}
		stop, _, err := NewRegistry().callStop(cfg, call)
		if err != nil {
			t.Fatal(err)
		}
		_, done := stop.Detect(coreturn.DetectionInput{SilenceFor: silence, Text: "hello there", TextFinal: true})
		return done
	}
	agentCall := CallSpec{Turn: &agent.TurnSettings{SpeechTimeoutMS: ptr(900)}}
	if ends(agentCall, vadStop+899*time.Millisecond) {
		t.Fatal("the turn ended before the flow's 900 ms resume window")
	}
	if !ends(agentCall, vadStop+900*time.Millisecond) {
		t.Fatal("the turn did not end at the flow's 900 ms resume window")
	}
	if !ends(CallSpec{}, vadStop+600*time.Millisecond) || ends(CallSpec{}, vadStop+599*time.Millisecond) {
		t.Fatal("a flow without the setting must keep the server's 600 ms")
	}
}
