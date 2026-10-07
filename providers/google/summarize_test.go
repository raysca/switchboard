package google

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/resilience"
)

func summaryProc(t *testing.T, c LLMStreamClient, br *resilience.Breaker, timeout time.Duration) *LLMProcessor {
	t.Helper()
	cfg := DefaultLLMConfig()
	cfg.Client, cfg.CircuitBreaker = c, br
	cfg.RetryPolicy.AttemptTimeout = timeout
	p, err := NewLLMProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSummarizeText(t *testing.T) {
	var sys string
	var hist []core.Message
	c := &mockLLMClient{generateFn: func(_ context.Context, _ string, system string, h []core.Message) (<-chan LLMStreamChunk, <-chan error, error) {
		sys, hist = system, h
		ch, ec := make(chan LLMStreamChunk, 2), make(chan error)
		ch <- LLMStreamChunk{Text: " the "}
		ch <- LLMStreamChunk{Text: "summary ", IsDone: true}
		close(ch)
		close(ec)
		return ch, ec, nil
	}}
	got, err := summaryProc(t, c, nil, time.Second).SummarizeText(context.Background(), "sys", "user text")
	if err != nil || got != "the summary" || sys != "sys" || len(hist) != 1 || hist[0].Role != core.RoleUser || hist[0].Content != "user text" {
		t.Fatalf("got %q err %v sys=%q hist=%v", got, err, sys, hist)
	}
}

func TestSummarizeTextFailuresNeverTouchBreakerAndRedact(t *testing.T) {
	br := resilience.NewBreaker(resilience.Config{FailureThreshold: 1, Cooldown: time.Minute})
	failing := &mockLLMClient{generateFn: func(context.Context, string, string, []core.Message) (<-chan LLMStreamChunk, <-chan error, error) {
		ec := make(chan error, 1)
		ec <- MapError("llm", errors.New("boom key=AIzaSyAbcdefghijklmnopqrstuvwxyz012345"))
		close(ec)
		return nil, ec, nil
	}}
	p := summaryProc(t, failing, br, time.Second)
	for i := 0; i < 3; i++ {
		if _, err := p.SummarizeText(context.Background(), "s", "u"); err == nil || strings.Contains(err.Error(), "AIzaSyAbcdef") {
			t.Fatalf("err = %v", err)
		}
	}
	if br.WouldRefuse() {
		t.Fatal("summary failures opened the LLM breaker")
	}
	hung := &mockLLMClient{generateFn: func(ctx context.Context, _ string, _ string, _ []core.Message) (<-chan LLMStreamChunk, <-chan error, error) {
		return make(chan LLMStreamChunk), make(chan error), nil // never answers
	}}
	start := time.Now()
	if _, err := summaryProc(t, hung, br, 50*time.Millisecond).SummarizeText(context.Background(), "s", "u"); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("timeout not enforced: err=%v after %v", err, time.Since(start))
	}
}

func TestSummarizeTextBlockedEmptyAndOpenBreaker(t *testing.T) {
	stream := func(chunks ...LLMStreamChunk) *mockLLMClient {
		return &mockLLMClient{generateFn: func(context.Context, string, string, []core.Message) (<-chan LLMStreamChunk, <-chan error, error) {
			ch, ec := make(chan LLMStreamChunk, len(chunks)), make(chan error)
			for _, c := range chunks {
				ch <- c
			}
			close(ch)
			close(ec)
			return ch, ec, nil
		}}
	}
	blocked := stream(LLMStreamChunk{Text: "partial "}, LLMStreamChunk{FinishReason: "SAFETY"})
	if got, err := summaryProc(t, blocked, nil, time.Second).SummarizeText(context.Background(), "s", "u"); err == nil || got != "" {
		t.Fatalf("safety-blocked summary accepted: %q %v", got, err)
	}
	if got, err := summaryProc(t, stream(LLMStreamChunk{FinishReason: "STOP"}), nil, time.Second).SummarizeText(context.Background(), "s", "u"); err != nil || got != "" {
		t.Fatalf("empty: %q %v (core rejects the empty summary)", got, err)
	}
	br := resilience.NewBreaker(resilience.Config{FailureThreshold: 1, Cooldown: time.Minute})
	br.RecordFailure()
	called := false
	c := &mockLLMClient{generateFn: func(context.Context, string, string, []core.Message) (<-chan LLMStreamChunk, <-chan error, error) {
		called = true
		return nil, nil, errors.New("down")
	}}
	if _, err := summaryProc(t, c, br, time.Second).SummarizeText(context.Background(), "s", "u"); !errors.Is(err, resilience.ErrCircuitOpen) || called {
		t.Fatalf("open breaker: err=%v called=%v", err, called)
	}
}
