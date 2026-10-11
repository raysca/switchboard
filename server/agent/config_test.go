package agent

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestDecodeConfigIsStrictButAllowsDrafts(t *testing.T) {
	for _, bad := range []string{`[]`, `{"stt":"google"}`, `{"pipeline":"default"}`, `{"llm":{"provider":"google","temperature":1}}`, `{"context":{"max_tokens":-1}}`} {
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
	ok(`{"llm":{"provider":"xai"},"context":{"summarization":{"enabled":true}}}`) // trigger defaults to 3/4 of max
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
