package google

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"govoice/core"
)

func runGeminiTurn(t *testing.T, chunks ...LLMStreamChunk) (frames []core.Frame) {
	t.Helper()
	cfg := DefaultLLMConfig()
	cfg.Client = &mockLLMClient{generateFn: func(context.Context, string, string, []core.Message) (<-chan LLMStreamChunk, <-chan error, error) {
		ch, errCh := make(chan LLMStreamChunk, len(chunks)), make(chan error)
		for _, c := range chunks {
			ch <- c
		}
		close(ch)
		close(errCh)
		return ch, errCh, nil
	}}
	proc, err := NewLLMProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	pipe, _ := core.NewPipeline(proc)
	session, _ := core.NewSession(1, pipe, core.DefaultResourceBudget())
	var mu sync.Mutex
	session.OnOutput = func(f core.Frame) { mu.Lock(); frames = append(frames, f); mu.Unlock() }
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	_ = session.Post(&core.UserMessageFrame{Turn: "t", Gen: 1, Content: "hi"})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		for _, f := range frames {
			switch f.(type) {
			case *core.LLMTextFrame, *core.ErrorFrame:
				out := append([]core.Frame(nil), frames...)
				mu.Unlock()
				return out
			}
		}
		mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("turn produced neither text nor error")
	return nil
}

func lastError(frames []core.Frame) error {
	for _, f := range frames {
		if ef, ok := f.(*core.ErrorFrame); ok {
			return ef.Err
		}
	}
	return nil
}

// M3-08: a SAFETY stop with no text, or a blocked prompt, is an error, not a
// silent end of turn.
func TestGeminiBlockedReplySurfacesError(t *testing.T) {
	frames := runGeminiTurn(t, LLMStreamChunk{FinishReason: "SAFETY"})
	err := lastError(frames)
	if !errors.Is(err, ErrBlockedReply) {
		t.Fatalf("err = %v (frames %+v), want ErrBlockedReply", err, frames)
	}
	for _, f := range frames {
		if _, ok := f.(*core.LLMTextFrame); ok {
			t.Fatal("blocked reply also produced an (empty) text frame")
		}
	}
}

func TestGeminiEmptyStopSurfacesError(t *testing.T) {
	if err := lastError(runGeminiTurn(t, LLMStreamChunk{FinishReason: "STOP"})); !errors.Is(err, ErrEmptyReply) {
		t.Fatalf("err = %v, want ErrEmptyReply", err)
	}
}

// An empty reply cut by the max_output_tokens cap (thinking tokens count toward it)
// is an empty reply with a cause, not a safety block.
func TestGeminiMaxTokensEmptyIsEmptyReplyNotBlocked(t *testing.T) {
	err := lastError(runGeminiTurn(t, LLMStreamChunk{FinishReason: "MAX_TOKENS"}))
	if !errors.Is(err, ErrEmptyReply) || errors.Is(err, ErrBlockedReply) {
		t.Fatalf("err = %v, want ErrEmptyReply and not ErrBlockedReply", err)
	}
	if !strings.Contains(err.Error(), "max_output_tokens") {
		t.Fatalf("err = %v, want it to name max_output_tokens", err)
	}
}

func TestGeminiTextWithSafetyStopStillSpeaks(t *testing.T) {
	frames := runGeminiTurn(t, LLMStreamChunk{Text: "Partial."}, LLMStreamChunk{FinishReason: "SAFETY"})
	if err := lastError(frames); err != nil {
		t.Fatalf("a partial reply raised %v", err)
	}
}
