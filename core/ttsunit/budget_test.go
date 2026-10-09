package ttsunit

import (
	"context"
	"testing"
	"time"

	"govoice/core"
)

// Queued units must not each hold a session task slot: a reply with more
// sentences than MaxAsyncTasks loses none of them (M1-03).
func TestUnitsBeyondTaskBudgetAreNotDropped(t *testing.T) {
	release := make(chan struct{})
	h := start(t, func(ctx context.Context, u *Unit, _ int) error {
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		_ = u.Write(make([]byte, 640))
		return u.Finish()
	})
	const n = 40 // budget is 16 tasks
	for i := 1; i <= n; i++ {
		h.say(t, uint64(i), "A sentence to speak.", i == n)
	}
	time.Sleep(100 * time.Millisecond) // every unit is queued behind the first
	close(release)
	waitFor(t, "all units finished", func() bool { return len(of[*core.TTSStoppedFrame](h)) == n })
	if errs := of[*core.ErrorFrame](h); len(errs) != 0 {
		t.Fatalf("%d units failed, first: %v", len(errs), errs[0].Err)
	}
}
