package turnquality

import "testing"

// TestGate is the CI gate: the committed thresholds hold on the offline
// strategies (fake oracles; see the package comment for what that proves).
func TestGate(t *testing.T) {
	rs, err := RunDefault()
	if err != nil {
		t.Fatal(err)
	}
	th, err := DefaultThresholds()
	if err != nil {
		t.Fatal(err)
	}
	if fails := th.Check(rs); len(fails) > 0 {
		t.Fatalf("turn-quality regression: %v", fails)
	}
	for _, r := range rs {
		if r.Strategy == "llm-completion-perfect" && (r.Errors != 0 || r.N < 100) {
			t.Fatalf("plumbing: %+v", r)
		}
	}
}

func TestGateCatchesRegression(t *testing.T) {
	th := Thresholds{"x": {MaxFalseCut: 0.1, MaxLateCut: 0.1, Calibrated: true}}
	if len(th.Check([]Rates{{Strategy: "x", Status: "ok", FalseCut: 0.2}})) != 1 {
		t.Fatal("regression not flagged")
	}
}
