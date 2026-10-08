package server

import (
	"errors"
	"strings"
	"testing"

	"govoice/core"
	"govoice/providers/deepgram"
)

func TestBuildStageUsesTheRegisteredFactory(t *testing.T) {
	r := NewRegistry()
	var got StageSpec
	r.Register("probe.tts", func(_ *Config, spec StageSpec) (core.Processor, error) {
		got = spec
		return nil, errors.New("built")
	})
	if _, err := r.BuildStage(DefaultConfig(ProfileDevelopment), "tts", StageSpec{Provider: "probe", Voice: "v"}); err == nil || err.Error() != "built" || got.Voice != "v" {
		t.Fatalf("factory not used: err=%v spec=%+v", err, got)
	}
	_, err := r.BuildStage(DefaultConfig(ProfileDevelopment), "stt", StageSpec{Provider: "nope", Secret: []byte("sk-secret-canary")})
	if err == nil || strings.Contains(err.Error(), "canary") {
		t.Fatalf("unknown provider: %v", err)
	}
}

func TestClassifyStageError(t *testing.T) {
	for st, err := range map[string]error{"stt": &deepgram.ProviderError{ClientError: true}, "tts": clientErr{}, "llm": clientErr{}} {
		if c := ClassifyStageError(st, err); c != "client_error" {
			t.Errorf("%s: %q want client_error", st, c)
		}
	}
	// STT classifies only provider errors as client errors: a bare 401-looking error stays retryable.
	if c := ClassifyStageError("stt", clientErr{}); c == "client_error" {
		t.Errorf("stt generic error: %q", c)
	}
}
