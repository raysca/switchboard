package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/coretest"
	"govoice/core/record"
	"govoice/core/redact"
	"govoice/server/otelexport"
	"govoice/server/storage"
	"govoice/transport/ws"
	"govoice/vad"
)

func TestStoredTurnsAreRedacted(t *testing.T) {
	rd, err := redact.New(redact.Config{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{redactor: rd}
	row := s.redactTurn(storage.TurnRecord{Transcript: "DTMF: 4111#", AssistantText: "your card 4111 1111 1111 1111"})
	if strings.Contains(row.Transcript, "4111") || strings.Contains(row.AssistantText, "4111") {
		t.Fatalf("raw content stored: %+v", row)
	}
	// Default policy (text rules off) still masks DTMF.
	d, _ := redact.New(redact.Config{})
	got := (&Server{redactor: d}).redactTurn(storage.TurnRecord{Transcript: "DTMF: 1*2*3*4#"}).Transcript
	if got != "DTMF: [REDACTED:dtmf 7]" {
		t.Fatal(got)
	}
}

// rawLeak reports a raw secret in stored/exported text. The DTMF secret has
// '*' separators and the card number a separator, so neither can occur in a
// timestamp (a bare "1234" or "4111" can: ".123443Z").
func rawLeak(s string) bool {
	return strings.Contains(s, "1*2*3*4") || strings.Contains(s, "4111 1111") || strings.Contains(s, "4111111111111111")
}

func TestRawLeakIgnoresTimestamps(t *testing.T) {
	if rawLeak(`{"ended_at":"2026-10-18T10:20:24.123443Z","n":"4111"}`) {
		t.Fatal("timestamp digits flagged as a leak")
	}
	for _, leak := range []string{"DTMF: 1*2*3*4#", "card 4111 1111 1111 1111", "4111111111111111"} {
		if !rawLeak(leak) {
			t.Fatalf("real leak missed: %q", leak)
		}
	}
}

func TestRedactionConfigValidated(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Privacy.Redaction.Rules = []string{"bogus"}
	if err := ValidateConfig(cfg); err == nil {
		t.Fatal("unknown rule accepted")
	}
}

// A DTMF aggregate flowing through the turn recorder reaches storage masked,
// with the default policy (text rules off).
func TestDTMFTurnStoredMasked(t *testing.T) {
	d, _ := redact.New(redact.Config{})
	s := &Server{redactor: d}
	var mu sync.Mutex
	var rows []storage.TurnRecord
	r := newTurnRecorder(func(_ context.Context, row storage.TurnRecord) error {
		mu.Lock()
		rows = append(rows, s.redactTurn(row))
		mu.Unlock()
		return nil
	}, nil, nil, "sess", slog.New(slog.NewTextHandler(io.Discard, nil)), newTurnLatencies())
	pipe, _ := core.NewPipeline(r)
	session, _ := core.NewSession(1, pipe, core.DefaultResourceBudget())
	_ = session.Start()
	_ = session.Post(&core.UserMessageFrame{Turn: "t1", Content: "DTMF: 4111#", Timestamp: time.Now()})
	_ = session.Post(&core.AssistantMessageFrame{Turn: "t1", Content: "ok", Timestamp: time.Now()})
	time.Sleep(50 * time.Millisecond)
	_ = session.Close()
	r.close(t.Context())
	mu.Lock()
	defer mu.Unlock()
	if len(rows) != 1 || rows[0].Transcript != "DTMF: [REDACTED:dtmf 7]" {
		t.Fatalf("%+v", rows)
	}
}

// echoLLM records what the model stage received and answers by repeating a
// card number, so both sides of a turn carry sensitive content.
type echoLLM struct {
	sess core.SessionController
	idx  int
	mu   sync.Mutex
	got  []string
}

func (l *echoLLM) Attach(s core.SessionController, i int) error { l.sess, l.idx = s, i; return nil }

func (l *echoLLM) ProcessFrame(_ context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	if v, ok := f.(*core.UserMessageFrame); ok && dir == core.Downstream {
		l.mu.Lock()
		l.got = append(l.got, v.Content)
		l.mu.Unlock()
		if err := e.PushDirection(f, dir); err != nil {
			return err
		}
		return l.sess.Go(func(context.Context) {
			_ = l.sess.Reenter(&core.LLMTextFrame{Text: "Card 4111 1111 1111 1111 noted.", Turn: v.Turn, Gen: v.Gen}, core.Downstream, l.idx+1, v.Gen)
		})
	}
	return e.PushDirection(f, dir)
}

func (l *echoLLM) received() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.got, "|")
}

// End to end through BuildCallPipeline: key presses and a spoken card number
// reach the model raw, while stored turns, the recording sidecar and OTel
// export only ever see placeholders.
func TestRedactionEndToEnd(t *testing.T) {
	var otlp bytes.Buffer
	var otlpMu sync.Mutex
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		otlpMu.Lock()
		otlp.Write(b)
		otlpMu.Unlock()
	}))
	defer collector.Close()

	cfg := DefaultConfig(ProfileDevelopment)
	cfg.DTMF.Detect, cfg.DTMF.Aggregate = true, true
	cfg.Recording.Enabled = true
	rd, err := redact.New(redact.Config{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	sink, err := record.NewLocalSink(dir)
	if err != nil {
		t.Fatal(err)
	}
	exp, err := otelexport.New(otelexport.Config{Enabled: true, Endpoint: collector.URL + "/v1/traces", CaptureContent: true}, os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	exp.SetRedactor(rd.Redact)
	s := &Server{cfg: cfg, redactor: rd, recSink: sink, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	var mu sync.Mutex
	var rows []storage.TurnRecord
	history := newTurnRecorder(func(_ context.Context, row storage.TurnRecord) error {
		mu.Lock()
		rows = append(rows, s.redactTurn(row)) // as resolveCall does
		mu.Unlock()
		return nil
	}, nil, nil, "sess", s.logger, newTurnLatencies())

	llm := &echoLLM{}
	reg := NewRegistry()
	reg.Register("google.stt", func(*Config, StageSpec) (core.Processor, error) { return passSTTStage{}, nil })
	reg.Register("google.tts", func(*Config, StageSpec) (core.Processor, error) {
		return coretest.NewTTSProcessor(coretest.TTSConfig{FramesPerUnit: 1}), nil
	})
	reg.Register("google.llm", func(*Config, StageSpec) (core.Processor, error) { return llm, nil })
	call := CallSpec{STT: StageSpec{Provider: "google"}, TTS: StageSpec{Provider: "google"}, LLM: StageSpec{Provider: "google"},
		MaxMessages: 20, MaxTokens: 4000, History: history, Recording: true}
	vadProc := vad.NewProcessor(&mockVADExecutor{ready: true, available: 1, total: 1}, vad.DefaultProcessorConfig())
	pipe, err := reg.BuildCallPipeline(cfg, call, vadProc, coretest.NewTransportOutput(coretest.TransportConfig{}))
	if err != nil {
		t.Fatal(err)
	}
	userCtx := -1
	for i, p := range pipe.Processors() {
		if _, ok := p.(*core.UserContextProcessor); ok {
			userCtx = i
		}
	}
	sess, err := core.NewSession(1, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.SetTracer(exp.Session("flow", "v1"), exp.Policy()); err != nil {
		t.Fatal(err)
	}
	stopRec := s.attachRecording(call, "flow", 1, &ws.Connection{}, sess)
	if stopRec == nil {
		t.Fatal("recording not attached")
	}
	if err := sess.Start(); err != nil {
		t.Fatal(err)
	}
	wait := func(what string, ok func() bool) {
		t.Helper()
		for end := time.Now().Add(5 * time.Second); !ok(); time.Sleep(5 * time.Millisecond) {
			if time.Now().After(end) {
				t.Fatalf("timed out waiting for %s", what)
			}
		}
	}
	nrows := func() int { mu.Lock(); defer mu.Unlock(); return len(rows) }

	for _, d := range []string{"1", "*", "2", "*", "3", "*", "4", "#"} {
		if err := sess.Post(&core.InputDTMFFrame{Digit: d}); err != nil {
			t.Fatal(err)
		}
	}
	wait("DTMF at the model", func() bool { return strings.Contains(llm.received(), "DTMF: 1*2*3*4#") })
	wait("first turn stored", func() bool { return nrows() >= 1 })
	gen := sess.CurrentGeneration()
	if err := sess.Reenter(&core.FinalTranscriptionFrame{Text: "my card is 4111 1111 1111 1111", Turn: "t2", Gen: gen, Committed: true, Timestamp: time.Now()}, core.Downstream, userCtx, gen); err != nil {
		t.Fatal(err)
	}
	wait("card at the model", func() bool { return strings.Contains(llm.received(), "4111 1111 1111 1111") })
	wait("second turn stored", func() bool { return nrows() >= 2 })

	_ = sess.Close()
	history.close(t.Context())
	stopRec()
	if err := exp.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	stored, _ := json.Marshal(rows)
	mu.Unlock()
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	if len(files) != 1 {
		t.Fatalf("sidecars: %v", files)
	}
	sidecar, _ := os.ReadFile(files[0])
	otlpMu.Lock()
	exported := otlp.String()
	otlpMu.Unlock()
	if !strings.Contains(exported, "govoice.turn") {
		t.Fatalf("nothing exported to the collector (%d bytes)", len(exported))
	}
	for name, got := range map[string]string{"turns": string(stored), "sidecar": string(sidecar), "otel": exported} {
		if rawLeak(got) {
			t.Errorf("%s holds raw content: %s", name, got)
		}
	}
	for name, got := range map[string]string{"turns": string(stored), "sidecar": string(sidecar)} {
		if !strings.Contains(got, "[REDACTED:dtmf 7]") || !strings.Contains(got, "[REDACTED:card]") {
			t.Errorf("%s lacks placeholders: %s", name, got)
		}
	}
}
