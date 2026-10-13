package core_test

import (
	"context"
	"testing"

	"govoice/core"
	"govoice/core/coretest"
)

type summarizingFake struct {
	*coretest.LLMProcessor
	calls int
}

func (f *summarizingFake) SummarizeText(_ context.Context, system, user string) (string, error) {
	f.calls++
	return system + "|" + user, nil
}

func TestLLMFailoverSummarizeTextForwardsToPrimaryOnly(t *testing.T) {
	primary, fallback := &summarizingFake{LLMProcessor: coretest.NewLLMProcessor(coretest.LLMConfig{})}, &summarizingFake{LLMProcessor: coretest.NewLLMProcessor(coretest.LLMConfig{})}
	w, err := core.NewLLMFailover(core.LLMFailoverConfig{Candidates: []core.LLMCandidate{{Provider: "a", Proc: primary}, {Provider: "b", Proc: fallback}}})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := w.SummarizeText(context.Background(), "s", "u"); err != nil || got != "s|u" || primary.calls != 1 || fallback.calls != 0 {
		t.Fatalf("got %q err %v calls %d/%d", got, err, primary.calls, fallback.calls)
	}
	// A primary without the capability errors; the fallback is never asked.
	w, _ = core.NewLLMFailover(core.LLMFailoverConfig{Candidates: []core.LLMCandidate{{Provider: "a", Proc: coretest.NewLLMProcessor(coretest.LLMConfig{})}, {Provider: "b", Proc: fallback}}})
	if _, err := w.SummarizeText(context.Background(), "s", "u"); err == nil || fallback.calls != 0 {
		t.Fatalf("err %v fallback calls %d", err, fallback.calls)
	}
}
