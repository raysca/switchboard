package xai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"govoice/core"
)

// A request that runs past the timeout is a provider failure the user must
// hear about, not a silent end of the turn.
func TestLLMRequestTimeoutSurfacesAnError(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	proc, err := NewProcessor(Config{
		Endpoint: server.URL, Model: "m", APIKey: "k", HTTPClient: server.Client(),
		Timeout: 50 * time.Millisecond, RetryPolicy: RetryPolicy{MaxAttempts: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	session := newMockSessionController(1, 1)
	_ = proc.Attach(session, 0)
	if err := proc.ProcessFrame(context.Background(), &core.UserMessageFrame{Turn: "t", Gen: 1, Content: "hi"}, core.Downstream, &mockEmitter{}); err != nil {
		t.Fatal(err)
	}
	session.taskWg.Wait()
	session.reenteredMu.Lock()
	defer session.reenteredMu.Unlock()
	for _, f := range session.reentered {
		if _, ok := f.(*core.ErrorFrame); ok {
			return
		}
	}
	t.Fatalf("timed-out request ended silently: %#v", session.reentered)
}
