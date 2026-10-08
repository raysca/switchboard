//go:build integration

package nvidia

import (
	"context"
	"os"
	"testing"
	"time"
)

// Opens a hosted Riva stream (NVIDIA_API_KEY, NVIDIA_FUNCTION_ID) and checks it
// accepts the config message. Costs nothing: no audio is sent.
func TestIntegration_HostedStreamOpens(t *testing.T) {
	key, fn := os.Getenv("NVIDIA_API_KEY"), os.Getenv("NVIDIA_FUNCTION_ID")
	if key == "" || fn == "" {
		t.Skip("NVIDIA_API_KEY and NVIDIA_FUNCTION_ID not set")
	}
	cfg := DefaultSTTConfig()
	cfg.APIKey, cfg.FunctionID = key, fn
	p, err := NewSTTProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := p.Warm(ctx); err != nil {
		t.Fatalf("warm: %v", err)
	}
	st, err := p.openOnce(ctx)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_ = st.rpc.CloseSend()
}
