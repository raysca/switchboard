package adapter

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestLimiterBoundsConcurrencyAndTimesOut(t *testing.T) {
	l := NewLimiter(1, 20*time.Millisecond)
	rel, err := l.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Acquire(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatalf("second acquire: %v, want ErrBusy", err)
	}
	rel()
	rel2, err := l.Acquire(context.Background())
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	rel2()
}

func TestLimiterHonoursCancel(t *testing.T) {
	l := NewLimiter(1, time.Minute)
	rel, _ := l.Acquire(context.Background())
	defer rel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := l.Acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}
