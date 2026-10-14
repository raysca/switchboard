package storage

import (
	"context"
	"strings"
	"testing"
)

// The schema says flows, steps and routes only: no table or column carries the old "agent" word.
func TestSchemaUsesFlowVocabulary(t *testing.T) {
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
			if strings.Contains(table, "agent") || strings.Contains(column, "agent") {
				t.Errorf("%s.%s still says agent", table, column)
			}
		}
		for _, want := range []string{"flows", "flow_versions", "flow_mcp_tools"} {
			if !tables[want] {
				t.Errorf("table %s is missing", want)
			}
		}
		for _, want := range []string{"sessions.flow_id", "sessions.flow_version_id", "flow_versions.flow_id"} {
			if !columns[want] {
				t.Errorf("column %s is missing", want)
			}
		}
	})
}
