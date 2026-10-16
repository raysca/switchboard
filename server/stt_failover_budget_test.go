package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"govoice/core/resilience"
	coreturn "govoice/core/turn"
	"govoice/providers/cartesia"
	"govoice/providers/deepgram"
	"govoice/providers/elevenlabs"
	"govoice/providers/google"
	"govoice/providers/nvidia"
)

// Review Focus 1: a failover candidate whose STT dial hangs must give up (so
// the ErrorFrame reaches STTFailover) before the turn processor drops the
// untranscribed turn. NoTranscriptTimeout counts from silence onset and the
// dial starts at speech onset, so finishing within it is a conservative bound.
// Both retry helpers the adapters use (OpenBounded, Execute) are timed with
// the real policy against a dial that hangs until its attempt is cut.
func TestSTTFailoverPolicyReportsAHungDialBeforeTheTurnIsDropped(t *testing.T) {
	retry := func(error) resilience.Verdict { return resilience.Verdict{Retryable: true} }
	for _, tc := range []struct {
		name string
		base resilience.Policy
	}{
		{"deepgram", deepgram.DefaultSTTConfig().RetryPolicy},
		{"cartesia", cartesia.DefaultSTTConfig().RetryPolicy},
		{"elevenlabs", elevenlabs.DefaultSTTConfig().RetryPolicy},
		{"nvidia", nvidia.DefaultSTTConfig().RetryPolicy},
		{"google", google.DefaultSTTOpenPolicy()}, // xai's DefaultSTTOpenPolicy is the same 2 x 5 s (build-tagged)
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pol := failoverPolicy("stt", tc.base)
			start := time.Now()
			_, err := resilience.OpenBounded(context.Background(), pol, retry, func(ctx context.Context) (struct{}, error) {
				<-ctx.Done()
				return struct{}{}, ctx.Err()
			})
			if took := time.Since(start); !errors.Is(err, resilience.ErrTimeout) || took >= coreturn.DefaultNoTranscriptTimeout {
				t.Fatalf("OpenBounded: err=%v after %v; must give up before %v", err, took, coreturn.DefaultNoTranscriptTimeout)
			}
			start = time.Now()
			err = resilience.Execute(context.Background(), nil, pol, retry, nil, func(ctx context.Context, _ int) error {
				<-ctx.Done()
				return ctx.Err()
			})
			if took := time.Since(start); !errors.Is(err, resilience.ErrTimeout) || took >= coreturn.DefaultNoTranscriptTimeout {
				t.Fatalf("Execute: err=%v after %v; must give up before %v", err, took, coreturn.DefaultNoTranscriptTimeout)
			}
		})
	}
}
