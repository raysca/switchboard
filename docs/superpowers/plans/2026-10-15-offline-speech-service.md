# Offline Speech Service Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A standalone Go service (`offline/`) that speaks xAI's STT and TTS wire protocols and is backed by a local Speaches server (faster-whisper + Kokoro), plus a compose bundle, so the existing `xai` adapters run whole calls with real audio and no network. The server knows it as provider `offline`, and an end-to-end test runs a call through it.

**Architecture:** `offline/adapter` is a thin protocol adapter: `GET /v1/stt` (WebSocket), `GET /v1/tts` (WebSocket), `POST /v1/tts` (unary), `GET /healthz`. STT builds streaming on top of batch Whisper: a pure-Go energy endpointer cuts utterances, a single ordered worker re-decodes the growing utterance for interims and decodes once more for `speech_final`, with generation fencing so a stale interim never follows a final. TTS forwards each sentence unit to Speaches and streams the PCM back as `audio.delta`. The LLM is **not** proxied: `xai_llm.endpoint` points straight at Ollama (spiked). A separate `offline/contract` module drives the real `providers/xai` clients against the adapter. In `server/`, a small `offline` provider (`offline.stt/tts/llm`) reuses the xai processors with its own endpoints, breakers and no key, so one server can mix real xAI and offline agents; server e2e tests run a call through it (hermetic with fake backends, and opt-in against the real stack).

**Tech Stack:** Go 1.27.1, `github.com/coder/websocket` v1.8.15, `net/http` stdlib, Docker Compose, Speaches (`ghcr.io/speaches-ai/speaches`), Ollama.

**Spec:** `docs/superpowers/specs/2026-10-15-offline-speech-service-design.md` (wire contract, spike results, compose profiles). **Deviation from the spec, decided while planning:** the spec said to reuse `vad/` for endpointing. `vad/` is cgo + onnxruntime behind supervised worker processes with a 576-sample IPC protocol, which would pull cgo and a native library into the adapter image. This plan uses a ~60-line pure-Go RMS-energy endpointer instead (ceiling: noisy microphones; upgrade path is `vad/` or sherpa-onnx). Task 1 step 1 corrects the spec.

## Global Constraints

- Separate Go modules inside the existing `go.work`: `govoice/offline` (adapter, depends only on `github.com/coder/websocket`) and `govoice/offline/contract` (tests that import `govoice/core` and `govoice/providers/xai`). The adapter module must not import `server/`, `core/` or `providers/`. `core/` and `providers/` are not modified. `server/` gains the `offline` provider (Task 8) and imports `govoice/offline/adapter` **only from `_test.go` files** (Task 10).
- AGENTS.md rules apply: every queue bounded; every goroutine has a termination path (`ctx.Done()` or a closed channel); no `context.Context` in struct fields; `defer cancel()` right after creating a context; no mutex held across I/O; monotonic timing only (`time.Since`); only the producer closes a channel; errors wrapped with `%w`, inspected with `errors.Is/As`; exported sentinel errors are `var Err... = errors.New(...)`; WebSocket handlers must not let a client anomaly crash the process.
- All code passes `go test -race ./...` and `gofmt`.
- Provider secrets: never write or store keys in files. The adapter accepts any bearer token unless `OFFLINE_TOKEN` is set in the environment. The server's `offline` provider reads no credential (it sends the constant bearer `offline`, which the local stack ignores); if `OFFLINE_TOKEN` is set on the adapter, the offline provider is not usable until a token option is added (documented limit).
- Nothing downloads at runtime. Only `make offline-models` needs network.
- xAI wire facts (copied from the spec, do not change): STT query `sample_rate=16000&encoding=pcm&language=en&interim_results=true&model=...`; server sends `{"type":"transcript.created"}` first; client sends binary PCM16LE mono then text `{"type":"audio.done"}`; server sends `transcript.partial` (`speech_final:true` carries the **whole utterance**; this plan never sends `is_final`), then `transcript.done`, then closes. TTS WebSocket: client `text.delta` + `text.done` per unit, server `audio.delta` (base64 PCM16LE, even byte count) + `audio.done`, **connection stays open** for the next unit. TTS unary: `POST /v1/tts` body `{"text","voice_id","language","output_format":{"codec":"pcm","sample_rate":24000}}` returns raw PCM16LE. Bad voice = HTTP 400; bad token = 401.

## Review Focus

Failure modes the spec implies but a happy-path test would miss, most likely first. Each has a test in the task named in brackets.

1. **Client disconnects mid-synthesis or mid-decode** (barge-in closes the TTS socket; a call can end mid-utterance): the backend HTTP request must be cancelled and no goroutine may leak. [Task 4 `TestTTSWSCancelsSynthesisOnClose`, Task 6 `TestSTTWorkerStopsOnDisconnect`]
2. **Empty, oversized or odd-length input:** empty `text.done` must still answer `audio.done`; text beyond the cap is an error not unbounded memory; an odd trailing PCM byte from the backend must not produce an odd-length `audio.delta`. [Task 3 `TestStreamPCMDropsTrailingOddByte`, Task 4 `TestTTSWSEmptyUnitAndTextCap`]
3. **Silence, clicks and short blips:** a stream of silence or a 100 ms click must produce no transcript and no Whisper call (no "Thank you." hallucination). [Task 5 `TestEndpointerShortBlipAborts`, Task 6 `TestSTTSilenceNeverDecodes`]
4. **Backend down or overloaded:** TTS returns an `error` message (the adapter retries unary); STT final failure sends `error` and closes; a full queue is HTTP 503 / `error`, never a hang. [Task 3 `TestTTSPostBackendErrors`, Task 6 `TestSTTFinalErrorSendsErrorAndCloses`]
5. **Ordering:** a stale interim decoded after its utterance's `speech_final` must be dropped; two utterances in one stream must finalize in order; an utterance longer than `MaxUtterance` is cut with a `speech_final`, not buffered without bound. [Task 6 `TestSTTStaleInterimDropped`, `TestSTTUtteranceCap`]
6. **Auth:** WebSocket upgrades without the token get 401 when `OFFLINE_TOKEN` is set; `/healthz` stays open. [Task 1 `TestRequireToken`]
7. **Provider isolation:** an offline outage must not open the real xAI breakers (separate breakers); a build without `-tags xaispeech` must report `offline` STT/TTS as unsupported, not crash. [Task 8 `TestOfflineBreakersAreSeparate`, `TestOfflineSpeechAbsentWithoutXAISpeechTag`]
8. **Call level:** a caller's audio must come back as a spoken reply through the real server, and a dead adapter must end the call cleanly, not hang it. [Task 10 `TestOfflineCallEndToEnd`, `TestOfflineAdapterDownFailsTheStage`]

---

## File Structure

```
offline/
  go.mod                         module govoice/offline
  Dockerfile                     adapter image (static binary, distroless)
  compose.yaml                   speaches + adapter (+ ollama in profile container-llm)
  Modelfile                      pinned local LLM: temperature 0, seed 42
  README.md                      how to run both profiles
  cmd/offline-adapter/main.go    env -> Config, http.Server, -healthcheck flag
  cmd/toolcheck/main.go          picks the small LLM: does a model do a tool-call round trip on /v1/responses
  adapter/
    config.go                    Config, LoadConfig, Config.Voice
    auth.go                      requireToken middleware, writeError
    limiter.go                   Limiter (bounded concurrency + queue deadline)
    backend.go                   TTSBackend / STTBackend interfaces, BackendError
    speaches.go                  Speaches HTTP client (TTS, STT, health), wavHeader
    server.go                    Server, New, Handler, /healthz
    tts.go                       POST /v1/tts, GET /v1/tts (WS), streamPCM
    endpoint.go                  Endpointer (RMS energy), Event
    stt.go                       GET /v1/stt (WS): read loop + ordered decode worker
    *_test.go                    one test file per source file + helpers_test.go
  contract/
    go.mod                       module govoice/offline/contract
    contract_test.go             real xai clients vs in-process adapter + fake backends (tag xaispeech)
    stack_test.go                real xai clients vs the running compose stack (tags xaispeech offlinestack)
```

Server (Task 8, 10): `server/registry_offline.go`, `server/registry_offline_speech.go` (tag xaispeech), `server/registry_offline_test.go`, `server/catalog/data/offline.yaml`, `server/offline_e2e_test.go`, `server/offline_stack_e2e_test.go`; modified `server/config.go`, `server/config_defaults.go`, `server/registry.go`, `server/registry_noxaispeech.go`, `server/go.mod`, `server/catalog/catalog_test.go`.

Modified elsewhere: `go.work` (add `./offline`, `./offline/contract`), `Makefile` (MODULES, `offline-*` targets), `docs/providers.md`, `docs/privacy.md`, the spec.

---

### Task 1: Module scaffold, config, auth, limiter, server shell

**Files:**
- Create: `offline/go.mod`, `offline/adapter/config.go`, `offline/adapter/auth.go`, `offline/adapter/limiter.go`, `offline/adapter/backend.go`, `offline/adapter/server.go`, `offline/cmd/offline-adapter/main.go`
- Test: `offline/adapter/config_test.go`, `offline/adapter/auth_test.go`, `offline/adapter/limiter_test.go`, `offline/adapter/server_test.go`
- Modify: `go.work`, `Makefile` (MODULES line), `docs/superpowers/specs/2026-10-15-offline-speech-service-design.md`

**Interfaces:**
- Produces:
  - `type Config struct { Listen, Token, SpeachesURL, STTModel, TTSModel, DefaultVoice string; VoiceAliases map[string]string; MaxConcurrency int; QueueWait, InterimEvery, EndpointSilence, MaxUtterance time.Duration; EnergyThreshold float64 }`
  - `func LoadConfig(getenv func(string) string) (Config, error)`
  - `func (c Config) Voice(name string) string`
  - `func requireToken(token string, next http.Handler) http.Handler`
  - `func writeError(w http.ResponseWriter, status int, msg string)`
  - `type Limiter`, `func NewLimiter(n int, wait time.Duration) *Limiter`, `func (l *Limiter) Acquire(ctx context.Context) (release func(), err error)`, `var ErrBusy`
  - `type TTSBackend interface { Synthesize(ctx context.Context, text, voice string, sampleRate int) (io.ReadCloser, error) }`
  - `type STTBackend interface { Transcribe(ctx context.Context, pcm []byte, sampleRate int, language string) (string, error) }`
  - `type BackendError struct { Status int; Msg string }` with `Error()`; `func httpStatusFor(err error) int` (400 for a backend 4xx, 503 for `ErrBusy`, 502 otherwise)
  - `type Server`, `func New(cfg Config, tts TTSBackend, stt STTBackend, ready func(context.Context) error) *Server`, `func (s *Server) Handler() http.Handler`

- [ ] **Step 1: Correct the spec**

In `docs/superpowers/specs/2026-10-15-offline-speech-service-design.md` replace every statement that the adapter reuses `vad/` (the "Why Go" paragraph, the STT components row, and STT item 1) with: "A pure-Go RMS-energy endpointer (`adapter/endpoint.go`, threshold `OFFLINE_ENERGY_THRESHOLD`, default 0.01 of full scale). `vad/` was considered and rejected: it is cgo + onnxruntime behind worker processes. Ceiling: a noisy microphone needs a real VAD; the upgrade path is `vad/` or sherpa-onnx." Also in "Wire contract > Components" nothing else changes.

- [ ] **Step 2: Write the failing tests**

`offline/adapter/config_test.go`:

```go
package adapter

import (
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestLoadConfigDefaults(t *testing.T) {
	c, err := LoadConfig(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8099" || c.SpeachesURL != "http://127.0.0.1:8000" || c.MaxConcurrency != 2 ||
		c.InterimEvery != 700*time.Millisecond || c.EndpointSilence != 600*time.Millisecond ||
		c.MaxUtterance != 25*time.Second || c.EnergyThreshold != 0.01 {
		t.Fatalf("defaults: %+v", c)
	}
	if c.Voice("") != "af_heart" || c.Voice("eve") != "af_heart" || c.Voice("af_bella") != "af_bella" {
		t.Fatalf("voice resolution: %q %q %q", c.Voice(""), c.Voice("eve"), c.Voice("af_bella"))
	}
}

func TestLoadConfigOverridesAndAliases(t *testing.T) {
	c, err := LoadConfig(env(map[string]string{
		"OFFLINE_LISTEN": ":9000", "OFFLINE_TOKEN": "s3", "OFFLINE_MAX_CONCURRENCY": "4",
		"OFFLINE_INTERIM_EVERY": "1s", "OFFLINE_VOICE_ALIASES": "eve=af_bella, ara=am_adam",
		"OFFLINE_ENERGY_THRESHOLD": "0.02",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":9000" || c.Token != "s3" || c.MaxConcurrency != 4 || c.InterimEvery != time.Second ||
		c.EnergyThreshold != 0.02 || c.Voice("eve") != "af_bella" || c.Voice("ara") != "am_adam" {
		t.Fatalf("overrides: %+v", c)
	}
}

func TestLoadConfigRejectsBadValues(t *testing.T) {
	for k, v := range map[string]string{
		"OFFLINE_INTERIM_EVERY": "soon", "OFFLINE_MAX_CONCURRENCY": "0", "OFFLINE_ENERGY_THRESHOLD": "-1",
		"OFFLINE_VOICE_ALIASES": "noequals",
	} {
		if _, err := LoadConfig(env(map[string]string{k: v})); err == nil {
			t.Errorf("%s=%q accepted", k, v)
		}
	}
}
```

`offline/adapter/auth_test.go`:

```go
package adapter

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequireToken(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	for _, tc := range []struct {
		token, header string
		want          int
	}{
		{"", "", 204}, {"", "Bearer anything", 204}, // no token configured: any request passes
		{"s3", "Bearer s3", 204}, {"s3", "Bearer nope", 401}, {"s3", "", 401},
	} {
		r := httptest.NewRequest("GET", "/v1/stt", nil)
		if tc.header != "" {
			r.Header.Set("Authorization", tc.header)
		}
		w := httptest.NewRecorder()
		requireToken(tc.token, ok).ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("token=%q header=%q: got %d want %d", tc.token, tc.header, w.Code, tc.want)
		}
	}
}
```

`offline/adapter/limiter_test.go`:

```go
package adapter

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestLimiterBoundsConcurrencyAndTimesOut(t *testing.T) {
	l := NewLimiter(1, 20*time.Millisecond)
	rel, err := l.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Acquire(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatalf("second acquire: %v, want ErrBusy", err)
	}
	rel()
	rel2, err := l.Acquire(context.Background())
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	rel2()
}

func TestLimiterHonoursCancel(t *testing.T) {
	l := NewLimiter(1, time.Minute)
	rel, _ := l.Acquire(context.Background())
	defer rel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := l.Acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}
```

`offline/adapter/server_test.go`:

```go
package adapter

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthzReflectsBackend(t *testing.T) {
	var down error
	s := New(mustConfig(t), nil, nil, func(context.Context) error { return down })
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	get := func() int {
		resp, err := http.Get(srv.URL + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := get(); got != 200 {
		t.Fatalf("healthy: %d", got)
	}
	down = errors.New("speaches down")
	if got := get(); got != 503 {
		t.Fatalf("backend down: %d", got)
	}
}

func TestHealthzIsOpenButV1RequiresToken(t *testing.T) {
	cfg := mustConfig(t)
	cfg.Token = "s3"
	srv := httptest.NewServer(New(cfg, nil, nil, func(context.Context) error { return nil }).Handler())
	defer srv.Close()
	for path, want := range map[string]int{"/healthz": 200, "/v1/tts": 401, "/v1/stt": 401} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("%s: got %d want %d", path, resp.StatusCode, want)
		}
	}
}
```

`offline/adapter/helpers_test.go`:

```go
package adapter

import "testing"

func mustConfig(t *testing.T) Config {
	t.Helper()
	c, err := LoadConfig(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	return c
}
```

- [ ] **Step 3: Create the module and run the tests to see them fail**

`offline/go.mod`:

```
module govoice/offline

go 1.27.1

require github.com/coder/websocket v1.8.15
```

Add `./offline` to `go.work` (keep the list alphabetical-ish like the rest) and `offline` to the `MODULES :=` line in the `Makefile`. Then:

Run: `cd offline && go mod tidy && go test ./... 2>&1 | head`
Expected: FAIL (compile errors: `LoadConfig`, `requireToken`, `NewLimiter`, `New` undefined).

- [ ] **Step 4: Implement**

`offline/adapter/config.go`:

```go
package adapter

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Config is read from the environment (LoadConfig); every field has a default that works with the
// compose file.
type Config struct {
	Listen          string            // OFFLINE_LISTEN
	Token           string            // OFFLINE_TOKEN (empty: any bearer token is accepted)
	SpeachesURL     string            // OFFLINE_SPEACHES_URL
	STTModel        string            // OFFLINE_STT_MODEL
	TTSModel        string            // OFFLINE_TTS_MODEL
	DefaultVoice    string            // OFFLINE_DEFAULT_VOICE
	VoiceAliases    map[string]string // OFFLINE_VOICE_ALIASES "eve=af_heart,ara=af_bella"
	MaxConcurrency  int               // OFFLINE_MAX_CONCURRENCY: simultaneous Speaches requests
	QueueWait       time.Duration     // OFFLINE_QUEUE_WAIT: how long a request waits for a slot
	InterimEvery    time.Duration     // OFFLINE_INTERIM_EVERY
	EndpointSilence time.Duration     // OFFLINE_ENDPOINT_SILENCE
	MaxUtterance    time.Duration     // OFFLINE_MAX_UTTERANCE
	EnergyThreshold float64           // OFFLINE_ENERGY_THRESHOLD: RMS of full scale that counts as speech
}

func LoadConfig(getenv func(string) string) (Config, error) {
	c := Config{
		Listen: ":8099", SpeachesURL: "http://127.0.0.1:8000",
		STTModel: "Systran/faster-whisper-small.en", TTSModel: "speaches-ai/Kokoro-82M-v1.0-ONNX",
		DefaultVoice: "af_heart", VoiceAliases: map[string]string{"eve": "af_heart"},
		MaxConcurrency: 2, QueueWait: 5 * time.Second, InterimEvery: 700 * time.Millisecond,
		EndpointSilence: 600 * time.Millisecond, MaxUtterance: 25 * time.Second, EnergyThreshold: 0.01,
	}
	var errs []error
	str := func(key string, dst *string) {
		if v := getenv(key); v != "" {
			*dst = v
		}
	}
	dur := func(key string, dst *time.Duration) {
		if v := getenv(key); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil || d <= 0 {
				errs = append(errs, fmt.Errorf("%s: want a positive duration, got %q", key, v))
				return
			}
			*dst = d
		}
	}
	str("OFFLINE_LISTEN", &c.Listen)
	str("OFFLINE_TOKEN", &c.Token)
	str("OFFLINE_SPEACHES_URL", &c.SpeachesURL)
	str("OFFLINE_STT_MODEL", &c.STTModel)
	str("OFFLINE_TTS_MODEL", &c.TTSModel)
	str("OFFLINE_DEFAULT_VOICE", &c.DefaultVoice)
	dur("OFFLINE_QUEUE_WAIT", &c.QueueWait)
	dur("OFFLINE_INTERIM_EVERY", &c.InterimEvery)
	dur("OFFLINE_ENDPOINT_SILENCE", &c.EndpointSilence)
	dur("OFFLINE_MAX_UTTERANCE", &c.MaxUtterance)
	if v := getenv("OFFLINE_MAX_CONCURRENCY"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			errs = append(errs, fmt.Errorf("OFFLINE_MAX_CONCURRENCY: want an integer >= 1, got %q", v))
		} else {
			c.MaxConcurrency = n
		}
	}
	if v := getenv("OFFLINE_ENERGY_THRESHOLD"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f <= 0 || f >= 1 {
			errs = append(errs, fmt.Errorf("OFFLINE_ENERGY_THRESHOLD: want a number in (0,1), got %q", v))
		} else {
			c.EnergyThreshold = f
		}
	}
	if v := getenv("OFFLINE_VOICE_ALIASES"); v != "" {
		c.VoiceAliases = map[string]string{}
		for _, pair := range strings.Split(v, ",") {
			k, val, ok := strings.Cut(strings.TrimSpace(pair), "=")
			if !ok || k == "" || val == "" {
				errs = append(errs, fmt.Errorf("OFFLINE_VOICE_ALIASES: want name=voice pairs, got %q", pair))
				continue
			}
			c.VoiceAliases[strings.TrimSpace(k)] = strings.TrimSpace(val)
		}
	}
	return c, errors.Join(errs...)
}

// Voice maps a requested voice to a Speaches voice: empty is the default, an alias is replaced
// (xAI names such as "eve"), anything else passes through and Speaches rejects an unknown one.
func (c Config) Voice(name string) string {
	if name == "" {
		return c.DefaultVoice
	}
	if v, ok := c.VoiceAliases[name]; ok {
		return v
	}
	return name
}
```

`offline/adapter/auth.go`:

```go
package adapter

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
)

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": msg}})
}

// requireToken guards a handler with a bearer token. An empty token disables the check: the
// service is a local test fixture and the real xai adapters always send some token.
func requireToken(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			writeError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		next.ServeHTTP(w, r)
	})
}
```

`offline/adapter/limiter.go`:

```go
package adapter

import (
	"context"
	"errors"
	"time"
)

// ErrBusy means no backend slot freed up within the queue deadline.
var ErrBusy = errors.New("adapter busy: no backend slot within the queue deadline")

// Limiter bounds simultaneous backend requests. Waiting is bounded by wait, so a burst cannot
// build an unbounded queue of goroutines holding audio.
type Limiter struct {
	slots chan struct{}
	wait  time.Duration
}

func NewLimiter(n int, wait time.Duration) *Limiter {
	return &Limiter{slots: make(chan struct{}, n), wait: wait}
}

func (l *Limiter) Acquire(ctx context.Context) (release func(), err error) {
	t := time.NewTimer(l.wait)
	defer t.Stop()
	select {
	case l.slots <- struct{}{}:
		return func() { <-l.slots }, nil
	case <-t.C:
		return nil, ErrBusy
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
```

`offline/adapter/backend.go`:

```go
package adapter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// TTSBackend returns raw PCM16LE mono at sampleRate; the caller closes the reader.
type TTSBackend interface {
	Synthesize(ctx context.Context, text, voice string, sampleRate int) (io.ReadCloser, error)
}

// STTBackend transcribes one complete utterance of PCM16LE mono.
type STTBackend interface {
	Transcribe(ctx context.Context, pcm []byte, sampleRate int, language string) (string, error)
}

// BackendError is a non-200 answer from a model server.
type BackendError struct {
	Status int
	Msg    string
}

func (e *BackendError) Error() string { return fmt.Sprintf("backend status %d: %s", e.Status, e.Msg) }

// httpStatusFor maps a backend failure to what the xAI adapters expect: a backend 4xx (unknown
// voice) is a client error (400), a full queue is 503, anything else is retryable (502).
func httpStatusFor(err error) int {
	var be *BackendError
	switch {
	case errors.Is(err, ErrBusy):
		return http.StatusServiceUnavailable
	case errors.As(err, &be) && be.Status >= 400 && be.Status < 500:
		return http.StatusBadRequest
	default:
		return http.StatusBadGateway
	}
}
```

`offline/adapter/server.go`:

```go
package adapter

import (
	"context"
	"net/http"
	"time"
)

// Server serves the xAI-compatible STT and TTS endpoints on top of two model backends.
type Server struct {
	cfg   Config
	tts   TTSBackend
	stt   STTBackend
	ready func(context.Context) error
	lim   *Limiter
}

func New(cfg Config, tts TTSBackend, stt STTBackend, ready func(context.Context) error) *Server {
	return &Server{cfg: cfg, tts: tts, stt: stt, ready: ready, lim: NewLimiter(cfg.MaxConcurrency, cfg.QueueWait)}
}

func (s *Server) Handler() http.Handler {
	authed := http.NewServeMux()
	authed.HandleFunc("POST /v1/tts", s.handleTTSPost)
	authed.HandleFunc("GET /v1/tts", s.handleTTSWS)
	authed.HandleFunc("GET /v1/stt", s.handleSTT)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.Handle("/v1/", requireToken(s.cfg.Token, authed))
	return mux
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := s.ready(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}
```

The three `handle*` methods do not exist yet. Add temporary stubs at the bottom of `server.go` so the package compiles, and delete each stub in the task that implements it:

```go
func (s *Server) handleTTSPost(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }
func (s *Server) handleTTSWS(w http.ResponseWriter, r *http.Request)   { http.NotFound(w, r) }
func (s *Server) handleSTT(w http.ResponseWriter, r *http.Request)     { http.NotFound(w, r) }
```

`offline/cmd/offline-adapter/main.go`:

```go
// Command offline-adapter serves the xAI STT/TTS wire protocols on top of a local Speaches server.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"govoice/offline/adapter"
)

func main() {
	healthcheck := flag.Bool("healthcheck", false, "probe /healthz on the configured port and exit (for container healthchecks)")
	flag.Parse()
	cfg, err := adapter.LoadConfig(os.Getenv)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if *healthcheck {
		os.Exit(probe(cfg.Listen))
	}
	sp := adapter.NewSpeaches(cfg)
	srv := &http.Server{Addr: cfg.Listen, Handler: adapter.New(cfg, sp, sp, sp.Health).Handler(), ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Printf("offline-adapter listening on %s, backend %s", cfg.Listen, cfg.SpeachesURL)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// probe is the container healthcheck: the image always listens on ":PORT".
func probe(listen string) int {
	resp, err := http.Get("http://127.0.0.1" + listen + "/healthz")
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
```

`adapter.NewSpeaches` and `Speaches.Health` arrive in Task 2; until then build with `go vet ./adapter/...` only.

- [ ] **Step 5: Run the tests**

Run: `cd offline && go test -race ./adapter/...`
Expected: PASS (config, auth, limiter, server tests).

- [ ] **Step 6: Commit**

```bash
git add offline go.work Makefile docs/superpowers/specs/2026-10-15-offline-speech-service-design.md
git commit -m "feat(offline): adapter module scaffold with config, auth, limiter and healthz"
```

---

### Task 2: Speaches client (TTS, STT, health)

**Files:**
- Create: `offline/adapter/speaches.go`
- Test: `offline/adapter/speaches_test.go`

**Interfaces:**
- Consumes: `Config`, `TTSBackend`, `STTBackend`, `BackendError` (Task 1).
- Produces: `type Speaches struct{...}`, `func NewSpeaches(cfg Config) *Speaches`, methods `Synthesize`, `Transcribe`, `Health(ctx) error` satisfying the two interfaces; `func wavHeader(dataLen, sampleRate int) []byte` (44-byte PCM16 mono WAV header).

- [ ] **Step 1: Write the failing tests**

```go
package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func speachesFor(t *testing.T, h http.HandlerFunc) *Speaches {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	cfg := mustConfig(t)
	cfg.SpeachesURL = srv.URL
	return NewSpeaches(cfg)
}

func TestSpeachesSynthesizeRequestAndBody(t *testing.T) {
	var got map[string]any
	sp := speachesFor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/speech" || r.Method != "POST" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte{1, 2, 3, 4})
	})
	rc, err := sp.Synthesize(context.Background(), "hello", "af_heart", 24000)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	if len(b) != 4 || got["input"] != "hello" || got["voice"] != "af_heart" || got["response_format"] != "pcm" ||
		got["sample_rate"] != float64(24000) || got["model"] != mustConfig(t).TTSModel {
		t.Fatalf("body=%v request=%v", b, got)
	}
}

func TestSpeachesSynthesizeBackendError(t *testing.T) {
	sp := speachesFor(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no such voice", 404) })
	_, err := sp.Synthesize(context.Background(), "x", "nope", 24000)
	var be *BackendError
	if !errors.As(err, &be) || be.Status != 404 || !strings.Contains(be.Msg, "no such voice") {
		t.Fatalf("got %v", err)
	}
}

func TestSpeachesTranscribeSendsWavAndVadFilter(t *testing.T) {
	pcm := make([]byte, 3200)
	sp := speachesFor(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			t.Fatal(err)
		}
		wav, _ := io.ReadAll(f)
		if len(wav) != 44+len(pcm) || string(wav[:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
			t.Errorf("wav: len=%d head=%q", len(wav), wav[:12])
		}
		if r.FormValue("vad_filter") != "true" || r.FormValue("language") != "en" || r.FormValue("model") != mustConfig(t).STTModel {
			t.Errorf("form: %v", r.MultipartForm.Value)
		}
		_, _ = w.Write([]byte(`{"text":" hello there "}`))
	})
	text, err := sp.Transcribe(context.Background(), pcm, 16000, "en")
	if err != nil || text != "hello there" {
		t.Fatalf("text=%q err=%v", text, err)
	}
}

func TestSpeachesHealth(t *testing.T) {
	status := 200
	sp := speachesFor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("path %s", r.URL.Path)
		}
		w.WriteHeader(status)
	})
	if err := sp.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	status = 500
	if err := sp.Health(context.Background()); err == nil {
		t.Fatal("500 reported healthy")
	}
}

func TestWavHeaderFields(t *testing.T) {
	h := wavHeader(100, 16000)
	if len(h) != 44 || string(h[:4]) != "RIFF" || string(h[36:40]) != "data" {
		t.Fatalf("header: %q", h)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `cd offline && go test ./adapter/ -run 'Speaches|Wav' 2>&1 | head`
Expected: FAIL (`NewSpeaches` undefined).

- [ ] **Step 3: Implement `offline/adapter/speaches.go`**

```go
package adapter

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
)

// Speaches is the HTTP client for a Speaches server (OpenAI-compatible faster-whisper + Kokoro).
type Speaches struct {
	base     string
	sttModel string
	ttsModel string
	hc       *http.Client // no overall timeout: TTS bodies are read as a stream; callers pass a ctx
}

func NewSpeaches(cfg Config) *Speaches {
	return &Speaches{base: strings.TrimRight(cfg.SpeachesURL, "/"), sttModel: cfg.STTModel, ttsModel: cfg.TTSModel, hc: &http.Client{}}
}

func (s *Speaches) Synthesize(ctx context.Context, text, voice string, sampleRate int) (io.ReadCloser, error) {
	body, err := json.Marshal(map[string]any{
		"model": s.ttsModel, "input": text, "voice": voice, "response_format": "pcm", "sample_rate": sampleRate,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal speech request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base+"/v1/audio/speech", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build speech request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("speech request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, &BackendError{Status: resp.StatusCode, Msg: strings.TrimSpace(string(msg))}
	}
	return resp.Body, nil
}

func (s *Speaches) Transcribe(ctx context.Context, pcm []byte, sampleRate int, language string) (string, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "audio.wav")
	if err != nil {
		return "", fmt.Errorf("build transcription form: %w", err)
	}
	_, _ = fw.Write(wavHeader(len(pcm), sampleRate))
	_, _ = fw.Write(pcm)
	fields := map[string]string{"model": s.sttModel, "response_format": "json", "vad_filter": "true"}
	if language != "" {
		fields["language"] = language
	}
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			return "", fmt.Errorf("build transcription form: %w", err)
		}
	}
	if err := mw.Close(); err != nil {
		return "", fmt.Errorf("build transcription form: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base+"/v1/audio/transcriptions", &body)
	if err != nil {
		return "", fmt.Errorf("build transcription request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := s.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("transcription request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", &BackendError{Status: resp.StatusCode, Msg: strings.TrimSpace(string(msg))}
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", fmt.Errorf("decode transcription: %w", err)
	}
	return strings.TrimSpace(out.Text), nil
}

// Health reports whether Speaches answers; models are loaded lazily by Speaches itself.
func (s *Speaches) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"/health", nil)
	if err != nil {
		return fmt.Errorf("build health request: %w", err)
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return fmt.Errorf("speaches health: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("speaches health: status %d", resp.StatusCode)
	}
	return nil
}

// wavHeader is the 44-byte RIFF header for PCM16 mono of dataLen bytes.
func wavHeader(dataLen, sampleRate int) []byte {
	h := make([]byte, 44)
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], uint32(36+dataLen))
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1) // PCM
	binary.LittleEndian.PutUint16(h[22:], 1) // mono
	binary.LittleEndian.PutUint32(h[24:], uint32(sampleRate))
	binary.LittleEndian.PutUint32(h[28:], uint32(sampleRate*2))
	binary.LittleEndian.PutUint16(h[32:], 2)  // block align
	binary.LittleEndian.PutUint16(h[34:], 16) // bits per sample
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], uint32(dataLen))
	return h
}
```

- [ ] **Step 4: Run tests, then build the binary**

Run: `cd offline && go test -race ./adapter/... && go build ./...`
Expected: PASS and a clean build (main.go now compiles).

- [ ] **Step 5: Commit**

```bash
git add offline
git commit -m "feat(offline): Speaches client for TTS, STT and health"
```

---

### Task 3: TTS unary endpoint

**Files:**
- Create: `offline/adapter/tts.go`
- Modify: `offline/adapter/server.go` (delete the `handleTTSPost` stub)
- Test: `offline/adapter/tts_test.go`, `offline/adapter/helpers_test.go` (add fakes)

**Interfaces:**
- Consumes: `Server`, `TTSBackend`, `BackendError`, `httpStatusFor`, `Limiter`, `writeError`, `Config.Voice` (Task 1).
- Produces: `func (s *Server) handleTTSPost`; `func streamPCM(r io.Reader, chunk int, send func([]byte) error) error` (reads `r` in `chunk`-byte pieces, drops a trailing odd byte); `func queryInt(r *http.Request, key string, def int) int`.

- [ ] **Step 1: Add the fakes to `helpers_test.go`**

```go
package adapter

import (
	"bytes"
	"context"
	"io"
	"sync/atomic"
	"testing"
)

func mustConfig(t *testing.T) Config {
	t.Helper()
	c, err := LoadConfig(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// fakeTTS returns pcm for any text; it records the voice and reports cancellation of the request.
type fakeTTS struct {
	pcm       []byte
	err       error
	voice     atomic.Value // string
	gotText   atomic.Value // string
	cancelled chan struct{}
	block     chan struct{} // when non-nil, Synthesize blocks until ctx is cancelled
}

func (f *fakeTTS) Synthesize(ctx context.Context, text, voice string, _ int) (io.ReadCloser, error) {
	f.voice.Store(voice)
	f.gotText.Store(text)
	if f.block != nil {
		<-ctx.Done()
		if f.cancelled != nil {
			close(f.cancelled)
		}
		return nil, ctx.Err()
	}
	if f.err != nil {
		return nil, f.err
	}
	return io.NopCloser(bytes.NewReader(f.pcm)), nil
}

// fakeSTT calls fn for every transcription.
type fakeSTT struct {
	fn    func(ctx context.Context, pcm []byte) (string, error)
	calls atomic.Int32
}

func (f *fakeSTT) Transcribe(ctx context.Context, pcm []byte, _ int, _ string) (string, error) {
	f.calls.Add(1)
	return f.fn(ctx, pcm)
}

func okReady(context.Context) error { return nil }
```

(Replace the earlier `mustConfig` in this file; `package adapter` imports must be merged.)

- [ ] **Step 2: Write the failing tests `offline/adapter/tts_test.go`**

```go
package adapter

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func postTTS(t *testing.T, srv *httptest.Server, body string) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.Post(srv.URL+"/v1/tts", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func TestTTSPostReturnsPCMAndMapsVoice(t *testing.T) {
	ft := &fakeTTS{pcm: bytes.Repeat([]byte{7, 0}, 100)}
	srv := httptest.NewServer(New(mustConfig(t), ft, nil, okReady).Handler())
	defer srv.Close()
	resp, b := postTTS(t, srv, `{"text":"hi","voice_id":"eve","language":"en","output_format":{"codec":"pcm","sample_rate":24000}}`)
	if resp.StatusCode != 200 || len(b) != 200 || ft.voice.Load() != "af_heart" {
		t.Fatalf("status=%d len=%d voice=%v", resp.StatusCode, len(b), ft.voice.Load())
	}
}

func TestTTSPostBackendErrors(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{&BackendError{Status: 404, Msg: "no voice"}, 400},
		{&BackendError{Status: 500, Msg: "boom"}, 502},
		{ErrBusy, 503},
		{errors.New("connection refused"), 502},
	} {
		srv := httptest.NewServer(New(mustConfig(t), &fakeTTS{err: tc.err}, nil, okReady).Handler())
		resp, _ := postTTS(t, srv, `{"text":"hi","voice_id":"x"}`)
		srv.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("%v: got %d want %d", tc.err, resp.StatusCode, tc.want)
		}
	}
}

func TestTTSPostRejectsBadJSONAndEmptyText(t *testing.T) {
	srv := httptest.NewServer(New(mustConfig(t), &fakeTTS{}, nil, okReady).Handler())
	defer srv.Close()
	for _, body := range []string{`{`, `{"text":""}`} {
		if resp, _ := postTTS(t, srv, body); resp.StatusCode != 400 {
			t.Errorf("%q: got %d want 400", body, resp.StatusCode)
		}
	}
}

func TestStreamPCMDropsTrailingOddByte(t *testing.T) {
	var got [][]byte
	err := streamPCM(bytes.NewReader(make([]byte, 9)), 4, func(b []byte) error {
		got = append(got, append([]byte(nil), b...))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, b := range got {
		if len(b)%2 != 0 {
			t.Fatalf("odd chunk %d", len(b))
		}
		total += len(b)
	}
	if total != 8 {
		t.Fatalf("total %d, want 8 (one odd byte dropped)", total)
	}
}

func TestStreamPCMStopsOnSendError(t *testing.T) {
	boom := errors.New("write failed")
	err := streamPCM(bytes.NewReader(make([]byte, 100)), 10, func([]byte) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
}
```

- [ ] **Step 3: Run to verify failure**

Run: `cd offline && go test ./adapter/ -run 'TTSPost|StreamPCM' 2>&1 | head`
Expected: FAIL (`streamPCM` undefined; POST returns 404 from the stub).

- [ ] **Step 4: Implement `offline/adapter/tts.go` (unary part) and delete the `handleTTSPost` stub**

```go
package adapter

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
)

const maxTTSBody = 1 << 20 // a sentence unit is a few hundred bytes; 1 MiB is far beyond any real request

func queryInt(r *http.Request, key string, def int) int {
	if n, err := strconv.Atoi(r.URL.Query().Get(key)); err == nil && n > 0 {
		return n
	}
	return def
}

// handleTTSPost is the unary fallback the xAI adapter uses when the stream fails before audio.
func (s *Server) handleTTSPost(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text         string `json:"text"`
		VoiceID      string `json:"voice_id"`
		OutputFormat struct {
			SampleRate int `json:"sample_rate"`
		} `json:"output_format"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxTTSBody)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		writeError(w, http.StatusBadRequest, "text is required")
		return
	}
	rate := req.OutputFormat.SampleRate
	if rate <= 0 {
		rate = 24000
	}
	release, err := s.lim.Acquire(r.Context())
	if err != nil {
		writeError(w, httpStatusFor(err), err.Error())
		return
	}
	defer release()
	body, err := s.tts.Synthesize(r.Context(), req.Text, s.cfg.Voice(req.VoiceID), rate)
	if err != nil {
		writeError(w, httpStatusFor(err), err.Error())
		return
	}
	defer body.Close()
	w.Header().Set("Content-Type", "audio/pcm")
	// The status is already sent once the copy starts; a mid-body failure surfaces to the xAI client
	// as a truncated body, which it treats as a retryable transport error.
	_ = streamPCM(body, 9600, func(b []byte) error { _, werr := w.Write(b); return werr })
}

// streamPCM reads r in chunk-byte pieces and hands each to send. Every piece has an even length:
// PCM16 backends do not send an odd total, so a trailing odd byte is dropped.
func streamPCM(r io.Reader, chunk int, send func([]byte) error) error {
	buf := make([]byte, chunk&^1)
	for {
		n, err := io.ReadFull(r, buf)
		n &^= 1
		if n > 0 {
			if serr := send(buf[:n]); serr != nil {
				return serr
			}
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
```

- [ ] **Step 5: Run the tests**

Run: `cd offline && go test -race ./adapter/...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add offline
git commit -m "feat(offline): unary TTS endpoint with voice aliases and backend error mapping"
```

---

### Task 4: TTS WebSocket endpoint

**Files:**
- Modify: `offline/adapter/tts.go` (add `handleTTSWS`), `offline/adapter/server.go` (delete the `handleTTSWS` stub)
- Test: `offline/adapter/tts_ws_test.go`

**Interfaces:**
- Consumes: Task 3's `streamPCM`, `queryInt`; Task 1's `Server`, `Limiter`, `Config.Voice`.
- Produces: `func (s *Server) handleTTSWS`; `type ttsMsg struct { Type, Delta, Message string }` (JSON `type`, `delta`, `message`, omitempty); `func writeJSON(ctx context.Context, c *websocket.Conn, v any) error`; const `maxTTSText = 60000` (the xAI per-message text limit).

- [ ] **Step 1: Write the failing tests `offline/adapter/tts_ws_test.go`**

```go
package adapter

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"net/http/httptest"
)

func dialWS(t *testing.T, srv *httptest.Server, path string) (*websocket.Conn, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.CloseNow() })
	return c, ctx
}

func sendJSON(t *testing.T, ctx context.Context, c *websocket.Conn, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	if err := c.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}

// readUnit reads audio.delta messages until audio.done (or error) and returns the decoded audio.
func readUnit(t *testing.T, ctx context.Context, c *websocket.Conn) ([]byte, ttsMsg) {
	t.Helper()
	var audio []byte
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var m ttsMsg
		_ = json.Unmarshal(data, &m)
		switch m.Type {
		case "audio.delta":
			b, err := base64.StdEncoding.DecodeString(m.Delta)
			if err != nil || len(b)%2 != 0 {
				t.Fatalf("bad audio.delta: err=%v len=%d", err, len(b))
			}
			audio = append(audio, b...)
		case "audio.done", "error":
			return audio, m
		}
	}
}

func TestTTSWSServesTwoUnitsOnOneConnection(t *testing.T) {
	ft := &fakeTTS{pcm: bytes.Repeat([]byte{3, 0}, 12000)} // 24000 bytes: several 100 ms chunks
	srv := httptest.NewServer(New(mustConfig(t), ft, nil, okReady).Handler())
	defer srv.Close()
	c, ctx := dialWS(t, srv, "/v1/tts?voice=eve&language=en&codec=pcm&sample_rate=24000")
	for i, text := range []string{"First sentence.", "Second sentence."} {
		sendJSON(t, ctx, c, ttsMsg{Type: "text.delta", Delta: text})
		sendJSON(t, ctx, c, ttsMsg{Type: "text.done"})
		audio, end := readUnit(t, ctx, c)
		if end.Type != "audio.done" || len(audio) != 24000 {
			t.Fatalf("unit %d: end=%s audio=%d", i, end.Type, len(audio))
		}
		if got := ft.gotText.Load(); got != text {
			t.Fatalf("unit %d: backend got %q", i, got)
		}
	}
	if ft.voice.Load() != "af_heart" {
		t.Fatalf("voice %v", ft.voice.Load())
	}
}

func TestTTSWSEmptyUnitAndTextCap(t *testing.T) {
	srv := httptest.NewServer(New(mustConfig(t), &fakeTTS{pcm: []byte{1, 0}}, nil, okReady).Handler())
	defer srv.Close()
	c, ctx := dialWS(t, srv, "/v1/tts")
	sendJSON(t, ctx, c, ttsMsg{Type: "text.done"})
	if _, end := readUnit(t, ctx, c); end.Type != "audio.done" {
		t.Fatalf("empty unit: %s", end.Type)
	}
	sendJSON(t, ctx, c, ttsMsg{Type: "text.delta", Delta: strings.Repeat("a", maxTTSText+1)})
	if _, end := readUnit(t, ctx, c); end.Type != "error" {
		t.Fatalf("oversized text: %s", end.Type)
	}
}

func TestTTSWSBackendErrorSendsError(t *testing.T) {
	srv := httptest.NewServer(New(mustConfig(t), &fakeTTS{err: &BackendError{Status: 404, Msg: "no voice"}}, nil, okReady).Handler())
	defer srv.Close()
	c, ctx := dialWS(t, srv, "/v1/tts?voice=brokenvoice")
	sendJSON(t, ctx, c, ttsMsg{Type: "text.delta", Delta: "hi"})
	sendJSON(t, ctx, c, ttsMsg{Type: "text.done"})
	if _, end := readUnit(t, ctx, c); end.Type != "error" || end.Message == "" {
		t.Fatalf("got %+v", end)
	}
}

func TestTTSWSCancelsSynthesisOnClose(t *testing.T) {
	ft := &fakeTTS{block: make(chan struct{}), cancelled: make(chan struct{})}
	srv := httptest.NewServer(New(mustConfig(t), ft, nil, okReady).Handler())
	defer srv.Close()
	c, ctx := dialWS(t, srv, "/v1/tts")
	sendJSON(t, ctx, c, ttsMsg{Type: "text.delta", Delta: "a long sentence"})
	sendJSON(t, ctx, c, ttsMsg{Type: "text.done"})
	time.Sleep(50 * time.Millisecond) // let the handler reach Synthesize
	c.CloseNow()                      // barge-in: the client just hangs up
	select {
	case <-ft.cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("backend request was not cancelled after the client closed")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `cd offline && go test ./adapter/ -run TTSWS 2>&1 | head`
Expected: FAIL (`ttsMsg`, `maxTTSText` undefined).

- [ ] **Step 3: Implement in `offline/adapter/tts.go` and delete the `handleTTSWS` stub**

Add imports `context`, `encoding/base64`, `github.com/coder/websocket`, then:

```go
const maxTTSText = 60000 // xAI's documented limit for one text.delta message

type ttsMsg struct {
	Type    string `json:"type"`
	Delta   string `json:"delta,omitempty"`
	Message string `json:"message,omitempty"`
}

func writeJSON(ctx context.Context, c *websocket.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.Write(ctx, websocket.MessageText, b)
}

// handleTTSWS serves xAI's streaming TTS: one connection per reply, reused for its units. A unit is
// text.delta... then text.done; the answer is audio.delta... then audio.done, and the connection
// stays open for the next unit. Closing the socket (barge-in) cancels the running synthesis.
func (s *Server) handleTTSWS(w http.ResponseWriter, r *http.Request) {
	voice := s.cfg.Voice(r.URL.Query().Get("voice"))
	rate := queryInt(r, "sample_rate", 24000)
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()
	c.SetReadLimit(2 << 20)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// One reader goroutine: it is the only place a close is noticed while a unit is synthesizing.
	// It owns msgs (producer-only close) and cancels ctx when the socket dies.
	msgs := make(chan ttsMsg, 4)
	go func() {
		defer close(msgs)
		defer cancel()
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			var m ttsMsg
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			select {
			case msgs <- m:
			case <-ctx.Done():
				return
			}
		}
	}()

	var text strings.Builder
	fail := func(msg string) { _ = writeJSON(ctx, c, ttsMsg{Type: "error", Message: msg}) }
	for m := range msgs {
		switch m.Type {
		case "text.delta":
			if text.Len()+len(m.Delta) > maxTTSText {
				fail("text exceeds the per-unit limit")
				return
			}
			text.WriteString(m.Delta)
		case "text.done":
			unit := text.String()
			text.Reset()
			if err := s.speak(ctx, c, unit, voice, rate); err != nil {
				if ctx.Err() == nil {
					fail(err.Error())
				}
				return
			}
		}
	}
}

func (s *Server) speak(ctx context.Context, c *websocket.Conn, text, voice string, rate int) error {
	if strings.TrimSpace(text) == "" {
		return writeJSON(ctx, c, ttsMsg{Type: "audio.done"})
	}
	release, err := s.lim.Acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	body, err := s.tts.Synthesize(ctx, text, voice, rate)
	if err != nil {
		return err
	}
	defer body.Close()
	// 100 ms of PCM16 mono per audio.delta.
	err = streamPCM(body, rate*2/10, func(b []byte) error {
		return writeJSON(ctx, c, ttsMsg{Type: "audio.delta", Delta: base64.StdEncoding.EncodeToString(b)})
	})
	if err != nil {
		return err
	}
	return writeJSON(ctx, c, ttsMsg{Type: "audio.done"})
}
```

- [ ] **Step 4: Run the tests**

Run: `cd offline && go test -race -count=3 ./adapter/...`
Expected: PASS, repeated three times to flush out timing flakes.

- [ ] **Step 5: Commit**

```bash
git add offline
git commit -m "feat(offline): streaming TTS WebSocket with per-unit reuse and cancel on close"
```

---

### Task 5: Energy endpointer

**Files:**
- Create: `offline/adapter/endpoint.go`
- Test: `offline/adapter/endpoint_test.go`

**Interfaces:**
- Produces: `type Event int` with `EventNone`, `EventSpeechStart`, `EventSpeechEnd`, `EventSpeechAbort`; `type Endpointer`; `func NewEndpointer(rate int, silence time.Duration, threshold float64) *Endpointer`; `func (e *Endpointer) Feed(pcm []byte) Event`; `func (e *Endpointer) Reset()`. `Feed` takes PCM16LE mono of any length. `EventSpeechStart` fires on the first voiced chunk; `EventSpeechEnd` after `silence` of quiet that follows at least 200 ms of speech; `EventSpeechAbort` after `silence` of quiet that follows less speech than that (a click).

- [ ] **Step 1: Write the failing tests `offline/adapter/endpoint_test.go`**

```go
package adapter

import (
	"encoding/binary"
	"math"
	"testing"
	"time"
)

const testRate = 16000

// pcmTone is a 440 Hz sine of amplitude amp (0..1 of full scale) lasting d.
func pcmTone(d time.Duration, amp float64) []byte {
	n := int(d * testRate / time.Second)
	b := make([]byte, 2*n)
	for i := 0; i < n; i++ {
		v := int16(amp * 32767 * math.Sin(2*math.Pi*440*float64(i)/testRate))
		binary.LittleEndian.PutUint16(b[2*i:], uint16(v))
	}
	return b
}

func pcmSilence(d time.Duration) []byte { return make([]byte, 2*int(d*testRate/time.Second)) }

// feed pushes pcm in 20 ms frames and returns the events that were not EventNone.
func feed(e *Endpointer, pcm []byte) []Event {
	var out []Event
	step := 2 * testRate / 50
	for i := 0; i < len(pcm); i += step {
		end := min(i+step, len(pcm))
		if ev := e.Feed(pcm[i:end]); ev != EventNone {
			out = append(out, ev)
		}
	}
	return out
}

func TestEndpointerSpeechThenSilence(t *testing.T) {
	e := NewEndpointer(testRate, 600*time.Millisecond, 0.01)
	pcm := append(append(pcmSilence(300*time.Millisecond), pcmTone(time.Second, 0.3)...), pcmSilence(time.Second)...)
	got := feed(e, pcm)
	if len(got) != 2 || got[0] != EventSpeechStart || got[1] != EventSpeechEnd {
		t.Fatalf("events %v", got)
	}
}

func TestEndpointerShortBlipAborts(t *testing.T) {
	e := NewEndpointer(testRate, 600*time.Millisecond, 0.01)
	got := feed(e, append(pcmTone(100*time.Millisecond, 0.3), pcmSilence(time.Second)...))
	if len(got) != 2 || got[0] != EventSpeechStart || got[1] != EventSpeechAbort {
		t.Fatalf("events %v", got)
	}
}

func TestEndpointerPauseInsideSpeechDoesNotEnd(t *testing.T) {
	e := NewEndpointer(testRate, 600*time.Millisecond, 0.01)
	pcm := append(append(pcmTone(500*time.Millisecond, 0.3), pcmSilence(300*time.Millisecond)...), pcmTone(500*time.Millisecond, 0.3)...)
	got := feed(e, pcm)
	if len(got) != 1 || got[0] != EventSpeechStart {
		t.Fatalf("a 300 ms pause ended the utterance: %v", got)
	}
}

func TestEndpointerSilenceAloneNeverStarts(t *testing.T) {
	e := NewEndpointer(testRate, 600*time.Millisecond, 0.01)
	if got := feed(e, pcmSilence(5*time.Second)); len(got) != 0 {
		t.Fatalf("events on silence: %v", got)
	}
}

func TestEndpointerResetAllowsNewUtterance(t *testing.T) {
	e := NewEndpointer(testRate, 600*time.Millisecond, 0.01)
	feed(e, pcmTone(time.Second, 0.3))
	e.Reset()
	if got := feed(e, pcmTone(time.Second, 0.3)); len(got) != 1 || got[0] != EventSpeechStart {
		t.Fatalf("after Reset: %v", got)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `cd offline && go test ./adapter/ -run Endpointer 2>&1 | head`
Expected: FAIL (`NewEndpointer` undefined).

- [ ] **Step 3: Implement `offline/adapter/endpoint.go`**

```go
package adapter

import (
	"encoding/binary"
	"math"
	"time"
)

type Event int

const (
	EventNone Event = iota
	EventSpeechStart
	EventSpeechEnd   // silence after a real utterance: decode it
	EventSpeechAbort // silence after a click or blip: discard it
)

const minSpeech = 200 * time.Millisecond

// Endpointer decides utterance boundaries from RMS energy. ponytail: a fixed energy threshold works
// for clean and synthetic audio; a noisy microphone needs a real VAD (the repo's vad/ or sherpa-onnx).
type Endpointer struct {
	rate      int
	silence   time.Duration
	threshold float64
	inSpeech  bool
	speech    time.Duration // voiced time of the current utterance
	quiet     time.Duration // consecutive quiet time since the last voiced chunk
}

func NewEndpointer(rate int, silence time.Duration, threshold float64) *Endpointer {
	return &Endpointer{rate: rate, silence: silence, threshold: threshold}
}

func (e *Endpointer) Reset() { e.inSpeech, e.speech, e.quiet = false, 0, 0 }

// Feed consumes a chunk of PCM16LE mono of any length.
func (e *Endpointer) Feed(pcm []byte) Event {
	n := len(pcm) / 2
	if n == 0 {
		return EventNone
	}
	d := time.Duration(n) * time.Second / time.Duration(e.rate)
	voiced := rms(pcm, n) >= e.threshold
	if !e.inSpeech {
		if !voiced {
			return EventNone
		}
		e.inSpeech, e.speech, e.quiet = true, d, 0
		return EventSpeechStart
	}
	if voiced {
		e.speech += d
		e.quiet = 0
		return EventNone
	}
	e.quiet += d
	if e.quiet < e.silence {
		return EventNone
	}
	short := e.speech < minSpeech
	e.Reset()
	if short {
		return EventSpeechAbort
	}
	return EventSpeechEnd
}

func rms(pcm []byte, n int) float64 {
	var sum float64
	for i := 0; i < n; i++ {
		v := float64(int16(binary.LittleEndian.Uint16(pcm[2*i:]))) / 32768
		sum += v * v
	}
	return math.Sqrt(sum / float64(n))
}
```

- [ ] **Step 4: Run the tests**

Run: `cd offline && go test -race ./adapter/ -run Endpointer -v`
Expected: PASS (5 tests).

- [ ] **Step 5: Commit**

```bash
git add offline
git commit -m "feat(offline): pure-Go energy endpointer"
```

---

### Task 6: STT WebSocket endpoint

**Files:**
- Create: `offline/adapter/stt.go`
- Modify: `offline/adapter/server.go` (delete the `handleSTT` stub)
- Test: `offline/adapter/stt_test.go`

**Interfaces:**
- Consumes: `Endpointer` (Task 5), `writeJSON` (Task 4), `queryInt`, `STTBackend`, `Limiter`, `Config` (Task 1).
- Produces: `func (s *Server) handleSTT`; `type sttMsg struct { Type, ID, Text, Language, Message, Code string; IsFinal, SpeechFinal bool }` (JSON tags `type,id,text,language,message,code,is_final,speech_final`, all omitempty); `type sttJob struct { kind string; id uint64; pcm []byte }` with kinds `"interim"` and `"final"`.

Design (one read loop, one ordered decode worker):
- The handler reads frames. The `Endpointer` marks utterance starts and ends; audio is appended to a bounded utterance buffer (`MaxUtterance`), with 200 ms of pre-roll prepended at the start.
- Every `InterimEvery` while speaking, an `interim` job (a copy of the buffer so far) is queued **only if the job queue is empty**, so at most one interim waits. A `final` job is always queued (blocking, but cancellable).
- `cur` (atomic) is the id of the utterance in progress. A job carries the id it was made for; flushing an utterance queues its `final` and then increments `cur`. The worker drops an `interim` whose id is no longer `cur`: a stale interim can never follow a `speech_final`. Finals are never dropped.
- The worker is the only goroutine that calls the backend and the only one that writes transcripts. It sends `transcript.partial`, `speech_final`, and at the end `transcript.done`.
- An interim failure is ignored (a busy queue or transient error); a final failure sends `error`, cancels the context and ends the stream.

- [ ] **Step 1: Write the failing tests `offline/adapter/stt_test.go`**

```go
package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func fastCfg(t *testing.T) Config {
	c := mustConfig(t)
	c.InterimEvery = 100 * time.Millisecond
	c.EndpointSilence = 300 * time.Millisecond
	c.MaxConcurrency = 4
	return c
}

// sendAudio streams pcm as 20 ms binary frames, paced faster than real time.
func sendAudio(t *testing.T, ctx context.Context, c *websocket.Conn, pcm []byte, pace time.Duration) {
	t.Helper()
	step := 640
	for i := 0; i < len(pcm); i += step {
		if err := c.Write(ctx, websocket.MessageBinary, pcm[i:min(i+step, len(pcm))]); err != nil {
			t.Fatalf("write audio: %v", err)
		}
		time.Sleep(pace)
	}
}

func readSTT(t *testing.T, ctx context.Context, c *websocket.Conn) sttMsg {
	t.Helper()
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m sttMsg
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// drain reads until transcript.done or error and returns every message.
func drainSTT(t *testing.T, ctx context.Context, c *websocket.Conn) []sttMsg {
	t.Helper()
	var all []sttMsg
	for {
		m := readSTT(t, ctx, c)
		all = append(all, m)
		if m.Type == "transcript.done" || m.Type == "error" {
			return all
		}
	}
}

func speechFinals(ms []sttMsg) (out []string) {
	for _, m := range ms {
		if m.Type == "transcript.partial" && m.SpeechFinal {
			out = append(out, m.Text)
		}
	}
	return out
}

func TestSTTCreatedThenInterimThenFinalThenDone(t *testing.T) {
	var n atomic.Int32
	stt := &fakeSTT{fn: func(_ context.Context, pcm []byte) (string, error) {
		return "decode " + string(rune('a'+n.Add(1)-1)), nil
	}}
	srv := httptest.NewServer(New(fastCfg(t), nil, stt, okReady).Handler())
	defer srv.Close()
	c, ctx := dialWS(t, srv, "/v1/stt?sample_rate=16000&encoding=pcm&language=en&interim_results=true")
	if m := readSTT(t, ctx, c); m.Type != "transcript.created" || m.ID == "" {
		t.Fatalf("first message %+v", m)
	}
	sendAudio(t, ctx, c, append(pcmTone(1200*time.Millisecond, 0.3), pcmSilence(500*time.Millisecond)...), 2*time.Millisecond)
	if err := c.Write(ctx, websocket.MessageText, []byte(`{"type":"audio.done"}`)); err != nil {
		t.Fatal(err)
	}
	all := drainSTT(t, ctx, c)
	last := all[len(all)-1]
	finals := speechFinals(all)
	if last.Type != "transcript.done" || len(finals) != 1 {
		t.Fatalf("messages %+v", all)
	}
	interims := 0
	for _, m := range all {
		if m.Type == "transcript.partial" && !m.SpeechFinal && m.Text != "" {
			interims++
		}
	}
	if interims == 0 {
		t.Fatalf("no interim before the final: %+v", all)
	}
	for _, m := range all {
		if m.IsFinal {
			t.Fatalf("is_final must never be sent: %+v", m)
		}
	}
}

func TestSTTSilenceNeverDecodes(t *testing.T) {
	stt := &fakeSTT{fn: func(context.Context, []byte) (string, error) { return "Thank you.", nil }}
	srv := httptest.NewServer(New(fastCfg(t), nil, stt, okReady).Handler())
	defer srv.Close()
	c, ctx := dialWS(t, srv, "/v1/stt")
	readSTT(t, ctx, c) // created
	sendAudio(t, ctx, c, pcmSilence(2*time.Second), 0)
	sendAudio(t, ctx, c, append(pcmTone(80*time.Millisecond, 0.5), pcmSilence(time.Second)...), 0) // a click
	_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"audio.done"}`))
	all := drainSTT(t, ctx, c)
	if stt.calls.Load() != 0 || len(speechFinals(all)) != 0 {
		t.Fatalf("decoded silence/click: calls=%d msgs=%+v", stt.calls.Load(), all)
	}
	if all[len(all)-1].Type != "transcript.done" {
		t.Fatalf("stream did not end cleanly: %+v", all)
	}
}

func TestSTTTwoUtterancesFinalizeInOrder(t *testing.T) {
	var n atomic.Int32
	stt := &fakeSTT{fn: func(_ context.Context, pcm []byte) (string, error) {
		time.Sleep(30 * time.Millisecond)
		return strings.Repeat("w", int(n.Add(1))), nil
	}}
	cfg := fastCfg(t)
	cfg.InterimEvery = time.Hour // finals only: the text of the Nth decode is N letters
	srv := httptest.NewServer(New(cfg, nil, stt, okReady).Handler())
	defer srv.Close()
	c, ctx := dialWS(t, srv, "/v1/stt")
	readSTT(t, ctx, c)
	for i := 0; i < 2; i++ {
		sendAudio(t, ctx, c, append(pcmTone(time.Second, 0.3), pcmSilence(500*time.Millisecond)...), time.Millisecond)
	}
	_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"audio.done"}`))
	finals := speechFinals(drainSTT(t, ctx, c))
	if len(finals) != 2 || finals[0] != "w" || finals[1] != "ww" {
		t.Fatalf("finals %v", finals)
	}
}

func TestSTTStaleInterimDropped(t *testing.T) {
	gate := make(chan struct{})
	stt := &fakeSTT{fn: func(_ context.Context, pcm []byte) (string, error) {
		if len(pcm) < 16000*2*2 { // an interim of a still-short utterance: held back until after the final
			<-gate
			return "STALE", nil
		}
		return "final text", nil
	}}
	cfg := fastCfg(t)
	cfg.InterimEvery = 50 * time.Millisecond
	srv := httptest.NewServer(New(cfg, nil, stt, okReady).Handler())
	defer srv.Close()
	c, ctx := dialWS(t, srv, "/v1/stt")
	readSTT(t, ctx, c)
	go func() { time.Sleep(900 * time.Millisecond); close(gate) }()
	sendAudio(t, ctx, c, append(pcmTone(2300*time.Millisecond, 0.3), pcmSilence(500*time.Millisecond)...), time.Millisecond)
	_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"audio.done"}`))
	for _, m := range drainSTT(t, ctx, c) {
		if m.Text == "STALE" {
			t.Fatalf("a stale interim was delivered: %+v", m)
		}
	}
}

func TestSTTUtteranceCap(t *testing.T) {
	stt := &fakeSTT{fn: func(context.Context, []byte) (string, error) { return "cut", nil }}
	cfg := fastCfg(t)
	cfg.MaxUtterance = time.Second
	cfg.InterimEvery = time.Hour
	srv := httptest.NewServer(New(cfg, nil, stt, okReady).Handler())
	defer srv.Close()
	c, ctx := dialWS(t, srv, "/v1/stt")
	readSTT(t, ctx, c)
	sendAudio(t, ctx, c, pcmTone(3500*time.Millisecond, 0.3), 0) // continuous speech, no pause
	_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"audio.done"}`))
	if finals := speechFinals(drainSTT(t, ctx, c)); len(finals) < 3 {
		t.Fatalf("a 3.5 s monologue with a 1 s cap gave %d finals", len(finals))
	}
}

func TestSTTFinalErrorSendsErrorAndCloses(t *testing.T) {
	stt := &fakeSTT{fn: func(context.Context, []byte) (string, error) { return "", errors.New("whisper crashed") }}
	cfg := fastCfg(t)
	cfg.InterimEvery = time.Hour
	srv := httptest.NewServer(New(cfg, nil, stt, okReady).Handler())
	defer srv.Close()
	c, ctx := dialWS(t, srv, "/v1/stt")
	readSTT(t, ctx, c)
	sendAudio(t, ctx, c, append(pcmTone(time.Second, 0.3), pcmSilence(500*time.Millisecond)...), time.Millisecond)
	all := drainSTT(t, ctx, c)
	if last := all[len(all)-1]; last.Type != "error" || last.Message == "" {
		t.Fatalf("got %+v", all)
	}
}

func TestSTTWorkerStopsOnDisconnect(t *testing.T) {
	started, cancelled := make(chan struct{}), make(chan struct{})
	stt := &fakeSTT{fn: func(ctx context.Context, _ []byte) (string, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return "", ctx.Err()
	}}
	cfg := fastCfg(t)
	cfg.InterimEvery = time.Hour
	srv := httptest.NewServer(New(cfg, nil, stt, okReady).Handler())
	defer srv.Close()
	c, ctx := dialWS(t, srv, "/v1/stt")
	readSTT(t, ctx, c)
	sendAudio(t, ctx, c, append(pcmTone(time.Second, 0.3), pcmSilence(500*time.Millisecond)...), time.Millisecond)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("decode never started")
	}
	c.CloseNow()
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("decode was not cancelled after the client disconnected")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `cd offline && go test ./adapter/ -run STT 2>&1 | head`
Expected: FAIL (`sttMsg` undefined).

- [ ] **Step 3: Implement `offline/adapter/stt.go` and delete the `handleSTT` stub**

```go
package adapter

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

type sttMsg struct {
	Type        string `json:"type"`
	ID          string `json:"id,omitempty"`
	Text        string `json:"text,omitempty"`
	Language    string `json:"language,omitempty"`
	Message     string `json:"message,omitempty"`
	Code        string `json:"code,omitempty"`
	IsFinal     bool   `json:"is_final,omitempty"`
	SpeechFinal bool   `json:"speech_final,omitempty"`
}

type sttJob struct {
	kind string // "interim" | "final"
	id   uint64 // the utterance this job belongs to
	pcm  []byte
}

const preRoll = 200 * time.Millisecond

// handleSTT serves xAI's streaming STT on top of batch Whisper. Whisper cannot stream, so an
// Endpointer cuts utterances, interims re-decode the utterance so far, and speech_final decodes it
// once more. A single worker owns the backend calls and every transcript write, so results are
// ordered; a generation id drops an interim that finishes after its utterance's final.
func (s *Server) handleSTT(w http.ResponseWriter, r *http.Request) {
	rate := queryInt(r, "sample_rate", 16000)
	lang := r.URL.Query().Get("language")
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()
	c.SetReadLimit(1 << 20)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	var idb [8]byte
	_, _ = rand.Read(idb[:])
	if writeJSON(ctx, c, sttMsg{Type: "transcript.created", ID: hex.EncodeToString(idb[:])}) != nil {
		return
	}

	var cur atomic.Uint64 // id of the utterance in progress
	jobs := make(chan sttJob, 4)
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		s.sttWorker(ctx, cancel, c, jobs, &cur, rate, lang)
	}()

	clean := s.sttReadLoop(ctx, c, jobs, &cur, rate)
	if !clean {
		cancel() // the client is gone: stop decoding
	}
	close(jobs)
	<-workerDone
	if clean && ctx.Err() == nil {
		_ = c.Close(websocket.StatusNormalClosure, "done")
	}
}

// sttReadLoop feeds frames to the endpointer and queues jobs. It returns true when the client sent
// audio.done and false on a read error or cancellation. It is the producer of jobs.
func (s *Server) sttReadLoop(ctx context.Context, c *websocket.Conn, jobs chan<- sttJob, cur *atomic.Uint64, rate int) bool {
	ep := NewEndpointer(rate, s.cfg.EndpointSilence, s.cfg.EnergyThreshold)
	maxBytes := int(s.cfg.MaxUtterance/time.Second) * rate * 2
	preBytes := int(preRoll*time.Duration(rate)/time.Second) * 2
	var utt, pre []byte
	inUtt := false
	var lastInterim time.Time

	flush := func() bool {
		if len(utt) > 0 {
			select {
			case jobs <- sttJob{kind: "final", id: cur.Load(), pcm: utt}:
			case <-ctx.Done():
				return false
			}
			cur.Add(1) // any interim still queued or decoding for the old id is now stale
		}
		utt, inUtt = nil, false
		return true
	}

	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			return false
		}
		if typ == websocket.MessageText {
			var m sttMsg
			if json.Unmarshal(data, &m) == nil && m.Type == "audio.done" {
				return flush()
			}
			continue
		}
		ev := ep.Feed(data)
		switch ev {
		case EventSpeechStart:
			inUtt = true
			utt = append(make([]byte, 0, len(pre)+len(data)), pre...)
			lastInterim = time.Now()
		case EventSpeechAbort:
			utt, inUtt = nil, false
		}
		if inUtt {
			utt = append(utt, data...)
		} else {
			pre = append(pre, data...)
			if len(pre) > preBytes {
				pre = pre[len(pre)-preBytes:]
			}
		}
		if ev == EventSpeechEnd || (inUtt && len(utt) >= maxBytes) {
			ep.Reset()
			if !flush() {
				return false
			}
			continue
		}
		if inUtt && time.Since(lastInterim) >= s.cfg.InterimEvery && len(jobs) == 0 {
			lastInterim = time.Now()
			select {
			case jobs <- sttJob{kind: "interim", id: cur.Load(), pcm: append([]byte(nil), utt...)}:
			default:
			}
		}
	}
}

// sttWorker is the only caller of the backend and the only writer of transcripts. It runs jobs in
// order and ends when jobs is closed (audio.done or a dead client) or ctx is cancelled.
func (s *Server) sttWorker(ctx context.Context, cancel context.CancelFunc, c *websocket.Conn, jobs <-chan sttJob, cur *atomic.Uint64, rate int, lang string) {
	var last string
	for j := range jobs {
		if ctx.Err() != nil {
			return
		}
		if j.kind == "interim" && j.id != cur.Load() {
			continue
		}
		text, err := s.transcribe(ctx, j.pcm, rate, lang)
		if err != nil {
			if j.kind == "interim" || ctx.Err() != nil {
				continue // an interim is best effort; a cancelled ctx means the client left
			}
			_ = writeJSON(ctx, c, sttMsg{Type: "error", Message: err.Error(), Code: "BACKEND_ERROR"})
			cancel()
			return
		}
		if text == "" {
			continue
		}
		if j.kind == "interim" && j.id != cur.Load() {
			continue // the utterance was finalized while this decode ran
		}
		msg := sttMsg{Type: "transcript.partial", Text: text, Language: lang, SpeechFinal: j.kind == "final"}
		if writeJSON(ctx, c, msg) != nil {
			return
		}
		if j.kind == "final" {
			last = text
		}
	}
	if ctx.Err() == nil {
		_ = writeJSON(ctx, c, sttMsg{Type: "transcript.done", Text: last})
	}
}

func (s *Server) transcribe(ctx context.Context, pcm []byte, rate int, lang string) (string, error) {
	release, err := s.lim.Acquire(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	return s.stt.Transcribe(ctx, pcm, rate, lang)
}
```

- [ ] **Step 4: Run the tests repeatedly under the race detector**

Run: `cd offline && go test -race -count=5 ./adapter/...`
Expected: PASS five times. If `TestSTTStaleInterimDropped` or `TestSTTUtteranceCap` is timing-sensitive on a slow machine, widen the tone durations in the test, never the production code.

- [ ] **Step 5: Commit**

```bash
git add offline
git commit -m "feat(offline): streaming STT over batch Whisper with endpointing and generation fencing"
```

---

### Task 7: Contract tests with the real xAI clients

**Files:**
- Create: `offline/contract/go.mod`, `offline/contract/contract_test.go`, `offline/contract/stack_test.go`
- Modify: `go.work` (add `./offline/contract`), `Makefile` (MODULES: add `offline/contract`)

**Interfaces:**
- Consumes: `govoice/offline/adapter` (`New`, `Config`, `LoadConfig`, `TTSBackend`, `STTBackend`); `govoice/providers/xai` (`DefaultSTTConfig`, `RealXAISTTOpener`, `STTStream`, `STTMessage`, `DefaultTTSConfig`, `NewRealXAITTSClient`, `NewStreamingTTSProcessor`); `govoice/core` (`NewPipeline`, `NewSession`, `SynthesisUnitFrame`, `OutputAudioFrame`), `govoice/core/coretest` (`PassthroughProcessor`).
- Produces: nothing for later tasks. `contract_test.go` always runs under `-tags xaispeech`; `stack_test.go` runs only with `-tags "xaispeech offlinestack"` and `OFFLINE_ADAPTER_URL`.

- [ ] **Step 1: Create the module**

`offline/contract/go.mod`:

```
module govoice/offline/contract

go 1.27.1

replace govoice/core => ../../core

replace govoice/providers/xai => ../../providers/xai

replace govoice/offline => ../

require (
	github.com/coder/websocket v1.8.15
	govoice/core v0.0.0
	govoice/offline v0.0.0
	govoice/providers/xai v0.0.0
)
```

Add `./offline/contract` to `go.work` and `offline/contract` to `MODULES`. Run `cd offline/contract && go mod tidy`. If tidy reports a missing `govoice` root requirement (xai imports `govoice/core` only; check `providers/xai/go.mod`), mirror what `providers/xai/go.mod` requires.

- [ ] **Step 2: Write `offline/contract/contract_test.go`**

```go
//go:build xaispeech

package contract

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"math"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/coretest"
	"govoice/offline/adapter"
	"govoice/providers/xai"
)

type fakeTTS struct{ pcm []byte }

func (f fakeTTS) Synthesize(context.Context, string, string, int) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(f.pcm)), nil
}

type fakeSTT struct{}

func (fakeSTT) Transcribe(_ context.Context, pcm []byte, _ int, _ string) (string, error) {
	return "I would like an appointment", nil
}

func tone(d time.Duration, rate int, amp float64) []byte {
	n := int(d * time.Duration(rate) / time.Second)
	b := make([]byte, 2*n)
	for i := 0; i < n; i++ {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(int16(amp*32767*math.Sin(2*math.Pi*440*float64(i)/float64(rate)))))
	}
	return b
}

func adapterServer(t *testing.T, tts adapter.TTSBackend, stt adapter.STTBackend) *httptest.Server {
	t.Helper()
	cfg, err := adapter.LoadConfig(func(k string) string {
		return map[string]string{"OFFLINE_INTERIM_EVERY": "100ms", "OFFLINE_ENDPOINT_SILENCE": "300ms"}[k]
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(adapter.New(cfg, tts, stt, func(context.Context) error { return nil }).Handler())
	t.Cleanup(srv.Close)
	return srv
}

// The xAI STT client dials ws://.../v1/stt?..., sends PCM then audio.done and reads partials.
func TestRealXAISTTClientAgainstAdapter(t *testing.T) {
	srv := adapterServer(t, nil, fakeSTT{})
	cfg := xai.DefaultSTTConfig()
	cfg.Endpoint = "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/stt"
	cfg.APIKey = "test-key-from-env"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := (&xai.RealXAISTTOpener{}).OpenStream(ctx, cfg)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	defer stream.Close()
	if m, err := stream.Recv(ctx); err != nil || m.Type != "transcript.created" {
		t.Fatalf("first message %+v err=%v", m, err)
	}
	audio := append(tone(time.Second, 16000, 0.3), make([]byte, 2*16000/2)...)
	for i := 0; i < len(audio); i += 640 {
		if err := stream.SendAudio(ctx, audio[i:min(i+640, len(audio))]); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	if err := stream.CloseSend(ctx); err != nil {
		t.Fatal(err)
	}
	var final string
	for {
		m, err := stream.Recv(ctx)
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		if m == nil {
			continue
		}
		if m.Type == "transcript.partial" && m.SpeechFinal {
			final = m.Text
		}
		if m.Type == "transcript.done" {
			break
		}
		if m.Type == "error" {
			t.Fatalf("error from adapter: %+v", m)
		}
	}
	if final != "I would like an appointment" {
		t.Fatalf("speech_final text %q", final)
	}
}

// The xAI unary TTS client POSTs JSON and reads the raw PCM body.
func TestRealXAIUnaryTTSAgainstAdapter(t *testing.T) {
	want := tone(500*time.Millisecond, 24000, 0.3)
	srv := adapterServer(t, fakeTTS{pcm: want}, nil)
	cfg := xai.DefaultTTSConfig()
	cfg.Endpoint = srv.URL + "/v1/tts"
	cfg.APIKey = "test-key-from-env"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got, err := xai.NewRealXAITTSClient().Synthesize(ctx, "hello", cfg)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("len(got)=%d len(want)=%d err=%v", len(got), len(want), err)
	}
}

// The real StreamingTTSProcessor dials the WebSocket, sends text.delta/text.done and turns
// audio.delta into OutputAudioFrames in a real core session.
func TestRealXAIStreamingTTSAgainstAdapter(t *testing.T) {
	pcm := tone(time.Second, 24000, 0.3)
	srv := adapterServer(t, fakeTTS{pcm: pcm}, nil)
	cfg := xai.DefaultTTSConfig()
	cfg.Endpoint = srv.URL + "/v1/tts"
	cfg.APIKey = "test-key-from-env"
	proc, err := xai.NewStreamingTTSProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sink := &coretest.PassthroughProcessor{}
	pipe, err := core.NewPipeline(proc, sink)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := core.NewSession(1, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Start(); err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	for i, text := range []string{"First sentence.", "Second sentence."} {
		if err := sess.Post(&core.SynthesisUnitFrame{Turn: "t1", Gen: 1, UnitID: uint64(i + 1), Text: text, IsFinal: i == 1}); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		var bytesOut, finals int
		for _, f := range sink.GetFrames() {
			if a, ok := f.(*core.OutputAudioFrame); ok {
				bytesOut += len(a.Samples)
				if a.IsFinal {
					finals++
				}
			}
		}
		if finals >= 2 && bytesOut >= 2*len(pcm)-2*960 { // the processor holds back at most one 20 ms frame per unit
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %d bytes out, %d finals", bytesOut, finals)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
```

- [ ] **Step 3: Run**

Run: `cd offline/contract && go test -race -count=1 -tags xaispeech ./...`
Expected: PASS (3 tests). If the streaming test's byte accounting is off by a frame, adjust the tolerance by reading how `core/ttsunit` holds back the final frame, not by weakening the assertion that both units finish.

- [ ] **Step 4: Write `offline/contract/stack_test.go` (real models, opt-in)**

```go
//go:build xaispeech && offlinestack

package contract

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"govoice/providers/xai"
)

// Round trip through the real compose stack: synthesize a phrase with the adapter's TTS, resample
// nothing (ask for 16 kHz directly), feed it to the adapter's STT, expect the key word back.
// Run with: OFFLINE_ADAPTER_URL=http://127.0.0.1:8099 go test -tags "xaispeech offlinestack" ./...
func TestStackRoundTrip(t *testing.T) {
	base := os.Getenv("OFFLINE_ADAPTER_URL")
	if base == "" {
		t.Skip("OFFLINE_ADAPTER_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	ttsCfg := xai.DefaultTTSConfig()
	ttsCfg.Endpoint, ttsCfg.APIKey, ttsCfg.SampleRate = base+"/v1/tts", "test-key-from-env", 16000
	pcm, err := xai.NewRealXAITTSClient().Synthesize(ctx, "I would like to book an appointment for next Tuesday.", ttsCfg)
	if err != nil || len(pcm) < 16000 {
		t.Fatalf("tts: %d bytes err=%v", len(pcm), err)
	}

	sttCfg := xai.DefaultSTTConfig()
	sttCfg.Endpoint = "ws" + strings.TrimPrefix(base, "http") + "/v1/stt"
	sttCfg.APIKey = "test-key-from-env"
	stream, err := (&xai.RealXAISTTOpener{}).OpenStream(ctx, sttCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	audio := append(pcm, make([]byte, 2*16000)...) // 1 s of trailing silence ends the utterance
	for i := 0; i < len(audio); i += 640 {
		if err := stream.SendAudio(ctx, audio[i:min(i+640, len(audio))]); err != nil {
			t.Fatal(err)
		}
	}
	if err := stream.CloseSend(ctx); err != nil {
		t.Fatal(err)
	}
	var final string
	for {
		m, err := stream.Recv(ctx)
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		if m == nil {
			continue
		}
		if m.Type == "error" {
			t.Fatalf("adapter error: %+v", m)
		}
		if m.Type == "transcript.partial" && m.SpeechFinal {
			final = m.Text
		}
		if m.Type == "transcript.done" {
			break
		}
	}
	if !strings.Contains(strings.ToLower(final), "appointment") {
		t.Fatalf("round trip lost the key word: %q", final)
	}
}
```

- [ ] **Step 5: Verify the opt-in test compiles and skips**

Run: `cd offline/contract && go vet -tags "xaispeech offlinestack" ./... && go test -tags "xaispeech offlinestack" -run TestStackRoundTrip -v ./...`
Expected: `--- SKIP` (no stack running yet; it passes for real in Task 9).

- [ ] **Step 6: Commit**

```bash
git add offline go.work Makefile
git commit -m "test(offline): real xai STT/TTS clients against the adapter, plus an opt-in stack round trip"
```

---

### Task 8: `offline` provider in the server

**Files:**
- Create: `server/registry_offline.go`, `server/registry_offline_speech.go` (`//go:build xaispeech`), `server/catalog/data/offline.yaml`
- Test: `server/registry_offline_test.go`
- Modify: `server/config.go`, `server/config_defaults.go`, `server/registry.go`, `server/registry_noxaispeech.go`, `server/catalog/catalog_test.go`

**Interfaces:**
- Consumes: `xai.NewProcessor`, `xai.Config`, `xai.DefaultSTTConfig`, `xai.NewSTTProcessor`, `xai.DefaultTTSConfig`, `xai.NewStreamingTTSProcessor` (existing); `Registry.Register`, `StageSpec`, `failoverPolicy`, `xaiSpeechEnabled` (existing).
- Produces: registry keys `offline.llm`, `offline.stt`, `offline.tts`; `cfg.Providers.Offline.{LLMEndpoint,STTEndpoint,TTSEndpoint}`; `func xaiLLMProcessor(spec StageSpec, endpoint, key string, br *resilience.Breaker, who string) (core.Processor, error)` (the xai.llm factory body, now shared); Registry fields `offlineLLMBreaker`, `offlineSTTBreaker`, `offlineTTSBreaker`.

The provider reuses the real xai processors (so retries, breaker, usage, tracing and the tool loop are the real ones) and needs no key. The existing `xai` provider is untouched in behaviour.

- [ ] **Step 1: Write the failing tests `server/registry_offline_test.go`**

```go
package server

import "testing"

func TestOfflineRegisteredAndBuildsWithoutAKey(t *testing.T) {
	reg := NewRegistry()
	cfg := DefaultConfig(ProfileDevelopment)
	stages := map[string]StageSpec{"llm": {Model: "govoice-test"}}
	if xaiSpeechEnabled {
		stages["stt"], stages["tts"] = StageSpec{Model: "s"}, StageSpec{Voice: "af_heart"}
	}
	for stage, spec := range stages {
		if !reg.Supports("offline", stage) {
			t.Errorf("offline.%s is not registered", stage)
			continue
		}
		p, err := reg.factories["offline."+stage](cfg, spec) // no Secret: offline reads none
		if err != nil {
			t.Errorf("offline.%s with no secret: %v", stage, err)
			continue
		}
		_ = p.Close()
	}
}

func TestOfflineSpeechAbsentWithoutXAISpeechTag(t *testing.T) {
	if xaiSpeechEnabled {
		t.Skip("built with xaispeech")
	}
	reg := NewRegistry()
	if reg.Supports("offline", "stt") || reg.Supports("offline", "tts") {
		t.Fatal("offline STT/TTS registered in a build without xaispeech")
	}
	if !reg.Supports("offline", "llm") {
		t.Fatal("offline.llm must exist on every build")
	}
}

func TestOfflineBreakersAreSeparate(t *testing.T) {
	reg := NewRegistry()
	if reg.offlineLLMBreaker == reg.xaiLLMBreaker || reg.offlineSTTBreaker == reg.xaiSTTBreaker || reg.offlineTTSBreaker == reg.xaiTTSBreaker {
		t.Fatal("offline shares a breaker with xai: a local outage would open the real provider's circuit")
	}
}

func TestOfflineDefaultEndpoints(t *testing.T) {
	c := DefaultConfig(ProfileDevelopment).Providers.Offline
	if c.LLMEndpoint != "http://127.0.0.1:11434/v1/responses" || c.STTEndpoint != "ws://127.0.0.1:8099/v1/stt" || c.TTSEndpoint != "http://127.0.0.1:8099/v1/tts" {
		t.Fatalf("defaults: %+v", c)
	}
}
```

Also add `"offline"` to the provider id list in `server/catalog/catalog_test.go:35`.

- [ ] **Step 2: Run to verify failure**

Run: `cd server && go test -tags xaispeech -run 'Offline|Catalog' -count=1 ./... 2>&1 | head`
Expected: FAIL (`Providers.Offline`, `offlineLLMBreaker` undefined; catalog has no `offline`).

- [ ] **Step 3: Implement**

`server/config.go`: add next to the `XAITTS` block:

```go
		// Offline is the local stack in offline/: the xai processors pointed at a local adapter (STT,
		// TTS) and Ollama (LLM). It needs no key. Defaults match offline/compose.yaml.
		Offline struct {
			LLMEndpoint string `yaml:"llm_endpoint"`
			STTEndpoint string `yaml:"stt_endpoint"`
			TTSEndpoint string `yaml:"tts_endpoint"`
		} `yaml:"offline"`
```

`server/config_defaults.go`, next to the xAI defaults:

```go
	cfg.Providers.Offline.LLMEndpoint = "http://127.0.0.1:11434/v1/responses"
	cfg.Providers.Offline.STTEndpoint = "ws://127.0.0.1:8099/v1/stt"
	cfg.Providers.Offline.TTSEndpoint = "http://127.0.0.1:8099/v1/tts"
```

`server/registry.go`: add fields `offlineLLMBreaker, offlineSTTBreaker, offlineTTSBreaker *resilience.Breaker` beside the xai ones (line ~143), initialise them with `resilience.NewBreaker(resilience.DefaultConfig())` in `NewRegistry` (line ~245), call `registerOffline(r)` right after `registerEcho(r)` (line ~460), and replace the body of the `xai.llm` factory with a call to the shared helper:

```go
	r.Register("xai.llm", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		key, err := apiKey(spec.Secret)
		if err != nil {
			return nil, fmt.Errorf("xai.llm: %w", err)
		}
		return xaiLLMProcessor(spec, cfg.Providers.XAILLM.Endpoint, key, r.xaiLLMBreaker, "xai.llm")
	})
```

```go
// xaiLLMProcessor is the xAI Responses-API stage, shared by xai.llm and offline.llm.
func xaiLLMProcessor(spec StageSpec, endpoint, key string, br *resilience.Breaker, who string) (core.Processor, error) {
	xaiCfg := xai.Config{Model: spec.Model, Endpoint: endpoint, SystemPrompt: spec.SystemPrompt, APIKey: key, CircuitBreaker: br}
	if len(spec.Tools) > 0 {
		loop, err := coretools.NewToolLoop(spec.Tools, spec.ToolExecutor, spec.History, spec.toolLoopConfig())
		if err != nil {
			return nil, fmt.Errorf("%s tools: %w", who, err)
		}
		xaiCfg.ToolDefinitions, xaiCfg.ToolLoop = spec.Tools, loop
	}
	return xai.NewProcessor(xaiCfg)
}
```

`server/registry_offline.go`:

```go
package server

import "govoice/core"

// offlineBearer is the constant token the xai processors send to the local stack, which ignores it.
// The offline provider reads no credential, so no key is stored for it.
const offlineBearer = "offline"

// registerOffline registers the local stack (offline/) on every build: offline.llm needs no build
// tag; offline.stt and offline.tts exist only where the xAI speech adapters do.
func registerOffline(r *Registry) {
	r.Register("offline.llm", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		return xaiLLMProcessor(spec, cfg.Providers.Offline.LLMEndpoint, offlineBearer, r.offlineLLMBreaker, "offline.llm")
	})
	registerOfflineSpeech(r)
}
```

`server/registry_offline_speech.go`:

```go
//go:build xaispeech

package server

import (
	"govoice/core"
	"govoice/providers/xai"
)

// registerOfflineSpeech wires offline.stt and offline.tts to the adapter in offline/.
func registerOfflineSpeech(r *Registry) {
	r.Register("offline.stt", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		sttCfg := xai.DefaultSTTConfig()
		if spec.Model != "" {
			sttCfg.Model = spec.Model
		}
		if spec.Language != "" {
			sttCfg.Language = spec.Language
		}
		sttCfg.Endpoint, sttCfg.APIKey, sttCfg.CircuitBreaker = cfg.Providers.Offline.STTEndpoint, offlineBearer, r.offlineSTTBreaker
		if spec.Failover {
			sttCfg.RetryPolicy = failoverPolicy("stt", sttCfg.RetryPolicy)
		}
		return xai.NewSTTProcessor(sttCfg)
	})
	r.Register("offline.tts", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		ttsCfg := xai.DefaultTTSConfig()
		if spec.Voice != "" {
			ttsCfg.Voice = spec.Voice
		}
		if cfg.Providers.GoogleTTS.SampleRate > 0 { // the pipeline's output rate is configured once, under google_tts
			ttsCfg.SampleRate = cfg.Providers.GoogleTTS.SampleRate
		}
		ttsCfg.Endpoint, ttsCfg.APIKey, ttsCfg.CircuitBreaker = cfg.Providers.Offline.TTSEndpoint, offlineBearer, r.offlineTTSBreaker
		if spec.Failover {
			ttsCfg.RetryPolicy = failoverPolicy("tts", ttsCfg.RetryPolicy)
		}
		return xai.NewStreamingTTSProcessor(ttsCfg)
	})
}
```

`server/registry_noxaispeech.go`: add `func registerOfflineSpeech(*Registry) {}` with a one-line comment.

`server/catalog/data/offline.yaml` (closed lists would stop users naming their own Ollama model, so leave `custom` at its default):

```yaml
# The local stack in offline/: STT and TTS through the adapter, the LLM through Ollama. No key.
id: offline
name: Offline (local models)
services:
  llm:
    keyless: true
    models:
      - {id: govoice-test, label: Local model (offline/Modelfile), default: true}
  stt:
    keyless: true
    models:
      - {id: Systran/faster-whisper-small.en, label: Whisper small (English), default: true}
    languages:
      - {id: en, label: English, default: true}
  tts:
    keyless: true
    voices:
      - {id: af_heart, label: Kokoro Heart (en-US), default: true}
```

If the catalog loader rejects a field here (compare with `echo.yaml` and `xai.yaml`), mirror the accepted shape; do not change the loader.

- [ ] **Step 4: Run, with and without the tag**

Run: `cd server && go vet ./... && go vet -tags xaispeech ./... && go test -count=1 -run 'Offline|Catalog|Registry|Config' ./... && go test -tags xaispeech -count=1 -run 'Offline|Catalog|Registry|Config' ./...`
Expected: PASS both ways (the tag-absent run exercises `TestOfflineSpeechAbsentWithoutXAISpeechTag`).

- [ ] **Step 5: Docs contract**

Run: `cd server && go test -tags xaispeech -run 'Docs|Contract' -count=1 ./...`
Expected: PASS. If a contract test lists documented config keys or providers, add `providers.offline.*` / `offline` to the doc it reads (`docs/providers.md`, `docs/api/v1.md` if it lists providers) in this task.

- [ ] **Step 6: Commit**

```bash
git add server
git commit -m "feat(server): offline provider reusing the xai processors with its own endpoints and breakers"
```

---

### Task 9: Compose bundle, LLM model choice, Make targets, docs

**Files:**
- Create: `offline/Dockerfile`, `offline/compose.yaml`, `offline/Modelfile`, `offline/README.md`, `offline/cmd/toolcheck/main.go`
- Modify: `Makefile` (new targets), `docs/providers.md`, `docs/privacy.md`

**Interfaces:**
- Consumes: the `-healthcheck` flag of `offline-adapter` (Task 1), the contract stack test (Task 7).
- Produces: `make offline-up [PROFILE=container-llm|host-llm]`, `make offline-models`, `make offline-down`, `make offline-test`.

- [ ] **Step 1: `offline/Dockerfile`**

Before writing it, read `deploy/Dockerfile` and reuse its Go builder base image tag. Then:

```dockerfile
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/offline-adapter ./cmd/offline-adapter

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/offline-adapter /offline-adapter
EXPOSE 8099
ENTRYPOINT ["/offline-adapter"]
```

The build context is `offline/`; the module has no local `replace` directives (only the contract module does), so nothing outside `offline/` is needed.

- [ ] **Step 2: `offline/compose.yaml`**

```yaml
# Offline speech stack. Profiles: container-llm adds Ollama; host-llm expects Ollama on the host.
# Speaches is pinned by digest (the digest the spike ran on 2026-10-15); bump deliberately.
services:
  speaches:
    image: ghcr.io/speaches-ai/speaches@sha256:21e3df06d842fb7802ab470dd77c25f0e8c0d22950e8d8c6ae886e851af53ef8
    ports: ["127.0.0.1:8000:8000"]  # published for `make offline-models`; the adapter reaches it by service name
    volumes: ["hf-cache:/home/ubuntu/.cache/huggingface/hub"]
    healthcheck:
      test: ["CMD", "python3", "-c", "import urllib.request; urllib.request.urlopen('http://localhost:8000/health')"]
      interval: 5s
      timeout: 3s
      retries: 30
  adapter:
    build: .
    ports: ["8099:8099"]
    environment:
      OFFLINE_SPEACHES_URL: http://speaches:8000
      OFFLINE_TOKEN: ${OFFLINE_TOKEN:-}
    depends_on:
      speaches: {condition: service_healthy}
    healthcheck:
      test: ["CMD", "/offline-adapter", "-healthcheck"]
      interval: 5s
      timeout: 3s
      retries: 12
  ollama:
    profiles: ["container-llm"]
    image: ollama/ollama:latest   # pin to a version tag after the first successful run: `docker image inspect`
    ports: ["127.0.0.1:11434:11434"]
    volumes: ["ollama:/root/.ollama"]
    healthcheck:
      test: ["CMD", "ollama", "list"]
      interval: 5s
      timeout: 5s
      retries: 30
volumes:
  hf-cache:
  ollama:
```

If the Speaches image has no `python3` on `PATH` the healthcheck never turns healthy; verify with `docker run --rm --entrypoint sh <image> -c 'command -v python3 curl'` and use whichever exists.

- [ ] **Step 3: Choose the small LLM with `offline/cmd/toolcheck/main.go`**

```go
// Command toolcheck reports whether an Ollama model completes a tool-call round trip on /v1/responses
// the way the xAI adapter uses it. Usage: toolcheck -model qwen3:4b [-url http://127.0.0.1:11434]
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

var tools = []map[string]any{{
	"type": "function", "name": "transfer_to_human", "description": "Transfer the caller to a human agent",
	"parameters": map[string]any{"type": "object", "properties": map[string]any{"reason": map[string]any{"type": "string"}}, "required": []string{"reason"}},
}}

type event struct {
	Type  string          `json:"type"`
	Delta string          `json:"delta"`
	Item  json.RawMessage `json:"item"`
	Resp  struct {
		Output []json.RawMessage `json:"output"`
	} `json:"response"`
}

func call(url, model string, input []any) (text string, outputs []json.RawMessage, err error) {
	body, _ := json.Marshal(map[string]any{"model": model, "store": false, "stream": true, "tools": tools, "input": input})
	resp, err := (&http.Client{Timeout: 3 * time.Minute}).Post(url+"/v1/responses", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		line, ok := strings.CutPrefix(sc.Text(), "data:")
		if !ok {
			continue
		}
		var e event
		if json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		switch e.Type {
		case "response.output_text.delta":
			text += e.Delta
		case "response.completed":
			outputs = e.Resp.Output
		}
	}
	return text, outputs, sc.Err()
}

func main() {
	model := flag.String("model", "", "Ollama model name")
	url := flag.String("url", "http://127.0.0.1:11434", "Ollama base URL")
	flag.Parse()
	if *model == "" {
		fmt.Fprintln(os.Stderr, "-model is required")
		os.Exit(2)
	}
	start := time.Now()
	sys := map[string]any{"role": "system", "content": "You are a dental receptionist. Use tools when appropriate. Be brief."}
	user := map[string]any{"role": "user", "content": "Please transfer me to a human, I'm upset about my bill."}
	_, outs, err := call(*url, *model, []any{sys, user})
	if err != nil {
		fmt.Println("FAIL round 1:", err)
		os.Exit(1)
	}
	var callID string
	input := []any{sys, user}
	for _, o := range outs {
		var probe map[string]any
		_ = json.Unmarshal(o, &probe)
		input = append(input, probe)
		if probe["type"] == "function_call" {
			callID, _ = probe["call_id"].(string)
		}
	}
	if callID == "" {
		fmt.Println("FAIL: the model did not call the tool")
		os.Exit(1)
	}
	input = append(input, map[string]any{"type": "function_call_output", "call_id": callID, "output": `{"status":"transferred"}`})
	text, _, err := call(*url, *model, input)
	if err != nil || strings.TrimSpace(text) == "" {
		fmt.Println("FAIL round 2:", err, "text:", text)
		os.Exit(1)
	}
	fmt.Printf("OK  model=%s tool-call+result round trip in %s\n    reply: %s\n", *model, time.Since(start).Round(time.Millisecond), strings.TrimSpace(text))
}
```

Run it against candidates that fit in 2-4 GB (try `qwen3:4b`, `llama3.2:3b`, `qwen2.5:3b`; pull each with `ollama pull`):

Run: `cd offline && go run ./cmd/toolcheck -model qwen3:4b`
Expected: `OK` for at least one candidate. Record the chosen model, the time, and the models that failed in `offline/README.md`.

- [ ] **Step 4: `offline/Modelfile` (fill in the chosen base)**

```
FROM <chosen model from step 3, e.g. qwen3:4b>
PARAMETER temperature 0
PARAMETER seed 42
```

- [ ] **Step 5: Make targets** (append to `Makefile`, keep the existing `## group|description` comment style used by `make help`)

```make
# Offline speech stack (docs/superpowers/specs/2026-10-15-offline-speech-service-design.md).
# PROFILE=container-llm runs Ollama in Docker; PROFILE=host-llm expects Ollama on the host (Apple Silicon).
OFFLINE_PROFILE ?= container-llm
OFFLINE_COMPOSE := docker compose -f offline/compose.yaml $(if $(filter container-llm,$(OFFLINE_PROFILE)),--profile container-llm)
OFFLINE_LLM_BASE := $(shell sed -n 's/^FROM //p' offline/Modelfile)

offline-up: ## offline|Start the offline speech stack (OFFLINE_PROFILE=container-llm|host-llm)
	$(OFFLINE_COMPOSE) up -d --build --wait
	@if [ "$(OFFLINE_PROFILE)" = host-llm ]; then curl -sf http://127.0.0.1:11434/api/tags >/dev/null || { echo "host-llm: Ollama is not answering on 127.0.0.1:11434"; exit 1; }; fi
.PHONY: offline-up

offline-models: ## offline|Download Whisper, Kokoro and the LLM into the stack's volumes (needs network, run once)
	curl -sf -X POST http://127.0.0.1:8000/v1/models/Systran/faster-whisper-small.en
	curl -sf -X POST http://127.0.0.1:8000/v1/models/speaches-ai/Kokoro-82M-v1.0-ONNX
ifeq ($(OFFLINE_PROFILE),host-llm)
	ollama pull $(OFFLINE_LLM_BASE) && ollama create govoice-test -f offline/Modelfile
else
	$(OFFLINE_COMPOSE) exec -T ollama ollama pull $(OFFLINE_LLM_BASE)
	$(OFFLINE_COMPOSE) exec -T ollama sh -c 'cat > /tmp/Modelfile && ollama create govoice-test -f /tmp/Modelfile' < offline/Modelfile
endif
.PHONY: offline-models

offline-down: ## offline|Stop the offline stack (volumes are kept)
	$(OFFLINE_COMPOSE) down
.PHONY: offline-down

offline-test: ## offline|Round trip through the running stack with the real xai clients (needs offline-up and offline-models)
	cd offline/contract && OFFLINE_ADAPTER_URL=http://127.0.0.1:8099 go test -count=1 -v -tags "xaispeech offlinestack" ./...
.PHONY: offline-test
```

- [ ] **Step 6: Bring the stack up and run the real round trip**

Run: `make offline-up OFFLINE_PROFILE=host-llm && make offline-models OFFLINE_PROFILE=host-llm && make offline-test`
Expected: `--- PASS: TestStackRoundTrip`. If it fails, the failure is the spike's remaining unknowns: an unknown-voice status code (Speaches may answer 422 or 404; both are 4xx and map to 400), Kokoro rejecting a 16 kHz `sample_rate`, or the energy threshold missing the synthesized audio level. Fix in `adapter/` with a unit test that reproduces it, not in the stack test.

- [ ] **Step 7: Docs**

`offline/README.md`: what it is (3 lines), the two profiles, `make offline-up/models/test/down`, the config snippet below, the env var table from `adapter/config.go`, the chosen LLM and why, and the known limits (server-wide endpoints; usage labelled `xai`; energy endpointer; Speaches TTS returns each sentence whole).

Server config to document (no key is written anywhere; the `offline` provider reads no credential):

```yaml
providers:
  offline:
    llm_endpoint: http://127.0.0.1:11434/v1/responses   # Ollama directly
    stt_endpoint: ws://127.0.0.1:8099/v1/stt
    tts_endpoint: http://127.0.0.1:8099/v1/tts
```

These are the defaults, so a default server config already works. Agents use provider `offline`: STT model any string, LLM model `govoice-test`, TTS voice `af_heart` (xAI voice names such as `eve` are aliased by the adapter).

Add a short "Offline (privacy) mode" section to `docs/providers.md` pointing at `offline/README.md`, and one paragraph in `docs/privacy.md` stating that with this stack audio and text never leave the host and that the only network step is `make offline-models`.

- [ ] **Step 8: Verify docs did not break the docs contract**

Run: `cd server && go test -tags xaispeech -run 'Docs|Contract' -count=1 ./...`
Expected: PASS. If a docs contract test lists known provider doc headings, add the new section name to it.

- [ ] **Step 9: Commit**

```bash
git add offline Makefile docs
git commit -m "feat(offline): compose bundle with two profiles, Make targets, LLM tool check and docs"
```

---

### Task 10: Server end-to-end test through the offline provider

**Files:**
- Create: `server/offline_e2e_test.go` (`//go:build xaispeech`), `server/offline_stack_e2e_test.go` (`//go:build xaispeech && offlinestack`)
- Modify: `server/go.mod` (require and replace `govoice/offline`), `Makefile` (`offline-test` also runs the server stack e2e)

**Interfaces:**
- Consumes: `newFlowRigE2E`, `newProbe`, `flowRigE2E.dial/createRoutedFlow`'s pieces (`r.s.flows.Create`, `flow.CreateRequest`), `e2eCall` (`send`, `waitReplies`, `maxPeak`, `ended`), `eventually` (all in `server/flow_e2e_test.go`, `call_behavior_e2e_test.go`); `adapter.New`, `adapter.LoadConfig` from `govoice/offline/adapter`; `ws.EncodeAudioFrame` from `govoice/transport/ws`.
- Produces: `func (c *e2eCall) sendAudio(pcm []byte)` (test helper: 20 ms frames at 16 kHz, paced), `func offlineFlow(t *testing.T, r *flowRigE2E, cfg string) string`.

The caller speaks **audio** here (every other server e2e sends text), so this proves the whole path: caller audio, adapter STT (`speech_final`), turn, LLM Responses stream, adapter TTS audio back to the caller.

- [ ] **Step 1: Make `govoice/offline` importable from the server tests**

In `server/go.mod` add next to the other replaces and in the require block, mirroring them:

```
replace govoice/offline => ../offline
```

```
	govoice/offline v0.0.0-00010101000000-000000000000
```

Run: `cd server && go mod tidy`
Expected: no error. (The server imports the adapter only from test files; `go mod tidy` keeps the require because tests use it.)

- [ ] **Step 2: Write the hermetic e2e `server/offline_e2e_test.go`**

```go
//go:build xaispeech

package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"govoice/offline/adapter"
	"govoice/server/flow"
	"govoice/transport/ws"
)

// offlineFakeTTS speaks a quiet 440 Hz tone, so "bot audio arrived" is a non-zero peak.
type offlineFakeTTS struct{}

func (offlineFakeTTS) Synthesize(_ context.Context, _, _ string, rate int) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(offlineTone(time.Second, rate, 0.2))), nil
}

// offlineFakeSTT hears whatever the caller said as one fixed sentence.
type offlineFakeSTT struct{}

func (offlineFakeSTT) Transcribe(context.Context, []byte, int, string) (string, error) {
	return "I would like to book an appointment", nil
}

func offlineTone(d time.Duration, rate int, amp float64) []byte {
	n := int(d * time.Duration(rate) / time.Second)
	b := make([]byte, 2*n)
	for i := 0; i < n; i++ {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(int16(amp*32767*math.Sin(2*math.Pi*440*float64(i)/float64(rate)))))
	}
	return b
}

// sendAudio streams pcm to the server as 20 ms frames at 16 kHz, a little faster than real time.
func (c *e2eCall) sendAudio(pcm []byte) {
	c.t.Helper()
	const step = 640
	for i := 0; i < len(pcm); i += step {
		frame, err := ws.EncodeAudioFrame(pcm[i:min(i+step, len(pcm))], 16000, 1)
		if err != nil {
			c.t.Fatal(err)
		}
		if err := c.conn.Write(c.ctx, websocketBinary, frame); err != nil {
			c.t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// fakeResponsesLLM streams xAI's Responses events for a fixed reply.
func fakeResponsesLLM(t *testing.T, reply []string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for _, d := range reply {
			fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\n", d)
			fl.Flush()
		}
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":5,\"output_tokens\":6,\"total_tokens\":11}}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	t.Cleanup(srv.Close)
	return srv
}

func offlineFlow(t *testing.T, r *flowRigE2E, cfg string) string {
	t.Helper()
	f, err := r.s.flows.Create(t.Context(), flow.CreateRequest{Name: "offline", Config: []byte(cfg)})
	if err != nil || f.Readiness.State != flow.ReadinessReady {
		t.Fatalf("offline flow: %+v err=%v", f.Readiness, err)
	}
	return f.ID
}

const offlineFlowConfig = `{"stt":{"provider":"offline","model":"s"},"llm":{"provider":"offline","model":"govoice-test","system_prompt":"Be brief."},"tts":{"provider":"offline","voice":"eve"}}`

func TestOfflineCallEndToEnd(t *testing.T) {
	acfg, err := adapter.LoadConfig(func(k string) string {
		return map[string]string{"OFFLINE_INTERIM_EVERY": "100ms", "OFFLINE_ENDPOINT_SILENCE": "300ms"}[k]
	})
	if err != nil {
		t.Fatal(err)
	}
	ad := httptest.NewServer(adapter.New(acfg, offlineFakeTTS{}, offlineFakeSTT{}, func(context.Context) error { return nil }).Handler())
	defer ad.Close()
	llm := fakeResponsesLLM(t, []string{"Hello ", "from the offline stack."})

	rig := newFlowRigE2E(t, newProbe(), func(c *Config) {
		c.Providers.Offline.STTEndpoint = "ws" + strings.TrimPrefix(ad.URL, "http") + "/v1/stt"
		c.Providers.Offline.TTSEndpoint = ad.URL + "/v1/tts"
		c.Providers.Offline.LLMEndpoint = llm.URL + "/v1/responses"
	})
	call := rig.dial(offlineFlow(t, rig, offlineFlowConfig))

	// One second of speech, then silence long enough for the adapter to end the utterance.
	call.sendAudio(append(offlineTone(time.Second, 16000, 0.3), make([]byte, 2*16000)...))

	call.waitReplies("from the offline stack", 1)
	eventually(t, "bot audio from the adapter's TTS", func() bool { return call.maxPeak() > 0 })
}

func TestOfflineAdapterDownFailsTheStage(t *testing.T) {
	down := httptest.NewServer(http.NotFoundHandler())
	url := down.URL
	down.Close() // nothing listens here any more
	llm := fakeResponsesLLM(t, []string{"never spoken"})
	rig := newFlowRigE2E(t, newProbe(), func(c *Config) {
		c.Providers.Offline.STTEndpoint = "ws" + strings.TrimPrefix(url, "http") + "/v1/stt"
		c.Providers.Offline.TTSEndpoint = url + "/v1/tts"
		c.Providers.Offline.LLMEndpoint = llm.URL + "/v1/responses"
	})
	call := rig.dial(offlineFlow(t, rig, offlineFlowConfig))
	call.sendAudio(append(offlineTone(time.Second, 16000, 0.3), make([]byte, 2*16000)...))
	// The call must not hang on a dead adapter: either the stage fails over/ends the call, or at the
	// least no reply is invented. It is fine for the call to stay up; it is not fine to speak.
	time.Sleep(2 * time.Second)
	if call.replies("never spoken") != 0 {
		t.Fatal("the bot replied although its STT was unreachable")
	}
}
```

`websocketBinary` is `websocket.MessageBinary` from `github.com/coder/websocket`; import it as `"github.com/coder/websocket"` and use `websocket.MessageBinary` directly (the alias above only shortens the snippet).

- [ ] **Step 3: Run it and resolve the one open question**

Run: `cd server && go test -race -tags xaispeech -run 'TestOfflineCallEndToEnd|TestOfflineAdapterDown' -count=1 -v ./...`
Expected: PASS.

Open question this step settles: the shared rig uses `mockVADExecutor`, whose `Register` returns a nil stream, so no local VAD runs. The turn must therefore close on the STT provider's `EndOfUtterance` (`speech_final`) alone. If `TestOfflineCallEndToEnd` times out waiting for the reply, check in this order: (1) the adapter sent `speech_final` (add a temporary log in the fake STT); (2) `core/turn.go` and `server/call.go` use of `cfg.Turn.Manager` and the VAD processor: if a turn only ends on a VAD stop, switch this test to the real executor (`vad.New` with the repo's ONNX model, as `make build` already requires `ort`) instead of the mock, and say so in a comment; (3) the sample rate: the call must send 16 kHz. Record which it was in the test's doc comment.

- [ ] **Step 4: The opt-in real-stack e2e `server/offline_stack_e2e_test.go`**

```go
//go:build xaispeech && offlinestack

package server

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"govoice/providers/xai"
)

// A real call through the server on the real local models: Whisper hears the caller, Ollama answers,
// Kokoro speaks. Needs `make offline-up offline-models` first. The caller's audio is made by the
// stack's own TTS, so no recording is needed.
//
//	OFFLINE_ADAPTER_URL=http://127.0.0.1:8099 go test -tags "xaispeech offlinestack" -run TestOfflineStackCall ./server/...
func TestOfflineStackCall(t *testing.T) {
	base := os.Getenv("OFFLINE_ADAPTER_URL")
	if base == "" {
		t.Skip("OFFLINE_ADAPTER_URL not set")
	}
	ttsCfg := xai.DefaultTTSConfig()
	ttsCfg.Endpoint, ttsCfg.APIKey, ttsCfg.SampleRate = base+"/v1/tts", "offline", 16000
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()
	pcm, err := xai.NewRealXAITTSClient().Synthesize(ctx, "Hello, can you tell me your opening hours?", ttsCfg)
	if err != nil {
		t.Fatalf("make caller audio: %v", err)
	}

	rig := newFlowRigE2E(t, newProbe(), func(c *Config) {
		c.Providers.Offline.STTEndpoint = "ws" + strings.TrimPrefix(base, "http") + "/v1/stt"
		c.Providers.Offline.TTSEndpoint = base + "/v1/tts"
		if u := os.Getenv("OFFLINE_LLM_URL"); u != "" {
			c.Providers.Offline.LLMEndpoint = u
		}
	})
	call := rig.dial(offlineFlow(t, rig, offlineFlowConfig))
	call.sendAudio(append(pcm, make([]byte, 2*16000*2)...))

	eventually2 := func(what string, ok func() bool) {
		for end := time.Now().Add(90 * time.Second); !ok(); time.Sleep(100 * time.Millisecond) {
			if time.Now().After(end) {
				t.Fatalf("timed out waiting for %s", what)
			}
		}
	}
	eventually2("a spoken reply (bot audio)", func() bool { return call.maxPeak() > 0 })
	if call.replyCount() == 0 {
		t.Fatal("audio arrived but no reply text")
	}
}
```

(`eventually` in `flow_e2e_test.go` waits 8 s; real models need longer, hence the local waiter. Use `t.Context()` consistently; `rig.dial` already bounds the call at 30 s, so raise that bound for this test by passing a longer-lived context if the real stack needs it, e.g. add an optional `dialTimeout` field to the rig only if the test actually hits the 30 s limit.)

- [ ] **Step 5: Add the stack e2e to `make offline-test`**

Append to the `offline-test` recipe:

```make
	cd server && OFFLINE_ADAPTER_URL=http://127.0.0.1:8099 go test -count=1 -v -tags "xaispeech offlinestack" -run TestOfflineStackCall ./...
```

Run: `make offline-up OFFLINE_PROFILE=host-llm && make offline-test`
Expected: both the contract round trip and `TestOfflineStackCall` PASS. If the real stack's reply is slow or the LLM does not answer, the failure belongs in the findings you report, not a loosened assertion.

- [ ] **Step 6: Race run of the hermetic tests**

Run: `cd server && go test -race -tags xaispeech -run 'Offline' -count=3 ./...`
Expected: PASS three times.

- [ ] **Step 7: Commit**

```bash
git add server offline Makefile
git commit -m "test(server): end-to-end calls through the offline provider, hermetic and on the real stack"
```

---

### Task 11: Independent review and verification gate (AGENTS.md)

**Files:** none new; fixes land where the findings are.

- [ ] **Step 1: Format and run the module tests**

Run: `gofmt -l offline && cd offline && go vet ./... && go test -race -count=3 ./... && cd contract && go vet -tags xaispeech ./... && go test -race -count=1 -tags xaispeech ./...`
Expected: no `gofmt` output, all PASS.

- [ ] **Step 2: Adversarial review in a fresh context**

Dispatch an independent reviewer (superpowers:requesting-code-review) on `git diff main...HEAD` with this brief: evaluate (a) YAGNI and bloat: any abstraction, config knob or file the xAI-compatible path does not use; (b) goroutine leaks, channel ownership (only producers close), a `context` stored in a struct, a lock or semaphore held across a blocking send, ordering of `transcript.partial` writes, the `cur` generation fence, and `time.Since` use; (c) the Review Focus list in this plan, each item against its named test; (d) that `core/` and `providers/` are untouched and `server/` changed only by Tasks 8 and 10 (`git diff main... --stat`); (e) the `xai` provider's behaviour is unchanged by the `xaiLLMProcessor` refactor. Apply fixes for confirmed findings, each with a regression test.

- [ ] **Step 3: Full verification gate**

Run: `gofmt -l . ; make verify`
Expected: no `gofmt` output; `make verify` passes, including the two new modules in `MODULES` and the hermetic server e2e tests. Note: `make verify` does not start the compose stack; `make offline-test` (contract round trip and `TestOfflineStackCall`) was run in Tasks 9 and 10.

- [ ] **Step 4: Clean status and final commit**

Run: `git status --short`
Expected: only intended files. Commit any review fixes with conventional messages.

---

## Self-review (run against the spec)

- **Wire contract coverage:** STT created/partial/speech_final/done/error and no `is_final` (Task 6); TTS WS multi-unit reuse, `audio.done` keeps the connection open, even-length chunks, error message (Tasks 3, 4); TTS unary body and status mapping (Task 3); auth 401 and open `/healthz` (Task 1). LLM is direct to Ollama (Task 9 docs/config). Real xai clients prove all of it (Task 7).
- **Spec sections with a task:** adapter design (1-6), spike-derived backend choices (2, 9), compose profiles and models step (9), server wiring (8), testing (7, 9, 10), rollout order (TTS first: Tasks 3-4, then STT: 5-6), docs (9). Known limits are documented in Task 9 step 7.
- **Not covered, by decision:** sherpa-onnx streaming path (spec lists it as an alternative STT; add as a follow-up plan if Whisper re-decode CPU is too high); `offline.*` registry alias (spec limits 1 and 2); automatic reconnection or per-call model switching.
- **Placeholders:** the only deliberate blank is the LLM base model in `Modelfile`, which Task 9 step 3 decides by running `toolcheck`; the plan cannot know the answer before that run.
- **Type consistency:** `Config`, `Limiter.Acquire`, `TTSBackend`/`STTBackend`, `writeJSON`, `ttsMsg`, `sttMsg`, `sttJob`, `Endpointer.Feed/Reset`, `streamPCM`, `queryInt` are used with the same signatures everywhere. `fakeTTS`/`fakeSTT` in `helpers_test.go` are defined in Task 3 and used in Tasks 4 and 6 (Task 1 defines only `mustConfig`; Task 3 replaces that file wholesale with the version that includes it).
- **Not compiled:** the code in this plan was written without compiling it. Each task's first step is a failing test run, so a typo surfaces immediately; treat compile errors in plan code as plan bugs and fix them in the implementation.
