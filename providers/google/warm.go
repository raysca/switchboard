package google

import (
	"context"
	"errors"
	"time"

	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/keepalive"

	"govoice/core/netpool"
	"govoice/core/resilience"
)

// grpcKeepalive pings only while an RPC is in flight (no PermitWithoutStream:
// Google's front ends send GOAWAY too_many_pings to idle channels that ping),
// so a half-dead connection mid-utterance fails in ~80 s instead of hanging.
// Time stays >= 60 s, the shortest interval Google's own Go clients use against
// the same front ends (pubsublite 1 min, spanner 2 min); shorter risks
// ENHANCE_YOUR_CALM on a long, silent STT stream where the server sends nothing.
func grpcKeepalive() option.ClientOption {
	return option.WithGRPCDialOption(grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 60 * time.Second, Timeout: 20 * time.Second}))
}

// warmConn starts the lazy gRPC channel (DNS + TCP + TLS + HTTP/2 settings)
// and waits for READY or ctx. No RPC is sent: no token fetch, no billing.
func warmConn(ctx context.Context, conn *grpc.ClientConn, stage string) error {
	if !netpool.PredialEnabled() {
		netpool.NoteSkipped("google", stage)
		return nil
	}
	start := time.Now()
	conn.Connect()
	var err error
	for s := conn.GetState(); s != connectivity.Ready; s = conn.GetState() {
		if s == connectivity.Shutdown {
			err = errors.New("google: channel closed")
			break
		}
		if s == connectivity.TransientFailure {
			err = errors.New("google: channel connect failed")
			break
		}
		if !conn.WaitForStateChange(ctx, s) {
			err = ctx.Err()
			break
		}
	}
	netpool.Observe("google", stage, time.Since(start), err)
	return err
}

func breakerOpen(b *resilience.Breaker, stage string) bool {
	if b != nil && b.WouldRefuse() {
		netpool.NoteSkipped("google", stage)
		return true
	}
	return false
}

// Warm brings the session's STT channel to READY before the first utterance.
func (o *RealGoogleSTTOpener) Warm(ctx context.Context) error {
	if o.client == nil {
		return nil
	}
	return warmConn(ctx, o.client.Connection(), "stt")
}

// Warm brings the session's TTS channel to READY before the first sentence.
func (c *RealGoogleTTSClient) Warm(ctx context.Context) error {
	if c.client == nil {
		return nil
	}
	return warmConn(ctx, c.client.Connection(), "tts")
}

// Warm pre-dials TCP+TLS to the Gemini API host (API-key mode only: Vertex
// uses its own authenticated transport).
func (c *RealGoogleLLMClient) Warm(ctx context.Context) error {
	if c.pool == nil {
		return nil
	}
	return c.pool.Warm(ctx, geminiAPIURL)
}

const geminiAPIURL = "https://generativelanguage.googleapis.com/"

// Warm delegates to the STT stream opener when it can warm itself.
func (p *STTProcessor) Warm(ctx context.Context) error {
	w, ok := p.cfg.StreamOpener.(netpool.Warmer)
	if !ok || breakerOpen(p.cfg.CircuitBreaker, "stt") {
		return nil
	}
	return w.Warm(ctx)
}

// Warm delegates to the synthesis client when it can warm itself.
func (p *TTSProcessor) Warm(ctx context.Context) error {
	w, ok := p.cfg.Client.(netpool.Warmer)
	if !ok || breakerOpen(p.cfg.CircuitBreaker, "tts") {
		return nil
	}
	return w.Warm(ctx)
}

// Warm delegates to the LLM client when it can warm itself.
func (p *LLMProcessor) Warm(ctx context.Context) error {
	w, ok := p.cfg.Client.(netpool.Warmer)
	if !ok || breakerOpen(p.cfg.CircuitBreaker, "llm") {
		return nil
	}
	return w.Warm(ctx)
}
