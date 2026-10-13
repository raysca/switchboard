package server

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRetryUntilSucceedsAfterTransientFailures(t *testing.T) {
	calls := 0
	err := retryUntil(t.Context(), time.Millisecond, func(context.Context) error {
		calls++
		if calls < 3 {
			return errors.New("MCP server unavailable")
		}
		return nil
	}, nil)
	if err != nil || calls != 3 {
		t.Fatalf("err=%v calls=%d, want nil after 3 attempts", err, calls)
	}
}

func TestRetryUntilReturnsTheLastErrorWhenTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Millisecond)
	defer cancel()
	want := errors.New("still unavailable")
	var retries int
	err := retryUntil(ctx, 5*time.Millisecond, func(context.Context) error { return want }, func(error) { retries++ })
	if !errors.Is(err, want) || retries == 0 {
		t.Fatalf("err=%v retries=%d", err, retries)
	}
}

func TestRetryUntilStopsPromptlyOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- retryUntil(ctx, time.Hour, func(context.Context) error { return errors.New("x") }, nil)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled retry must report the failure")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("retryUntil did not stop when the context was cancelled")
	}
}
