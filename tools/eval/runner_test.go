package eval

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestRunAllProducesDistinctDeterministicP0Results(t *testing.T) {
	first, err := RunAll()
	if err != nil {
		t.Fatal(err)
	}
	second, err := RunAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Results) < 12 {
		t.Fatalf("expected P0 and independent compatibility scenarios, got %d", len(first.Results))
	}
	var a, b bytes.Buffer
	if err := json.NewEncoder(&a).Encode(first); err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(&b).Encode(second); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatalf("harness output is nondeterministic\n%s\n%s", a.Bytes(), b.Bytes())
	}
	seen := map[string]bool{}
	for _, result := range first.Results {
		if result.Scenario == "" || seen[result.Scenario] {
			t.Fatalf("missing or duplicate scenario %q", result.Scenario)
		}
		seen[result.Scenario] = true
		if result.DurationNS < 0 || len(result.Steps) == 0 {
			t.Fatalf("incomplete result: %#v", result)
		}
	}
	if first.Compatibility.RTVI.Name == first.Compatibility.Whisker.Name || first.Compatibility.Whisker.Name == first.Compatibility.Pipecat.Name {
		t.Fatalf("compatibility suite statuses are not independently represented: %#v", first.Compatibility)
	}
	if first.Compatibility.RTVI.Status != "separate_suite" || first.Compatibility.Whisker.Status != "pass" || first.Compatibility.Pipecat.Status != "separate_suite" {
		t.Fatalf("unrun compatibility suites were promoted or collapsed: %#v", first.Compatibility)
	}
	if first.Compatibility.PlaybackClear != "UNMODIFIED_CLIENT_CANNOT_CLEAR_BUFFERED_PLAYBACK_ON_WIRE_EVENT" {
		t.Fatalf("playback probe limitation was lost: %q", first.Compatibility.PlaybackClear)
	}
}

func TestFailedScenarioMakesExitCodeNonzero(t *testing.T) {
	code := ExitCode(Report{Results: []Result{{Scenario: "probe", Passed: false}}})
	if code == 0 {
		t.Fatal("failed result returned a successful exit code")
	}
}
