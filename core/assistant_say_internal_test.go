package core

import "testing"

func TestSayTurnsAreSyntheticForTheCompletionGate(t *testing.T) {
	if !isSyntheticTurn("say-3") {
		t.Fatal("a greeting would be judged as a model reply (verdict metrics, marker stripping, held text)")
	}
}
