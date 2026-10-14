package server

import (
	"context"
	"encoding/json"
	"github.com/coder/websocket"
	"net/http"
	"strings"
	"testing"
	"time"

	"govoice/server/flow"
)

func TestDraftRegistryIsBoundedSingleUseAndKeepsAGraceAfterExpiry(t *testing.T) {
	d, now := newDraftCalls(), time.Now()
	for i := 0; i < maxDrafts; i++ {
		if err := d.put(string(rune('a'+i)), draftCall{expires: now.Add(time.Minute)}, now); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	if err := d.put("overflow", draftCall{expires: now.Add(time.Minute)}, now); err != errTooManyDrafts {
		t.Fatalf("over the cap: %v", err)
	}
	if _, found := d.take("a"); !found {
		t.Fatal("first take")
	}
	if _, found := d.take("a"); found {
		t.Fatal("a draft was taken twice")
	}
	// The token validator already enforces the token's expiry; a token accepted in its last
	// instant must still find its draft, so entries outlive their expiry by a grace period.
	atExpiry := now.Add(time.Minute)
	if err := d.put("x", draftCall{expires: atExpiry}, atExpiry); err != nil {
		t.Fatal(err)
	}
	if _, found := d.take("b"); !found {
		t.Fatal("a draft was dropped at its expiry instant")
	}
	// Long dead entries are swept by the next put and free their slot.
	later := now.Add(time.Minute + draftGrace + time.Second)
	if err := d.put("fresh", draftCall{expires: later.Add(time.Minute)}, later); err != nil {
		t.Fatalf("a long-expired entry must free its slot: %v", err)
	}
	if _, found := d.take("c"); found {
		t.Fatal("a long-expired draft survived the sweep")
	}
}

const (
	publishedPrompt = "PUBLISHED"
	draftPrompt     = "DRAFT"
)

func echoFlowConfig(prompt string) string {
	return `{"stt":{"provider":"echo","model":"s"},"llm":{"provider":"echo","model":"m","system_prompt":"` + prompt + `"},"tts":{"provider":"echo","voice":"v"}}`
}

// draftRig is a server with one published echo flow whose prompt is PUBLISHED.
func draftRig(t *testing.T) (*flowRigE2E, string) {
	t.Helper()
	r := newFlowRigE2E(t, newProbe())
	f, err := r.s.flows.Create(t.Context(), flow.CreateRequest{Name: "Support", Config: json.RawMessage(echoFlowConfig(publishedPrompt))})
	if err != nil || f.Readiness.State != flow.ReadinessReady {
		t.Fatalf("flow: %+v err %v", f.Readiness, err)
	}
	r.flowID = f.ID
	return r, f.ID
}

// issueDraftToken asks for a token carrying a draft; it returns the response.
func issueDraftToken(r *flowRigE2E, flowID, config string) (code int, token string, body string) {
	payload, _ := json.Marshal(map[string]any{"principal": "alice", "flow_id": flowID, "draft": map[string]any{"config": json.RawMessage(config)}})
	w := apiRequest(r.s, http.MethodPost, "/api/v1/voice-tokens", e2eMgmtKey, string(payload))
	var resp struct {
		Data struct{ Token string } `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return w.Code, resp.Data.Token, w.Body.String()
}

func TestVoiceTokenWithADraftIsValidatedLikeAPublish(t *testing.T) {
	r, id := draftRig(t)
	for name, tc := range map[string]struct {
		flow, config string
		code         int
	}{
		"not an object": {id, `[1]`, http.StatusBadRequest},
		"unattached":    {id, `{"stt":{"provider":"echo","model":"s"},"llm":{"provider":"echo"},"tts":{"provider":"echo","voice":"v"}}`, http.StatusBadRequest},
		"foreign tool":  {id, `{"start":"a","steps":[{"id":"a","name":"A","tools":["srv/lookup"],"config":` + echoFlowConfig("D") + `}],"routes":[]}`, http.StatusBadRequest},
		"unknown flow":  {"0194c5f8-0000-7000-8000-000000000000", echoFlowConfig("D"), http.StatusNotFound},
		"a valid draft": {id, echoFlowConfig(draftPrompt), http.StatusCreated},
	} {
		code, token, body := issueDraftToken(r, tc.flow, tc.config)
		if code != tc.code {
			t.Errorf("%s: status %d, want %d: %s", name, code, tc.code, body)
		}
		if (code == http.StatusCreated) != (token != "") {
			t.Errorf("%s: token %q for status %d", name, token, code)
		}
	}
	f, err := r.s.flows.Get(t.Context(), id)
	if err != nil || f.Version.Number != 1 {
		t.Fatalf("a draft request must never publish: %+v err %v", f.Version, err)
	}
}

func TestVoiceTokenWithoutADraftIsUnchanged(t *testing.T) {
	r, id := draftRig(t)
	c := r.dial(id)
	c.say("hello")
	c.waitReplies("["+publishedPrompt, 1)
}

func TestDraftCallRunsTheDraftNotThePublishedVersion(t *testing.T) {
	r, id := draftRig(t)
	code, token, body := issueDraftToken(r, id, echoFlowConfig(draftPrompt))
	if code != http.StatusCreated {
		t.Fatalf("%d %s", code, body)
	}
	c := r.dialToken(token)
	c.say("hello")
	c.waitReplies("["+draftPrompt, 1)
	if n := c.replies("[" + publishedPrompt); n != 0 {
		t.Fatalf("the published prompt answered a draft call %d times", n)
	}
	// An ordinary call on the same flow still runs the published version.
	c2 := r.dial(id)
	c2.say("hello")
	c2.waitReplies("["+publishedPrompt, 1)
	if n := c2.replies("[" + draftPrompt); n != 0 {
		t.Fatalf("the draft leaked into an ordinary call %d times", n)
	}
}

func TestDraftCallIsRecordedAsADraftOnTheCurrentVersion(t *testing.T) {
	r, id := draftRig(t)
	_, token, _ := issueDraftToken(r, id, echoFlowConfig(draftPrompt))
	c := r.dialToken(token)
	c.say("hello")
	c.waitReplies("["+draftPrompt, 1)
	f, _ := r.s.flows.Get(t.Context(), id)
	var sessionHash string
	eventually(t, "the draft session row", func() bool {
		rec, err := r.s.store.Sessions().Get(t.Context(), c.sessionID)
		if err != nil {
			return false
		}
		sessionHash = rec.ConfigHash
		return rec.Draft && rec.FlowVersionID == f.Version.ID
	})
	if sessionHash == f.Version.ConfigHash {
		t.Fatal("the session must carry the draft's hash, not the published version's")
	}
	w := apiRequest(r.s, http.MethodGet, "/api/v1/sessions/"+c.sessionID, e2eMgmtKey, "")
	if !strings.Contains(w.Body.String(), `"draft":true`) {
		t.Fatalf("the API must say it was a draft: %s", w.Body)
	}
}

func TestDraftSurvivesAPublishDuringTheCall(t *testing.T) {
	r, id := draftRig(t)
	_, token, _ := issueDraftToken(r, id, echoFlowConfig(draftPrompt))
	c := r.dialToken(token)
	c.say("one")
	c.waitReplies("["+draftPrompt, 1)
	if _, err := r.s.flows.Update(t.Context(), id, flow.UpdateRequest{Config: json.RawMessage(echoFlowConfig("PUBLISHED2"))}); err != nil {
		t.Fatal(err)
	}
	c.say("two")
	c.waitReplies("["+draftPrompt, 2) // still the draft, still up
	if n := c.replies("[PUBLISHED2"); n != 0 {
		t.Fatalf("a publish changed a running draft call: %d", n)
	}
}

func TestADraftTokenIsSingleUse(t *testing.T) {
	r, id := draftRig(t)
	_, token, _ := issueDraftToken(r, id, echoFlowConfig(draftPrompt))
	c := r.dialToken(token)
	c.say("hello")
	c.waitReplies("["+draftPrompt, 1)
	// The token is spent: the validator's replay guard refuses the second connection.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(r.ts.URL, "http")+"?token="+token, nil); err == nil {
		_ = conn.CloseNow()
		t.Fatal("a second connection with the same draft token was accepted")
	}
}

func TestADraftCallIsRefusedOnceItsFlowIsArchived(t *testing.T) {
	r, id := draftRig(t)
	_, token, _ := issueDraftToken(r, id, echoFlowConfig(draftPrompt))
	if err := r.s.flows.Archive(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	c := r.dialToken(token)
	c.say("hello")
	time.Sleep(1500 * time.Millisecond)
	if n := c.replies("["); n != 0 {
		t.Fatalf("an archived flow answered a draft call %d times", n)
	}
}
