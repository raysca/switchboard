package xai

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"govoice/core"
)

// Live finding: the Responses API ends the stream with response.completed and sends no "[DONE]".
// That must complete the turn instead of failing with ErrMissingTerminalEvent.
func TestLLM_ResponsesStreamEndsWithCompletedAndNoDone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range []struct{ ev, data string }{
			{"response.created", `{"type":"response.created","response":{"output":[]}}`},
			{"response.output_item.added", `{"type":"response.output_item.added","item":{"id":"msg_1","role":"assistant","type":"message","content":[]},"output_index":0}`},
			{"response.output_text.delta", `{"type":"response.output_text.delta","delta":"Two plus two","item_id":"msg_1","output_index":0}`},
			{"response.output_text.delta", `{"type":"response.output_text.delta","delta":" is four.","item_id":"msg_1","output_index":0}`},
			{"response.output_text.done", `{"type":"response.output_text.done","text":"Two plus two is four.","item_id":"msg_1","output_index":0}`},
			{"response.completed", `{"type":"response.completed","response":{"output":[{"id":"msg_1","role":"assistant","type":"message","content":[{"type":"output_text","text":"Two plus two is four."}]}],"usage":{"input_tokens":12,"output_tokens":6,"total_tokens":18}}}`},
		} {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.ev, e.data)
		}
	}))
	defer srv.Close()
	proc, err := NewProcessor(Config{Endpoint: srv.URL, Model: "m", APIKey: "xai-test-key-1234567890", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	s := newMockSessionController(1, 1)
	if err := proc.Attach(s, 0); err != nil {
		t.Fatal(err)
	}
	if err := proc.ProcessFrame(context.Background(), &core.UserMessageFrame{Turn: "t", Gen: 1, Content: "q"}, core.Downstream, &mockEmitter{}); err != nil {
		t.Fatal(err)
	}
	s.taskWg.Wait()
	s.reenteredMu.Lock()
	defer s.reenteredMu.Unlock()
	var deltas int
	var final string
	for _, f := range s.reentered {
		switch v := f.(type) {
		case *core.ErrorFrame:
			t.Fatalf("unexpected error: %v", v.Err)
		case *core.LLMTextDeltaFrame:
			deltas++
		case *core.LLMTextFrame:
			final = v.Text
		}
	}
	if deltas != 2 || final != "Two plus two is four." {
		t.Fatalf("deltas=%d final=%q", deltas, final)
	}
}
