package resilience

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
)

func TestExecuteReportsEachRetryBeforeItsBackoff(t *testing.T) {
	slept := noSleep(t)
	var retries []string
	p := Policy{MaxAttempts: 3, OnRetry: func(retry int) {
		retries = append(retries, fmt.Sprintf("retry %d after %d sleeps", retry, len(*slept)))
	}}
	if err := Execute(context.Background(), nil, p, classify, nil, fail(errOutage)); !errors.Is(err, errOutage) {
		t.Fatalf("err = %v", err)
	}
	if want := []string{"retry 1 after 0 sleeps", "retry 2 after 1 sleeps"}; !slices.Equal(retries, want) {
		t.Fatalf("retries %q, want %q", retries, want)
	}
	retries = nil
	_ = Execute(context.Background(), nil, p, classify, nil, fail(errClient))
	if len(retries) != 0 {
		t.Fatalf("a client error was retried: %q", retries)
	}
}

func TestExecuteContainsAPanickingRetryHook(t *testing.T) {
	noSleep(t)
	calls := 0
	p := Policy{MaxAttempts: 2, OnRetry: func(int) { panic("observer bug") }}
	err := Execute(context.Background(), nil, p, classify, nil, func(context.Context, int) error { calls++; return errOutage })
	if !errors.Is(err, errOutage) || calls != 2 {
		t.Fatalf("err = %v after %d attempts", err, calls)
	}
}
