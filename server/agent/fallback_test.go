package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"govoice/core/resilience"
	"govoice/server/storage"
)

func chainConfig(t *testing.T, llmFallbacks, extra string) json.RawMessage {
	t.Helper()
	return json.RawMessage(`{"stt":{"provider":"google","model":"s"},` +
		`"llm":{"provider":"google","model":"m1"` + llmFallbacks + `},` +
		`"tts":{"provider":"google","voice":"v"}` + extra + `}`)
}

func fb(model string) string {
	return `{"provider":"google","model":"` + model + `"}`
}

func TestSilenceTakesNoFallbacksAndIsNotOne(t *testing.T) {
	text := `"stt":{"provider":"silence","model":"text"%s},"llm":{"provider":"google","model":"m"%s},"tts":{"provider":"silence","voice":"none"%s}`
	cfg := func(sttFB, llmFB, ttsFB string) json.RawMessage {
		return json.RawMessage("{" + fmt.Sprintf(text, sttFB, llmFB, ttsFB) + "}")
	}
	if _, err := DecodeConfig(cfg("", "", "")); err != nil {
		t.Fatalf("a plain text-only agent must be valid: %v", err)
	}
	for name, c := range map[string]json.RawMessage{
		"silence stt with a real fallback": cfg(`,"fallbacks":[{"provider":"google","model":"s"}]`, "", ""),
		"silence tts with a real fallback": cfg("", "", `,"fallbacks":[{"provider":"google","voice":"v"}]`),
		"silence as a model fallback":      cfg("", `,"fallbacks":[{"provider":"silence","model":"x"}]`, ""),
		"silence as a real tts's fallback": json.RawMessage(`{"stt":{"provider":"google","model":"s"},"llm":{"provider":"google","model":"m"},"tts":{"provider":"google","voice":"v","fallbacks":[{"provider":"silence","voice":"none"}]}}`),
		"silence as a real stt's fallback": json.RawMessage(`{"stt":{"provider":"google","model":"s","fallbacks":[{"provider":"silence","model":"text"}]},"llm":{"provider":"google","model":"m"},"tts":{"provider":"google","voice":"v"}}`),
	} {
		if _, err := DecodeConfig(c); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("%s: err=%v, want ErrInvalidConfig", name, err)
		}
	}
}

func TestChainValidation(t *testing.T) {
	bad := map[string]string{
		"too long":       `,"fallbacks":[` + fb("a") + `,` + fb("b") + `,` + fb("c") + `]`,
		"duplicate":      `,"fallbacks":[` + fb("a") + `,` + fb("a") + `]`,
		"dup of primary": `,"fallbacks":[` + fb("m1") + `]`,
		"no model":       `,"fallbacks":[{"provider":"google"}]`,
		"unknown field":  `,"fallbacks":[{"provider":"google","model":"a","temperature":1}]`,
	}
	for name, fbs := range bad {
		if _, err := DecodeConfig(chainConfig(t, fbs, "")); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("%s: err=%v, want ErrInvalidConfig", name, err)
		}
	}
	if _, err := DecodeConfig(chainConfig(t, "", `,"failover":{"on":["bogus"]}`)); !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("unknown class accepted: %v", err)
	}
	if _, err := DecodeConfig(chainConfig(t, `,"fallbacks":[`+fb("a")+`,`+fb("b")+`]`, `,"failover":{"on":["timeout","client_error"]}`)); err != nil {
		t.Fatalf("max-length chain rejected: %v", err)
	}
}

func TestCreateValidatesFallbackAvailability(t *testing.T) {
	svc := testServiceWith(t, fakeAvail{
		"acme/llm":     {Reason: "unknown_provider"},
		"xai/llm":      {Reason: "missing_credential"},
		"deepgram/tts": {Reason: "unsupported_provider"},
	})
	ctx := context.Background()
	for name, f := range map[string]string{
		"unknown provider": `{"provider":"acme","model":"a"}`,
		"no credential":    `{"provider":"xai","model":"a"}`,
	} {
		_, err := svc.Create(ctx, CreateRequest{Name: name, Config: chainConfig(t, `,"fallbacks":[`+f+`]`, "")})
		if !errors.Is(err, ErrNotAttached) || !strings.Contains(err.Error(), "_llm_fallback_1") {
			t.Errorf("%s: err=%v", name, err)
		}
	}
	// Stage capability: deepgram is STT-only, so it cannot back up TTS.
	tts := strings.Replace(string(chainConfig(t, "", "")), `"voice":"v"`, `"voice":"v","fallbacks":[{"provider":"deepgram"}]`, 1)
	if _, err := svc.Create(ctx, CreateRequest{Name: "cap", Config: json.RawMessage(tts)}); err == nil || !strings.Contains(err.Error(), "unsupported_provider_tts_fallback_1") {
		t.Errorf("tts fallback without capability: err=%v", err)
	}
}

func TestFailoverPolicyDefaults(t *testing.T) {
	var p FailoverPolicy
	if !p.Allows(ClassTimeout) || !p.Allows(ClassServer) || !p.Allows(ClassCircuit) || p.Allows(ClassClient) || p.Allows("") {
		t.Fatal("default policy wrong")
	}
	if p = (FailoverPolicy{On: []string{ClassClient}}); !p.Allows(ClassClient) || p.Allows(ClassTimeout) {
		t.Fatal("explicit policy wrong")
	}
}

// The classes must be readable from what providers actually return.
func TestClassOf(t *testing.T) {
	wrapped := func(err error) error { return fmt.Errorf("google llm: %w", err) }
	cases := []struct {
		err  error
		v    resilience.Verdict
		want string
	}{
		{wrapped(resilience.ErrCircuitOpen), resilience.Verdict{}, ClassCircuit},
		{wrapped(resilience.ErrTimeout), resilience.Verdict{Retryable: true}, ClassTimeout},
		{wrapped(context.Canceled), resilience.Verdict{ClientError: true}, ""},
		{wrapped(context.DeadlineExceeded), resilience.Verdict{}, ""},
		{errors.New("401"), resilience.Verdict{ClientError: true}, ClassClient},
		{errors.New("503"), resilience.Verdict{Retryable: true}, ClassServer},
		{errors.New("429"), resilience.Verdict{Retryable: true, RetryAfter: time.Second}, ClassServer},
		{nil, resilience.Verdict{}, ""},
	}
	for _, c := range cases {
		if got := ClassOf(c.err, c.v); got != c.want {
			t.Errorf("ClassOf(%v)=%q want %q", c.err, got, c.want)
		}
	}
}

// Legacy configs (no fallbacks) still work, stay byte-identical, and have a
// single-entry chain. The chain lives in config_json, so no schema migration.
func TestLegacyConfigHasEmptyChain(t *testing.T) {
	svc := testService(t)
	a, err := svc.Create(context.Background(), CreateRequest{Name: "legacy", Config: validConfig()})
	if err != nil {
		t.Fatal(err)
	}
	want, err := storage.CanonicalJSON(validConfig())
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(want)
	if !bytes.Equal(a.Version.Config, want) || a.Version.ConfigHash != hex.EncodeToString(sum[:]) {
		t.Fatalf("legacy config changed: %s hash=%s", a.Version.Config, a.Version.ConfigHash)
	}
	cfg, _ := DecodeConfig(a.Version.Config)
	if len(cfg.Chain("llm")) != 1 || len(cfg.Chain("tts")) != 1 {
		t.Fatal("legacy chain should be primary only")
	}
}

// A session pins the chain with its agent version: a later update moves the
// current pointer but the session still resolves the old chain.
func TestSessionPinsChain(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()
	a, err := svc.Create(ctx, CreateRequest{Name: "chain", Config: chainConfig(t, `,"fallbacks":[`+fb("backup")+`]`, "")})
	if err != nil {
		t.Fatal(err)
	}
	pin, err := svc.StartSession(ctx, a.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Update(ctx, a.ID, UpdateRequest{Config: validConfig()}); err != nil {
		t.Fatal(err)
	}
	snap, err := svc.ResolveSession(ctx, pin.ID)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := DecodeConfig(snap.Version.Config)
	if err != nil {
		t.Fatal(err)
	}
	if c := cfg.Chain("llm"); len(c) != 2 || c[1].Model != "backup" {
		t.Fatalf("pinned chain=%+v", c)
	}
}
