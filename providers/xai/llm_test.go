package xai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"govoice/core"
)

type mockSessionController struct {
	id          uint64
	gen         uint64
	reenteredMu sync.Mutex
	reentered   []core.Frame
	taskWg      sync.WaitGroup
}

func newMockSessionController(id, gen uint64) *mockSessionController {
	return &mockSessionController{id: id, gen: gen}
}

func (s *mockSessionController) ID() uint64 { return s.id }

func (s *mockSessionController) CurrentGeneration() uint64 { return s.gen }

func (s *mockSessionController) IncrementGeneration() uint64 {
	s.gen++
	return s.gen
}

func (s *mockSessionController) Go(fn func(ctx context.Context)) error {
	s.taskWg.Add(1)
	go func() {
		defer s.taskWg.Done()
		fn(context.Background())
	}()
	return nil
}

func (s *mockSessionController) Reenter(f core.Frame, dir core.Direction, resumeIndex int, gen uint64) error {
	s.reenteredMu.Lock()
	defer s.reenteredMu.Unlock()
	if gen == s.gen {
		s.reentered = append(s.reentered, f)
	}
	return nil
}

type mockEmitter struct {
	mu     sync.Mutex
	pushed []core.Frame
}

func (m *mockEmitter) Push(f core.Frame) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pushed = append(m.pushed, f)
	return nil
}

func (m *mockEmitter) PushDirection(f core.Frame, dir core.Direction) error {
	return m.Push(f)
}

func TestLLM_StoreFalseAndContextResending(t *testing.T) {
	var receivedBody responsesRequest
	var authHeader string
	var reqMu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqMu.Lock()
		authHeader = r.Header.Get("Authorization")
		bodyBytes, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(bodyBytes, &receivedBody)
		reqMu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "data: {\"choices\": [{\"delta\": {\"content\": \"Hello \"}}]}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"choices\": [{\"delta\": {\"content\": \"world!\"}}], \"usage\": {\"prompt_tokens\": 10, \"completion_tokens\": 2, \"total_tokens\": 12}}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	cfg := Config{
		Endpoint:   server.URL,
		Model:      "grok-2-1212",
		APIKey:     "xai-test-key-1234567890",
		HTTPClient: server.Client(),
		ContextProvider: func() []core.Message {
			return []core.Message{
				{Role: core.RoleSystem, Content: "System prompt"},
				{Role: core.RoleUser, Content: "Hello assistant"},
				{Role: core.RoleAssistant, Content: "Prior response"},
				{Role: core.RoleUser, Content: "Current question"},
			}
		},
	}

	proc, err := NewProcessor(cfg)
	if err != nil {
		t.Fatalf("new processor: %v", err)
	}

	session := newMockSessionController(1, 1)
	if err := proc.Attach(session, 0); err != nil {
		t.Fatalf("attach: %v", err)
	}

	userMsg := &core.UserMessageFrame{
		Turn:    "turn-1",
		Gen:     1,
		Content: "Current question",
	}

	emitter := &mockEmitter{}
	if err := proc.ProcessFrame(context.Background(), userMsg, core.Downstream, emitter); err != nil {
		t.Fatalf("process frame: %v", err)
	}

	session.taskWg.Wait()

	reqMu.Lock()
	body := receivedBody
	auth := authHeader
	reqMu.Unlock()

	// Verify store: false is strictly enforced
	if body.Store != false {
		t.Fatalf("expected store: false, got: %v", body.Store)
	}

	// Verify model
	if body.Model != "grok-2-1212" {
		t.Fatalf("expected model grok-2-1212, got: %s", body.Model)
	}

	// Verify auth header
	if auth != "Bearer xai-test-key-1234567890" {
		t.Fatalf("expected Bearer header, got: %s", auth)
	}

	// Verify complete context resent
	if len(body.Messages) != 4 {
		t.Fatalf("expected 4 messages resent in request, got %d", len(body.Messages))
	}
	if body.Messages[0].Content != "System prompt" || body.Messages[3].Content != "Current question" {
		t.Fatalf("message content mismatch: %+v", body.Messages)
	}

	// Verify re-entered frames: deltas and final text
	session.reenteredMu.Lock()
	reentered := session.reentered
	session.reenteredMu.Unlock()

	var textDeltas []string
	var fullText string
	for _, f := range reentered {
		switch df := f.(type) {
		case *core.LLMTextDeltaFrame:
			textDeltas = append(textDeltas, df.Delta)
		case *core.LLMTextFrame:
			fullText = df.Text
		}
	}

	if len(textDeltas) != 2 || textDeltas[0] != "Hello " || textDeltas[1] != "world!" {
		t.Fatalf("expected 2 deltas, got: %+v", textDeltas)
	}
	if fullText != "Hello world!" {
		t.Fatalf("expected full text 'Hello world!', got %q", fullText)
	}

	// Verify token usage tracked
	if proc.PromptTokens.Load() != 10 || proc.OutputTokens.Load() != 2 || proc.TotalTokens.Load() != 12 {
		t.Fatalf("token usage mismatch: p=%d, o=%d, t=%d",
			proc.PromptTokens.Load(), proc.OutputTokens.Load(), proc.TotalTokens.Load())
	}
}

func TestLLM_GenerationFencing(t *testing.T) {
	// Tests that deltas arriving after generation has advanced are discarded
	streamStarted := make(chan struct{})
	continueStream := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "data: {\"choices\": [{\"delta\": {\"content\": \"First delta\"}}]}\n\n")
		w.(http.Flusher).Flush()

		close(streamStarted)
		<-continueStream

		_, _ = fmt.Fprint(w, "data: {\"choices\": [{\"delta\": {\"content\": \"Late delta\"}}]}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	cfg := Config{
		Endpoint:   server.URL,
		Model:      "grok-2-1212",
		APIKey:     "xai-test-key",
		HTTPClient: server.Client(),
	}

	proc, err := NewProcessor(cfg)
	if err != nil {
		t.Fatalf("new processor: %v", err)
	}

	session := newMockSessionController(1, 1)
	_ = proc.Attach(session, 0)

	userMsg := &core.UserMessageFrame{
		Turn:    "turn-1",
		Gen:     1,
		Content: "test",
	}

	_ = proc.ProcessFrame(context.Background(), userMsg, core.Downstream, &mockEmitter{})

	<-streamStarted

	// Interruption occurs: session advances generation to 2
	session.IncrementGeneration()

	close(continueStream)
	session.taskWg.Wait()

	session.reenteredMu.Lock()
	reentered := session.reentered
	session.reenteredMu.Unlock()

	// Verify that "Late delta" was rejected by generation fencing
	for _, f := range reentered {
		if df, ok := f.(*core.LLMTextDeltaFrame); ok {
			if df.Delta == "Late delta" {
				t.Fatal("late delta for old generation was not rejected")
			}
		}
	}
}

func TestLLM_InterruptionCancellation(t *testing.T) {
	reqCancelled := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()

		<-r.Context().Done()
		close(reqCancelled)
	}))
	defer server.Close()

	cfg := Config{
		Endpoint:   server.URL,
		Model:      "grok-2-1212",
		APIKey:     "xai-test-key",
		HTTPClient: server.Client(),
	}

	proc, err := NewProcessor(cfg)
	if err != nil {
		t.Fatalf("new processor: %v", err)
	}

	session := newMockSessionController(1, 1)
	_ = proc.Attach(session, 0)

	userMsg := &core.UserMessageFrame{
		Turn:    "turn-1",
		Gen:     1,
		Content: "test cancel",
	}

	emitter := &mockEmitter{}
	_ = proc.ProcessFrame(context.Background(), userMsg, core.Downstream, emitter)

	// User barge-in: InterruptionFrame arrives
	time.Sleep(20 * time.Millisecond)
	interruption := &core.InterruptionFrame{
		Turn: "turn-1",
		Gen:  1,
	}
	_ = proc.ProcessFrame(context.Background(), interruption, core.Downstream, emitter)

	select {
	case <-reqCancelled:
		// Active HTTP request was cancelled immediately
	case <-time.After(500 * time.Millisecond):
		t.Fatal("active xAI request was not cancelled on interruption")
	}
}
