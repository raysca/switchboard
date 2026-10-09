package openai

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
)

// M3-07: a reply that is only reasoning must raise an error, not end the turn
// as an empty (silent) text.
func TestReasoningOnlyReplyIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sse(w, `{"choices":[{"delta":{"reasoning_content":"thinking..."}}]}`, `{"choices":[{"delta":{"reasoning":"more"}}]}`, "[DONE]")
	}))
	defer srv.Close()
	s := run(t, Config{BaseURL: srv.URL})
	for _, f := range s.got {
		if lf, ok := f.(*core.LLMTextFrame); ok {
			t.Fatalf("empty text frame %q played as silence", lf.Text)
		}
	}
	ef, ok := s.got[len(s.got)-1].(*core.ErrorFrame)
	if !ok || !errors.Is(ef.Err, ErrEmptyReply) || ef.Fatal {
		t.Fatalf("frames=%+v", s.got)
	}
}

// M3-07: once the model has started thinking, a timeout is not retried (a retry
// would think again from scratch: 3x the wait and the bill).
func TestSlowReasoningIsNotRetried(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"hmm\"}}]}\n\n"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	s := run(t, Config{BaseURL: srv.URL, Timeout: 200 * time.Millisecond})
	if hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1 (no retry after reasoning began)", hits.Load())
	}
	if _, ok := s.got[len(s.got)-1].(*core.ErrorFrame); !ok {
		t.Fatalf("frames=%+v", s.got)
	}
}

func TestReasoningThenContentSpeaksOnlyContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sse(w, `{"choices":[{"delta":{"reasoning_content":"think"}}]}`, `{"choices":[{"delta":{"content":"Four."}}]}`, "[DONE]")
	}))
	defer srv.Close()
	s := run(t, Config{BaseURL: srv.URL})
	var final string
	for _, f := range s.got {
		if lf, ok := f.(*core.LLMTextFrame); ok {
			final = lf.Text
		}
	}
	if final != "Four." {
		t.Fatalf("final=%q frames=%+v", final, s.got)
	}
}
