package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// A draft may select MCP tools the agent has not published: the call captures exactly the draft's
// tools, the published agent stays without them, and an ordinary call cannot use them.
func TestADraftCallCanUseAToolThatIsNotPublished(t *testing.T) {
	r := newMCPCallRig(t)
	id, hashes := r.server("ok", r.ts.URL+"/ok", "MCP_TOKEN_TEST")
	a := r.agentWith(id, hashes, false, nil) // no tools attached
	cfg := `{"stt":{"provider":"echo","model":"s"},"llm":{"provider":"echo","model":"m","system_prompt":"P"},"tts":{"provider":"echo","voice":"v"}}`
	payload, _ := json.Marshal(map[string]any{"principal": "alice", "agent_id": a.ID, "draft": map[string]any{
		"config":    json.RawMessage(cfg),
		"mcp_tools": []map[string]any{{"mcp_server_id": id, "remote_name": "lookup_order", "schema_hash": hashes["lookup_order"], "required": false}},
	}})
	w := apiRequest(r.s, http.MethodPost, "/api/v1/voice-tokens", e2eMgmtKey, string(payload))
	var resp struct {
		Data struct{ Token string } `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || w.Code != http.StatusCreated {
		t.Fatalf("token: %d %s", w.Code, w.Body)
	}
	c := r.dialToken(resp.Data.Token)
	c.say(`call tool lookup_order {"order_id":"42"}`)
	c.waitReplies("shipped", 1)
	if got := r.lastCall("lookup_order"); got.Auth != "match" || string(got.Arguments) != `{"order_id":"42"}` {
		t.Fatalf("the server saw %+v", got)
	}
	eventually(t, "the draft session row to record the tool", func() bool {
		rec, err := r.s.store.Sessions().Get(t.Context(), c.sessionID)
		return err == nil && rec.Draft && len(rec.RuntimeTools) == 1 && rec.RuntimeTools[0].RemoteName == "lookup_order"
	})
	published, err := r.s.agents.Get(t.Context(), a.ID)
	if err != nil || len(published.Version.MCPTools) != 0 {
		t.Fatalf("the published agent gained tools: %+v err %v", published.Version.MCPTools, err)
	}

	// An ordinary call on the same agent has no such tool.
	before := len(r.mcp.Records())
	c2 := r.dial(a.ID)
	c2.say(`call tool lookup_order {"order_id":"42"}`)
	time.Sleep(1500 * time.Millisecond)
	if after := len(r.mcp.Records()); after != before {
		t.Fatalf("an ordinary call reached the MCP server (%d records before, %d after)", before, after)
	}
}
