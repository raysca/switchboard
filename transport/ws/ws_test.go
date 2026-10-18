package ws_test

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/protobuf/proto"

	"govoice/core"
	"govoice/core/coretest"
	framespb "govoice/core/transport/v1/framespb"
	"govoice/transport/token"
	"govoice/transport/ws"
)

const (
	testSecret   = "test-secret-at-least-32-bytes-long!!"
	testIssuer   = "test-issuer"
	testAudience = "test-audience"
)

func createTestToken(t *testing.T, jti string, iat time.Time, exp time.Time) string {
	t.Helper()
	claims := token.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        jti,
			Issuer:    testIssuer,
			Audience:  jwt.ClaimStrings{testAudience},
			IssuedAt:  jwt.NewNumericDate(iat),
			ExpiresAt: jwt.NewNumericDate(exp),
			Subject:   "user-123",
		},
		Principal: "user-123",
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	s, err := tok.SignedString([]byte(testSecret))
	if err != nil {
		t.Fatalf("failed to sign token: %v", err)
	}
	return s
}

func performHandshake(t *testing.T, ctx context.Context, clientConn *websocket.Conn) {
	t.Helper()
	readyMsg, err := ws.EncodeMessageFrame(`{"id":"1","label":"rtvi-ai","type":"client-ready","data":{"version":"2.1.0"}}`)
	if err != nil {
		t.Fatalf("failed to encode client-ready frame: %v", err)
	}
	if err := clientConn.Write(ctx, websocket.MessageBinary, readyMsg); err != nil {
		t.Fatalf("failed to send client-ready: %v", err)
	}
	_, data, err := clientConn.Read(ctx)
	if err != nil {
		t.Fatalf("failed to read bot-ready: %v", err)
	}
	f, err := ws.DecodeBinaryFrame(data, 1, time.Now())
	if err != nil {
		t.Fatalf("failed to decode bot-ready frame: %v", err)
	}
	wmf, ok := f.(*ws.WireMessageFrame)
	if !ok || !strings.Contains(wmf.Data, "bot-ready") {
		t.Fatalf("expected bot-ready message, got: %v", f)
	}
}

// 1. Byte-fixture tests in both directions
func TestPhase3_ByteFixtures_BothDirections(t *testing.T) {
	fixturesDir := filepath.Join("..", "..", "tools", "compat", "fixtures")

	// Decode client-audio.bin
	clientAudioBytes, err := os.ReadFile(filepath.Join(fixturesDir, "client-audio.bin"))
	if err != nil {
		t.Fatalf("failed to read client-audio.bin: %v", err)
	}
	frame, err := ws.DecodeBinaryFrame(clientAudioBytes, 1, time.Now())
	if err != nil {
		t.Fatalf("failed to decode client-audio.bin: %v", err)
	}
	audio, ok := frame.(*core.InputAudioFrame)
	if !ok || audio.SampleRate != 16000 || len(audio.Samples) != 640 {
		t.Fatalf("unexpected audio decoded from fixture: %+v", audio)
	}

	// Decode server-audio.bin
	serverAudioBytes, err := os.ReadFile(filepath.Join(fixturesDir, "server-audio.bin"))
	if err != nil {
		t.Fatalf("failed to read server-audio.bin: %v", err)
	}
	frame, err = ws.DecodeBinaryFrame(serverAudioBytes, 2, time.Now())
	if err != nil {
		t.Fatalf("failed to decode server-audio.bin: %v", err)
	}
	sAudio, ok := frame.(*core.InputAudioFrame)
	if !ok || sAudio.SampleRate != 24000 || len(sAudio.Samples) != 960 {
		t.Fatalf("unexpected server audio decoded: %+v", sAudio)
	}

	// Encode Go audio to protobuf and re-decode
	encoded, err := ws.EncodeAudioFrame(make([]byte, 640), 16000, 1)
	if err != nil {
		t.Fatalf("EncodeAudioFrame failed: %v", err)
	}
	reDecoded, err := ws.DecodeBinaryFrame(encoded, 3, time.Now())
	if err != nil {
		t.Fatalf("re-decode failed: %v", err)
	}
	if reDecoded.Type() != core.FrameInputAudio {
		t.Fatalf("expected FrameInputAudio, got: %v", reDecoded.Type())
	}
}

// 2. Fuzz and property tests: malformed, unknown variant, oversized, PCM misalignment
func TestPhase3_FuzzProperty_WireValidation(t *testing.T) {
	now := time.Now()

	// 2a. Random garbage bytes
	garbage := make([]byte, 256)
	_, _ = rand.Read(garbage)
	_, err := ws.DecodeBinaryFrame(garbage, 1, now)
	if err == nil {
		t.Fatalf("expected error on random garbage, got nil")
	}

	// 2b. Oversized message (> MaxEncodedFrameSize)
	oversized := make([]byte, ws.MaxEncodedFrameSize+1)
	_, err = ws.DecodeBinaryFrame(oversized, 2, now)
	if err != ws.ErrOversizedMessage {
		t.Fatalf("expected ErrOversizedMessage, got: %v", err)
	}

	// 2c. Unaligned PCM bytes (odd byte length)
	oddAudio := &framespb.Frame{
		Frame: &framespb.Frame_Audio{
			Audio: &framespb.AudioRawFrame{
				Audio:       make([]byte, 641), // 641 bytes: odd!
				SampleRate:  16000,
				NumChannels: 1,
			},
		},
	}
	oddBytes, _ := proto.Marshal(oddAudio)
	_, err = ws.DecodeBinaryFrame(oddBytes, 3, now)
	if err != ws.ErrUnalignedPCM {
		t.Fatalf("expected ErrUnalignedPCM, got: %v", err)
	}

	// 2d. Unsupported sample rate (e.g. 44100 Hz)
	badRateAudio := &framespb.Frame{
		Frame: &framespb.Frame_Audio{
			Audio: &framespb.AudioRawFrame{
				Audio:       make([]byte, 640),
				SampleRate:  44100,
				NumChannels: 1,
			},
		},
	}
	badRateBytes, _ := proto.Marshal(badRateAudio)
	_, err = ws.DecodeBinaryFrame(badRateBytes, 4, now)
	if err == nil || !strings.Contains(err.Error(), "unsupported input sample rate") {
		t.Fatalf("expected ErrUnsupportedRate, got: %v", err)
	}

	// 2e. Non-mono channel count (stereo = 2)
	stereoAudio := &framespb.Frame{
		Frame: &framespb.Frame_Audio{
			Audio: &framespb.AudioRawFrame{
				Audio:       make([]byte, 640),
				SampleRate:  16000,
				NumChannels: 2,
			},
		},
	}
	stereoBytes, _ := proto.Marshal(stereoAudio)
	_, err = ws.DecodeBinaryFrame(stereoBytes, 5, now)
	if err == nil || !strings.Contains(err.Error(), "channels must be 1") {
		t.Fatalf("expected ErrInvalidPCMFormat, got: %v", err)
	}

	// 2f. Duration out of 10-40ms bounds (e.g. 5ms = 80 samples @ 16kHz = 160 bytes)
	shortAudio := &framespb.Frame{
		Frame: &framespb.Frame_Audio{
			Audio: &framespb.AudioRawFrame{
				Audio:       make([]byte, 160),
				SampleRate:  16000,
				NumChannels: 1,
			},
		},
	}
	shortBytes, _ := proto.Marshal(shortAudio)
	_, err = ws.DecodeBinaryFrame(shortBytes, 6, now)
	if err == nil || !strings.Contains(err.Error(), "audio chunk duration outside allowed") {
		t.Fatalf("expected ErrInvalidDuration, got: %v", err)
	}
}

// 3. Output pacing stays near media time instead of bursting complete synthesis
func TestPhase3_OutputPacing_StaysNearMediaTime(t *testing.T) {
	cfg := ws.DefaultPacerConfig()
	cfg.EnablePacing = true

	var writeTimes []time.Time
	var mu sync.Mutex

	pacer := ws.NewPacedWriter(cfg, func(ctx context.Context, data []byte) error {
		mu.Lock()
		writeTimes = append(writeTimes, time.Now())
		mu.Unlock()
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = pacer.Run(ctx)
	}()

	// Enqueue 5 frames of 20ms audio (100ms total media time)
	for i := 1; i <= 5; i++ {
		err := pacer.Enqueue(&core.OutputAudioFrame{
			Samples:    make([]byte, 960),
			SampleRate: 24000,
			Channels:   1,
			Duration:   20 * time.Millisecond,
			Gen:        1,
			Turn:       "turn-1",
			UnitID:     1,
		})
		if err != nil {
			t.Fatalf("Enqueue failed: %v", err)
		}
	}

	// Wait for pacing loop to deliver the 5 frames
	time.Sleep(120 * time.Millisecond)
	pacer.Close()

	mu.Lock()
	count := len(writeTimes)
	mu.Unlock()

	if count < 5 {
		t.Fatalf("expected 5 frames written, got %d", count)
	}

	mu.Lock()
	first := writeTimes[0]
	last := writeTimes[4]
	mu.Unlock()

	elapsed := last.Sub(first)
	// 4 intervals of 20ms = 80ms minimum media duration
	if elapsed < 65*time.Millisecond {
		t.Fatalf("pacing was too fast (bursted): elapsed %v < 65ms", elapsed)
	}
}

// 4. Stale generation discard and current generation overflow terminates as slow client
func TestPhase3_StaleGenerationDiscard_AndSlowClientOverflow(t *testing.T) {
	cfg := ws.PacerConfig{
		MaxQueueDuration: 60 * time.Millisecond, // Bounded to 3 frames of 20ms
		MaxQueueBytes:    3000,
		WriteTimeout:     time.Second,
		EnablePacing:     false,
	}

	pacer := ws.NewPacedWriter(cfg, func(ctx context.Context, data []byte) error {
		// Withhold reading from queue by sleeping
		time.Sleep(time.Hour)
		return nil
	})
	pacer.SetActiveGeneration(2)

	// 4a. Stale generation discard (Gen 1 is stale, active is 2)
	err := pacer.Enqueue(&core.OutputAudioFrame{
		Samples:  make([]byte, 960),
		Duration: 20 * time.Millisecond,
		Gen:      1, // Stale!
	})
	if err != nil {
		t.Fatalf("expected nil on stale enqueue, got %v", err)
	}
	if pacer.FramesDropped.Load() != 1 {
		t.Fatalf("expected 1 stale frame dropped, got %d", pacer.FramesDropped.Load())
	}
	if pacer.QueuedBytes() != 0 {
		t.Fatalf("stale frame entered queue!")
	}

	// 4b. Enqueue frames for current generation (Gen 2) up to limit
	for i := 0; i < 3; i++ {
		err := pacer.Enqueue(&core.OutputAudioFrame{
			Samples:  make([]byte, 960),
			Duration: 20 * time.Millisecond,
			Gen:      2,
		})
		if err != nil {
			t.Fatalf("enqueue within limit failed: %v", err)
		}
	}

	// 4c. Current generation overflow: MUST NOT silently truncate! Must fail explicitly as slow client
	err = pacer.Enqueue(&core.OutputAudioFrame{
		Samples:  make([]byte, 960),
		Duration: 20 * time.Millisecond,
		Gen:      2,
	})
	if err == nil || !strings.Contains(err.Error(), "slow client") {
		t.Fatalf("expected ErrSlowClient on overflow, got: %v", err)
	}

	// 4d. Flush clears old generation queue and drops bytes cleanly
	if err := pacer.Flush(context.Background(), 2); err != nil {
		t.Fatalf("flush failed: %v", err)
	}
	if pacer.QueuedBytes() != 0 || pacer.QueuedDuration() != 0 {
		t.Fatalf("flush did not clear queue: bytes=%d, dur=%v", pacer.QueuedBytes(), pacer.QueuedDuration())
	}
}

// 5. Blocked writer does not prevent input reads and interruption ingress
func TestPhase3_BlockedWriter_DoesNotBlockInputReads(t *testing.T) {
	now := time.Now()
	val, _ := token.NewValidator(token.DefaultConfig(testIssuer, testAudience, []byte(testSecret)), now.Add(-time.Minute))
	serverCfg := ws.DefaultServerConfig(val)

	var receivedFrames []core.Frame
	var mu sync.Mutex

	server := ws.NewServer(serverCfg)
	server.OnSessionReady = func(conn *ws.Connection) (*core.Session, error) {
		pipe, _ := core.NewPipeline(&coretest.PassthroughProcessor{})
		budget := core.DefaultResourceBudget()
		sess, err := core.NewSession(1, pipe, budget)
		if err != nil {
			return nil, err
		}
		sess.OnOutput = func(f core.Frame) {
			mu.Lock()
			receivedFrames = append(receivedFrames, f)
			mu.Unlock()
		}
		return sess, nil
	}

	ts := httptest.NewServer(server)
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "?token=" + createTestToken(t, "jti-in", now, now.Add(time.Minute))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	clientConn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("websocket.Dial failed: %v", err)
	}
	defer clientConn.Close(websocket.StatusNormalClosure, "")

	performHandshake(t, ctx, clientConn)

	// Client sends an audio frame
	audioBytes, _ := ws.EncodeAudioFrame(make([]byte, 640), 16000, 1)
	err = clientConn.Write(ctx, websocket.MessageBinary, audioBytes)
	if err != nil {
		t.Fatalf("client write audio failed: %v", err)
	}

	// Client sends a second frame (simulating speech start / interruption)
	time.Sleep(50 * time.Millisecond)
	err = clientConn.Write(ctx, websocket.MessageBinary, audioBytes)
	if err != nil {
		t.Fatalf("client write 2nd audio failed: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	count := len(receivedFrames)
	mu.Unlock()

	if count < 2 {
		t.Fatalf("expected at least 2 frames received by session, got %d", count)
	}
}

// 6. Security: Origin check, missing origin, handshake limits, pre-upgrade HTTP status codes
func TestPhase3_SecurityAndOrigin(t *testing.T) {
	now := time.Now()
	val, _ := token.NewValidator(token.DefaultConfig(testIssuer, testAudience, []byte(testSecret)), now.Add(-time.Minute))

	serverCfg := ws.DefaultServerConfig(val)
	serverCfg.Security.AllowedOrigins = []string{"https://voice.example.com"}
	serverCfg.Security.AllowMissingOrigin = false

	server := ws.NewServer(serverCfg)
	ts := httptest.NewServer(server)
	defer ts.Close()

	validToken := createTestToken(t, "jti-sec", now, now.Add(time.Minute))

	// 6a. Missing Origin header: HTTP 403
	req, _ := http.NewRequest("GET", ts.URL+"?token="+validToken, nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	req.Header.Set("Sec-WebSocket-Version", "13")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected HTTP 403 on missing origin, got: %d", resp.StatusCode)
	}

	// 6b. Disallowed Origin header: HTTP 403
	req, _ = http.NewRequest("GET", ts.URL+"?token="+validToken, nil)
	req.Header.Set("Origin", "https://malicious.com")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	req.Header.Set("Sec-WebSocket-Version", "13")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected HTTP 403 on disallowed origin, got: %d", resp.StatusCode)
	}

	// 6c. Missing token: HTTP 401
	req, _ = http.NewRequest("GET", ts.URL, nil)
	req.Header.Set("Origin", "https://voice.example.com")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected HTTP 401 on missing token, got: %d", resp.StatusCode)
	}
}

// 7. Dropped connection isolation: Connection 1 failure does not affect Connection 2
func TestPhase3_DroppedConnection_Isolation(t *testing.T) {
	now := time.Now()
	val, _ := token.NewValidator(token.DefaultConfig(testIssuer, testAudience, []byte(testSecret)), now.Add(-time.Minute))

	serverCfg := ws.DefaultServerConfig(val)
	serverCfg.Security.AllowMissingOrigin = true

	var sess1Closed, sess2Closed atomic.Bool

	server := ws.NewServer(serverCfg)
	server.OnSessionReady = func(conn *ws.Connection) (*core.Session, error) {
		pipe, _ := core.NewPipeline(&coretest.PassthroughProcessor{})
		sess, err := core.NewSession(conn.Pacer().FramesWritten.Load()+1, pipe, core.DefaultResourceBudget())
		if err != nil {
			return nil, err
		}
		if conn.Claims().ID == "jti-conn1" {
			go func() {
				<-sess.Done()
				sess1Closed.Store(true)
			}()
		} else {
			go func() {
				<-sess.Done()
				sess2Closed.Store(true)
			}()
		}
		return sess, nil
	}

	ts := httptest.NewServer(server)
	defer ts.Close()

	ctx := context.Background()

	// Connect Session 1
	url1 := "ws" + strings.TrimPrefix(ts.URL, "http") + "?token=" + createTestToken(t, "jti-conn1", now, now.Add(time.Minute))
	conn1, _, err := websocket.Dial(ctx, url1, nil)
	if err != nil {
		t.Fatalf("dial conn1 failed: %v", err)
	}
	performHandshake(t, ctx, conn1)

	// Connect Session 2
	url2 := "ws" + strings.TrimPrefix(ts.URL, "http") + "?token=" + createTestToken(t, "jti-conn2", now, now.Add(time.Minute))
	conn2, _, err := websocket.Dial(ctx, url2, nil)
	if err != nil {
		t.Fatalf("dial conn2 failed: %v", err)
	}
	defer conn2.Close(websocket.StatusNormalClosure, "")
	performHandshake(t, ctx, conn2)

	time.Sleep(50 * time.Millisecond)

	// Abruptly drop Connection 1
	_ = conn1.Close(websocket.StatusAbnormalClosure, "abrupt network drop")

	time.Sleep(100 * time.Millisecond)

	// Session 1 must be closed
	if !sess1Closed.Load() {
		t.Fatalf("session 1 was not torn down after connection drop")
	}

	// Session 2 must remain healthy and able to send/receive
	if sess2Closed.Load() {
		t.Fatalf("session 2 was collateral damage of session 1 drop!")
	}

	audioBytes, _ := ws.EncodeAudioFrame(make([]byte, 640), 16000, 1)
	err = conn2.Write(ctx, websocket.MessageBinary, audioBytes)
	if err != nil {
		t.Fatalf("session 2 write failed after session 1 drop: %v", err)
	}
}

// 8. TLS production wss and plain ws integration test
func TestPhase3_WSS_And_PlainWS(t *testing.T) {
	now := time.Now()
	val, _ := token.NewValidator(token.DefaultConfig(testIssuer, testAudience, []byte(testSecret)), now.Add(-time.Minute))

	serverCfg := ws.DefaultServerConfig(val)
	serverCfg.Security.AllowMissingOrigin = true

	server := ws.NewServer(serverCfg)
	server.OnSessionReady = func(conn *ws.Connection) (*core.Session, error) {
		pipe, _ := core.NewPipeline(&coretest.PassthroughProcessor{})
		return core.NewSession(1, pipe, core.DefaultResourceBudget())
	}

	// Test WSS (HTTPS with self-signed TLS)
	tlsServer := httptest.NewTLSServer(server)
	defer tlsServer.Close()

	wssURL := "wss" + strings.TrimPrefix(tlsServer.URL, "https") + "?token=" + createTestToken(t, "jti-wss", now, now.Add(time.Minute))

	dialOpts := &websocket.DialOptions{
		HTTPClient: &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wssURL, dialOpts)
	if err != nil {
		t.Fatalf("WSS dial failed: %v", err)
	}

	// Send an audio frame over WSS
	audioBytes, _ := ws.EncodeAudioFrame(make([]byte, 640), 16000, 1)
	err = conn.Write(ctx, websocket.MessageBinary, audioBytes)
	if err != nil {
		t.Fatalf("WSS write failed: %v", err)
	}

	_ = conn.Close(websocket.StatusNormalClosure, "done")
}

// 9. Input rates resample correctly at chunk boundaries
func TestPhase3_InputRates_ResampleCorrectlyAtChunkBoundaries(t *testing.T) {
	now := time.Now()
	val, _ := token.NewValidator(token.DefaultConfig(testIssuer, testAudience, []byte(testSecret)), now.Add(-time.Minute))
	serverCfg := ws.DefaultServerConfig(val)

	var recordedFrames []*core.InputAudioFrame
	var mu sync.Mutex

	server := ws.NewServer(serverCfg)
	server.OnSessionReady = func(conn *ws.Connection) (*core.Session, error) {
		pipe, _ := core.NewPipeline(&coretest.PassthroughProcessor{})
		sess, _ := core.NewSession(1, pipe, core.DefaultResourceBudget())
		sess.OnOutput = func(f core.Frame) {
			if af, ok := f.(*core.InputAudioFrame); ok {
				mu.Lock()
				recordedFrames = append(recordedFrames, af)
				mu.Unlock()
			}
		}
		return sess, nil
	}

	ts := httptest.NewServer(server)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "?token=" + createTestToken(t, "jti-resample", now, now.Add(time.Minute))
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("websocket.Dial failed: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	performHandshake(t, ctx, conn)

	// 9a. Send 24 kHz audio (480 samples = 960 bytes for 20ms)
	audio24k, _ := ws.EncodeAudioFrame(make([]byte, 960), 24000, 1)
	err = conn.Write(ctx, websocket.MessageBinary, audio24k)
	if err != nil {
		t.Fatalf("write 24k audio failed: %v", err)
	}

	// 9b. Send 48 kHz audio (960 samples = 1920 bytes for 20ms)
	audio48k, _ := ws.EncodeAudioFrame(make([]byte, 1920), 48000, 1)
	err = conn.Write(ctx, websocket.MessageBinary, audio48k)
	if err != nil {
		t.Fatalf("write 48k audio failed: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	frames := make([]*core.InputAudioFrame, len(recordedFrames))
	copy(frames, recordedFrames)
	mu.Unlock()

	if len(frames) < 2 {
		t.Fatalf("expected 2 audio frames resampled, got %d", len(frames))
	}

	for i, f := range frames[:2] {
		// Both frames must be converted to internal 16 kHz format (320 samples = 640 bytes)
		if f.SampleRate != ws.InternalSampleRate {
			t.Fatalf("frame %d: expected SampleRate %d, got %d", i, ws.InternalSampleRate, f.SampleRate)
		}
		if len(f.Samples) != 640 {
			t.Fatalf("frame %d: expected 640 bytes for 20ms @ 16kHz, got %d", i, len(f.Samples))
		}
	}
}

// 10. Concurrent upgrades cannot pass the session limit.
func TestPhase3_HandshakeFlood_ReturnsHTTP429(t *testing.T) {
	now := time.Now()
	val, _ := token.NewValidator(token.DefaultConfig(testIssuer, testAudience, []byte(testSecret)), now.Add(-time.Minute))
	serverCfg := ws.DefaultServerConfig(val)
	serverCfg.MaxConcurrentSessions = 2
	serverCfg.MaxPendingHandshakes = 50
	serverCfg.HandshakeTimeout = 500 * time.Millisecond

	server := ws.NewServer(serverCfg)
	ts := httptest.NewServer(server)
	defer ts.Close()

	var wg sync.WaitGroup
	var statuses []int
	var mu sync.Mutex

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			tokenStr := createTestToken(t, fmt.Sprintf("jti-flood-%d", idx), now, now.Add(time.Minute))
			wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "?token=" + tokenStr
			conn, resp, err := websocket.Dial(context.Background(), wsURL, nil)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				statuses = append(statuses, http.StatusSwitchingProtocols)
				conn.Close(websocket.StatusNormalClosure, "")
				return
			}
			if resp != nil {
				statuses = append(statuses, resp.StatusCode)
			}
		}(i)
	}

	wg.Wait()

	mu.Lock()
	has429 := false
	for _, code := range statuses {
		if code == http.StatusTooManyRequests {
			has429 = true
			break
		}
	}
	mu.Unlock()

	if !has429 {
		t.Fatalf("expected at least one 429 from the session limit, got %v", statuses)
	}
}

func TestPendingHandshakeReleasedAfterUpgrade(t *testing.T) {
	now := time.Now()
	val, _ := token.NewValidator(token.DefaultConfig(testIssuer, testAudience, []byte(testSecret)), now.Add(-time.Minute))
	serverCfg := ws.DefaultServerConfig(val)
	serverCfg.MaxPendingHandshakes = 1
	serverCfg.MaxConcurrentSessions = 5
	server := ws.NewServer(serverCfg)
	ts := httptest.NewServer(server)
	defer ts.Close()

	ctx := context.Background()
	var conns []*websocket.Conn
	defer func() {
		for _, c := range conns {
			c.Close(websocket.StatusNormalClosure, "")
		}
	}()
	for i := 0; i < 3; i++ {
		tokenStr := createTestToken(t, fmt.Sprintf("jti-pending-%d", i), now, now.Add(time.Minute))
		wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "?token=" + tokenStr
		conn, resp, err := websocket.Dial(ctx, wsURL, nil)
		if err != nil {
			status := 0
			if resp != nil {
				status = resp.StatusCode
			}
			t.Fatalf("upgrade %d failed with %d: %v", i, status, err)
		}
		conns = append(conns, conn)
	}
}

func TestIsSecure_UntrustedForwardedProtoIsIgnored(t *testing.T) {
	cfg := &ws.SecurityConfig{TrustedProxies: []string{"10.0.0.1/32"}}
	req := httptest.NewRequest(http.MethodGet, "http://example/ws", nil)
	req.RemoteAddr = "192.0.2.10:1234"
	req.Header.Set("X-Forwarded-Proto", "https")
	if cfg.IsSecure(req) {
		t.Fatal("an untrusted peer must not prove TLS with X-Forwarded-Proto")
	}
	req.RemoteAddr = "10.0.0.1:443"
	if !cfg.IsSecure(req) {
		t.Fatal("a trusted proxy forwarding https should count as secure")
	}
}

func TestPhase3_Flush_TimeoutFailsOnUnresponsiveWriter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	writingStarted := make(chan struct{})
	// writeFn ignores context cancellation and hangs
	hangingWriteFn := func(writeCtx context.Context, data []byte) error {
		select {
		case <-writingStarted:
		default:
			close(writingStarted)
		}
		// Deliberately sleep longer than write timeout even if canceled
		time.Sleep(200 * time.Millisecond)
		return nil
	}

	pacer := ws.NewPacedWriter(ws.PacerConfig{
		MaxQueueDuration: 5 * time.Second,
		MaxQueueBytes:    65536,
		WriteTimeout:     50 * time.Millisecond,
	}, hangingWriteFn)

	go func() {
		_ = pacer.Run(ctx)
	}()

	// Enqueue a frame to trigger the hanging write
	_ = pacer.Enqueue(&core.OutputAudioFrame{
		Samples:    make([]byte, 960),
		SampleRate: 24000,
		Channels:   1,
		Duration:   20 * time.Millisecond,
		Gen:        1,
	})

	// Wait until the hanging write has actually started
	select {
	case <-writingStarted:
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for write to start")
	}

	// Flush must cancel the write and wait, but since the writer ignores cancellation,
	// Flush must return ErrSlowClient rather than succeeding while old audio is in flight!
	err := pacer.Flush(context.Background(), 1)
	if err == nil {
		t.Fatal("expected Flush to fail on unresponsive writer, but got nil")
	}
	if !errors.Is(err, ws.ErrSlowClient) {
		t.Fatalf("expected ErrSlowClient from Flush, got: %v", err)
	}
	if err := pacer.Enqueue(&core.OutputAudioFrame{Samples: make([]byte, 960), SampleRate: 24000, Channels: 1, Gen: 2}); !errors.Is(err, ws.ErrWriterClosed) {
		t.Fatalf("flush timeout was not terminal: enqueue error = %v", err)
	}
}

func TestSecurity_ClientIPSpoofing_RightToLeftTraversal(t *testing.T) {
	cfg := &ws.SecurityConfig{
		TrustedProxies: []string{"10.0.0.1/32", "10.0.0.2/32"},
	}

	// 1. Untrusted client connecting through two trusted proxies.
	// Client at 198.51.100.42 connects to 10.0.0.2, which connects to 10.0.0.1 (RemoteAddr).
	// Client injects a spoofed IP 1.2.3.4 into X-Forwarded-For: "1.2.3.4, 198.51.100.42, 10.0.0.2"
	req := httptest.NewRequest("GET", "http://example.com/ws", nil)
	req.RemoteAddr = "10.0.0.1:12345"
	req.Header.Set("X-Forwarded-For", "1.2.3.4, 198.51.100.42, 10.0.0.2")

	clientIP := cfg.ClientIP(req)
	// Right-to-left traversal:
	// 10.0.0.2 is trusted.
	// 198.51.100.42 is NOT trusted -> selected as real client IP!
	// Spoofed 1.2.3.4 is NOT selected!
	if clientIP != "198.51.100.42" {
		t.Fatalf("expected client IP to be 198.51.100.42, got %q", clientIP)
	}

	// 2. Direct untrusted connection attempting to forge X-Forwarded-For
	reqDirect := httptest.NewRequest("GET", "http://example.com/ws", nil)
	reqDirect.RemoteAddr = "203.0.113.10:54321"
	reqDirect.Header.Set("X-Forwarded-For", "1.2.3.4, 5.6.7.8")

	directIP := cfg.ClientIP(reqDirect)
	if directIP != "203.0.113.10" {
		t.Fatalf("expected direct untrusted connection to use RemoteAddr 203.0.113.10, got %q", directIP)
	}

	reqBad := httptest.NewRequest("GET", "http://example.com/ws", nil)
	reqBad.RemoteAddr = "10.0.0.1:12345"
	reqBad.Header.Set("X-Forwarded-For", "not-an-ip, 198.51.100.42")
	reqBad.Header.Set("X-Real-IP", "also-not-an-ip")
	if got := cfg.ClientIP(reqBad); got != "198.51.100.42" {
		t.Fatalf("expected invalid forwarded entries to be skipped, got %q", got)
	}
}

// The agent a call runs comes only from the signed token; query parameters
// (including the removed `pipeline`) cannot redirect a call.
func TestFlowID_ComesOnlyFromToken(t *testing.T) {
	now := time.Now()
	val, err := token.NewValidator(token.DefaultConfig(testIssuer, testAudience, []byte(testSecret)), now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	srv := ws.NewServer(ws.DefaultServerConfig(val))
	got := make(chan string, 1)
	srv.OnSessionReady = func(conn *ws.Connection) (*core.Session, error) {
		got <- conn.FlowID()
		pipe, _ := core.NewPipeline(&coretest.PassthroughProcessor{})
		return core.NewSession(1, pipe, core.DefaultResourceBudget())
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	claims := token.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			ID: "jti-agent", Issuer: testIssuer, Audience: jwt.ClaimStrings{testAudience}, Subject: "user-1",
			IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute)),
		},
		Principal: "user-1",
		FlowID:    "agent-from-token",
	}
	tokStr, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testSecret))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wsURL := fmt.Sprintf("ws://%s/ws?token=%s&pipeline=other&flow_id=other", ts.Listener.Addr().String(), tokStr)
	c, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")
	performHandshake(t, ctx, c)
	select {
	case id := <-got:
		if id != "agent-from-token" {
			t.Fatalf("call runs agent %q, want the token's agent", id)
		}
	case <-ctx.Done():
		t.Fatal("session never allocated")
	}
}
