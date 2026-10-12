package flow

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type fakeAvail map[string]Unavailable // key "provider/service"; absent = available

func (f fakeAvail) Check(provider, service string) Unavailable { return f[provider+"/"+service] }

func ready() fakeAvail { return fakeAvail{} }

func completeConfig(t *testing.T) Config {
	t.Helper()
	c, err := DecodeConfig(validConfig())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestAttachmentProblemsAvailable(t *testing.T) {
	if r, e := AttachmentProblems(completeConfig(t), ready()); len(r) != 0 || len(e) != 0 {
		t.Fatalf("reasons=%v env=%v", r, e)
	}
}

func TestAttachmentProblemsReportsReasonAndEnv(t *testing.T) {
	av := fakeAvail{"google/stt": {Reason: "missing_credential", MissingEnv: []string{"GOOGLE_APPLICATION_CREDENTIALS"}}}
	r, e := AttachmentProblems(completeConfig(t), av)
	if !reflect.DeepEqual(r, []string{"missing_credential_stt"}) || !reflect.DeepEqual(e, []string{"GOOGLE_APPLICATION_CREDENTIALS"}) {
		t.Fatalf("reasons=%v env=%v", r, e)
	}
}

func TestAttachmentProblemsListsSharedEnvOnce(t *testing.T) {
	u := Unavailable{Reason: "missing_credential", MissingEnv: []string{"GOOGLE_APPLICATION_CREDENTIALS"}}
	r, e := AttachmentProblems(completeConfig(t), fakeAvail{"google/stt": u, "google/llm": u})
	if len(r) != 2 || !reflect.DeepEqual(e, []string{"GOOGLE_APPLICATION_CREDENTIALS"}) {
		t.Fatalf("reasons=%v env=%v", r, e)
	}
}

func TestFallbackProblemsNamesStageAndIndex(t *testing.T) {
	c, err := DecodeConfig(chainConfig(t, `,"fallbacks":[{"provider":"xai","model":"a"}]`, ""))
	if err != nil {
		t.Fatal(err)
	}
	r, e := FallbackProblems(c, fakeAvail{"xai/llm": {Reason: "missing_credential", MissingEnv: []string{"XAI_API_KEY"}}})
	if !reflect.DeepEqual(r, []string{"missing_credential_llm_fallback_1"}) || !reflect.DeepEqual(e, []string{"XAI_API_KEY"}) {
		t.Fatalf("reasons=%v env=%v", r, e)
	}
}

func TestDecodeConfigRejectsCredentialID(t *testing.T) {
	cases := map[string]string{
		"stage":    `{"stt":{"provider":"google","credential_id":"x"},"llm":{"provider":"google","model":"m"},"tts":{"provider":"google"}}`,
		"fallback": `{"llm":{"provider":"google","model":"m","fallbacks":[{"provider":"xai","model":"a","credential_id":"x"}]}}`,
	}
	for name, raw := range cases {
		if _, err := DecodeConfig(json.RawMessage(raw)); !errors.Is(err, ErrInvalidConfig) || !strings.Contains(err.Error(), "credential_id") {
			t.Errorf("%s: err=%v", name, err)
		}
	}
	step := `{"entry":"a","nodes":[{"id":"a","name":"A","config":{"stt":{"provider":"google"},"llm":{"provider":"google","model":"m","credential_id":"x"},"tts":{"provider":"google"}}}],"edges":[]}`
	if _, err := Decode(json.RawMessage(step)); !errors.Is(err, ErrInvalidConfig) || !strings.Contains(err.Error(), "credential_id") {
		t.Errorf("graph node: err=%v", err)
	}
}

func TestFlowProblemsPrefixNonEntrySteps(t *testing.T) {
	g, err := Decode(twoStepFlow("google"))
	if err != nil {
		t.Fatal(err)
	}
	// The sales step borrows the entry's STT, so a missing STT is reported for both.
	av := fakeAvail{"google/stt": {Reason: "missing_credential", MissingEnv: []string{"GOOGLE_APPLICATION_CREDENTIALS"}}}
	r, e := g.Problems(av)
	if !reflect.DeepEqual(r, []string{"missing_credential_stt", "sales:missing_credential_stt"}) || len(e) != 1 {
		t.Fatalf("reasons=%v env=%v", r, e)
	}
}
