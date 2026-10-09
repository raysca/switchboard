package xai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"govoice/core"
)

func runResponsesTurn(t *testing.T, events []string) (final string, errs []error, hits int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, d := range events {
			fmt.Fprintf(w, "data: %s\n\n", d)
		}
	}))
	defer srv.Close()
	proc, err := NewProcessor(Config{Endpoint: srv.URL, Model: "m", APIKey: "xai-test-key-1234567890", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	s := newMockSessionController(1, 1)
	_ = proc.Attach(s, 0)
	if err := proc.ProcessFrame(context.Background(), &core.UserMessageFrame{Turn: "t", Gen: 1, Content: "q"}, core.Downstream, &mockEmitter{}); err != nil {
		t.Fatal(err)
	}
	s.taskWg.Wait()
	s.reenteredMu.Lock()
	defer s.reenteredMu.Unlock()
	for _, f := range s.reentered {
		switch v := f.(type) {
		case *core.ErrorFrame:
			errs = append(errs, v.Err)
		case *core.LLMTextFrame:
			final = v.Text
		}
	}
	return final, errs, n.Load()
}

// M3-10: a request-caused "error" event is surfaced with the server's code and
// message, is not retried, and does not count as an outage.
func TestResponsesErrorEventIsClassifiedAndNotRetried(t *testing.T) {
	_, errs, hits := runResponsesTurn(t, []string{
		`{"type":"error","code":"context_length_exceeded","message":"prompt is too long"}`,
	})
	var pe *ProviderError
	if len(errs) != 1 || !errors.As(errs[0], &pe) {
		t.Fatalf("errors = %v", errs)
	}
	if pe.Class != ClassificationNonRetryable || pe.Code != "CONTEXT_LENGTH_EXCEEDED" || pe.Message != "prompt is too long" || hits != 1 {
		t.Fatalf("class=%v code=%s msg=%q hits=%d", pe.Class, pe.Code, pe.Message, hits)
	}
	if v := classify(errs[0]); v.Retryable || !v.ClientError {
		t.Fatalf("verdict %+v: a request-caused failure must not count on the breaker", v)
	}
}

func TestResponsesFailedKeepsServerReason(t *testing.T) {
	var c chatChunkResponse
	if err := json.Unmarshal([]byte(`{"type":"response.failed","response":{"error":{"code":"server_error","message":"boom xai-abcdefghijklmnopqrstuvwxyz"}}}`), &c); err != nil {
		t.Fatal(err)
	}
	pe := responseEventProviderError(&c)
	if pe.Class != ClassificationRetryable || pe.Code != "SERVER_ERROR" || pe.Message != "boom xai-[REDACTED]" {
		t.Fatalf("%+v", pe)
	}
	c = chatChunkResponse{}
	_ = json.Unmarshal([]byte(`{"type":"response.failed","response":{"error":{"code":"rate_limit_exceeded","message":"slow down"}}}`), &c)
	if pe := responseEventProviderError(&c); pe.Class != ClassificationRateLimit {
		t.Fatalf("rate limit class = %v", pe.Class)
	}
}

// response.incomplete is terminal: the streamed text is the reply, no retry.
func TestResponsesIncompleteEndsTheTurnWithStreamedText(t *testing.T) {
	final, errs, hits := runResponsesTurn(t, []string{
		`{"type":"response.output_text.delta","delta":"Half an answ","item_id":"m","output_index":0}`,
		`{"type":"response.incomplete","response":{"incomplete_details":{"reason":"max_output_tokens"}}}`,
	})
	if len(errs) != 0 || final != "Half an answ" || hits != 1 {
		t.Fatalf("final=%q errs=%v hits=%d", final, errs, hits)
	}
}
