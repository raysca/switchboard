package server

import (
	"encoding/json"
	"testing"

	"govoice/server/flow"
)

// A flow's llm sampling reaches the model call of the step that sets it: the
// echo model (labelled replies) says what it was given. A step without it, and
// the other step's values, show nothing.
func TestFlowE2E_LLMSamplingReachesTheModel(t *testing.T) {
	r := newFlowRigE2E(t, newProbe())
	stage := func(extra string) string { return `"provider":"echo"` + extra }
	a, err := r.s.flows.Create(t.Context(), flow.CreateRequest{Name: "Sampling", Config: json.RawMessage(`{"start":"front","steps":[` +
		`{"id":"front","name":"Front desk","config":{"stt":{` + stage("") + `},"llm":{` + stage(`,"model":"m","system_prompt":"FRONT","temperature":0.3,"top_p":0.9,"max_output_tokens":200`) + `},"tts":{` + stage("") + `}}},` +
		`{"id":"sales","name":"Sales","config":{"llm":{` + stage(`,"model":"m","system_prompt":"SALES","top_p":0.5`) + `},"tts":{` + stage("") + `}}}],` +
		`"routes":[{"id":"e1","from":"front","to":"sales","condition":"The caller wants to buy."}]}`)})
	if err != nil || a.Readiness.State != flow.ReadinessReady {
		t.Fatalf("create: %+v err=%v", a.Readiness, err)
	}
	r.flowID = a.ID
	c := r.dial(a.ID)
	eventually(t, "session", func() bool { return r.activeSessions() == 1 })

	c.say("hello")
	c.waitReplies("[FRONT] {temperature=0.3 top_p=0.9 max_output_tokens=200} You said: hello", 1)

	c.say("transfer to sales")
	c.waitReplies("[SALES] {top_p=0.5} ", 1)
	if c.replies("[SALES] {temperature") != 0 {
		t.Fatal("the front step's sampling leaked into the sales step")
	}
}
