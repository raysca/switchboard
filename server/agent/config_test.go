package agent

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestDecodeConfigIsStrictButAllowsDrafts(t *testing.T) {
	for _, bad := range []string{`[]`, `{"stt":"google"}`, `{"pipeline":"default"}`, `{"llm":{"provider":"google","frequency_penalty":1}}`, `{"context":{"max_tokens":-1}}`} {
		if _, err := DecodeConfig(json.RawMessage(bad)); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("%s: err=%v, want ErrInvalidConfig", bad, err)
		}
	}
	draft, err := DecodeConfig(json.RawMessage(`{"llm":{"provider":" Google "}}`))
	if err != nil {
		t.Fatal(err)
	}
	if draft.LLM.Provider != "google" || !reflect.DeepEqual(draft.Missing(), []string{"missing_stt", "missing_llm", "missing_tts"}) {
		t.Fatalf("draft=%+v missing=%v", draft, draft.Missing())
	}
}

func TestContextLimitsDefaultAndBounds(t *testing.T) {
	if _, err := DecodeConfig(json.RawMessage(`{"context":{"max_messages":1}}`)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("max_messages=1 accepted: %v", err)
	}
	m, k := ContextLimits{}.Limits()
	if m != DefaultMaxMessages || k != DefaultMaxTokens {
		t.Fatalf("unset limits = %d/%d, want defaults", m, k)
	}
	if m, k := (ContextLimits{MaxMessages: 8, MaxTokens: 500}).Limits(); m != 8 || k != 500 {
		t.Fatalf("explicit limits = %d/%d", m, k)
	}
}

func TestDecodeConfigAsyncTools(t *testing.T) {
	c, err := DecodeConfig(json.RawMessage(`{"async_tools":["srv-1/long_report"]}`))
	if err != nil || !c.IsAsyncTool("srv-1", "long_report") || c.IsAsyncTool("srv-1", "other") {
		t.Fatalf("cfg=%+v err=%v", c, err)
	}
	if c, _ := DecodeConfig(json.RawMessage(`{}`)); c.IsAsyncTool("srv-1", "long_report") {
		t.Fatal("tools are synchronous by default")
	}
	for _, bad := range []string{`{"async_tools":["noslash"]}`, `{"async_tools":["/x"]}`, `{"async_tools":["x/"]}`, `{"async_tools":["a/b","a/b"]}`, `{"async_tools":"a/b"}`} {
		if _, err := DecodeConfig(json.RawMessage(bad)); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("%s accepted: %v", bad, err)
		}
	}
}

func TestDecodeConfigSummarization(t *testing.T) {
	ok := func(raw string) {
		t.Helper()
		if _, err := DecodeConfig(json.RawMessage(raw)); err != nil {
			t.Errorf("%s: %v", raw, err)
		}
	}
	bad := func(raw string) {
		t.Helper()
		if _, err := DecodeConfig(json.RawMessage(raw)); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("%s: err=%v, want ErrInvalidConfig", raw, err)
		}
	}
	ok(`{"llm":{"provider":"google"}}`) // off by default
	ok(`{"llm":{"provider":"openai"},"context":{"max_tokens":2000,"summarization":{"enabled":true,"trigger_tokens":1500,"keep_turns":3}}}`)
	ok(`{"llm":{"provider":"xai"},"context":{"summarization":{"enabled":true}}}`)     // trigger defaults to 3/4 of max
	ok(`{"llm":{"provider":"echo"},"context":{"summarization":{"enabled":true}}}`)    // echo runs the real Google LLM processor, which summarizes
	ok(`{"llm":{"provider":"offline"},"context":{"summarization":{"enabled":true}}}`) // offline runs the xai LLM processor against the local endpoint
	bad(`{"llm":{"provider":"google"},"context":{"max_tokens":2000,"summarization":{"enabled":true,"trigger_tokens":2000}}}`)
	bad(`{"llm":{"provider":"google"},"context":{"summarization":{"enabled":true,"trigger_tokens":4000}}}`) // default max is 4000
	bad(`{"llm":{"provider":"google"},"context":{"summarization":{"enabled":true,"keep_turns":-1}}}`)
	bad(`{"llm":{"provider":"google"},"context":{"summarization":{"enabled":true,"trigger_tokens":-5}}}`)
	bad(`{"llm":{"provider":"acme"},"context":{"summarization":{"enabled":true}}}`) // no provider capability
	bad(`{"llm":{"provider":"google"},"context":{"summarization":{"enabled":true,"model":"x"}}}`)
	bad(`{"llm":{"provider":"google"},"context":{"summarization":{"prompt":"` + strings.Repeat("x", 3000) + `"}}}`)
	if DefaultSummaryTrigger(4000) != 3000 {
		t.Fatal("default trigger")
	}
}

func TestDecodeConfigTTSVoiceSettings(t *testing.T) {
	tts := func(provider, vs string) string {
		return `{"tts":{"provider":"` + provider + `","voice_settings":` + vs + `}}`
	}
	for _, ok := range []string{
		`{"stability":0,"similarity_boost":1,"style":0.5,"speed":0.7}`,
		`{"speed":1.2}`,
		`{}`,
	} {
		c, err := DecodeConfig(json.RawMessage(tts("elevenlabs", ok)))
		if err != nil || c.TTS.VoiceSettings == nil {
			t.Errorf("%s: err=%v vs=%v", ok, err, c.TTS.VoiceSettings)
		}
	}
	for _, bad := range []struct{ raw, field string }{
		{tts("elevenlabs", `{"stability":1.1}`), "tts.voice_settings.stability"},
		{tts("elevenlabs", `{"similarity_boost":-0.1}`), "tts.voice_settings.similarity_boost"},
		{tts("elevenlabs", `{"style":2}`), "tts.voice_settings.style"},
		{tts("elevenlabs", `{"speed":0.69}`), "tts.voice_settings.speed"},
		{tts("elevenlabs", `{"speed":1.21}`), "tts.voice_settings.speed"},
		{tts("elevenlabs", `{"pitch":1}`), "pitch"},
		{tts("cartesia", `{"speed":1}`), "tts.voice_settings"},
	} {
		_, err := DecodeConfig(json.RawMessage(bad.raw))
		if !errors.Is(err, ErrInvalidConfig) || !strings.Contains(err.Error(), bad.field) {
			t.Errorf("%s: err=%v, want ErrInvalidConfig mentioning %s", bad.raw, err, bad.field)
		}
	}
}

func TestDecodeCallLimit(t *testing.T) {
	cases := []struct {
		name, extra, want string // want "" = accepted
	}{
		{"absent", ``, ""},
		{"zero means the server's", `,"max_call_duration_seconds":0`, ""},
		{"minimum", `,"max_call_duration_seconds":60`, ""},
		{"maximum", `,"max_call_duration_seconds":86400`, ""},
		{"under a minute", `,"max_call_duration_seconds":59`, "max_call_duration_seconds must be 0"},
		{"negative", `,"max_call_duration_seconds":-1`, "max_call_duration_seconds must be 0"},
		{"over a day", `,"max_call_duration_seconds":86401`, "max_call_duration_seconds must be 0"},
		{"overflows an int", `,"max_call_duration_seconds":99999999999999999999`, "max_call_duration_seconds"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := DecodeConfig(json.RawMessage(`{"stt":{"provider":"google"},"llm":{"provider":"google","model":"m"},"tts":{"provider":"google"}` + c.extra + `}`))
			if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
}

func TestLLMSamplingValidationAndRoundTrip(t *testing.T) {
	c, err := DecodeConfig(json.RawMessage(`{"llm":{"provider":"google","model":"m","temperature":0.4,"top_p":0.9,"max_output_tokens":256}}`))
	if err != nil {
		t.Fatal(err)
	}
	s := c.LLM.LLMSampling
	if s.Temperature == nil || *s.Temperature != 0.4 || s.TopP == nil || *s.TopP != 0.9 || s.MaxOutputTokens == nil || *s.MaxOutputTokens != 256 {
		t.Fatalf("sampling=%+v", s)
	}
	out, _ := json.Marshal(c.LLM)
	for _, k := range []string{`"temperature":0.4`, `"top_p":0.9`, `"max_output_tokens":256`} {
		if !strings.Contains(string(out), k) {
			t.Errorf("%s missing from %s", k, out)
		}
	}
	zero, err := DecodeConfig(json.RawMessage(`{"llm":{"provider":"google","temperature":0,"top_p":0}}`))
	if err != nil || zero.LLM.Temperature == nil || *zero.LLM.Temperature != 0 {
		t.Fatalf("explicit zero must be kept: %v %+v", err, zero.LLM.LLMSampling)
	}
	null, err := DecodeConfig(json.RawMessage(`{"llm":{"provider":"google","temperature":null,"top_p":null,"max_output_tokens":null}}`))
	if err != nil || null.LLM.LLMSampling != (LLMSampling{}) {
		t.Fatalf("null = provider default: %v %+v", err, null.LLM.LLMSampling)
	}
	for _, bad := range []string{`"temperature":-0.1`, `"temperature":2.1`, `"top_p":-0.1`, `"top_p":1.1`, `"max_output_tokens":0`, `"max_output_tokens":-5`, `"max_output_tokens":8193`, `"max_output_tokens":1.5`, `"temperature":"hot"`} {
		if _, err := DecodeConfig(json.RawMessage(`{"llm":{"provider":"google",` + bad + `}}`)); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("%s: err=%v, want ErrInvalidConfig", bad, err)
		}
	}
	for _, ok := range []string{`"temperature":2`, `"top_p":1`, `"max_output_tokens":1`, `"max_output_tokens":8192`} {
		if _, err := DecodeConfig(json.RawMessage(`{"llm":{"provider":"google",` + ok + `}}`)); err != nil {
			t.Errorf("%s rejected: %v", ok, err)
		}
	}
}
