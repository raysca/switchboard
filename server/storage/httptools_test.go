package storage

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func newHTTPTool(t *testing.T, s *Store, name, hash string) HTTPTool {
	t.Helper()
	tool := HTTPTool{ID: NewID(), Name: name, CurrentHash: hash, Enabled: true}
	if err := s.HTTPTools().Create(context.Background(), tool, []byte(`{"name":"`+name+`","v":"`+hash+`"}`)); err != nil {
		t.Fatal(err)
	}
	return tool
}

func TestHTTPToolDefinitionsAreImmutableAndVersioned(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		getOrder := "get_order_" + NewID()[28:] // the Postgres database is shared by the tests: names are unique per run
		tool := newHTTPTool(t, s, getOrder, "h1")
		if err := s.HTTPTools().NewDefinition(ctx, tool.ID, getOrder, "h2", []byte(`{"v":"h2"}`)); err != nil {
			t.Fatal(err)
		}
		got, err := s.HTTPTools().Get(ctx, tool.ID)
		if err != nil || got.CurrentHash != "h2" {
			t.Fatalf("get = %+v %v", got, err)
		}
		for _, hash := range []string{"h1", "h2"} {
			if raw, err := s.HTTPTools().Definition(ctx, tool.ID, hash); err != nil || len(raw) == 0 {
				t.Fatalf("definition %s: %q %v", hash, raw, err)
			}
		}
		// going back to a definition seen before reuses it
		if err := s.HTTPTools().NewDefinition(ctx, tool.ID, getOrder, "h1", []byte(`{"v":"other"}`)); err != nil {
			t.Fatal(err)
		}
		if raw, _ := s.HTTPTools().Definition(ctx, tool.ID, "h1"); string(raw) != `{"name":"`+getOrder+`","v":"h1"}` {
			t.Fatalf("a stored definition was changed: %s", raw)
		}
		if _, err := s.DB().ExecContext(ctx, s.bind("UPDATE http_tool_definitions SET definition_json=? WHERE tool_id=?"), "{}", tool.ID); err == nil {
			t.Fatal("a definition row could be updated")
		}
		if err := s.HTTPTools().NewDefinition(ctx, NewID(), "x", "h", []byte(`{}`)); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("unknown tool: %v", err)
		}
	})
}

func TestHTTPToolNamesAreUnique(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		getOrder := "get_order_" + NewID()[28:] // the Postgres database is shared by the tests: names are unique per run
		first := newHTTPTool(t, s, getOrder, "h1")
		if err := s.HTTPTools().Create(ctx, HTTPTool{ID: NewID(), Name: getOrder, CurrentHash: "h9", Enabled: true}, []byte(`{}`)); !errors.Is(err, ErrHTTPToolNameTaken) {
			t.Fatalf("duplicate create: %v", err)
		}
		other := newHTTPTool(t, s, "add_note_"+NewID()[28:], "h2")
		if err := s.HTTPTools().NewDefinition(ctx, other.ID, getOrder, "h3", []byte(`{}`)); !errors.Is(err, ErrHTTPToolNameTaken) {
			t.Fatalf("rename onto a taken name: %v", err)
		}
		// a tool keeps its own name across edits
		if err := s.HTTPTools().NewDefinition(ctx, first.ID, getOrder, "h4", []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	})
}

func TestHTTPToolSelectionsPinTheCurrentDefinition(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		getOrder := "get_order_" + NewID()[28:] // the Postgres database is shared by the tests: names are unique per run
		r := newToolRig(t, s)
		tool := newHTTPTool(t, s, getOrder, "h1")
		sel := SelectedTool{ServerID: HTTPToolRefPrefix + tool.ID, RemoteName: getOrder, SchemaHash: "h1", Required: true}
		for label, bad := range map[string]SelectedTool{
			"stale hash":   {ServerID: sel.ServerID, RemoteName: getOrder, SchemaHash: "old"},
			"wrong name":   {ServerID: sel.ServerID, RemoteName: "other", SchemaHash: "h1"},
			"unknown tool": {ServerID: HTTPToolRefPrefix + NewID(), RemoteName: getOrder, SchemaHash: "h1"},
			"bad id":       {ServerID: HTTPToolRefPrefix + "nope", RemoteName: getOrder, SchemaHash: "h1"},
		} {
			if err := s.Agents().ValidateSelections(ctx, []SelectedTool{bad}); !errors.Is(err, ErrInvalidToolSelection) {
				t.Errorf("%s accepted: %v", label, err)
			}
		}
		both := []SelectedTool{sel, r.sel("echo", false)} // an MCP tool and an HTTP tool in one version
		if _, _, err := r.publish(t, AgentMutation{Tools: &both}); err != nil {
			t.Fatalf("publish: %v", err)
		}
		refs, err := s.HTTPTools().CurrentReferences(ctx, tool.ID)
		if err != nil || len(refs) != 1 || refs[0].AgentID != r.agentID || !refs[0].Required {
			t.Fatalf("references = %+v %v", refs, err)
		}
		if err := s.HTTPTools().Delete(ctx, tool.ID); !errors.Is(err, ErrHTTPToolReferenced) {
			t.Fatalf("delete while pinned: %v", err)
		}
		if err := s.HTTPTools().SetEnabled(ctx, tool.ID, false); err != nil {
			t.Fatal(err)
		}
		if err := s.Agents().ValidateSelections(ctx, []SelectedTool{sel}); !errors.Is(err, ErrInvalidToolSelection) {
			t.Fatalf("a disabled tool was accepted: %v", err)
		}
		// an edit moves the current hash: the old pin must be reviewed again
		_ = s.HTTPTools().SetEnabled(ctx, tool.ID, true)
		_ = s.HTTPTools().NewDefinition(ctx, tool.ID, getOrder, "h2", []byte(`{}`))
		if err := s.Agents().ValidateSelections(ctx, []SelectedTool{sel}); !errors.Is(err, ErrInvalidToolSelection) {
			t.Fatalf("an edited tool kept its old pin: %v", err)
		}
		none := []SelectedTool{}
		if _, _, err := r.publish(t, AgentMutation{Tools: &none}); err != nil {
			t.Fatal(err)
		}
		// no current agent pins it now, but an older version does: its definitions stay readable
		if refs, _ := s.HTTPTools().CurrentReferences(ctx, tool.ID); len(refs) != 0 {
			t.Fatalf("current references = %+v", refs)
		}
		if err := s.HTTPTools().Delete(ctx, tool.ID); !errors.Is(err, ErrHTTPToolReferenced) {
			t.Fatalf("delete with history: %v", err)
		}
		// a tool nothing ever pinned can go, with its definitions
		fresh := newHTTPTool(t, s, "never_used_"+NewID()[28:], "hx")
		if err := s.HTTPTools().Delete(ctx, fresh.ID); err != nil {
			t.Fatalf("delete never used: %v", err)
		}
		if _, err := s.HTTPTools().Get(ctx, fresh.ID); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("deleted tool: %v", err)
		}
		if err := s.HTTPTools().Delete(ctx, fresh.ID); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("delete twice: %v", err)
		}
	})
}
