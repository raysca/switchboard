package xai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
)

type mockSessionController struct {
	id          uint64
	gen         atomic.Uint64
	reenteredMu sync.Mutex
	reentered   []core.Frame
	taskWg      sync.WaitGroup
}

func newMockSessionController(id, gen uint64) *mockSessionController {
	s := &mockSessionController{id: id}
	s.gen.Store(gen)
	return s
}

func (s *mockSessionController) ID() uint64 { return s.id }

func (s *mockSessionController) Context() context.Context { return context.Background() }

func (s *mockSessionController) CurrentGeneration() uint64 { return s.gen.Load() }

func (s *mockSessionController) IncrementGeneration() uint64 {
	return s.gen.Add(1)
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
	if gen == s.gen.Load() {
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
	var rawBody []byte
	var authHeader string
	var reqMu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqMu.Lock()
		authHeader = r.Header.Get("Authorization")
		bodyBytes, _ := io.ReadAll(r.Body)
		rawBody = append([]byte(nil), bodyBytes...)
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
	raw := append([]byte(nil), rawBody...)
	auth := authHeader
	reqMu.Unlock()

	if bytes.Contains(raw, []byte(`"messages"`)) {
		t.Fatalf("Responses API rejects messages; request body: %s", raw)
	}
	if !bytes.Contains(raw, []byte(`"input"`)) {
		t.Fatalf("Responses API requires input; request body: %s", raw)
	}

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
	if len(body.Input) != 4 {
		t.Fatalf("expected 4 input items resent in request, got %d", len(body.Input))
	}
	if body.Input[0].Content != "System prompt" || body.Input[3].Content != "Current question" {
		t.Fatalf("input content mismatch: %+v", body.Input)
	}

	// Verify re-entered frames: deltas and final text
	session.reenteredMu.Lock()
	reentered := session.reentered
	session.reenteredMu.Unlock()

	var textDeltas []string
	var fullText string
	var usageFrames []*core.UsageMetricsFrame
	for _, f := range reentered {
		switch df := f.(type) {
		case *core.LLMTextDeltaFrame:
			textDeltas = append(textDeltas, df.Delta)
		case *core.LLMTextFrame:
			fullText = df.Text
		case *core.UsageMetricsFrame:
			usageFrames = append(usageFrames, df)
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

	// Verify UsageMetricsFrame emission and correlation
	if len(usageFrames) != 1 {
		t.Fatalf("expected 1 UsageMetricsFrame, got %d", len(usageFrames))
	}
	uf := usageFrames[0]
	if uf.Turn != "turn-1" || uf.Gen != 1 || uf.Provider != "xai" || uf.Service != "llm" {
		t.Fatalf("usage frame correlation mismatch: %+v", uf)
	}
	if uf.PromptTokens == nil || *uf.PromptTokens != 10 {
		t.Fatalf("expected prompt tokens 10, got %v", uf.PromptTokens)
	}
	if uf.CompletionTokens == nil || *uf.CompletionTokens != 2 {
		t.Fatalf("expected completion tokens 2, got %v", uf.CompletionTokens)
	}
	if uf.TotalTokens == nil || *uf.TotalTokens != 12 {
		t.Fatalf("expected total tokens 12, got %v", uf.TotalTokens)
	}
	if uf.Characters != nil {
		t.Fatalf("expected characters to remain absent, got %v", uf.Characters)
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
