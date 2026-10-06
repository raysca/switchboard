package agent

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"govoice/server/provider"
	"govoice/server/storage"
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

func TestResolveCredential(t *testing.T) {
	reg := provider.NewRegistry(provider.Definitions())
	g1 := storage.Credential{ID: "g1", Provider: "google", ValidationState: "valid"}
	g2 := storage.Credential{ID: "g2", Provider: "google", ValidationState: "unvalidated"}
	bad := storage.Credential{ID: "bad", Provider: "google", ValidationState: "invalid"}
	x1 := storage.Credential{ID: "x1", Provider: "xai", ValidationState: "valid"}
	m1 := storage.Credential{ID: "m1", Provider: "mcp", ValidationState: "valid"}
	type resolveCase struct {
		name   string
		stage  Stage
		creds  []storage.Credential
		id     string
		reason string
	}
	cases := []resolveCase{
		{"explicit", Stage{"stt", "google", "g1"}, []storage.Credential{g1, x1}, "g1", ""},
		{"explicit picks one of many", Stage{"llm", "google", "g2"}, []storage.Credential{g1, g2}, "g2", ""},
		{"no implicit fallback", Stage{"stt", "google", ""}, []storage.Credential{g1}, "", "missing_credential_stt"},
		{"explicit wrong provider", Stage{"llm", "google", "x1"}, []storage.Credential{g1, x1}, "", "invalid_credential_llm"},
		{"explicit invalid", Stage{"tts", "google", "bad"}, []storage.Credential{bad}, "", "invalid_credential_tts"},
		{"explicit missing", Stage{"tts", "google", "nope"}, []storage.Credential{g1}, "", "missing_credential_tts"},
		{"provider lacks stage", Stage{"tts", "google", "g1"}, []storage.Credential{g1}, "g1", ""},
		{"unknown provider", Stage{"stt", "acme", ""}, nil, "", "unknown_provider_stt"},
		{"mcp is not a voice provider", Stage{"llm", "mcp", "m1"}, []storage.Credential{m1}, "", "unknown_provider_llm"},
	}
	// xAI speech exists only with -tags xaispeech; the expectation follows the catalogue.
	if reg.Supports("xai", provider.CapabilitySTT) {
		cases = append(cases, resolveCase{"xai stt (xaispeech build)", Stage{"stt", "xai", "x1"}, []storage.Credential{x1}, "x1", ""})
	} else {
		cases = append(cases, resolveCase{"provider lacks stage", Stage{"stt", "xai", "x1"}, []storage.Credential{x1}, "", "unsupported_provider_stt"})
	}
	for _, c := range cases {
		id, reason := ResolveCredential(c.stage, reg, c.creds)
		if id != c.id || reason != c.reason {
			t.Errorf("%s: got (%q,%q) want (%q,%q)", c.name, id, reason, c.id, c.reason)
		}
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
