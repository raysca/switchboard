//go:build fakeproviders

package server

import (
	"testing"

	"govoice/core"
	"govoice/server/runtime"
)

func TestFakeLLMPhrasesForMCPTools(t *testing.T) {
	name := runtime.ToolName("01a12f27-7f90-792f-85e5-6fd6c949fe6b", "lookup_order")
	phrases, spoken := mcpToolPhrases([]core.ToolDefinition{{Name: name}, {Name: "transfer_to_sales"}})
	if phrases["call tool lookup_order"] != name || spoken[name] != "lookup_order" {
		t.Fatalf("phrases=%v spoken=%v", phrases, spoken)
	}
	if len(phrases) != 1 {
		t.Fatalf("only MCP tools get a call-tool phrase: %v", phrases)
	}
}
