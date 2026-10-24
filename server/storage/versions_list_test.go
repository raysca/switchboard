package storage

import (
	"context"
	"fmt"
	"testing"
)

func TestListVersionsIsNewestFirstWithToolCountsAndBounded(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		r := newToolRig(t, s) // one published version, no tools
		tools := []SelectedTool{r.sel("echo", false), r.sel("lookup", true)}
		if _, _, err := r.publish(t, AgentMutation{Tools: &tools}); err != nil {
			t.Fatal(err)
		}
		got, err := s.Agents().ListVersions(ctx, r.agentID)
		if err != nil || len(got) != 2 || got[0].Number != 2 || got[0].ToolCount != 2 || got[1].Number != 1 || got[1].ToolCount != 0 {
			t.Fatalf("versions = %+v %v", got, err)
		}
		for i := 0; i < MaxVersionList+5; i++ {
			cfg := []byte(fmt.Sprintf(`{"stt":{"provider":"google","model":"s"},"llm":{"provider":"google","model":"l%d"},"tts":{"provider":"google","voice":"t"}}`, i))
			if _, _, err := s.Agents().MutateAndPublish(ctx, r.agentID, AgentMutation{}, cfg); err != nil {
				t.Fatal(err)
			}
		}
		got, _ = s.Agents().ListVersions(ctx, r.agentID)
		if len(got) != MaxVersionList || got[0].Number != MaxVersionList+7 {
			t.Fatalf("len=%d newest=%d", len(got), got[0].Number)
		}
	})
}
