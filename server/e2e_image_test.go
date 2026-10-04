//go:build live

package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/golang-jwt/jwt/v5"

	"govoice/examples/envload"
	"govoice/transport/token"
	"govoice/transport/ws"
)

func mintImageToken(t *testing.T, jti string) string {
	t.Helper()
	secret := os.Getenv("TOKEN_SECRET")
	if secret == "" {
		t.Fatal("TOKEN_SECRET is required and must match the container")
	}
	now := time.Now()
	claims := token.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        jti,
			Issuer:    "govoice-v1",
			Audience:  jwt.ClaimStrings{"govoice-v1-client"},
			Subject:   "image-call-user",
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(5 * time.Minute)),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tok.Header["kid"] = "v1"
	signed, err := tok.SignedString([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

// TestLiveImageCall places one two-turn Google call through the Compose proxy.
// The 30-minute image hold is a separate test and is not part of this run.
func TestLiveImageCall(t *testing.T) {
	if err := envload.Load(); err != nil {
		t.Fatalf("load env: %v", err)
	}
	first, err := synthesizeUtterance(t, "What is two plus two?")
	if err != nil {
		t.Fatalf("synthesize first: %v", err)
	}
	second, err := synthesizeUtterance(t, "What did I just ask you?")
	if err != nil {
		t.Fatalf("synthesize second: %v", err)
	}

	signed := mintImageToken(t, "jti-image-call")
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer dialCancel()
	conn, _, err := websocket.Dial(dialCtx, "wss://127.0.0.1/ws?token="+signed, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"https://app.govoice.internal"}},
		HTTPClient: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "done")

	callCtx, callCancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer callCancel()
	ready, _ := ws.EncodeMessageFrame(`{"id":"1","label":"rtvi-ai","type":"client-ready","data":{"version":"2.1.0"}}`)
	if err := conn.Write(callCtx, websocket.MessageBinary, ready); err != nil {
		t.Fatalf("client-ready: %v", err)
	}

	var mu sync.Mutex
	var texts []string
	go func() {
		for {
			_, data, err := conn.Read(callCtx)
			if err != nil {
				return
			}
			frame, err := ws.DecodeBinaryFrame(data, 1, time.Now())
			if err != nil {
				continue
			}
			if msg, ok := frame.(*ws.WireMessageFrame); ok {
				mu.Lock()
				texts = append(texts, msg.Data)
				mu.Unlock()
			}
		}
	}()

	if err := writePCM(callCtx, conn, first); err != nil {
		t.Fatalf("first audio: %v", err)
	}
	if !waitForText(&mu, &texts, "user-transcription", 30*time.Second) {
		t.Fatal("first transcript did not arrive")
	}
	if err := writePCM(callCtx, conn, second); err != nil {
		t.Fatalf("second audio: %v", err)
	}
	if !waitForText(&mu, &texts, "bot-llm-text", 45*time.Second) {
		mu.Lock()
		joined := strings.Join(texts, "\n")
		mu.Unlock()
		t.Fatalf("assistant text did not arrive: %s", truncate(joined, 1500))
	}
	mu.Lock()
	joined := strings.Join(texts, "\n")
	mu.Unlock()
	lower := strings.ToLower(joined)
	if !strings.Contains(lower, "2 + 2") && !strings.Contains(lower, "two plus two") {
		t.Fatalf("follow-up did not refer to the first question: %s", truncate(joined, 1500))
	}

	result := map[string]any{
		"transport":       "wss://127.0.0.1/ws",
		"transcripts":     texts,
		"assistant_texts": joined,
		"ran_at":          time.Now().Format(time.RFC3339),
	}
	raw, _ := json.MarshalIndent(result, "", "  ")
	if err := os.WriteFile("../docs/benchmarks/image_call_result.json", append(raw, '\n'), 0o644); err != nil {
		t.Fatalf("write evidence: %v", err)
	}
}
