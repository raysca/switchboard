package server

import (
	"strings"
	"testing"
)

// The MCP tool's call and result pass the voice stage of a real call, between the model and the transport.
func TestMCPCallToolFramesAgentThroughTheCallPipeline(t *testing.T) {
	r := newMCPCallRig(t)
	id, hashes := r.server("ok", r.ts.URL+"/ok", "MCP_TOKEN_TEST")
	c := r.dial(r.agentWith(id, hashes, true, nil, "lookup_order").ID)
	c.say(`call tool lookup_order {"order_id":"42"}`)
	c.waitReplies("shipped", 1)
	eventually(t, "the call and its result at the voice stage", func() bool { return len(r.probe.toolFrames("v")) >= 2 })
	got := r.probe.toolFrames("v")
	if len(got) != 2 || !strings.HasPrefix(got[0], "call:mcp__") || !strings.Contains(got[0], "_lookup_order") || got[1] != "result:succeeded" {
		t.Fatalf("tool frames at the voice stage = %q", got)
	}
}
