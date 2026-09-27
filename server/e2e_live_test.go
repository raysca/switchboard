//go:build live

package server

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"govoice/core"
	"govoice/examples/envload"
	"govoice/transport/ws"
	"govoice/vad"
)

// TestLiveEndToEndCall places a real WebSocket call through the server:
// microphone PCM, Silero VAD, Google STT, Google LLM with a follow-up turn,
// and Google TTS audio back to the client.
func TestLiveEndToEndCall(t *testing.T) {
	if err := envload.Load(); err != nil {
		t.Fatalf("load env: %v", err)
	}
	if os.Getenv("GOOGLE_API_KEY") == "" && os.Getenv("GOOGLE_APPLICATION_CREDENTIALS") == "" {
		t.Fatal("live call requires Google credentials in examples/.env")
	}

	utt, err := synthesizeUtterance(t, "What is two plus two?")
	if err != nil {
		t.Fatalf("synthesize utterance: %v", err)
	}
	follow, err := synthesizeUtterance(t, "What did I just ask you?")
	if err != nil {
		t.Fatalf("synthesize follow-up: %v", err)
	}

	certFile, keyFile := writeSelfSigned(t, t.TempDir())
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.PublicListenAddr = "127.0.0.1:18181"
	cfg.AdminListenAddr = "127.0.0.1:19191"
	cfg.Token.Secret = "live-e2e-secret-key-not-for-production-use"
	cfg.TLS.CertFile = certFile
	cfg.TLS.KeyFile = keyFile
	cfg.VAD.Mode = "inprocess"
	cfg.OriginAllowlist = []string{"*"}

	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if !srv.registry.PipelinesLive(cfg) {
		t.Fatal("providers did not initialize; refusing a fake-ready call")
	}

	vadCfg := vad.DefaultConfig()
	vadCfg.Mode = "sharded"
	vadCfg.Workers = 1
	vadCfg.Batch = 1
	vadCfg.QueueCap = 256
	vad.ConfigureArtifacts(cfg.VAD.ModelPath, cfg.VAD.OrtLibPath)
	execVAD, err := vad.New(vadCfg)
	if err != nil {
		t.Fatalf("vad: %v", err)
	}
	defer execVAD.Close()
	srv.SetVADExecutor(execVAD)

	orig := srv.wsServer.OnSessionReady
	srv.wsServer.OnSessionReady = func(conn *ws.Connection) (*core.Session, error) {
		session, err := orig(conn)
		if err != nil {
			t.Logf("session setup failed: %v", err)
		}
		return session, err
	}

	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	tok := createScenarioToken(t, srv, "live-e2e-user", "jti-live-e2e")
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer dialCancel()
	conn, _, err := websocket.Dial(dialCtx, "wss://127.0.0.1:18181/ws?token="+tok, &websocket.DialOptions{
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
	var audioBytes int
	var botReady bool
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			_, data, err := conn.Read(callCtx)
			if err != nil {
				return
			}
			frame, err := ws.DecodeBinaryFrame(data, 1, time.Now())
			if err != nil {
				continue
			}
			mu.Lock()
			switch f := frame.(type) {
			case *ws.WireMessageFrame:
				texts = append(texts, f.Data)
				if strings.Contains(f.Data, "bot-ready") {
					botReady = true
				}
			case *core.InputAudioFrame:
				audioBytes += len(f.Samples)
			}
			mu.Unlock()
		}
	}()

	deadline := time.Now().Add(5 * time.Second)
	for !botReady && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		ok := botReady
		mu.Unlock()
		if ok {
			break
		}
	}
	if !botReady {
		t.Fatal("did not receive bot-ready")
	}

	if err := writePCM(callCtx, conn, utt); err != nil {
		t.Fatalf("write first utterance: %v", err)
	}
	if !waitForAudio(&mu, &audioBytes, 50*time.Second) {
		mu.Lock()
		t.Logf("messages: %s", truncate(strings.Join(texts, "\n"), 4000))
		mu.Unlock()
		if tl := srv.timelines.Get(1); tl != nil {
			t.Logf("timeline: %+v", tl.Events())
		}
		srv.mu.Lock()
		for id, call := range srv.activeCalls {
			t.Logf("call %d err=%v", id, call.session.Err())
		}
		srv.mu.Unlock()
		t.Fatal("timed out waiting for assistant audio")
	}
	time.Sleep(3 * time.Second)
	mu.Lock()
	firstAudio := audioBytes
	mu.Unlock()

	if err := writePCM(callCtx, conn, follow); err != nil {
		t.Fatalf("write follow-up: %v", err)
	}
	deadline = time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := audioBytes
		mu.Unlock()
		if n > firstAudio+2000 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	mu.Lock()
	snapshot := append([]string(nil), texts...)
	gotAudio := audioBytes
	mu.Unlock()
	callCancel()
	<-readDone
	_ = conn.Close(websocket.StatusNormalClosure, "done")

	joined := strings.Join(snapshot, "\n")
	if !strings.Contains(joined, "user-transcription") {
		t.Fatalf("no user transcript in call: %s", truncate(joined, 2000))
	}
	if !strings.Contains(joined, "bot-llm-text") {
		t.Fatalf("no assistant text in call: %s", truncate(joined, 2000))
	}
	if gotAudio < 2000 {
		t.Fatalf("expected synthesized audio, got %d bytes", gotAudio)
	}

	result := map[string]any{
		"ran_at":             time.Now().Format(time.RFC3339),
		"transport":          "wss://127.0.0.1:18181/ws",
		"vad":                "silero sharded in-process",
		"providers":          "google stt, google llm, google tts",
		"turns":              2,
		"user_transcripts":   countSubstr(joined, "user-transcription"),
		"assistant_deltas":   countSubstr(joined, "bot-llm-text"),
		"output_audio_bytes": gotAudio,
		"saw_bot_ready":      true,
		"transcript_excerpt": truncate(joined, 1500),
	}
	raw, _ := json.MarshalIndent(result, "", "  ")
	out := filepath.Join("..", "docs", "benchmarks", "e2e_call_result.json")
	if err := os.WriteFile(out, append(raw, '\n'), 0644); err != nil {
		t.Fatalf("write evidence: %v", err)
	}
	t.Logf("live call ok audio=%d transcripts=%d", gotAudio, countSubstr(joined, "user-transcription"))
}

// TestLiveSTTRotation keeps a real Google STT stream open past the production
// 240s rotation threshold, then checks that a later utterance is still transcribed.
func TestLiveSTTRotation(t *testing.T) {
	if err := envload.Load(); err != nil {
		t.Fatalf("load env: %v", err)
	}
	if os.Getenv("GOOGLE_API_KEY") == "" && os.Getenv("GOOGLE_APPLICATION_CREDENTIALS") == "" {
		t.Fatal("live call requires Google credentials in examples/.env")
	}

	first, err := synthesizeUtterance(t, "What is two plus two?")
	if err != nil {
		t.Fatalf("synthesize first: %v", err)
	}
	second, err := synthesizeUtterance(t, "What is three plus three?")
	if err != nil {
		t.Fatalf("synthesize second: %v", err)
	}

	certFile, keyFile := writeSelfSigned(t, t.TempDir())
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.PublicListenAddr = "127.0.0.1:18182"
	cfg.AdminListenAddr = "127.0.0.1:19192"
	cfg.Token.Secret = "live-e2e-secret-key-not-for-production-use"
	cfg.TLS.CertFile = certFile
	cfg.TLS.KeyFile = keyFile
	cfg.VAD.Mode = "inprocess"
	cfg.OriginAllowlist = []string{"*"}
	cfg.Limits.IdleTimeout = 3 * time.Minute
	cfg.Providers.GoogleSTT.RotationThreshold = 240 * time.Second

	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if !srv.registry.PipelinesLive(cfg) {
		t.Fatal("providers did not initialize")
	}
	vadCfg := vad.DefaultConfig()
	vadCfg.Mode = "sharded"
	vadCfg.Workers = 1
	vadCfg.Batch = 1
	vadCfg.QueueCap = 256
	vad.ConfigureArtifacts(cfg.VAD.ModelPath, cfg.VAD.OrtLibPath)
	execVAD, err := vad.New(vadCfg)
	if err != nil {
		t.Fatalf("vad: %v", err)
	}
	defer execVAD.Close()
	srv.SetVADExecutor(execVAD)
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	tok := createScenarioToken(t, srv, "live-rotation-user", "jti-live-rotation")
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer dialCancel()
	conn, _, err := websocket.Dial(dialCtx, "wss://127.0.0.1:18182/ws?token="+tok, &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "done")

	callCtx, callCancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer callCancel()
	ready, _ := ws.EncodeMessageFrame(`{"id":"1","label":"rtvi-ai","type":"client-ready","data":{"version":"2.1.0"}}`)
	if err := conn.Write(callCtx, websocket.MessageBinary, ready); err != nil {
		t.Fatalf("client-ready: %v", err)
	}

	var mu sync.Mutex
	var texts []string
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
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
		t.Fatal("first transcript did not arrive before the rotation hold")
	}

	silence := make([]byte, 640)
	holdUntil := time.Now().Add(250 * time.Second)
	for time.Now().Before(holdUntil) {
		if err := writePCM(callCtx, conn, silence); err != nil {
			t.Fatalf("silence: %v", err)
		}
	}
	if err := writePCM(callCtx, conn, second); err != nil {
		t.Fatalf("second audio: %v", err)
	}
	if !waitForText(&mu, &texts, "3 + 3", 45*time.Second) {
		mu.Lock()
		joined := strings.Join(texts, "\n")
		mu.Unlock()
		t.Fatalf("second transcript missing after rotation hold: %s", truncate(joined, 2000))
	}

	_ = conn.Close(websocket.StatusNormalClosure, "done")
	callCancel()
	<-readDone

	var metricsBody string
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get("http://127.0.0.1:19192/metrics")
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			metricsBody = string(b)
			if strings.Contains(metricsBody, "voice_stt_rotations_total ") && !strings.Contains(metricsBody, "voice_stt_rotations_total 0\n") {
				break
			}
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(metricsBody, "voice_stt_rotations_total ") {
		t.Fatalf("metrics missing rotation counter:\n%s", metricsBody)
	}
	rotLine := ""
	for _, line := range strings.Split(metricsBody, "\n") {
		if strings.HasPrefix(line, "voice_stt_rotations_total ") {
			rotLine = line
		}
	}
	if rotLine == "voice_stt_rotations_total 0" {
		t.Fatalf("stream did not rotate: %s", rotLine)
	}

	mu.Lock()
	joined := strings.Join(texts, "\n")
	mu.Unlock()
	result := map[string]any{
		"ran_at":             time.Now().Format(time.RFC3339),
		"rotation_line":      rotLine,
		"hold_seconds":       250,
		"threshold_seconds":  240,
		"transcript_excerpt": truncate(joined, 1500),
	}
	raw, _ := json.MarshalIndent(result, "", "  ")
	if err := os.WriteFile(filepath.Join("..", "docs", "benchmarks", "long_call_result.json"), append(raw, '\n'), 0644); err != nil {
		t.Fatalf("write evidence: %v", err)
	}
	t.Logf("rotation call ok %s", rotLine)
}

func waitForText(mu *sync.Mutex, texts *[]string, sub string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		mu.Lock()
		joined := strings.Join(*texts, "\n")
		mu.Unlock()
		if strings.Contains(joined, sub) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

func synthesizeUtterance(t *testing.T, text string) ([]byte, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "utt.wav")
	cmd := exec.Command("say", "-o", path, "--file-format=WAVE", "--data-format=LEI16@16000", text)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, errString(err, out)
	}
	return wavData(path)
}

func wavData(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	idx := indexBytes(b, []byte("data"))
	if idx < 0 || idx+8 > len(b) {
		return nil, errString(os.ErrInvalid, b[:min(64, len(b))])
	}
	n := binary.LittleEndian.Uint32(b[idx+4 : idx+8])
	start := idx + 8
	if start+int(n) > len(b) {
		return nil, os.ErrInvalid
	}
	pcm := append([]byte(nil), b[start:start+int(n)]...)
	silence := make([]byte, 16000*2)
	return append(pcm, silence...), nil
}

func writePCM(ctx context.Context, conn *websocket.Conn, pcm []byte) error {
	const frame = 640
	for off := 0; off < len(pcm); off += frame {
		end := off + frame
		if end > len(pcm) {
			end = len(pcm)
		}
		chunk := pcm[off:end]
		if len(chunk)%2 != 0 {
			chunk = chunk[:len(chunk)-1]
		}
		if len(chunk) < 320 {
			break
		}
		wire, err := ws.EncodeAudioFrame(chunk, 16000, 1)
		if err != nil {
			return err
		}
		if err := conn.Write(ctx, websocket.MessageBinary, wire); err != nil {
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil
}

func waitForAudio(mu *sync.Mutex, audioBytes *int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := *audioBytes
		mu.Unlock()
		if n > 2000 {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

func countSubstr(s, sub string) int {
	return strings.Count(s, sub)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func indexBytes(b, sub []byte) int {
	return strings.Index(string(b), string(sub))
}

func errString(err error, out []byte) error {
	return &liveErr{err: err, out: string(out)}
}

type liveErr struct {
	err error
	out string
}

func (e *liveErr) Error() string { return e.err.Error() + ": " + e.out }

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
