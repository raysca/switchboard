package runtime

import (
	"testing"

	"govoice/server/agent"
)

func TestMCPToolNamesTheCapturedToolBehindAModelFacingName(t *testing.T) {
	snap := agent.MCPToolSnapshot{ServerID: "0192aaaa-0000-7000-8000-000000000001", RemoteName: "lookup_order", SchemaHash: "h1"}
	s := &Session{Snapshot: agent.Snapshot{MCPTools: []agent.MCPToolSnapshot{snap}}}
	got, ok := s.MCPTool(runtimeToolName(snap.ServerID, snap.RemoteName))
	if !ok || got.ServerID != snap.ServerID || got.RemoteName != "lookup_order" || got.SchemaHash != "h1" {
		t.Fatalf("MCPTool = %+v, %v", got, ok)
	}
	if _, ok := s.MCPTool("transfer_to_sales"); ok {
		t.Fatal("a built-in tool resolved to an MCP tool")
	}
}
