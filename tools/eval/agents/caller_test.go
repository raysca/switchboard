package main

import (
	"context"
	"testing"
)

func TestScriptCaller(t *testing.T) {
	next := ScriptCaller([]string{"one", "two"})
	for _, want := range []string{"one", "two"} {
		line, done, err := next(context.Background(), nil)
		if line != want || done || err != nil {
			t.Fatalf("got %q done=%v err=%v, want %q", line, done, err, want)
		}
	}
	for range 2 { // stays done
		if line, done, err := next(context.Background(), nil); line != "" || !done || err != nil {
			t.Fatalf("after the script: %q done=%v err=%v", line, done, err)
		}
	}
	if _, _, err := ScriptCaller([]string{"x"})(canceled(), nil); err == nil {
		t.Error("a canceled context did not stop the caller")
	}
}

func canceled() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}
