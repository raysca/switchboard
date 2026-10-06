package scenarios_test

import (
	"testing"

	"govoice/tools/eval/scenarios"
)

func TestSevenVerticalScenariosDefinitions(t *testing.T) {
	all := scenarios.SevenScenarios()
	if len(all) != 7 {
		t.Fatalf("expected exactly 7 vertical product scenarios, got %d", len(all))
	}

	seenIDs := make(map[string]bool)
	seenNumbers := make(map[int]bool)

	for _, sc := range all {
		if sc.ID == "" {
			t.Errorf("scenario %d has empty ID", sc.Number)
		}
		if seenIDs[sc.ID] {
			t.Errorf("duplicate scenario ID: %s", sc.ID)
		}
		seenIDs[sc.ID] = true

		if sc.Number < 1 || sc.Number > 7 {
			t.Errorf("invalid scenario number: %d", sc.Number)
		}
		if seenNumbers[sc.Number] {
			t.Errorf("duplicate scenario number: %d", sc.Number)
		}
		seenNumbers[sc.Number] = true

		if sc.Name == "" || sc.Description == "" {
			t.Errorf("scenario %d missing Name or Description", sc.Number)
		}
		if len(sc.Invariants) == 0 {
			t.Errorf("scenario %d has no invariants", sc.Number)
		}
		if len(sc.Steps) == 0 {
			t.Errorf("scenario %d has no steps", sc.Number)
		}

		retrieved, err := scenarios.GetScenario(sc.Number)
		if err != nil || retrieved.ID != sc.ID {
			t.Errorf("GetScenario by number %d failed: %v", sc.Number, err)
		}

		retrievedByID, err := scenarios.GetScenario(sc.ID)
		if err != nil || retrievedByID.Number != sc.Number {
			t.Errorf("GetScenario by ID %s failed: %v", sc.ID, err)
		}
	}
}

func TestExtendedParityScenariosDefinitions(t *testing.T) {
	all := scenarios.AllScenarios()
	if len(all) != 10 {
		t.Fatalf("expected 10 total scenarios (7 vertical + 3 extended parity), got %d", len(all))
	}

	p2 := scenarios.ExtendedParityScenarios()
	if len(p2) != 3 {
		t.Fatalf("expected 3 extended parity scenarios, got %d", len(p2))
	}

	for _, sc := range p2 {
		if sc.Number < 8 || sc.Number > 10 {
			t.Errorf("expected scenario number between 8 and 10, got %d", sc.Number)
		}
		if sc.ID == "" || sc.Name == "" || len(sc.Invariants) == 0 {
			t.Errorf("invalid extended scenario definition: %+v", sc)
		}
		retrieved, err := scenarios.GetScenario(sc.Number)
		if err != nil || retrieved.ID != sc.ID {
			t.Errorf("GetScenario(%d) failed: %v", sc.Number, err)
		}
	}
}
