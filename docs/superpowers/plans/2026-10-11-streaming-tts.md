# Streaming TTS for Google and xAI Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Google (Chirp 3 HD voices) and xAI speak each sentence as a stream: the first audio frame of a unit is emitted before the provider has finished synthesizing it. A failed stream retries as the existing unary request, and a config switch turns streaming off.

**Architecture:**
- Each provider gets a `StreamingTTSProcessor` built on the shared `core/ttsunit.Runner`. The Runner already provides strict unit order, barge-in cancel, retries before the first audio frame, error-then-close ordering for failover, tracing and the usage metric.
- **xAI:** one WebSocket per reply (`ttsunit.ReplySlot`), `text.delta` + `text.done` per unit, `audio.delta` chunks written as they arrive.
- **Google:** one `StreamingSynthesize` gRPC stream per unit on the already-warm connection, because a Google stream has no per-utterance boundary.
- The existing unary processors stay unchanged: they serve non-Chirp voices, the kill switch, and (through their client) the per-unit fallback.
- The shipped default Google voice moves to `en-US-Chirp3-HD-Aoede`.

**Tech Stack:** Go 1.27.1 (separate modules: `providers/google`, `providers/xai`, `server`), `cloud.google.com/go/texttospeech` v1.23.0, `github.com/coder/websocket`, React + vitest for the UI default.

**Spec:** `docs/superpowers/specs/2026-10-11-streaming-tts-design.md` (approved 2026-10-11).

## Global Constraints

- **xAI wire format.**
  - URL: the configured endpoint with `https` → `wss` (and `http` → `ws`), same path, query `voice`, `language`, `codec=pcm`, `sample_rate`, `optimize_streaming_latency=1`.
  - Header: `Authorization: Bearer <key>`.
  - Client messages: `{"type":"text.delta","delta":"<text>"}` then `{"type":"text.done"}`.
  - Server messages: `{"type":"audio.delta","delta":"<base64 PCM16LE>"}`, `{"type":"audio.done"}`, `{"type":"error","message":"..."}`.
  - The next unit's text is never sent before the previous unit's `audio.done`.
- **Google wire format.**
  - First message: `StreamingSynthesizeConfig` with the voice (`Name`, `LanguageCode` from the voice) and `StreamingAudioConfig{AudioEncoding: PCM, SampleRateHertz}`.
  - Then one input message per piece (`splitUnit` at `MaxSynthesisUnitChars`, 250), then `CloseSend`; read until `io.EOF`.
  - A leading `RIFF` WAV header on the first non-empty chunk is dropped.
- **Routing.**
  - Streaming is used when the Google voice contains `chirp3-hd` (case-insensitive) and `providers.google_tts.disable_streaming` is false.
  - xAI streams unless `providers.xai_tts.disable_streaming` is true.
  - Everything else builds the existing unary processor.
- **Fallback.** Attempt 0 streams. Any retry (attempt > 0) uses the unary request for that unit. The Runner only retries before the first audio frame is emitted, so audio already sent is never replayed.
- **`core/ttsunit` is not modified.** If a hook is missing, stop and treat it as a spec change.
- **Default Google voice:** `en-US-Chirp3-HD-Aoede` (`google.DefaultVoice`). Saved agents keep their voice.
- **Commands.**
  - Run each module's tests from its directory: `cd providers/google && go test ./...`, `cd providers/xai && go test ./... && go test -tags xaispeech ./...`, `cd server && go test ./... && go test -tags xaispeech ./...`.
  - Darwin cgo: `CGO_LDFLAGS=-Wl,-w`.
  - VAD in a worktree: `GOVOICE_ORT_LIB=/Users/raymondottun/Projects/go-voice/third_party/onnxruntime-osx-arm64-1.29.0/lib/libonnxruntime.dylib`.
  - Check exit codes, not `tail` output.
- **Commits:** one per task, only the named files. End each message with `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`.

## Review Focus

- **Barge-in mid-stream.** After an interruption, no further `OutputAudioFrame` is emitted, and the xAI connection is closed / the Google stream context is cancelled. Tasks 2 and 5 test this.
- **No replay after audio started.** A stream that fails after its first audio frame must not retry as unary (the caller would hear the sentence twice). Tasks 2 and 5 test this.
- **A bad voice or key is not hidden by the fallback.** A non-retryable error (xAI 401 on the WebSocket upgrade; Google bad request) is not retried, so the caller sees the real error. An xAI `error` message before audio is retried as unary, which returns the authoritative HTTP error. Tasks 2 and 5 test this.
- **Reply boundaries on xAI.** A unit of a new reply must not reuse the previous reply's connection. Task 5 tests this.
- **Chirp-named voices that are not Chirp 3 HD** (for example `en-US-Chirp-HD-D`) must not be routed to streaming. Task 1 tests this.
- **`MaxAttempts: 2`** (the failover policy) still reaches the unary fallback on attempt 1 before cross-provider failover. Task 2 tests this.

---

### Task 1: Google streaming wire code (voice check, stream runner, real client)

**Files:**
- Create: `providers/google/tts_stream.go`
- Test: `providers/google/tts_stream_test.go` (create)

**Interfaces:**
- Produces:
  - `func IsChirp3HD(voice string) bool`
  - `type StreamingTTSClient interface { TTSClient; StreamSynthesize(ctx context.Context, pieces []string, voice string, sampleRate int, onAudio func(pcm []byte) error) error }`
  - `func (c *RealGoogleTTSClient) StreamSynthesize(...)` (implements it)
  - unexported `synthStream`, `runSynthStream`, `dropWAVHeader`

- [ ] **Step 1: Write the failing tests** (create `providers/google/tts_stream_test.go`)

```go
package google

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	texttospeechpb "cloud.google.com/go/texttospeech/apiv1/texttospeechpb"
)

type recvStep func() (*texttospeechpb.StreamingSynthesizeResponse, error)

// fakeSynthStream scripts a StreamingSynthesize stream.
type fakeSynthStream struct {
	mu         sync.Mutex
	sent       []*texttospeechpb.StreamingSynthesizeRequest
	failSendAt int // index of the Send that returns sendErr; -1: none
	sendErr    error
	steps      []recvStep
	closedSend bool
}

func newFakeSynthStream(steps ...recvStep) *fakeSynthStream {
	return &fakeSynthStream{failSendAt: -1, steps: steps}
}

func (f *fakeSynthStream) Send(r *texttospeechpb.StreamingSynthesizeRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := len(f.sent)
	f.sent = append(f.sent, r)
	if n == f.failSendAt {
		return f.sendErr
	}
	return nil
}

func (f *fakeSynthStream) Recv() (*texttospeechpb.StreamingSynthesizeResponse, error) {
	f.mu.Lock()
	if len(f.steps) == 0 {
		f.mu.Unlock()
		return nil, io.EOF
	}
	s := f.steps[0]
	f.steps = f.steps[1:]
	f.mu.Unlock()
	return s()
}

func (f *fakeSynthStream) CloseSend() error {
	f.mu.Lock()
	f.closedSend = true
	f.mu.Unlock()
	return nil
}

func audioStep(b []byte) recvStep {
	return func() (*texttospeechpb.StreamingSynthesizeResponse, error) {
		return &texttospeechpb.StreamingSynthesizeResponse{AudioContent: b}, nil
	}
}

func errStep(err error) recvStep {
	return func() (*texttospeechpb.StreamingSynthesizeResponse, error) { return nil, err }
}

func TestIsChirp3HD(t *testing.T) {
	for voice, want := range map[string]bool{
		"en-US-Chirp3-HD-Aoede": true,
		"en-gb-chirp3-hd-kore":  true,
		"en-US-Journey-F":       false,
		"en-US-Neural2-C":       false,
		"en-US-Chirp-HD-D":      false, // an older Chirp family: not supported by streaming
		"":                      false,
	} {
		if got := IsChirp3HD(voice); got != want {
			t.Errorf("IsChirp3HD(%q) = %t, want %t", voice, got, want)
		}
	}
}

func TestRunSynthStream_ConfigThenPiecesInOrderThenCloseSend(t *testing.T) {
	s := newFakeSynthStream(audioStep([]byte{1, 2}))
	var got []byte
	err := runSynthStream(s, "en-US-Chirp3-HD-Aoede", "en-US", 24000, []string{"Hello there.", "How are you?"}, func(b []byte) error {
		got = append(got, b...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.sent) != 3 {
		t.Fatalf("sent %d messages, want config + 2 pieces", len(s.sent))
	}
	cfg := s.sent[0].GetStreamingConfig()
	if cfg == nil || cfg.GetVoice().GetName() != "en-US-Chirp3-HD-Aoede" || cfg.GetVoice().GetLanguageCode() != "en-US" {
		t.Fatalf("first message = %v, want the voice config", s.sent[0])
	}
	if ac := cfg.GetStreamingAudioConfig(); ac.GetAudioEncoding() != texttospeechpb.AudioEncoding_PCM || ac.GetSampleRateHertz() != 24000 {
		t.Fatalf("audio config = %v, want PCM at 24000", ac)
	}
	for i, want := range []string{"Hello there.", "How are you?"} {
		if text := s.sent[i+1].GetInput().GetText(); text != want {
			t.Errorf("piece %d = %q, want %q", i, text, want)
		}
	}
	if !s.closedSend {
		t.Error("CloseSend was not called")
	}
	if !bytes.Equal(got, []byte{1, 2}) {
		t.Errorf("audio = %v", got)
	}
}

func TestRunSynthStream_DeliversAudioBeforeTheStreamEnds(t *testing.T) {
	gate := make(chan struct{})
	s := newFakeSynthStream(audioStep([]byte{1, 2, 3, 4}), func() (*texttospeechpb.StreamingSynthesizeResponse, error) {
		<-gate
		return nil, io.EOF
	})
	got := make(chan []byte, 4)
	done := make(chan error, 1)
	go func() {
		done <- runSynthStream(s, "v", "en-US", 24000, []string{"Hi."}, func(b []byte) error {
			got <- append([]byte(nil), b...)
			return nil
		})
	}()
	select {
	case b := <-got:
		if !bytes.Equal(b, []byte{1, 2, 3, 4}) {
			t.Fatalf("first chunk = %v", b)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("audio was not delivered before the stream ended")
	}
	select {
	case <-done:
		t.Fatal("returned before the stream ended")
	default:
	}
	close(gate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestDropWAVHeader(t *testing.T) {
	hdr := []byte("RIFF\x00\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00\xc0\x5d\x00\x00\x80\xbb\x00\x00\x02\x00\x10\x00data\xff\xff\xff\xff")
	if got := dropWAVHeader(append(append([]byte(nil), hdr...), 9, 8)); !bytes.Equal(got, []byte{9, 8}) {
		t.Errorf("header + pcm = %v, want [9 8]", got)
	}
	if got := dropWAVHeader(hdr); len(got) != 0 {
		t.Errorf("header only = %v, want nothing to play", got)
	}
	if got := dropWAVHeader([]byte{1, 2, 3, 4}); !bytes.Equal(got, []byte{1, 2, 3, 4}) {
		t.Errorf("plain pcm changed: %v", got)
	}
	if got := dropWAVHeader([]byte("RIFF\x00\x00\x00\x00WAVE")); len(got) != 0 {
		t.Errorf("RIFF without a data chunk = %v, want nothing", got)
	}
}

func TestRunSynthStream_DropsALeadingWAVHeaderOnlyOnTheFirstChunk(t *testing.T) {
	hdr := []byte("RIFF\x00\x00\x00\x00WAVEdata\xff\xff\xff\xff")
	s := newFakeSynthStream(audioStep(append(append([]byte(nil), hdr...), 1, 2)), audioStep([]byte("RIFFxxxxxxxxxxxx")))
	var got []byte
	if err := runSynthStream(s, "v", "en-US", 24000, []string{"Hi."}, func(b []byte) error { got = append(got, b...); return nil }); err != nil {
		t.Fatal(err)
	}
	if want := append([]byte{1, 2}, []byte("RIFFxxxxxxxxxxxx")...); !bytes.Equal(got, want) {
		t.Errorf("audio = %q, want %q (a later chunk that happens to start with RIFF is audio)", got, want)
	}
}

func TestRunSynthStream_MapsStreamErrors(t *testing.T) {
	s := newFakeSynthStream(errStep(status.Error(codes.Unavailable, "backend down")))
	err := runSynthStream(s, "v", "en-US", 24000, []string{"Hi."}, func([]byte) error { return nil })
	var pe *ProviderError
	if !errors.As(err, &pe) || !pe.Retryable {
		t.Fatalf("err = %v, want a retryable ProviderError", err)
	}
}

func TestRunSynthStream_SendEOFReportsTheRecvError(t *testing.T) {
	s := newFakeSynthStream(errStep(status.Error(codes.Unauthenticated, "bad key")))
	s.failSendAt, s.sendErr = 1, io.EOF // the stream already ended: gRPC reports the cause through Recv
	err := runSynthStream(s, "v", "en-US", 24000, []string{"Hi."}, func([]byte) error { return nil })
	var pe *ProviderError
	if !errors.As(err, &pe) || pe.Retryable {
		t.Fatalf("err = %v, want the non-retryable cause from Recv", err)
	}
}

func TestRunSynthStream_OnAudioErrorStopsReading(t *testing.T) {
	boom := errors.New("boom")
	s := newFakeSynthStream(audioStep([]byte{1}), audioStep([]byte{2}))
	err := runSynthStream(s, "v", "en-US", 24000, []string{"Hi."}, func([]byte) error { return boom })
	if err != boom {
		t.Fatalf("err = %v, want the onAudio error unchanged", err)
	}
	if len(s.steps) != 1 {
		t.Errorf("%d steps left, want 1: reading must stop at the first onAudio error", len(s.steps))
	}
}
```

- [ ] **Step 2: Run them and confirm they fail**

Run: `cd providers/google && go test -run 'TestIsChirp3HD|TestRunSynthStream|TestDropWAVHeader' -count=1 .`
Expected: build failure (`IsChirp3HD`, `runSynthStream`, `dropWAVHeader` undefined).

- [ ] **Step 3: Implement** (create `providers/google/tts_stream.go`)

```go
package google

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"

	"govoice/core/netpool"

	texttospeechpb "cloud.google.com/go/texttospeech/apiv1/texttospeechpb"
)

// IsChirp3HD reports whether voice is a Chirp 3: HD voice ("en-US-Chirp3-HD-Aoede"), the only
// voice family StreamingSynthesize supports.
func IsChirp3HD(voice string) bool {
	return strings.Contains(strings.ToLower(voice), "chirp3-hd")
}

// StreamingTTSClient is a TTSClient that can also synthesize over one StreamingSynthesize stream.
type StreamingTTSClient interface {
	TTSClient
	// StreamSynthesize sends pieces, in order, on one stream and passes the PCM16LE audio to
	// onAudio as it arrives. It returns nil once the stream ended cleanly, otherwise the first
	// error: a provider error, or the one onAudio returned (unchanged).
	StreamSynthesize(ctx context.Context, pieces []string, voice string, sampleRate int, onAudio func(pcm []byte) error) error
}

var _ StreamingTTSClient = (*RealGoogleTTSClient)(nil)

// StreamSynthesize opens one StreamingSynthesize stream on the client's (warm) gRPC connection.
func (c *RealGoogleTTSClient) StreamSynthesize(ctx context.Context, pieces []string, voice string, sampleRate int, onAudio func([]byte) error) error {
	netpool.CountRequest("google", "tts")
	stream, err := c.client.StreamingSynthesize(ctx)
	if err != nil {
		return MapError("tts", err)
	}
	return runSynthStream(stream, voice, voiceLanguage(voice, c.languageCode), sampleRate, pieces, onAudio)
}

// synthStream is the part of the gRPC stream the wire code uses (tests fake it).
type synthStream interface {
	Send(*texttospeechpb.StreamingSynthesizeRequest) error
	Recv() (*texttospeechpb.StreamingSynthesizeResponse, error)
	CloseSend() error
}

// runSynthStream sends the voice config and the pieces, half-closes, and delivers audio until the
// response stream ends.
func runSynthStream(s synthStream, voice, language string, sampleRate int, pieces []string, onAudio func([]byte) error) error {
	sendErr := s.Send(&texttospeechpb.StreamingSynthesizeRequest{StreamingRequest: &texttospeechpb.StreamingSynthesizeRequest_StreamingConfig{
		StreamingConfig: &texttospeechpb.StreamingSynthesizeConfig{
			Voice: &texttospeechpb.VoiceSelectionParams{LanguageCode: language, Name: voice},
			StreamingAudioConfig: &texttospeechpb.StreamingAudioConfig{
				AudioEncoding:   texttospeechpb.AudioEncoding_PCM,
				SampleRateHertz: int32(sampleRate),
			},
		},
	}})
	for _, piece := range pieces {
		if sendErr != nil {
			break
		}
		sendErr = s.Send(&texttospeechpb.StreamingSynthesizeRequest{StreamingRequest: &texttospeechpb.StreamingSynthesizeRequest_Input{
			Input: &texttospeechpb.StreamingSynthesisInput{InputSource: &texttospeechpb.StreamingSynthesisInput_Text{Text: piece}},
		}})
	}
	switch {
	case sendErr == nil:
		if err := s.CloseSend(); err != nil {
			return MapError("tts", err)
		}
	case !errors.Is(sendErr, io.EOF):
		return MapError("tts", sendErr)
	}
	// io.EOF from Send means the stream already ended; Recv returns the real cause.
	first := true
	for {
		resp, err := s.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return MapError("tts", err)
		}
		audio := resp.GetAudioContent()
		if len(audio) == 0 {
			continue
		}
		if first {
			audio, first = dropWAVHeader(audio), false
		}
		if len(audio) == 0 {
			continue
		}
		if err := onAudio(audio); err != nil {
			return err
		}
	}
}

// dropWAVHeader removes a RIFF/WAVE header from the first audio chunk, if there is one.
// ponytail: looks for the "data" chunk id in the first 128 bytes only; a header split across
// chunks would need buffering. Streaming PCM is not expected to carry a header at all.
func dropWAVHeader(b []byte) []byte {
	if len(b) < 12 || !bytes.Equal(b[:4], []byte("RIFF")) {
		return b
	}
	if i := bytes.Index(b[:min(len(b), 128)], []byte("data")); i >= 0 && i+8 <= len(b) {
		return b[i+8:]
	}
	return nil
}
```

- [ ] **Step 4: Run them and confirm they pass**

Run: `cd providers/google && go vet ./... && go test -run 'TestIsChirp3HD|TestRunSynthStream|TestDropWAVHeader' -count=1 -v . 2>&1 | grep -E "^(--- |ok|FAIL)"`
Expected: all PASS. If `MapError` maps `codes.Unauthenticated` as retryable, read `providers/google/errors.go` (the `codes.` switch around line 240) and fix the test's expectation to match the mapping; do not change the mapping.

- [ ] **Step 5: Commit**

```bash
git add providers/google/tts_stream.go providers/google/tts_stream_test.go
git commit -m "feat(google): StreamingSynthesize wire code and Chirp 3 HD voice check"
```

---

### Task 2: Google `StreamingTTSProcessor` on the shared Runner

**Files:**
- Modify: `providers/google/tts.go` (extract the shared defaults; add `DefaultVoice`)
- Create: `providers/google/tts_streaming.go`
- Test: `providers/google/tts_streaming_test.go` (create)

**Interfaces:**
- Consumes: `StreamingTTSClient`, `IsChirp3HD` (Task 1); `ttsunit.New`, `ttsunit.Unit` (`Write`, `Finish`, `Frame`); `classify`, `splitUnit`, `breakerOpen`, `ttsRetryPolicy` (existing).
- Produces:
  - `const DefaultVoice = "en-US-Journey-F"` (Task 7 changes the value)
  - `func normalizeTTSConfig(cfg TTSConfig) TTSConfig`
  - `type StreamingTTSProcessor struct{ *ttsunit.Runner; ... }`
  - `func NewStreamingTTSProcessor(cfg TTSConfig) (*StreamingTTSProcessor, error)`
  - methods `Warm(ctx) error`, `Close() error`

- [ ] **Step 1: Write the failing tests** (create `providers/google/tts_streaming_test.go`)

```go
package google

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/resilience"
)

// streamTestClient fakes both the unary and the streaming client.
type streamTestClient struct {
	mockTTSClient // unary: SynthesizeSpeech and its recorded calls
	streamFn      func(ctx context.Context, pieces []string, onAudio func([]byte) error) error
	smu           sync.Mutex
	streams       [][]string
	active, peak  atomic.Int32
}

func (c *streamTestClient) StreamSynthesize(ctx context.Context, pieces []string, voice string, sampleRate int, onAudio func([]byte) error) error {
	c.smu.Lock()
	c.streams = append(c.streams, append([]string(nil), pieces...))
	fn := c.streamFn
	c.smu.Unlock()
	n := c.active.Add(1)
	defer c.active.Add(-1)
	for {
		p := c.peak.Load()
		if n <= p || c.peak.CompareAndSwap(p, n) {
			break
		}
	}
	if fn == nil {
		return nil
	}
	return fn(ctx, pieces, onAudio)
}

func (c *streamTestClient) streamCount() int {
	c.smu.Lock()
	defer c.smu.Unlock()
	return len(c.streams)
}

func (c *streamTestClient) unaryCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

type stHarness struct {
	proc *StreamingTTSProcessor
	sess *core.Session
	sink *core.PassthroughProcessor
}

func stStart(t *testing.T, client *streamTestClient, maxAttempts int) *stHarness {
	t.Helper()
	cfg := DefaultTTSConfig()
	cfg.Client = client
	cfg.RetryPolicy = resilience.Policy{MaxAttempts: maxAttempts, InitialBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond}
	proc, err := NewStreamingTTSProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sink := &core.PassthroughProcessor{}
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
	t.Cleanup(func() { sess.Close() })
	return &stHarness{proc: proc, sess: sess, sink: sink}
}

func (h *stHarness) say(t *testing.T, id uint64, text string, final bool) {
	t.Helper()
	if err := h.sess.Post(&core.SynthesisUnitFrame{Turn: "t1", Gen: 1, UnitID: id, Text: text, IsFinal: final}); err != nil {
		t.Fatal(err)
	}
}

func stFrames[T core.Frame](h *stHarness) (out []T) {
	for _, f := range h.sink.GetFrames() {
		if v, ok := f.(T); ok {
			out = append(out, v)
		}
	}
	return out
}

func stWait(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(3 * time.Second); !cond(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func retryableErr() error {
	return &ProviderError{Capability: "tts", BaseErr: ErrProviderUnavailable, SafeMessage: "backend down", Retryable: true}
}

func TestStreamingTTS_EmitsAudioBeforeTheStreamEnds(t *testing.T) {
	release := make(chan struct{})
	client := &streamTestClient{streamFn: func(ctx context.Context, _ []string, onAudio func([]byte) error) error {
		if err := onAudio(make([]byte, 1920)); err != nil {
			return err
		}
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		return onAudio(make([]byte, 960))
	}}
	h := stStart(t, client, 3)
	h.say(t, 1, "Hello there.", true)
	stWait(t, "the first audio frame while the stream is still open", func() bool { return len(stFrames[*core.OutputAudioFrame](h)) >= 1 })
	if n := len(stFrames[*core.TTSStoppedFrame](h)); n != 0 {
		t.Fatalf("%d TTSStopped before the stream ended", n)
	}
	close(release)
	stWait(t, "TTSStopped", func() bool { return len(stFrames[*core.TTSStoppedFrame](h)) == 1 })
	audio := stFrames[*core.OutputAudioFrame](h)
	if last := audio[len(audio)-1]; !last.IsFinal || !last.TurnFinal {
		t.Errorf("last frame final markers = %t %t, want true true", last.IsFinal, last.TurnFinal)
	}
}

func TestStreamingTTS_UnitsPlayInOrderOneStreamAtATime(t *testing.T) {
	first := make(chan struct{})
	var order []string
	var omu sync.Mutex
	client := &streamTestClient{streamFn: func(ctx context.Context, pieces []string, onAudio func([]byte) error) error {
		omu.Lock()
		order = append(order, pieces[0])
		n := len(order)
		omu.Unlock()
		if n == 1 {
			select {
			case <-first:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return onAudio(make([]byte, 1920))
	}}
	h := stStart(t, client, 3)
	h.say(t, 1, "one", false)
	h.say(t, 2, "two", true)
	stWait(t, "the first stream to start", func() bool { return client.streamCount() >= 1 })
	time.Sleep(50 * time.Millisecond)
	if client.streamCount() != 1 {
		t.Fatalf("%d streams open while unit 1 is still streaming, want 1", client.streamCount())
	}
	close(first)
	stWait(t, "both units to finish", func() bool { return len(stFrames[*core.TTSStoppedFrame](h)) == 2 })
	if client.peak.Load() != 1 {
		t.Errorf("peak concurrent streams = %d, want 1", client.peak.Load())
	}
	omu.Lock()
	defer omu.Unlock()
	if len(order) != 2 || order[0] != "one" || order[1] != "two" {
		t.Errorf("stream order = %v", order)
	}
}

func TestStreamingTTS_LongUnitIsOneStreamWithSeveralPieces(t *testing.T) {
	client := &streamTestClient{streamFn: func(_ context.Context, _ []string, onAudio func([]byte) error) error {
		return onAudio(make([]byte, 1920))
	}}
	h := stStart(t, client, 3)
	h.say(t, 1, strings.Repeat("word ", 60), true) // 300 runes
	stWait(t, "TTSStopped", func() bool { return len(stFrames[*core.TTSStoppedFrame](h)) == 1 })
	if client.streamCount() != 1 || client.unaryCount() != 0 {
		t.Fatalf("streams=%d unary=%d, want one stream and no unary", client.streamCount(), client.unaryCount())
	}
	client.smu.Lock()
	defer client.smu.Unlock()
	if got := client.streams[0]; len(got) != 2 {
		t.Errorf("pieces = %d, want 2 (300 runes at 250 per piece)", len(got))
	}
}

func TestStreamingTTS_FailedStreamRetriesAsUnary(t *testing.T) {
	client := &streamTestClient{streamFn: func(context.Context, []string, func([]byte) error) error { return retryableErr() }}
	h := stStart(t, client, 3)
	h.say(t, 1, "Hello there.", true)
	stWait(t, "TTSStopped", func() bool { return len(stFrames[*core.TTSStoppedFrame](h)) == 1 })
	if client.streamCount() != 1 || client.unaryCount() != 1 {
		t.Fatalf("streams=%d unary=%d, want the stream then one unary retry", client.streamCount(), client.unaryCount())
	}
	if len(stFrames[*core.OutputAudioFrame](h)) == 0 {
		t.Error("the unary fallback produced no audio")
	}
	if n := len(stFrames[*core.ErrorFrame](h)); n != 0 {
		t.Errorf("%d ErrorFrames, want none: the fallback recovered", n)
	}
}

func TestStreamingTTS_TwoAttemptsStillReachTheUnaryFallback(t *testing.T) {
	// The failover policy allows 2 attempts: stream, then unary, then cross-provider failover.
	client := &streamTestClient{streamFn: func(context.Context, []string, func([]byte) error) error { return retryableErr() }}
	h := stStart(t, client, 2)
	h.say(t, 1, "Hello there.", true)
	stWait(t, "TTSStopped", func() bool { return len(stFrames[*core.TTSStoppedFrame](h)) == 1 })
	if client.unaryCount() != 1 {
		t.Fatalf("unary calls = %d, want 1", client.unaryCount())
	}
}

func TestStreamingTTS_NoReplayOnceAudioHasStarted(t *testing.T) {
	client := &streamTestClient{streamFn: func(_ context.Context, _ []string, onAudio func([]byte) error) error {
		if err := onAudio(make([]byte, 3000)); err != nil { // two full 20 ms frames go out
			return err
		}
		return retryableErr()
	}}
	h := stStart(t, client, 3)
	h.say(t, 1, "Hello there.", true)
	stWait(t, "the error", func() bool { return len(stFrames[*core.ErrorFrame](h)) == 1 })
	stWait(t, "the closing TTSStopped", func() bool { return len(stFrames[*core.TTSStoppedFrame](h)) == 1 })
	if client.unaryCount() != 0 {
		t.Fatalf("%d unary calls: audio already played, a retry would replay the sentence", client.unaryCount())
	}
}

func TestStreamingTTS_ClientErrorIsNotRetried(t *testing.T) {
	bad := &ProviderError{Capability: "tts", BaseErr: ErrProviderBadRequest, SafeMessage: "unknown voice"}
	client := &streamTestClient{streamFn: func(context.Context, []string, func([]byte) error) error { return bad }}
	h := stStart(t, client, 3)
	h.say(t, 1, "Hello there.", true)
	stWait(t, "the error", func() bool { return len(stFrames[*core.ErrorFrame](h)) == 1 })
	if client.streamCount() != 1 || client.unaryCount() != 0 {
		t.Fatalf("streams=%d unary=%d, want exactly one attempt: a bad voice must surface", client.streamCount(), client.unaryCount())
	}
}

func TestStreamingTTS_InterruptionCancelsTheStream(t *testing.T) {
	canceled := make(chan struct{})
	client := &streamTestClient{streamFn: func(ctx context.Context, _ []string, onAudio func([]byte) error) error {
		if err := onAudio(make([]byte, 3000)); err != nil {
			return err
		}
		<-ctx.Done()
		close(canceled)
		return ctx.Err()
	}}
	h := stStart(t, client, 3)
	h.say(t, 1, "A long sentence.", true)
	stWait(t, "audio", func() bool { return len(stFrames[*core.OutputAudioFrame](h)) >= 1 })
	_ = h.sess.Post(&core.InterruptionFrame{})
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("the stream context was not cancelled on interruption")
	}
	time.Sleep(30 * time.Millisecond)
	if n := len(stFrames[*core.TTSStoppedFrame](h)); n != 0 {
		t.Errorf("%d TTSStopped after an interruption", n)
	}
	if n := len(stFrames[*core.ErrorFrame](h)); n != 0 {
		t.Errorf("%d ErrorFrames after an interruption", n)
	}
}

func TestNewStreamingTTSProcessor_NeedsAStreamingClient(t *testing.T) {
	cfg := DefaultTTSConfig()
	cfg.Client = &mockTTSClient{}
	if _, err := NewStreamingTTSProcessor(cfg); err == nil {
		t.Fatal("a unary-only client was accepted")
	}
	if _, err := NewStreamingTTSProcessor(DefaultTTSConfig()); err == nil {
		t.Fatal("a missing client was accepted")
	}
}
```

- [ ] **Step 2: Run them and confirm they fail**

Run: `cd providers/google && go test -run 'TestStreamingTTS|TestNewStreamingTTSProcessor' -count=1 .`
Expected: build failure (`NewStreamingTTSProcessor` undefined).

- [ ] **Step 3: Extract the shared defaults** (`providers/google/tts.go`)

Add the constant next to `DefaultTTSConfig`, and use it there:

```go
// DefaultVoice is the voice agents get when none is set.
const DefaultVoice = "en-US-Journey-F"
```

In `DefaultTTSConfig`, change `Voice: "en-US-Journey-F",` to `Voice: DefaultVoice,`.

Replace the default-filling block at the top of `NewTTSProcessor` (from `if cfg.Voice == "" {` through the `CircuitBreaker` default) with a call to a new function, and add the function:

```go
// normalizeTTSConfig fills the defaults the unary and streaming processors share.
func normalizeTTSConfig(cfg TTSConfig) TTSConfig {
	if cfg.Voice == "" {
		cfg.Voice = DefaultVoice
	}
	if cfg.SampleRate <= 0 {
		cfg.SampleRate = 24000
	}
	if cfg.MaxSynthesisUnitChars <= 0 {
		cfg.MaxSynthesisUnitChars = 250
	}
	if cfg.ChunkDuration <= 0 {
		cfg.ChunkDuration = 20 * time.Millisecond
	}
	if cfg.CircuitBreaker == nil {
		cfg.CircuitBreaker = resilience.NewBreaker(resilience.DefaultConfig())
	}
	return cfg
}
```

and in `NewTTSProcessor`:

```go
func NewTTSProcessor(cfg TTSConfig) (*TTSProcessor, error) {
	cfg = normalizeTTSConfig(cfg)
	p := &TTSProcessor{cfg: cfg}
	p.cond = sync.NewCond(&p.mu)
	return p, nil
}
```

- [ ] **Step 4: Implement the processor** (create `providers/google/tts_streaming.go`)

```go
package google

import (
	"context"
	"errors"

	"govoice/core/netpool"
	"govoice/core/ttsunit"
)

// StreamingTTSProcessor speaks Chirp 3 HD voices over StreamingSynthesize: audio is emitted as it
// arrives instead of after the whole sentence is synthesized. Units play in order on the shared
// ttsunit.Runner (barge-in cancel, retries before audio, tracing, usage). One stream per unit,
// because a Google stream has no per-utterance boundary. A retry after a failed stream uses the
// unary request for that unit, so a stream problem never silences the call.
type StreamingTTSProcessor struct {
	*ttsunit.Runner
	cfg    TTSConfig
	client StreamingTTSClient
}

// NewStreamingTTSProcessor needs cfg.Client to implement StreamingTTSClient.
func NewStreamingTTSProcessor(cfg TTSConfig) (*StreamingTTSProcessor, error) {
	cfg = normalizeTTSConfig(cfg)
	if cfg.RetryPolicy.MaxAttempts <= 0 {
		cfg.RetryPolicy = ttsRetryPolicy()
	}
	sc, ok := cfg.Client.(StreamingTTSClient)
	if !ok {
		return nil, errors.New("google tts streaming: the client does not implement StreamingTTSClient")
	}
	p := &StreamingTTSProcessor{cfg: cfg, client: sc}
	p.Runner = ttsunit.New(ttsunit.Config{
		Provider: "google", Model: cfg.Voice, SampleRate: cfg.SampleRate,
		Breaker: cfg.CircuitBreaker, Policy: cfg.RetryPolicy, Classify: classify,
		Attempt: p.attempt,
	})
	return p, nil
}

// attempt synthesizes one unit. Attempt 0 streams; a retry (the Runner only retries before the
// first audio frame) synthesizes the pieces with the unary request and plays them as one unit.
func (p *StreamingTTSProcessor) attempt(ctx context.Context, u *ttsunit.Unit, attempt int) error {
	pieces := splitUnit(u.Frame.Text, p.cfg.MaxSynthesisUnitChars)
	if len(pieces) == 0 {
		return u.Finish()
	}
	if attempt == 0 {
		if err := p.client.StreamSynthesize(ctx, pieces, p.cfg.Voice, p.cfg.SampleRate, u.Write); err != nil {
			return err
		}
		return u.Finish()
	}
	var audio []byte // nothing is written until every piece is synthesized, so this attempt may retry too
	for _, piece := range pieces {
		pcm, err := p.client.SynthesizeSpeech(ctx, piece, p.cfg.Voice, p.cfg.SampleRate)
		if err != nil {
			return err
		}
		audio = append(audio, pcm...)
	}
	if err := u.Write(audio); err != nil {
		return err
	}
	return u.Finish()
}

// Warm delegates to the synthesis client when it can warm itself.
func (p *StreamingTTSProcessor) Warm(ctx context.Context) error {
	w, ok := p.cfg.Client.(netpool.Warmer)
	if !ok || breakerOpen(p.cfg.CircuitBreaker, "tts") {
		return nil
	}
	return w.Warm(ctx)
}

// Close cancels in-flight synthesis and closes the client.
func (p *StreamingTTSProcessor) Close() error {
	err := p.Runner.Close()
	if c, ok := p.cfg.Client.(interface{ Close() error }); ok {
		_ = c.Close()
	}
	return err
}
```

- [ ] **Step 5: Run the whole module**

Run: `cd providers/google && go vet ./... && CGO_LDFLAGS=-Wl,-w go test ./... -count=1`
Expected: PASS, including the existing unary tests (the refactor of `NewTTSProcessor` must not change them).

- [ ] **Step 6: Prove the new tests catch regressions, then restore** (do not commit the breakage)

1. In `tts_streaming.go`, change `if attempt == 0 {` to `if true {`. `TestStreamingTTS_FailedStreamRetriesAsUnary` must FAIL.
2. Restore. Change `p.client.StreamSynthesize(ctx, pieces, ...)` to pass `pieces[:1]`. `TestStreamingTTS_LongUnitIsOneStreamWithSeveralPieces` must FAIL.
3. Restore. Run `git diff providers/google/tts_streaming.go`. It must print nothing.

- [ ] **Step 7: Commit**

```bash
git add providers/google/tts.go providers/google/tts_streaming.go providers/google/tts_streaming_test.go
git commit -m "feat(google): streaming TTS processor for Chirp 3 HD on the shared Runner"
```

---

### Task 3: Server routing for Google, kill switches, config

**Files:**
- Modify: `server/config.go` (both providers' `disable_streaming`)
- Modify: `server/registry.go` (the `google.tts` factory)
- Modify: `server/testdata/effective_development.json`, `server/testdata/effective_production.json` (regenerated)
- Test: `server/registry_test.go`

**Interfaces:**
- Consumes: `google.IsChirp3HD`, `google.NewStreamingTTSProcessor` (Tasks 1-2).
- Produces: `Config.Providers.GoogleTTS.DisableStreaming`, `Config.Providers.XAITTS.DisableStreaming` (Task 6 reads the second).

- [ ] **Step 1: Write the failing test** (append to `server/registry_test.go`; add `"govoice/providers/google"` to its imports if missing)

```go
// Chirp 3 HD voices stream, every other voice stays unary, and the kill switch restores unary.
func TestGoogleTTSFactoryStreamsChirp3HDOnly(t *testing.T) {
	reg := NewRegistry()
	f := reg.factories["google.tts"]
	for _, c := range []struct {
		voice     string
		disable   bool
		streaming bool
	}{
		{"en-US-Chirp3-HD-Aoede", false, true},
		{"en-US-Journey-F", false, false},
		{"en-US-Chirp-HD-D", false, false},
		{"en-US-Chirp3-HD-Aoede", true, false},
	} {
		cfg := DefaultConfig(ProfileDevelopment)
		cfg.Providers.GoogleTTS.DisableStreaming = c.disable
		p, err := f(cfg, StageSpec{Voice: c.voice, Secret: []byte("test-api-key-0123456789")})
		if err != nil {
			t.Fatalf("%s: %v", c.voice, err)
		}
		if _, streaming := p.(*google.StreamingTTSProcessor); streaming != c.streaming {
			t.Errorf("voice %s disable=%t: streaming=%t, want %t (%T)", c.voice, c.disable, streaming, c.streaming, p)
		}
		if cl, ok := p.(interface{ Close() error }); ok {
			_ = cl.Close()
		}
	}
}
```

- [ ] **Step 2: Run it and confirm it fails**

Run: `cd server && CGO_LDFLAGS=-Wl,-w go test -run TestGoogleTTSFactoryStreamsChirp3HDOnly -count=1 .`
Expected: build failure (`DisableStreaming` undefined).

- [ ] **Step 3: Implement.**

`server/config.go`, in the `Providers` struct:

```go
		GoogleTTS struct {
			SampleRate int `yaml:"sample_rate"`
			// DisableStreaming turns StreamingSynthesize off for Chirp 3 HD voices: they then use
			// the unary request like every other voice.
			DisableStreaming bool `yaml:"disable_streaming"`
		} `yaml:"google_tts"`
```

```go
		XAITTS struct {
			Endpoint string `yaml:"endpoint"`
			// DisableStreaming turns the WebSocket off: every unit is one POST /v1/tts again.
			DisableStreaming bool `yaml:"disable_streaming"`
		} `yaml:"xai_tts"`
```

`server/registry.go`, at the end of the `google.tts` factory replace `return google.NewTTSProcessor(ttsCfg)` with:

```go
		if google.IsChirp3HD(ttsCfg.Voice) && !cfg.Providers.GoogleTTS.DisableStreaming {
			return google.NewStreamingTTSProcessor(ttsCfg)
		}
		return google.NewTTSProcessor(ttsCfg)
```

- [ ] **Step 4: Regenerate the effective-config snapshots and review the diff**

Run: `cd server && UPDATE_GOLDEN=1 CGO_LDFLAGS=-Wl,-w go test -run Effective -count=1 . && git diff testdata/ | grep '^[+-] '`
Expected: only the two new keys per snapshot (`"DisableStreaming": false` under `GoogleTTS` and `XAITTS`). Anything else is a bug: stop and investigate.

- [ ] **Step 5: Run the server module**

Run: `cd server && CGO_LDFLAGS=-Wl,-w go test ./... -count=1`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add server/config.go server/registry.go server/registry_test.go server/testdata/effective_development.json server/testdata/effective_production.json
git commit -m "feat(server): stream Chirp 3 HD voices; disable_streaming switches for Google and xAI TTS"
```

---

### Task 4: xAI streaming wire (URL, messages, dial)

**Files:**
- Modify: `providers/xai/tts.go` (extract shared defaults)
- Create: `providers/xai/tts_stream.go` (build tag `xaispeech`)
- Test: `providers/xai/tts_stream_test.go` (create, build tag `xaispeech`)

**Interfaces:**
- Produces:
  - `func normalizeTTSConfig(cfg TTSConfig) TTSConfig`
  - `func streamURL(cfg TTSConfig) (string, error)`
  - `type wsMsg struct{ Type, Delta, Message, Error string }`
  - `func wsError(m wsMsg) *ProviderError`
  - `func dialTTS(ctx context.Context, pool *netpool.Pool, wsURL, apiKey string) (*websocket.Conn, error)`

- [ ] **Step 1: Write the failing tests** (create `providers/xai/tts_stream_test.go`)

```go
//go:build xaispeech

package xai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"govoice/core/netpool"
)

func TestStreamURL(t *testing.T) {
	cfg := normalizeTTSConfig(TTSConfig{Endpoint: "https://api.x.ai/v1/tts", Voice: "ara", Language: "fr", SampleRate: 16000})
	got, err := streamURL(cfg)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "wss" || u.Host != "api.x.ai" || u.Path != "/v1/tts" {
		t.Errorf("url = %s, want wss://api.x.ai/v1/tts", got)
	}
	q := u.Query()
	for k, want := range map[string]string{"voice": "ara", "language": "fr", "codec": "pcm", "sample_rate": "16000", "optimize_streaming_latency": "1"} {
		if q.Get(k) != want {
			t.Errorf("query %s = %q, want %q", k, q.Get(k), want)
		}
	}
	if got, _ := streamURL(normalizeTTSConfig(TTSConfig{Endpoint: "http://127.0.0.1:9/v1/tts"})); got == "" || got[:5] != "ws://" {
		t.Errorf("http endpoint became %q, want ws://", got)
	}
	if _, err := streamURL(TTSConfig{Endpoint: "api.x.ai/v1/tts"}); err == nil {
		t.Error("an endpoint without a scheme was accepted")
	}
	if cfg.Language != "fr" || normalizeTTSConfig(TTSConfig{}).Language != "en" {
		t.Error("language must default to en: the WebSocket requires it")
	}
}

func TestWSErrorIsRetryableAndSanitized(t *testing.T) {
	pe := wsError(wsMsg{Type: "error", Message: "voice not available, key xai-abcdefghijklmnopqrstuv"})
	if pe.Class != ClassificationRetryable {
		t.Errorf("class = %v, want retryable: the unary retry returns the authoritative error", pe.Class)
	}
	if !strings.Contains(pe.Message, "xai-[REDACTED]") || strings.Contains(pe.Message, "abcdefghijkl") {
		t.Errorf("message = %q, want the key redacted", pe.Message)
	}
	if got := wsError(wsMsg{Type: "error", Error: "from error field"}); !strings.Contains(got.Message, "from error field") {
		t.Errorf("message = %q, want the error field when message is empty", got.Message)
	}
}

func TestDialTTS_MapsUpgradeFailures(t *testing.T) {
	for status, wantClass := range map[int]ErrorClassification{
		http.StatusUnauthorized:       ClassificationNonRetryable,
		http.StatusServiceUnavailable: ClassificationRetryable,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, err := dialTTS(ctx, netpool.Get(netpool.Options{Provider: "xai", Stage: "tts", HTTP1Only: true}), "ws"+srv.URL[len("http"):]+"/v1/tts", "k")
		cancel()
		srv.Close()
		var pe *ProviderError
		if !errors.As(err, &pe) || pe.Class != wantClass || pe.StatusCode != status {
			t.Errorf("status %d: err = %v, want class %v with that status", status, err, wantClass)
		}
	}
}
```

- [ ] **Step 2: Run them and confirm they fail**

Run: `cd providers/xai && go test -tags xaispeech -run 'TestStreamURL|TestWSError|TestDialTTS' -count=1 .`
Expected: build failure (`normalizeTTSConfig`, `streamURL`, `wsMsg` undefined).

- [ ] **Step 3: Extract the shared defaults** (`providers/xai/tts.go`)

Add:

```go
// normalizeTTSConfig fills the defaults the unary and streaming processors share.
func normalizeTTSConfig(cfg TTSConfig) TTSConfig {
	if cfg.Endpoint == "" {
		cfg.Endpoint = "https://api.x.ai/v1/tts"
	}
	if cfg.Voice == "" {
		cfg.Voice = "eve"
	}
	if cfg.Language == "" {
		cfg.Language = "en" // the WebSocket requires a language
	}
	if cfg.SampleRate <= 0 {
		cfg.SampleRate = 24000
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.RetryPolicy.MaxAttempts <= 0 {
		cfg.RetryPolicy.MaxAttempts = resilience.DefaultPolicy().MaxAttempts
	}
	cfg.RetryPolicy.AttemptTimeout = cfg.Timeout // Timeout is the one knob: it bounds each synthesis request
	return cfg
}
```

and at the top of `NewTTSProcessor` replace the block from `if cfg.Endpoint == "" {` through `cfg.RetryPolicy.AttemptTimeout = cfg.Timeout ...` with `cfg = normalizeTTSConfig(cfg)`. Keep the `client := cfg.Client` lines that follow.

- [ ] **Step 4: Implement** (create `providers/xai/tts_stream.go`)

```go
//go:build xaispeech

package xai

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/coder/websocket"

	"govoice/core/netpool"
)

// wsMsg is one JSON message on the TTS WebSocket, in both directions.
type wsMsg struct {
	Type    string `json:"type"`
	Delta   string `json:"delta,omitempty"`   // text.delta: text; audio.delta: base64 PCM16LE
	Message string `json:"message,omitempty"` // error
	Error   string `json:"error,omitempty"`   // error (alternate field)
}

// streamURL is the WebSocket endpoint for cfg: the configured endpoint with the scheme swapped
// (https -> wss, http -> ws) and the audio parameters in the query.
func streamURL(cfg TTSConfig) (string, error) {
	u, err := url.Parse(cfg.Endpoint)
	if err != nil {
		return "", fmt.Errorf("xai tts endpoint: %w", err)
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	case "wss", "ws":
	default:
		return "", fmt.Errorf("xai tts endpoint %q needs an http(s) or ws(s) scheme", cfg.Endpoint)
	}
	q := url.Values{}
	q.Set("voice", cfg.Voice)
	q.Set("language", cfg.Language)
	q.Set("codec", "pcm")
	q.Set("sample_rate", strconv.Itoa(cfg.SampleRate))
	q.Set("optimize_streaming_latency", "1")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// wsError turns a server error message into a retryable ProviderError: the unary retry returns the
// authoritative classification (a bad voice is a 400 there), so the stream never hides it.
func wsError(m wsMsg) *ProviderError {
	msg := m.Message
	if msg == "" {
		msg = m.Error
	}
	return &ProviderError{Capability: "tts", Code: "STREAM_ERROR", Message: SanitizeString(msg), Class: ClassificationRetryable}
}

// dialTTS opens the TTS WebSocket through the shared HTTP/1.1 pool. A rejected upgrade is mapped
// like an HTTP failure (401 is a client error, 5xx is retryable); a transport error is retryable.
func dialTTS(ctx context.Context, pool *netpool.Pool, wsURL, apiKey string) (*websocket.Conn, error) {
	opts := &websocket.DialOptions{HTTPClient: pool.Client(0), HTTPHeader: http.Header{"Authorization": {"Bearer " + apiKey}}}
	c, resp, err := websocket.Dial(ctx, wsURL, opts)
	if err != nil {
		var body string
		if resp != nil && resp.Body != nil {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			body = string(b)
		}
		return nil, MapHTTPError("tts", resp, body, err)
	}
	c.SetReadLimit(4 << 20)
	return c, nil
}
```

- [ ] **Step 5: Run the tests**

Run: `cd providers/xai && go vet -tags xaispeech ./... && go vet ./... && go test -tags xaispeech ./... -count=1 && go test ./... -count=1`
Expected: PASS with and without the tag (the untagged build must not see `tts_stream.go`).

- [ ] **Step 6: Commit**

```bash
git add providers/xai/tts.go providers/xai/tts_stream.go providers/xai/tts_stream_test.go
git commit -m "feat(xai): TTS WebSocket wire code (URL, messages, dial)"
```

---

### Task 5: xAI `StreamingTTSProcessor`

**Files:**
- Modify: `providers/xai/tts_stream.go` (add the processor)
- Test: `providers/xai/tts_streaming_test.go` (create, build tag `xaispeech`)

**Interfaces:**
- Consumes: Task 4 helpers; `ttsunit.Runner`, `ttsunit.ReplySlot`; `classify`, `MapHTTPError`, `mockTTSClient` (existing test type).
- Produces: `type StreamingTTSProcessor struct{ *ttsunit.Runner; ... }`, `func NewStreamingTTSProcessor(cfg TTSConfig) (*StreamingTTSProcessor, error)`, `Warm(ctx) error`.

- [ ] **Step 1: Write the failing tests** (create `providers/xai/tts_streaming_test.go`)

```go
//go:build xaispeech

package xai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"govoice/core"
	"govoice/core/resilience"
)

// wsFake serves the xAI TTS WebSocket; handle runs once per accepted connection.
type wsFake struct {
	*httptest.Server
	opens   atomic.Int32
	queries chan url.Values
	auth    chan string
	closed  chan struct{} // one signal per finished connection handler
}

func newWSFake(t *testing.T, handle func(ctx context.Context, c *websocket.Conn)) *wsFake {
	f := &wsFake{queries: make(chan url.Values, 8), auth: make(chan string, 8), closed: make(chan struct{}, 8)}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.opens.Add(1)
		f.queries <- r.URL.Query()
		f.auth <- r.Header.Get("Authorization")
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { f.closed <- struct{}{} }()
		defer c.CloseNow()
		handle(r.Context(), c)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *wsFake) cfg(client TTSClient) TTSConfig {
	cfg := DefaultTTSConfig()
	cfg.Endpoint = f.URL + "/v1/tts" // http:// becomes ws://
	cfg.APIKey = "xai-test-key-0123456789"
	cfg.Timeout = 2 * time.Second
	cfg.RetryPolicy = resilience.Policy{MaxAttempts: 3, InitialBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond}
	cfg.Client = client
	return cfg
}

func sendMsg(ctx context.Context, c *websocket.Conn, m wsMsg) {
	b, _ := json.Marshal(m)
	_ = c.Write(ctx, websocket.MessageText, b)
}

func audioDelta(n int) wsMsg {
	return wsMsg{Type: "audio.delta", Delta: base64.StdEncoding.EncodeToString(make([]byte, n))}
}

// readUtterance reads text.delta messages until text.done and returns the text.
func readUtterance(ctx context.Context, c *websocket.Conn) (string, error) {
	var text strings.Builder
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			return "", err
		}
		var m wsMsg
		_ = json.Unmarshal(data, &m)
		switch m.Type {
		case "text.delta":
			text.WriteString(m.Delta)
		case "text.done":
			return text.String(), nil
		}
	}
}

type wsHarness struct {
	proc *StreamingTTSProcessor
	sess *core.Session
	sink *core.PassthroughProcessor
}

func wsStart(t *testing.T, cfg TTSConfig) *wsHarness {
	t.Helper()
	proc, err := NewStreamingTTSProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sink := &core.PassthroughProcessor{}
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
	t.Cleanup(func() { sess.Close() })
	return &wsHarness{proc: proc, sess: sess, sink: sink}
}

func (h *wsHarness) say(t *testing.T, turn string, id uint64, text string, final bool) {
	t.Helper()
	if err := h.sess.Post(&core.SynthesisUnitFrame{Turn: turn, Gen: 1, UnitID: id, Text: text, IsFinal: final}); err != nil {
		t.Fatal(err)
	}
}

func wsFrames[T core.Frame](h *wsHarness) (out []T) {
	for _, f := range h.sink.GetFrames() {
		if v, ok := f.(T); ok {
			out = append(out, v)
		}
	}
	return out
}

func wsWait(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(3 * time.Second); !cond(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func TestStreamingTTS_WireAndFirstAudioBeforeDone(t *testing.T) {
	release := make(chan struct{})
	got := make(chan string, 1)
	f := newWSFake(t, func(ctx context.Context, c *websocket.Conn) {
		text, err := readUtterance(ctx, c)
		if err != nil {
			return
		}
		got <- text
		sendMsg(ctx, c, audioDelta(1920))
		select {
		case <-release:
		case <-ctx.Done():
			return
		}
		sendMsg(ctx, c, audioDelta(960))
		sendMsg(ctx, c, wsMsg{Type: "audio.done"})
		_, _, _ = c.Read(ctx) // until the client hangs up
	})
	h := wsStart(t, f.cfg(&mockTTSClient{}))
	h.say(t, "t1", 1, "Hello world", true)

	q := <-f.queries
	for k, want := range map[string]string{"voice": "eve", "language": "en", "codec": "pcm", "sample_rate": "24000", "optimize_streaming_latency": "1"} {
		if q.Get(k) != want {
			t.Errorf("query %s = %q, want %q", k, q.Get(k), want)
		}
	}
	if a := <-f.auth; a != "Bearer xai-test-key-0123456789" {
		t.Errorf("Authorization = %q", a)
	}
	if text := <-got; text != "Hello world" {
		t.Errorf("text = %q", text)
	}
	wsWait(t, "the first audio frame while the utterance is still open", func() bool { return len(wsFrames[*core.OutputAudioFrame](h)) >= 1 })
	if n := len(wsFrames[*core.TTSStoppedFrame](h)); n != 0 {
		t.Fatalf("%d TTSStopped before audio.done", n)
	}
	close(release)
	wsWait(t, "TTSStopped", func() bool { return len(wsFrames[*core.TTSStoppedFrame](h)) == 1 })
	audio := wsFrames[*core.OutputAudioFrame](h)
	if last := audio[len(audio)-1]; !last.IsFinal || !last.TurnFinal {
		t.Errorf("last frame final markers = %t %t, want true true", last.IsFinal, last.TurnFinal)
	}
}

func TestStreamingTTS_OneConnectionPerReply(t *testing.T) {
	f := newWSFake(t, func(ctx context.Context, c *websocket.Conn) {
		for {
			if _, err := readUtterance(ctx, c); err != nil {
				return
			}
			sendMsg(ctx, c, audioDelta(1920))
			sendMsg(ctx, c, wsMsg{Type: "audio.done"})
		}
	})
	h := wsStart(t, f.cfg(&mockTTSClient{}))
	h.say(t, "t1", 1, "one", false)
	h.say(t, "t1", 2, "two", true)
	wsWait(t, "both units", func() bool { return len(wsFrames[*core.TTSStoppedFrame](h)) == 2 })
	if n := f.opens.Load(); n != 1 {
		t.Fatalf("%d connections for one reply, want 1", n)
	}
	h.say(t, "t2", 3, "three", true) // a new reply must not reuse the old connection
	wsWait(t, "the next reply", func() bool { return len(wsFrames[*core.TTSStoppedFrame](h)) == 3 })
	if n := f.opens.Load(); n != 2 {
		t.Fatalf("%d connections after a second reply, want 2", n)
	}
}

func TestStreamingTTS_InterruptionClosesTheConnection(t *testing.T) {
	f := newWSFake(t, func(ctx context.Context, c *websocket.Conn) {
		if _, err := readUtterance(ctx, c); err != nil {
			return
		}
		sendMsg(ctx, c, audioDelta(3000))
		_, _, _ = c.Read(ctx) // returns when the client hangs up
	})
	h := wsStart(t, f.cfg(&mockTTSClient{}))
	h.say(t, "t1", 1, "a long sentence", true)
	wsWait(t, "audio", func() bool { return len(wsFrames[*core.OutputAudioFrame](h)) >= 1 })
	_ = h.sess.Post(&core.InterruptionFrame{})
	select {
	case <-f.closed:
	case <-time.After(3 * time.Second):
		t.Fatal("the connection was not closed on interruption")
	}
	time.Sleep(30 * time.Millisecond)
	if n := len(wsFrames[*core.TTSStoppedFrame](h)); n != 0 {
		t.Errorf("%d TTSStopped after an interruption", n)
	}
	if n := len(wsFrames[*core.ErrorFrame](h)); n != 0 {
		t.Errorf("%d ErrorFrames after an interruption", n)
	}
}

func TestStreamingTTS_ErrorBeforeAudioRetriesAsUnary(t *testing.T) {
	f := newWSFake(t, func(ctx context.Context, c *websocket.Conn) {
		if _, err := readUtterance(ctx, c); err != nil {
			return
		}
		sendMsg(ctx, c, wsMsg{Type: "error", Message: "voice unavailable"})
		_, _, _ = c.Read(ctx)
	})
	unary := &mockTTSClient{pcmData: make([]byte, 1920)}
	h := wsStart(t, f.cfg(unary))
	h.say(t, "t1", 1, "Hello", true)
	wsWait(t, "TTSStopped", func() bool { return len(wsFrames[*core.TTSStoppedFrame](h)) == 1 })
	unary.mu.Lock()
	calls := unary.calls
	unary.mu.Unlock()
	if calls != 1 {
		t.Fatalf("unary calls = %d, want 1", calls)
	}
	if n := len(wsFrames[*core.ErrorFrame](h)); n != 0 {
		t.Errorf("%d ErrorFrames, want none: the fallback recovered", n)
	}
	if len(wsFrames[*core.OutputAudioFrame](h)) == 0 {
		t.Error("the unary fallback produced no audio")
	}
}

func TestStreamingTTS_DialFailureRetriesAsUnary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	t.Cleanup(srv.Close)
	cfg := DefaultTTSConfig()
	cfg.Endpoint, cfg.APIKey, cfg.Timeout = srv.URL+"/v1/tts", "xai-test-key-0123456789", 2*time.Second
	cfg.RetryPolicy = resilience.Policy{MaxAttempts: 3, InitialBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond}
	unary := &mockTTSClient{pcmData: make([]byte, 1920)}
	cfg.Client = unary
	h := wsStart(t, cfg)
	h.say(t, "t1", 1, "Hello", true)
	wsWait(t, "TTSStopped", func() bool { return len(wsFrames[*core.TTSStoppedFrame](h)) == 1 })
	unary.mu.Lock()
	defer unary.mu.Unlock()
	if unary.calls != 1 {
		t.Fatalf("unary calls = %d, want 1", unary.calls)
	}
}

func TestStreamingTTS_UnauthorizedIsNotRetried(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	t.Cleanup(srv.Close)
	cfg := DefaultTTSConfig()
	cfg.Endpoint, cfg.APIKey, cfg.Timeout = srv.URL+"/v1/tts", "xai-test-key-0123456789", 2*time.Second
	cfg.RetryPolicy = resilience.Policy{MaxAttempts: 3, InitialBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond}
	unary := &mockTTSClient{pcmData: make([]byte, 1920)}
	cfg.Client = unary
	h := wsStart(t, cfg)
	h.say(t, "t1", 1, "Hello", true)
	wsWait(t, "the error", func() bool { return len(wsFrames[*core.ErrorFrame](h)) == 1 })
	unary.mu.Lock()
	defer unary.mu.Unlock()
	if unary.calls != 0 {
		t.Fatalf("unary calls = %d: a rejected key must surface, not be retried", unary.calls)
	}
}

func TestStreamingTTS_NoReplayOnceAudioHasStarted(t *testing.T) {
	f := newWSFake(t, func(ctx context.Context, c *websocket.Conn) {
		if _, err := readUtterance(ctx, c); err != nil {
			return
		}
		sendMsg(ctx, c, audioDelta(3000)) // two full 20 ms frames go out
		sendMsg(ctx, c, wsMsg{Type: "error", Message: "synthesis failed"})
		_, _, _ = c.Read(ctx)
	})
	unary := &mockTTSClient{pcmData: make([]byte, 1920)}
	h := wsStart(t, f.cfg(unary))
	h.say(t, "t1", 1, "Hello", true)
	wsWait(t, "the error", func() bool { return len(wsFrames[*core.ErrorFrame](h)) == 1 })
	wsWait(t, "the closing TTSStopped", func() bool { return len(wsFrames[*core.TTSStoppedFrame](h)) == 1 })
	unary.mu.Lock()
	defer unary.mu.Unlock()
	if unary.calls != 0 {
		t.Fatalf("unary calls = %d: audio already played, a retry would replay the sentence", unary.calls)
	}
}

func TestNewStreamingTTSProcessor_RequiresAKey(t *testing.T) {
	if _, err := NewStreamingTTSProcessor(DefaultTTSConfig()); err == nil {
		t.Fatal("a missing API key was accepted")
	}
}
```

`mockTTSClient` is the existing type in `tts_test.go`; if its call counter field is not named `calls` or is not an `int`, read the type (`providers/xai/tts_test.go` lines 15-30) and use its real field.

- [ ] **Step 2: Run them and confirm they fail**

Run: `cd providers/xai && go test -tags xaispeech -run 'TestStreamingTTS|TestNewStreamingTTSProcessor' -count=1 .`
Expected: build failure (`NewStreamingTTSProcessor` undefined).

- [ ] **Step 3: Implement.** Append to `providers/xai/tts_stream.go` (add the imports `encoding/base64`, `encoding/json`, `errors`, `govoice/core/ttsunit` to its import block):

```go
// wsSession is the WebSocket shared by the units of one reply.
type wsSession struct{ conn *websocket.Conn }

// abort hangs up (barge-in, error, stale reply).
func (s *wsSession) abort() { s.conn.CloseNow() }

// StreamingTTSProcessor speaks over the xAI TTS WebSocket: audio is emitted as it arrives. One
// connection per reply (units reuse it); units play in order on the shared ttsunit.Runner. A retry
// after a failed stream uses the unary POST /v1/tts for that unit, so a stream problem never
// silences the call. The unary TTSProcessor is the kill-switch path.
type StreamingTTSProcessor struct {
	*ttsunit.Runner
	cfg   TTSConfig
	wsURL string
	pool  *netpool.Pool
	slot  ttsunit.ReplySlot[*wsSession]
}

// NewStreamingTTSProcessor needs an API key. cfg.Client serves the unary fallback.
func NewStreamingTTSProcessor(cfg TTSConfig) (*StreamingTTSProcessor, error) {
	if cfg.APIKey == "" {
		return nil, ErrMissingAPIKey
	}
	cfg = normalizeTTSConfig(cfg)
	if cfg.Client == nil {
		cfg.Client = NewRealXAITTSClient()
	}
	wsURL, err := streamURL(cfg)
	if err != nil {
		return nil, err
	}
	p := &StreamingTTSProcessor{cfg: cfg, wsURL: wsURL, pool: netpool.Get(netpool.Options{Provider: "xai", Stage: "tts", HTTP1Only: true})}
	p.slot.Close = (*wsSession).abort
	p.Runner = ttsunit.New(ttsunit.Config{
		Provider: "xai", Model: cfg.Voice, SampleRate: cfg.SampleRate,
		Breaker: cfg.CircuitBreaker, Policy: cfg.RetryPolicy, Classify: classify, EndReply: p.slot.Drop,
		Attempt: func(ctx context.Context, u *ttsunit.Unit, attempt int) error {
			return (&unitRun{p: p, Unit: u}).attempt(ctx, attempt)
		},
	})
	return p, nil
}

// Warm pre-dials TCP+TLS only: the reply's WebSocket is dialed with its first unit. Skipped while
// the breaker is open; never reports to it.
func (p *StreamingTTSProcessor) Warm(ctx context.Context) error {
	if br := p.cfg.CircuitBreaker; br != nil && br.WouldRefuse() {
		p.pool.NoteSkipped()
		return nil
	}
	return p.pool.Warm(ctx, p.wsURL)
}

// unitRun is one attempt's provider-side state.
type unitRun struct {
	p *StreamingTTSProcessor
	*ttsunit.Unit
}

// attempt synthesizes one unit. Attempt 0 streams on the reply's connection; a retry (the Runner
// only retries before the first audio frame) uses the unary request.
func (u *unitRun) attempt(ctx context.Context, attempt int) (err error) {
	if attempt > 0 {
		u.p.slot.Drop() // its state is unknown after a failure
		pcm, err := u.p.cfg.Client.Synthesize(ctx, u.Frame.Text, u.p.cfg)
		if err != nil {
			return err
		}
		if err := u.Write(pcm); err != nil {
			return err
		}
		return u.Finish()
	}
	slot := &u.p.slot
	s, ok := slot.Get(u.Frame)
	if !ok {
		conn, err := dialTTS(ctx, u.p.pool, u.p.wsURL, u.p.cfg.APIKey)
		if err != nil {
			return err
		}
		s = &wsSession{conn: conn}
		slot.Put(u.Frame, s)
	}
	defer func() {
		if err != nil {
			slot.Drop()
		}
	}()
	fail := func(err error) error { // report the cause (timeout/cancel), not the hang-up
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return MapHTTPError("tts", nil, "", err)
	}
	for _, m := range []wsMsg{{Type: "text.delta", Delta: u.Frame.Text}, {Type: "text.done"}} {
		b, _ := json.Marshal(m)
		if err := s.conn.Write(ctx, websocket.MessageText, b); err != nil {
			return fail(err)
		}
	}
	for {
		typ, data, err := s.conn.Read(ctx)
		if err != nil {
			return fail(err)
		}
		if typ != websocket.MessageText {
			continue
		}
		var m wsMsg
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		switch m.Type {
		case "audio.delta":
			if m.Delta == "" {
				continue
			}
			pcm, err := base64.StdEncoding.DecodeString(m.Delta)
			if err != nil {
				return &ProviderError{Capability: "tts", Code: "BAD_RESPONSE", Message: "undecodable audio chunk", Class: ClassificationRetryable, Cause: err}
			}
			if err := u.Write(pcm); err != nil {
				return err
			}
		case "audio.done":
			return u.Finish() // the reply's connection stays open for the next unit
		case "error":
			return wsError(m)
		}
	}
}
```

Add `encoding/base64`, `encoding/json` and `govoice/core/ttsunit` to the file's import block (the other imports come from Task 4).

- [ ] **Step 4: Run both builds**

Run: `cd providers/xai && go vet -tags xaispeech ./... && go vet ./... && go test -tags xaispeech ./... -count=1 && go test ./... -count=1`
Expected: PASS.
- If `TestStreamingTTS_ErrorBeforeAudioRetriesAsUnary` shows two unary calls, the retry loop ran past attempt 1: the stream error was classified non-retryable. Check `wsError`'s class, not the test.
- Race check: `go test -tags xaispeech -race -run TestStreamingTTS -count=5 .` must pass.

- [ ] **Step 5: Prove the tests catch regressions, then restore** (do not commit the breakage)

1. In `attempt`, change `if attempt > 0 {` to `if true {`. `TestStreamingTTS_WireAndFirstAudioBeforeDone` must FAIL.
2. Restore. In the `audio.done` case, replace `return u.Finish()` with `slot.Drop(); return u.Finish()`. `TestStreamingTTS_OneConnectionPerReply` must FAIL (two connections for one reply).
3. Restore. Run `git diff providers/xai/tts_stream.go` against the last commit's content for the processor: it must show no leftover edits.

- [ ] **Step 6: Commit**

```bash
git add providers/xai/tts_stream.go providers/xai/tts_streaming_test.go
git commit -m "feat(xai): streaming TTS processor, one WebSocket per reply with unary fallback"
```

---

### Task 6: xAI server wiring

**Files:**
- Modify: `server/registry_xaispeech.go`
- Test: `server/registry_xaispeech_test.go` (create, build tag `xaispeech`)

**Interfaces:**
- Consumes: `xai.NewStreamingTTSProcessor` (Task 5), `Config.Providers.XAITTS.DisableStreaming` (Task 3).

- [ ] **Step 1: Write the failing test** (create `server/registry_xaispeech_test.go`)

```go
//go:build xaispeech

package server

import (
	"testing"

	"govoice/providers/xai"
)

// xAI TTS streams unless providers.xai_tts.disable_streaming is set.
func TestXAITTSFactoryStreamsUnlessDisabled(t *testing.T) {
	reg := NewRegistry()
	f, ok := reg.factories["xai.tts"]
	if !ok {
		t.Fatal("xai.tts is not registered in this build")
	}
	for _, disable := range []bool{false, true} {
		cfg := DefaultConfig(ProfileDevelopment)
		cfg.Providers.XAITTS.DisableStreaming = disable
		p, err := f(cfg, StageSpec{Voice: "eve", Secret: []byte("xai-test-key-0123456789")})
		if err != nil {
			t.Fatal(err)
		}
		_, streaming := p.(*xai.StreamingTTSProcessor)
		_, unary := p.(*xai.TTSProcessor)
		if streaming == disable || unary != disable {
			t.Errorf("disable_streaming=%t built %T", disable, p)
		}
		if cl, ok := p.(interface{ Close() error }); ok {
			_ = cl.Close()
		}
	}
}
```

- [ ] **Step 2: Run it and confirm it fails**

Run: `cd server && CGO_LDFLAGS=-Wl,-w go test -tags xaispeech -run TestXAITTSFactoryStreamsUnlessDisabled -count=1 .`
Expected: FAIL (`disable_streaming=false built *xai.TTSProcessor`).

- [ ] **Step 3: Implement.** In `server/registry_xaispeech.go`, in the `xai.tts` factory replace `return xai.NewTTSProcessor(ttsCfg)` with:

```go
		if cfg.Providers.XAITTS.DisableStreaming {
			return xai.NewTTSProcessor(ttsCfg)
		}
		return xai.NewStreamingTTSProcessor(ttsCfg)
```

- [ ] **Step 4: Find anything that assumed the unary type**

Run: `grep -rn 'xai\.TTSProcessor\|\*xai\.TTS' --include='*.go' server tools | grep -v _test`
Expected: no output. If something type-asserts `*xai.TTSProcessor`, make it work for both processors (both implement `core.TTSCounters` and `Warm`), and add a line to the commit message.

- [ ] **Step 5: Run the server module with and without the tag**

Run: `cd server && CGO_LDFLAGS=-Wl,-w go test ./... -count=1 && CGO_LDFLAGS=-Wl,-w go test -tags xaispeech ./... -count=1`
Expected: PASS both.

- [ ] **Step 6: Commit**

```bash
git add server/registry_xaispeech.go server/registry_xaispeech_test.go
git commit -m "feat(server): xAI TTS streams over WebSocket unless disable_streaming"
```

---

### Task 7: Default voice moves to Chirp 3 HD

**Files:**
- Modify: `providers/google/tts.go` (`DefaultVoice` and the field comment)
- Modify: `ui/src/routes/onboarding/OnboardingView.tsx`, `ui/src/routes/onboarding/OnboardingView.test.tsx`, `ui/src/routes/agents/AgentsView.tsx`
- Modify: `scripts/dev-seed.mjs`, `deploy/smoke-google.sh`, `tools/livecheck/profiles.yaml`
- Modify: `README.md`, `docs/quickstart.md`, `docs/api/v1.md`, `docs/plans/stored-agents-drive-calls.md`
- Test: `providers/google/tts_streaming_test.go` (add one test)

**Interfaces:**
- Consumes: `google.DefaultVoice` (Task 2). Produces: the value `en-US-Chirp3-HD-Aoede`.

- [ ] **Step 1: Write the failing test** (append to `providers/google/tts_streaming_test.go`)

```go
// The shipped default is a Chirp 3 HD voice, so default calls stream.
func TestDefaultVoiceStreams(t *testing.T) {
	if !IsChirp3HD(DefaultVoice) || DefaultTTSConfig().Voice != DefaultVoice {
		t.Fatalf("DefaultVoice = %q (config voice %q): the default must be a Chirp 3 HD voice", DefaultVoice, DefaultTTSConfig().Voice)
	}
}
```

- [ ] **Step 2: Run it and confirm it fails**

Run: `cd providers/google && go test -run TestDefaultVoiceStreams -count=1 .`
Expected: FAIL (`DefaultVoice = "en-US-Journey-F"`).

- [ ] **Step 3: Implement.**
- `providers/google/tts.go`: `const DefaultVoice = "en-US-Chirp3-HD-Aoede"`. Update the `Voice` field comment to `// Default: "en-US-Chirp3-HD-Aoede" (streams); other voices use the unary request`.
- `ui/src/routes/onboarding/OnboardingView.tsx` line 38: `useState("en-US-Chirp3-HD-Aoede")`. In `OnboardingView.test.tsx` (~line 177) change the expected `voice: "en-US-Journey-F"` to `"en-US-Chirp3-HD-Aoede"`.
- `ui/src/routes/agents/AgentsView.tsx` (~line 386): placeholder `Provider default (e.g. en-US-Chirp3-HD-Aoede)`. Before editing, run `grep -rn "Provider default" ui/src`; if a test matches this placeholder text, update it too.
- `scripts/dev-seed.mjs:83` and `deploy/smoke-google.sh:58`: replace `en-US-Journey-F` with `en-US-Chirp3-HD-Aoede`.
- `tools/livecheck/profiles.yaml`: the three `tts: {provider: google, voice: en-US-Journey-F}` lines become `voice: en-US-Chirp3-HD-Aoede`. Leave `tools/livecheck/report_test.go` test data alone.
- Docs examples: `README.md:13` ("Journey TTS" → "Chirp 3 HD TTS"), `docs/quickstart.md:119`, `docs/api/v1.md:28`, `docs/plans/stored-agents-drive-calls.md:25`. Leave `docs/benchmarks/*` and `docs/audit/*` (historical results).
- Leave UI tests that save `en-US-Journey-F` as fixture data, and Go tests that pass it through a stored spec (`server/registry_test.go`, `server/management_test.go`).

- [ ] **Step 4: Run everything this touches**

```bash
cd providers/google && go test ./... -count=1
cd ../../ui && npx vitest run src/routes/onboarding src/routes/agents && npx tsc --noEmit
cd .. && go test ./tools/livecheck/ -count=1
GOVOICE_ORT_LIB=/Users/raymondottun/Projects/go-voice/third_party/onnxruntime-osx-arm64-1.29.0/lib/libonnxruntime.dylib CGO_LDFLAGS=-Wl,-w go run ./tools/livecheck -list
```

Expected: all pass; `-list` prints the `google`, `google-latest-long` and `xai-llm` profiles with the new voice.
- A shell syntax check for the smoke script: `bash -n deploy/smoke-google.sh`.

- [ ] **Step 5: Commit**

```bash
git add providers/google/tts.go providers/google/tts_streaming_test.go ui/src/routes/onboarding/OnboardingView.tsx ui/src/routes/onboarding/OnboardingView.test.tsx ui/src/routes/agents/AgentsView.tsx scripts/dev-seed.mjs deploy/smoke-google.sh tools/livecheck/profiles.yaml README.md docs/quickstart.md docs/api/v1.md docs/plans/stored-agents-drive-calls.md
git commit -m "feat: default Google TTS voice is Chirp 3 HD (en-US-Chirp3-HD-Aoede)"
```

---

### Task 8: Docs, matrix and runbook

**Files:**
- Modify: `docs/pipecat-parity-matrix.md`, `docs/audit/SUMMARY.md`, `docs/operations_runbook.md`, `docs/quickstart.md`

- [ ] **Step 1: Find the exact lines**

```bash
grep -n "xAI TTS is unary\|Streaming TTS\|StreamingSynthesize\|STT / LLM / TTS breadth" docs/pipecat-parity-matrix.md docs/audit/SUMMARY.md | cut -c1-120
grep -n -i "retries\|tts" server/metrics.go | grep -i "retr" | head -5
```

- [ ] **Step 2: Edit the matrix** (`docs/pipecat-parity-matrix.md`)
- Remove the known-gap bullet "**xAI TTS is unary**…" and replace it with: `- **TTS streaming.** xAI (WebSocket, one connection per reply) and Google Chirp 3 HD voices (StreamingSynthesize, one stream per unit) emit audio as it arrives; other Google voices stay unary. A failed stream retries as the unary request. Kill switches: \`providers.google_tts.disable_streaming\`, \`providers.xai_tts.disable_streaming\`. Not built: token-level input streaming, xAI word timestamps, a Google look-ahead for streamed units. Live S9 numbers: \`docs/benchmarks/streaming-tts-live.md\` (when run).`
- In the "STT / LLM / TTS breadth" row, add a sentence in the differences cell: `xAI TTS and Google Chirp 3 HD stream; Cartesia, ElevenLabs and Nvidia already did.`
- In "Remaining work", replace item 3 with: `3. **Streaming TTS (Pipecat parity).** Done 2026-10-11 for xAI and Google Chirp 3 HD. Still open: measure it live (S9 long reply, first audio and gaps) and add a Google look-ahead only if gaps remain.`

- [ ] **Step 3: Edit the audit summary** (`docs/audit/SUMMARY.md`): in the M3-01 row, replace "Deferred: true `StreamingSynthesize` for Google TTS" with "`StreamingSynthesize` built 2026-10-11 for Chirp 3 HD voices"; remove the "Google TTS `StreamingSynthesize` …" bullet from "Remaining before launch".

- [ ] **Step 4: Runbook** (`docs/operations_runbook.md`): add a section "TTS streaming" with:
  - what streams (xAI always; Google only Chirp 3 HD voices) and the two kill-switch keys;
  - Google quotas: 100 concurrent streaming sessions and 200 Chirp 3 requests per minute per project; one stream per speaking call, so about 100 simultaneously speaking calls per project; a quota error falls back to unary for that unit;
  - how to see fallbacks: the TTS retries counter named in Step 1's grep output (cite its real metric name); a rising retries rate for provider `google` or `xai` means streams are failing and units are being served unary;
  - what to do if WebSockets are blocked by a proxy (xAI): set `providers.xai_tts.disable_streaming: true`.

- [ ] **Step 5: Quickstart** (`docs/quickstart.md`): in the config section, list the two new keys with one line each.

- [ ] **Step 6: Run the matrix check**

Run: `cd tools/compat && go test -run TestParityMatrix -count=1 .`
Expected: PASS on the main checkout. In a worktree without `pipecat-impl`, only "cited path does not exist" errors may appear: count the others (must be 0).

- [ ] **Step 7: Commit**

```bash
git add docs/pipecat-parity-matrix.md docs/audit/SUMMARY.md docs/operations_runbook.md docs/quickstart.md
git commit -m "docs: streaming TTS in the matrix, audit summary, runbook and quickstart"
```

---

### Task 9: Live S9 evidence (paid; needs the user's explicit approval)

**Do not run this task without the user's approval to spend: about 24 provider calls.** If it is not approved, say so in the final report and stop here.

**Files:**
- Create: `docs/benchmarks/streaming-tts-live.md`

- [ ] **Step 1: Read the stored unary numbers**

Run: `grep -n "S9" docs/benchmarks/agent-matrix.md | cut -c1-260`
Record, for `google` and `xai`: first audio, gaps, TTS requests per reply.

- [ ] **Step 2: Run S9 on both profiles** (secrets in the environment; the `google` profile now uses the Chirp 3 HD voice)

```bash
GOVOICE_LIVE=1 make live-critical PROFILES=google SCENARIOS=S9 MAX_CALLS=20 LIVE_ARGS="-out .live-out/google-s9"
GOVOICE_LIVE=1 make live-critical PROFILES=xai SCENARIOS=S9 MAX_CALLS=30 LIVE_ARGS="-out .live-out/xai-s9"
```

Expected: both runs report S9 with first-audio and gap metrics. If a run fails a check, read its `agent-matrix.md` details before drawing conclusions.

- [ ] **Step 3: Write `docs/benchmarks/streaming-tts-live.md`:** the commands, the date and commit, a table of before (unary, from Step 1) and after (streaming) for first audio, gaps and requests, and a plain verdict: faster, same, or slower, and whether gaps remain. If gaps remain on Google, state that a look-ahead for streamed units is the follow-up.

- [ ] **Step 4: Update the matrix line** from Task 8 ("Live S9 numbers: … when run") to cite the real result.

- [ ] **Step 5: Commit**

```bash
git add docs/benchmarks/streaming-tts-live.md docs/pipecat-parity-matrix.md
git commit -m "docs(bench): live S9 results for streaming Google and xAI TTS"
```

---

## Final check

```bash
for m in providers/google providers/xai server; do (cd $m && go vet ./... && go vet -tags xaispeech ./... && CGO_LDFLAGS=-Wl,-w go test ./... -count=1 && CGO_LDFLAGS=-Wl,-w go test -tags xaispeech ./... -count=1) || echo "FAILED: $m"; done
```

```bash
for m in . core providers/cartesia providers/deepgram providers/elevenlabs providers/nvidia providers/openai; do (cd $m && go vet ./... && CGO_LDFLAGS=-Wl,-w go test ./... -count=1) || echo "FAILED: $m"; done
```

```bash
cd ui && npx vitest run && npx tsc --noEmit
```

Everything must pass. Then run a fresh reviewer on the branch diff, and after the merge run `tools/compat` on the main checkout.
