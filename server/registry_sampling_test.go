package server

import (
	"testing"

	"govoice/server/flow"
)

func testSampling() flow.LLMSampling {
	temp, topP, n := 0.0, 0.9, 200
	return flow.LLMSampling{Temperature: &temp, TopP: &topP, MaxOutputTokens: &n}
}

// Every LLM builder maps the stage's sampling onto its provider config; an
// unset sampling keeps the provider defaults.
func TestLLMBuildersMapSampling(t *testing.T) {
	spec := StageSpec{Sampling: testSampling()}
	t.Run("openai", func(t *testing.T) {
		c := openAILLMConfig(&Config{}, spec, "k", "https://x/v1", nil)
		if c.Temperature == nil || *c.Temperature != 0 || c.TopP == nil || *c.TopP != 0.9 || c.MaxOutputTokens != 200 {
			t.Fatalf("%+v", c)
		}
		if u := openAILLMConfig(&Config{}, StageSpec{}, "k", "https://x/v1", nil); u.Temperature != nil || u.TopP != nil || u.MaxOutputTokens != 0 {
			t.Fatalf("unset: %+v", u)
		}
	})
	t.Run("nvidia", func(t *testing.T) {
		c := nvidiaLLMConfig(spec, "m", "k", nil)
		if c.Temperature == nil || *c.Temperature != 0 || c.TopP == nil || *c.TopP != 0.9 || c.MaxOutputTokens != 200 {
			t.Fatalf("%+v", c)
		}
	})
}
