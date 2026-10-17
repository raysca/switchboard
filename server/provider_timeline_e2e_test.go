package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"govoice/core"
	"govoice/providers/google"
)

// flakyLLM is a model backend that is down: every request is a retryable 503.
type flakyLLM struct{}

func (flakyLLM) GenerateStream(context.Context, string, string, []core.Message) (<-chan google.LLMStreamChunk, <-chan error, error) {
	return nil, nil, &google.ProviderError{Capability: "llm", BaseErr: google.ErrProviderUnavailable, SafeMessage: "injected outage", StatusCode: http.StatusServiceUnavailable, Retryable: true}
}

// The real Google processor against a dead backend, with the echo breaker the server shares: one turn of five
// attempts retries four times and opens the circuit (threshold 5), and the call's timeline says both.
func TestCallBehaviorTimelineShowsProviderRetriesAndTheCircuitOpening(t *testing.T) {
	r := newFlowRigE2E(t, newProbe())
	healthy := r.s.registry.factories["echo.llm"]
	r.s.registry.Register("echo.llm", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		if spec.SystemPrompt != "Broken" {
			return healthy(cfg, spec) // the bystander's model works
		}
		llmCfg := googleLLMConfig(spec, r.s.registry.echoLLMBreaker)
		llmCfg.Provider = "echo"
		llmCfg.Client = flakyLLM{}
		llmCfg.RetryPolicy.MaxAttempts = 5
		return google.NewLLMProcessor(llmCfg)
	})
	c := r.dial(behaviorFlow(t, r, "Broken", "brokenmodel", ``))
	// A bystander call shares the echo LLM circuit but its own model works.
	bystander := r.dial(behaviorFlow(t, r, "Bystander", "bystandervoice", ``))
	eventually(t, "both calls to be live", func() bool {
		r.s.mu.Lock()
		defer r.s.mu.Unlock()
		return len(r.s.activeCalls) == 2
	})
	c.say("hello")
	eventually(t, "the circuit to open on the call's timeline", func() bool { return r.has(EventCircuitState, "llm open") })

	timeline := func(session int) (events []TimelineEvent) {
		rec := httptest.NewRecorder()
		r.s.timelines.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/timeline?session="+strconv.Itoa(session), nil))
		var body struct {
			Events []TimelineEvent `json:"events"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
			t.Fatalf("/debug/timeline: %d %s", rec.Code, rec.Body.String())
		}
		return body.Events
	}
	// Decision 4: the circuit is shared, so the bystander is told it opened; retries are the broken call's alone.
	eventually(t, "the bystander's timeline to show the shared circuit", func() bool {
		for _, e := range timeline(2) {
			if e.Type == EventCircuitState && e.Provider == "echo" && e.Details == "llm open" {
				return true
			}
		}
		return false
	})
	for _, e := range timeline(2) {
		if e.Type == EventProviderRetry {
			t.Fatalf("the bystander got a retry event: %+v", e)
		}
	}
	if bystander.ended() {
		t.Fatal("the bystander's call ended")
	}
	var body struct{ Events []TimelineEvent }
	body.Events = timeline(1)
	var retries, circuits []string
	for _, e := range body.Events {
		switch e.Type {
		case EventProviderRetry:
			retries = append(retries, e.Provider+":"+e.Details)
		case EventCircuitState:
			circuits = append(circuits, e.Provider+":"+e.Details)
		}
	}
	if want := "echo:llm retry 1,echo:llm retry 2,echo:llm retry 3,echo:llm retry 4"; strings.Join(retries, ",") != want {
		t.Fatalf("retries %q, want %s", retries, want)
	}
	if strings.Join(circuits, ",") != "echo:llm open" {
		t.Fatalf("circuit events %q", circuits)
	}
	if c.ended() {
		t.Fatal("an LLM outage ended the call")
	}
}
