package api

import (
	"encoding/json"
	"reflect"
	"testing"

	"govoice/server/storage"
)

// The UI reads runtime_tools with snake_case keys; storage.SelectedTool has no json tags.
func TestSessionDTORuntimeToolsUseAPIKeys(t *testing.T) {
	dto := toSessionDTO(storage.SessionRecord{RuntimeTools: []storage.SelectedTool{{ServerID: "s1", RemoteName: "find_slots", SchemaHash: "h", Required: true}}})
	raw, err := json.Marshal(dto)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Tools []map[string]any `json:"runtime_tools"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"mcp_server_id": "s1", "remote_name": "find_slots", "schema_hash": "h", "required": true}
	if len(got.Tools) != 1 || !reflect.DeepEqual(got.Tools[0], want) {
		t.Fatalf("runtime_tools = %v, want [%v]", got.Tools, want)
	}
}
