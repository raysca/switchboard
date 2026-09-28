package compat

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/protobuf/proto"

	framespb "govoice/core/transport/v1/framespb"
)

const (
	testHMACSecret = "v1-compat-secret-32-bytes-minimum!!"
	testIssuer     = "govoice-v1"
	testAudience   = "govoice-v1-client"
)

// 1. Fixture Tests: Go decodes JS fixtures and JS decodes Go-produced frames
func TestCompat_ProtobufFixtures(t *testing.T) {
	fixturesDir := filepath.Join(".", "fixtures")

	// 1a. client-audio.bin
	clientAudioBytes, err := os.ReadFile(filepath.Join(fixturesDir, "client-audio.bin"))
	if err != nil {
		t.Fatalf("Failed to read client-audio.bin: %v", err)
	}
	var clientFrame framespb.Frame
	if err := proto.Unmarshal(clientAudioBytes, &clientFrame); err != nil {
		t.Fatalf("Go failed to unmarshal client-audio.bin: %v", err)
	}
	audio := clientFrame.GetAudio()
	if audio == nil {
		t.Fatalf("clientFrame does not contain AudioRawFrame")
	}
	if audio.GetSampleRate() != 16000 || audio.GetNumChannels() != 1 || len(audio.GetAudio()) != 640 {
		t.Fatalf("Unexpected audio properties: rate=%d, chan=%d, bytes=%d",
			audio.GetSampleRate(), audio.GetNumChannels(), len(audio.GetAudio()))
	}

	// 1b. server-audio.bin
	serverAudioBytes, err := os.ReadFile(filepath.Join(fixturesDir, "server-audio.bin"))
	if err != nil {
		t.Fatalf("Failed to read server-audio.bin: %v", err)
	}
	var serverFrame framespb.Frame
	if err := proto.Unmarshal(serverAudioBytes, &serverFrame); err != nil {
		t.Fatalf("Go failed to unmarshal server-audio.bin: %v", err)
	}
	serverAudio := serverFrame.GetAudio()
	if serverAudio == nil || serverAudio.GetSampleRate() != 24000 || len(serverAudio.GetAudio()) != 960 {
		t.Fatalf("Unexpected server audio: rate=%v, bytes=%v",
			serverAudio.GetSampleRate(), len(serverAudio.GetAudio()))
	}

	// 1c. nested-rtvi.bin
	nestedRtviBytes, err := os.ReadFile(filepath.Join(fixturesDir, "nested-rtvi.bin"))
	if err != nil {
		t.Fatalf("Failed to read nested-rtvi.bin: %v", err)
	}
	var msgFrame framespb.Frame
	if err := proto.Unmarshal(nestedRtviBytes, &msgFrame); err != nil {
		t.Fatalf("Go failed to unmarshal nested-rtvi.bin: %v", err)
	}
	msg := msgFrame.GetMessage()
	if msg == nil {
		t.Fatalf("msgFrame does not contain MessageFrame")
	}
	var rtviPayload map[string]interface{}
	if err := json.Unmarshal([]byte(msg.GetData()), &rtviPayload); err != nil {
		t.Fatalf("Failed to parse nested RTVI JSON: %v", err)
	}
	if rtviPayload["label"] != "rtvi-ai" || rtviPayload["type"] != "bot-ready" {
		t.Fatalf("Unexpected RTVI payload: %v", rtviPayload)
	}

	// 1d. malformed.bin must fail
	malformedBytes, err := os.ReadFile(filepath.Join(fixturesDir, "malformed.bin"))
	if err != nil {
		t.Fatalf("Failed to read malformed.bin: %v", err)
	}
	var malformedFrame framespb.Frame
	if err := proto.Unmarshal(malformedBytes, &malformedFrame); err == nil {
		t.Fatalf("Unmarshal malformed.bin should have returned error, got nil")
	}

	// 1e. wrong-schema.bin must fail or have no frame oneof
	wrongSchemaBytes, err := os.ReadFile(filepath.Join(fixturesDir, "wrong-schema.bin"))
	if err != nil {
		t.Fatalf("Failed to read wrong-schema.bin: %v", err)
	}
	var wrongSchemaFrame framespb.Frame
	_ = proto.Unmarshal(wrongSchemaBytes, &wrongSchemaFrame)
	if wrongSchemaFrame.GetFrame() != nil {
		t.Fatalf("wrong-schema.bin should not have matched any valid Frame oneof variant")
	}
}

// 1f. Go produces protobuf bytes and verifies JS package can deserialize it
func TestCompat_GoToJSProtobufRoundtrip(t *testing.T) {
	// Produce audio frame in Go
	goAudioFrame := &framespb.Frame{
		Frame: &framespb.Frame_Audio{
			Audio: &framespb.AudioRawFrame{
				Id:          1001,
				Name:        "audio",
				Audio:       make([]byte, 960), // 20ms @ 24kHz
				SampleRate:  24000,
				NumChannels: 1,
			},
		},
	}
	goBytes, err := proto.Marshal(goAudioFrame)
	if err != nil {
		t.Fatalf("Failed to marshal goAudioFrame: %v", err)
	}

	tmpFile := filepath.Join(t.TempDir(), "go-audio.bin")
	if err := os.WriteFile(tmpFile, goBytes, 0644); err != nil {
		t.Fatalf("Failed to write tmp go-audio: %v", err)
	}

	// Run node script to deserialize with unmodified @pipecat-ai/websocket-transport
	nodeScript := fmt.Sprintf(`
		const fs = require('fs');
		const { ProtobufFrameSerializer } = require('@pipecat-ai/websocket-transport');
		const serializer = new ProtobufFrameSerializer();
		const data = fs.readFileSync('%s');
		serializer.deserialize(new Blob([data])).then(res => {
			if (res.type !== 'audio' || res.audio.length !== 480) {
				console.error('JS mismatch: type=' + res.type + ', len=' + res.audio.length);
				process.exit(1);
			}
			process.exit(0);
		}).catch(err => {
			console.error(err);
			process.exit(1);
		});
	`, tmpFile)

	cmd := exec.Command("node", "-e", nodeScript)
	cmd.Dir = "."
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Node failed to deserialize Go protobuf frame: %v, output: %s", err, string(output))
	}
}

// 2. Token Security & Query Redaction
type TokenClaims struct {
	jwt.RegisteredClaims
}

type TokenValidator struct {
	mu          sync.Mutex
	hmacSecret  []byte
	replayCache map[string]time.Time
	serverStart time.Time
	maxSkew     time.Duration
}

func NewTokenValidator(secret string, serverStart time.Time) *TokenValidator {
	return &TokenValidator{
		hmacSecret:  []byte(secret),
		replayCache: make(map[string]time.Time),
		serverStart: serverStart,
		maxSkew:     5 * time.Second,
	}
}

var (
	ErrTokenExpired       = errors.New("token expired")
	ErrTokenReplayed      = errors.New("token already consumed")
	ErrTokenInvalidClaims = errors.New("invalid claims or wrong issuer/audience")
	ErrTokenBeforeRestart = errors.New("token issued before process start")
)

func (v *TokenValidator) ValidateAndConsume(tokenStr string, now time.Time) (string, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &TokenClaims{}, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return v.hmacSecret, nil
	}, jwt.WithIssuer(testIssuer), jwt.WithAudience(testAudience))

	if err != nil || !token.Valid {
		return "", fmt.Errorf("%w: %v", ErrTokenInvalidClaims, err)
	}

	claims, ok := token.Claims.(*TokenClaims)
	if !ok || claims.ID == "" || claims.IssuedAt == nil || claims.ExpiresAt == nil {
		return "", ErrTokenInvalidClaims
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	// Check restart boundary: reject iat prior to server start minus skew
	if claims.IssuedAt.Time.Before(v.serverStart.Add(-v.maxSkew)) {
		return "", ErrTokenBeforeRestart
	}

	// Check single-use atomic consumption
	if _, replayed := v.replayCache[claims.ID]; replayed {
		return "", ErrTokenReplayed
	}

	v.replayCache[claims.ID] = claims.ExpiresAt.Time
	return claims.Subject, nil
}

func RedactTokenQuery(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	q := u.Query()
	if q.Has("token") {
		q.Set("token", "[REDACTED]")
		u.RawQuery = q.Encode()
	}
	return u.String()
}

func TestCompat_TokenSecurityAndRedaction(t *testing.T) {
	now := time.Now()
	validator := NewTokenValidator(testHMACSecret, now)

	// Helper to mint tokens
	mintToken := func(jti string, iat, exp time.Time, iss, aud string) string {
		claims := TokenClaims{
			RegisteredClaims: jwt.RegisteredClaims{
				ID:        jti,
				Issuer:    iss,
				Audience:  jwt.ClaimStrings{aud},
				IssuedAt:  jwt.NewNumericDate(iat),
				ExpiresAt: jwt.NewNumericDate(exp),
				Subject:   "user-test-1",
			},
		}
		tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
		tok.Header["kid"] = "v1"
		str, _ := tok.SignedString([]byte(testHMACSecret))
		return str
	}

	// 2a. Valid token consumes successfully
	tok1 := mintToken("tok-001", now, now.Add(5*time.Minute), testIssuer, testAudience)
	sub, err := validator.ValidateAndConsume(tok1, now)
	if err != nil || sub != "user-test-1" {
		t.Fatalf("Valid token should succeed: %v", err)
	}

	// 2b. Replay consumption must fail
	_, err = validator.ValidateAndConsume(tok1, now)
	if !errors.Is(err, ErrTokenReplayed) {
		t.Fatalf("Expected ErrTokenReplayed, got: %v", err)
	}

	// 2c. Token issued before restart boundary must fail
	oldTok := mintToken("tok-old", now.Add(-10*time.Minute), now.Add(5*time.Minute), testIssuer, testAudience)
	_, err = validator.ValidateAndConsume(oldTok, now)
	if !errors.Is(err, ErrTokenBeforeRestart) {
		t.Fatalf("Expected ErrTokenBeforeRestart, got: %v", err)
	}

	// 2d. Wrong issuer must fail
	wrongIssTok := mintToken("tok-bad-iss", now, now.Add(5*time.Minute), "other-issuer", testAudience)
	_, err = validator.ValidateAndConsume(wrongIssTok, now)
	if !errors.Is(err, ErrTokenInvalidClaims) {
		t.Fatalf("Expected ErrTokenInvalidClaims for wrong issuer, got: %v", err)
	}

	// 2e. Query redaction
	sampleURL := "wss://example.com/ws?token=secret12345&client=pipecat"
	redacted := RedactTokenQuery(sampleURL)
	if strings.Contains(redacted, "secret12345") {
		t.Fatalf("Token secret leaked in redacted URL: %s", redacted)
	}
	if !strings.Contains(redacted, "token=%5BREDACTED%5D") && !strings.Contains(redacted, "token=[REDACTED]") {
		t.Fatalf("Expected [REDACTED] in query: %s", redacted)
	}
}

// 3. Concurrent Microphone Read and Paced Output in Minimal Go Stub
func TestCompat_ConcurrentPacedWriteAndRead(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("WebSocket Accept error: %v", err)
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "done")

		// Supervised concurrent read and paced-write
		var wg sync.WaitGroup
		wg.Add(2)

		// Task 1: Reader (microphone ingress)
		readFrames := 0
		go func() {
			defer wg.Done()
			for {
				typ, data, err := conn.Read(ctx)
				if err != nil {
					return
				}
				if typ == websocket.MessageBinary {
					var frame framespb.Frame
					if err := proto.Unmarshal(data, &frame); err == nil && frame.GetAudio() != nil {
						readFrames++
					}
				}
			}
		}()

		// Task 2: Paced Writer (TTS output emulation at 20ms pacing)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(20 * time.Millisecond)
			defer ticker.Stop()

			sampleFrame := &framespb.Frame{
				Frame: &framespb.Frame_Audio{
					Audio: &framespb.AudioRawFrame{
						Id:          1,
						Audio:       make([]byte, 960), // 20ms @ 24kHz
						SampleRate:  24000,
						NumChannels: 1,
					},
				},
			}
			data, _ := proto.Marshal(sampleFrame)

			for i := 0; i < 10; i++ {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if err := conn.Write(ctx, websocket.MessageBinary, data); err != nil {
						return
					}
				}
			}
		}()

		wg.Wait()
	}))
	defer server.Close()

	// Client connection using coder/websocket
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	clientConn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("Failed to dial test server: %v", err)
	}
	defer clientConn.Close(websocket.StatusNormalClosure, "client close")

	var clientWg sync.WaitGroup
	clientWg.Add(2)

	// Client read task
	receivedServerFrames := 0
	go func() {
		defer clientWg.Done()
		for {
			typ, data, err := clientConn.Read(ctx)
			if err != nil {
				return
			}
			if typ == websocket.MessageBinary {
				var frame framespb.Frame
				if err := proto.Unmarshal(data, &frame); err == nil && frame.GetAudio() != nil {
					receivedServerFrames++
				}
			}
		}
	}()

	// Client send task (simulating mic audio)
	go func() {
		defer clientWg.Done()
		micFrame := &framespb.Frame{
			Frame: &framespb.Frame_Audio{
				Audio: &framespb.AudioRawFrame{
					Id:          2,
					Audio:       make([]byte, 640), // 20ms @ 16kHz
					SampleRate:  16000,
					NumChannels: 1,
				},
			},
		}
		data, _ := proto.Marshal(micFrame)

		for i := 0; i < 10; i++ {
			_ = clientConn.Write(ctx, websocket.MessageBinary, data)
			time.Sleep(20 * time.Millisecond)
		}
		clientConn.Close(websocket.StatusNormalClosure, "finished input")
	}()

	clientWg.Wait()

	if receivedServerFrames < 5 {
		t.Errorf("Expected at least 5 paced server audio frames, got %d", receivedServerFrames)
	}
}

// Unused dummy hmac check to suppress unused warning
var _ = hmac.New(sha256.New, []byte{})
