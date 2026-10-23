package storage

import (
	"context"
	"strings"
	"testing"
)

// The schema says agents, steps and handoffs only: no table, column, trigger, index or function body carries the old "flow" word.
func TestSchemaUsesAgentVocabulary(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		var query string
		if s.dialect == Postgres {
			query = "SELECT table_name, column_name FROM information_schema.columns WHERE table_schema = current_schema()"
		} else {
			query = "SELECT m.name, p.name FROM sqlite_master m JOIN pragma_table_info(m.name) p WHERE m.type = 'table'"
		}
		rows, err := s.DB().QueryContext(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		tables, columns := map[string]bool{}, map[string]bool{}
		for rows.Next() {
			var table, column string
			if err := rows.Scan(&table, &column); err != nil {
				t.Fatal(err)
			}
			tables[table], columns[table+"."+column] = true, true
			if strings.Contains(table, "flow") || strings.Contains(column, "flow") {
				t.Errorf("%s.%s still says flow", table, column)
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		// Triggers, indexes and functions carry the vocabulary too.
		var names string
		if s.dialect == Postgres {
			names = "SELECT tgname FROM pg_trigger WHERE NOT tgisinternal UNION SELECT indexname FROM pg_indexes WHERE schemaname = current_schema() UNION SELECT proname FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname = current_schema()"
		} else {
			names = "SELECT name FROM sqlite_master WHERE type IN ('trigger', 'index')"
		}
		objs, err := s.DB().QueryContext(ctx, names)
		if err != nil {
			t.Fatal(err)
		}
		defer objs.Close()
		for objs.Next() {
			var name string
			if err := objs.Scan(&name); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(name, "flow") {
				t.Errorf("schema object %s still says flow", name)
			}
		}
		if err := objs.Err(); err != nil {
			t.Fatal(err)
		}
		// Function bodies and trigger/index definitions (RAISE text included) carry the vocabulary too.
		var defs string
		if s.dialect == Postgres {
			defs = "SELECT p.prosrc FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname = current_schema()"
		} else {
			defs = "SELECT sql FROM sqlite_master WHERE sql IS NOT NULL"
		}
		bodies, err := s.DB().QueryContext(ctx, defs)
		if err != nil {
			t.Fatal(err)
		}
		defer bodies.Close()
		for bodies.Next() {
			var body string
			if err := bodies.Scan(&body); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(strings.ToLower(body), "flow") {
				t.Errorf("schema definition still says flow: %.120s", body)
			}
		}
		if err := bodies.Err(); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"agents", "agent_versions", "agent_mcp_tools"} {
			if !tables[want] {
				t.Errorf("table %s is missing", want)
			}
		}
		for _, want := range []string{"sessions.agent_id", "sessions.agent_version_id", "agent_versions.agent_id"} {
			if !columns[want] {
				t.Errorf("column %s is missing", want)
			}
		}
	})
}
