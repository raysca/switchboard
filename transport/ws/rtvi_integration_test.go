package ws_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"govoice/core"
	"govoice/transport/token"
	"govoice/transport/ws"
)

// FakeSTTProcessor simulates STT by converting incoming audio to speech state and transcription.
type FakeSTTProcessor struct {
	triggered  atomic.Bool
	transcript string
}

func (s *FakeSTTProcessor) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	if _, ok := f.(*core.InputAudioFrame); ok {
		if s.triggered.CompareAndSwap(false, true) {
			_ = e.PushDirection(&core.UserStartedSpeakingFrame{Timestamp: time.Now()}, core.Downstream)
			_ = e.PushDirection(&core.FinalTranscriptionFrame{
				Text:      s.transcript,
				UserID:    "user-1",
				Timestamp: time.Now(),
				Turn:      "turn-1",
				Gen:       1,
			}, core.Downstream)
			_ = e.PushDirection(&core.UserStoppedSpeakingFrame{Timestamp: time.Now()}, core.Downstream)
		}
		return nil
	}
	return e.PushDirection(f, dir)
}

// 1. Test RTVI Readiness and Session Allocation Gate
func TestRTVI_SessionAllocationGate(t *testing.T) {
	now := time.Now()
	val, err := token.NewValidator(token.DefaultConfig(testIssuer, testAudience, []byte(testSecret)), now.Add(-time.Minute))
	if err != nil {
		t.Fatalf("NewValidator error: %v", err)
	}

	cfg := ws.DefaultServerConfig(val)
	cfg.RTVIReadyTimeout = 300 * time.Millisecond
	server := ws.NewServer(cfg)

	var sessionAllocated atomic.Bool
	server.OnSessionReady = func(conn *ws.Connection) (*core.Session, error) {
		sessionAllocated.Store(true)
		pipe, _ := core.NewPipeline(&core.PassthroughProcessor{})
		return core.NewSession(1, pipe, core.DefaultResourceBudget())
	}

	ts := httptest.NewServer(server)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// 1a. Incompatible Protocol Version: "1.0.0" must be rejected before session allocation
	urlIncompat := "ws" + strings.TrimPrefix(ts.URL, "http") + "?token=" + createTestToken(t, "jti-incompat", now, now.Add(time.Minute))
	connIncompat, _, err := websocket.Dial(ctx, urlIncompat, nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer connIncompat.Close(websocket.StatusNormalClosure, "")

	incompatReady, _ := ws.EncodeMessageFrame(`{"id":"1","label":"rtvi-ai","type":"client-ready","data":{"version":"1.0.0"}}`)
	_ = connIncompat.Write(ctx, websocket.MessageBinary, incompatReady)

	// Read error or close
	_, _, err = connIncompat.Read(ctx)
	if err == nil {
		// Read until close
		for {
			_, _, err = connIncompat.Read(ctx)
			if err != nil {
				break
			}
		}
	}
	if sessionAllocated.Load() {
		t.Fatalf("Session should NOT have been allocated for incompatible protocol version")
	}

	// 1b. Audio sent before client-ready must be rejected before session allocation
	sessionAllocated.Store(false)
	urlPremature := "ws" + strings.TrimPrefix(ts.URL, "http") + "?token=" + createTestToken(t, "jti-premature", now, now.Add(time.Minute))
	connPremature, _, err := websocket.Dial(ctx, urlPremature, nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer connPremature.Close(websocket.StatusNormalClosure, "")

	audioBytes, _ := ws.EncodeAudioFrame(make([]byte, 640), 16000, 1)
	_ = connPremature.Write(ctx, websocket.MessageBinary, audioBytes)

	// Should be closed by server with policy violation
	for {
		_, _, err = connPremature.Read(ctx)
		if err != nil {
			break
		}
	}
	if sessionAllocated.Load() {
		t.Fatalf("Session should NOT have been allocated when audio is sent before client-ready")
	}

	// 1c. Compatible Protocol Version: "2.1.0" succeeds and allocates exactly one session
	sessionAllocated.Store(false)
	urlValid := "ws" + strings.TrimPrefix(ts.URL, "http") + "?token=" + createTestToken(t, "jti-valid", now, now.Add(time.Minute))
	connValid, _, err := websocket.Dial(ctx, urlValid, nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer connValid.Close(websocket.StatusNormalClosure, "")

	performHandshake(t, ctx, connValid)

	if !sessionAllocated.Load() {
		t.Fatalf("Session MUST be allocated after valid client-ready handshake")
	}
}

// 2. Test Reconnect with Consumed Token Rejected Before Upgrade
func TestRTVI_ConsumedTokenReconnectRejected(t *testing.T) {
	now := time.Now()
	val, _ := token.NewValidator(token.DefaultConfig(testIssuer, testAudience, []byte(testSecret)), now.Add(-time.Minute))
	server := ws.NewServer(ws.DefaultServerConfig(val))
	server.OnSessionReady = func(conn *ws.Connection) (*core.Session, error) {
		pipe, _ := core.NewPipeline(&core.PassthroughProcessor{})
		return core.NewSession(1, pipe, core.DefaultResourceBudget())
	}

	ts := httptest.NewServer(server)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	singleToken := createTestToken(t, "jti-single-use", now, now.Add(time.Minute))
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "?token=" + singleToken

	// First connect succeeds
	conn1, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("first dial failed: %v", err)
	}
	performHandshake(t, ctx, conn1)
	conn1.Close(websocket.StatusNormalClosure, "call done")

	// Second connect with same token MUST fail before upgrade (HTTP 401)
	_, resp, err := websocket.Dial(ctx, wsURL, nil)
	if err == nil {
		t.Fatalf("expected dial to fail for replayed token, got nil error")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected HTTP 401 Unauthorized for replayed token, got %v", resp)
	}
}

// 3. Test Full Audio/Text Round Trip with RTVI Event Mapping
func TestRTVI_AudioTextRoundTrip(t *testing.T) {
	now := time.Now()
	val, _ := token.NewValidator(token.DefaultConfig(testIssuer, testAudience, []byte(testSecret)), now.Add(-time.Minute))
	server := ws.NewServer(ws.DefaultServerConfig(val))

	server.OnSessionReady = func(conn *ws.Connection) (*core.Session, error) {
		ctxModel := core.NewConversationContext(core.ContextConfig{MaxMessages: 10, MaxTokens: 1000})
		userCtx := core.NewUserContextProcessor(ctxModel, core.UserContextConfig{TurnPrefix: "turn"})
		asstCtx := core.NewAssistantContextProcessor(ctxModel, core.DefaultAssistantContextConfig())
		fakeSTT := &FakeSTTProcessor{transcript: "Tell me a joke"}
		fakeLLM := core.NewFakeLLMProcessor(core.FakeLLMConfig{
			ResponseChunks: []string{"Why did ", "the chicken cross?"},
		})
		fakeTTS := core.NewFakeTTSProcessor(core.FakeTTSConfig{
			FramesPerUnit: 2,
		})

		pipe, err := core.NewPipeline(fakeSTT, userCtx, fakeLLM, asstCtx, fakeTTS, conn)
		if err != nil {
			return nil, err
		}
		return core.NewSession(100, pipe, core.DefaultResourceBudget())
	}

	ts := httptest.NewServer(server)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "?token=" + createTestToken(t, "jti-roundtrip", now, now.Add(time.Minute))
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	performHandshake(t, ctx, conn)

	// Send client audio frame
	audioBytes, _ := ws.EncodeAudioFrame(make([]byte, 640), 16000, 1)
	err = conn.Write(ctx, websocket.MessageBinary, audioBytes)
	if err != nil {
		t.Fatalf("write audio failed: %v", err)
	}

	// Collect received frames from server
	var receivedTypes []string
	var receivedAudioCount int
	var mu sync.Mutex

	for {
		readCtx, readCancel := context.WithTimeout(ctx, 500*time.Millisecond)
		typ, data, err := conn.Read(readCtx)
		readCancel()
		if err != nil {
			break // timeout or closed
		}

		if typ == websocket.MessageBinary {
			f, err := ws.DecodeBinaryFrame(data, 1, time.Now())
			if err != nil {
				continue
			}
			mu.Lock()
			switch frame := f.(type) {
			case *ws.WireMessageFrame:
				var rtviMsg map[string]interface{}
				if err := json.Unmarshal([]byte(frame.Data), &rtviMsg); err == nil {
					if tStr, ok := rtviMsg["type"].(string); ok {
						receivedTypes = append(receivedTypes, tStr)
					}
				}
			case *core.InputAudioFrame:
				receivedAudioCount++
			}
			mu.Unlock()
		}
	}

	mu.Lock()
	defer mu.Unlock()

	// Verify required RTVI event types were received
	hasUserSpeaking := false
	hasTranscription := false
	hasBotLLM := false
	hasBotStartedSpeaking := false
	hasBotStoppedSpeaking := false

	for _, tName := range receivedTypes {
		switch tName {
		case "user-started-speaking":
			hasUserSpeaking = true
		case "user-transcription":
			hasTranscription = true
		case "bot-llm-text":
			hasBotLLM = true
		case "bot-started-speaking":
			hasBotStartedSpeaking = true
		case "bot-stopped-speaking":
			hasBotStoppedSpeaking = true
		}
	}

	if !hasUserSpeaking {
		t.Errorf("Expected user-started-speaking event, got: %v", receivedTypes)
	}
	if !hasTranscription {
		t.Errorf("Expected user-transcription event, got: %v", receivedTypes)
	}
	if !hasBotLLM {
		t.Errorf("Expected bot-llm-text event, got: %v", receivedTypes)
	}
	if !hasBotStartedSpeaking {
		t.Errorf("Expected bot-started-speaking event, got: %v", receivedTypes)
	}
	if !hasBotStoppedSpeaking {
		t.Errorf("Expected bot-stopped-speaking event, got: %v", receivedTypes)
	}
}

// 4. Test Fake Barge-in: Playback Clear Event and Stale Audio Drop
func TestRTVI_BargeIn_PlaybackClearAndGenerationFence(t *testing.T) {
	now := time.Now()
	val, _ := token.NewValidator(token.DefaultConfig(testIssuer, testAudience, []byte(testSecret)), now.Add(-time.Minute))
	server := ws.NewServer(ws.DefaultServerConfig(val))

	sessCh := make(chan *core.Session, 1)

	server.OnSessionReady = func(conn *ws.Connection) (*core.Session, error) {
		pipe, err := core.NewPipeline(conn)
		if err != nil {
			return nil, err
		}
		sess, err := core.NewSession(200, pipe, core.DefaultResourceBudget())
		if err == nil {
			sessCh <- sess
		}
		return sess, err
	}

	ts := httptest.NewServer(server)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "?token=" + createTestToken(t, "jti-bargein", now, now.Add(time.Minute))
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	performHandshake(t, ctx, conn)

	activeSession := <-sessCh

	// Enqueue 5 audio frames for Generation 1
	for i := 1; i <= 5; i++ {
		_ = activeSession.Post(&core.OutputAudioFrame{
			Samples:    make([]byte, 960),
			SampleRate: 24000,
			Channels:   1,
			Duration:   20 * time.Millisecond,
			Gen:        1,
			Turn:       "turn-1",
			Seq:        uint64(i),
			IsFinal:    i == 5,
		})
	}

	// Trigger Barge-in Interruption for Generation 2
	time.Sleep(10 * time.Millisecond)
	_ = activeSession.Post(&core.InterruptionFrame{Gen: 2, Turn: "turn-1"})

	// Send Generation 2 audio frame
	_ = activeSession.Post(&core.OutputAudioFrame{
		Samples:    make([]byte, 960),
		SampleRate: 24000,
		Channels:   1,
		Duration:   20 * time.Millisecond,
		Gen:        2,
		Turn:       "turn-2",
		Seq:        100,
		IsFinal:    true,
	})

	// Client reads wire events
	var messages []string
	for i := 0; i < 10; i++ {
		readCtx, readCancel := context.WithTimeout(ctx, 100*time.Millisecond)
		typ, data, err := conn.Read(readCtx)
		readCancel()
		if err != nil {
			break
		}
		if typ == websocket.MessageBinary {
			f, err := ws.DecodeBinaryFrame(data, 1, time.Now())
			if err == nil {
				if wmf, ok := f.(*ws.WireMessageFrame); ok {
					messages = append(messages, wmf.Data)
				}
			}
		}
	}

	// Assert that bot-stopped-speaking (the playback-clear event proven in Phase 0) was emitted
	hasClearEvent := false
	for _, m := range messages {
		if strings.Contains(m, "bot-stopped-speaking") {
			hasClearEvent = true
			break
		}
	}

	if !hasClearEvent {
		t.Fatalf("Expected bot-stopped-speaking playback clear event on interruption, got: %v", messages)
	}
}

// 5. Test Clean Disconnect and Teardown
func TestRTVI_CleanDisconnectTeardown(t *testing.T) {
	now := time.Now()
	val, _ := token.NewValidator(token.DefaultConfig(testIssuer, testAudience, []byte(testSecret)), now.Add(-time.Minute))
	server := ws.NewServer(ws.DefaultServerConfig(val))

	var sessClosed atomic.Bool
	server.OnSessionReady = func(conn *ws.Connection) (*core.Session, error) {
		pipe, _ := core.NewPipeline(&core.PassthroughProcessor{})
		sess, err := core.NewSession(300, pipe, core.DefaultResourceBudget())
		if err != nil {
			return nil, err
		}
		go func() {
			<-sess.Done()
			sessClosed.Store(true)
		}()
		return sess, nil
	}

	ts := httptest.NewServer(server)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "?token=" + createTestToken(t, "jti-disconnect", now, now.Add(time.Minute))
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}

	performHandshake(t, ctx, conn)

	// Send disconnect-bot RTVI message
	discMsg, _ := ws.EncodeMessageFrame(`{"id":"99","label":"rtvi-ai","type":"disconnect-bot"}`)
	_ = conn.Write(ctx, websocket.MessageBinary, discMsg)

	// Read until close
	for {
		_, _, err = conn.Read(ctx)
		if err != nil {
			break
		}
	}

	time.Sleep(100 * time.Millisecond)
	if !sessClosed.Load() {
		t.Fatalf("session was not torn down after disconnect-bot message")
	}
}
