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
		if err := onAudio(make([]byte, 3000)); err != nil { // full 20 ms frames go out
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

// The shipped default is a Chirp 3 HD voice, so default calls stream.
func TestDefaultVoiceStreams(t *testing.T) {
	if !IsChirp3HD(DefaultVoice) || DefaultTTSConfig().Voice != DefaultVoice {
		t.Fatalf("DefaultVoice = %q (config voice %q): the default must be a Chirp 3 HD voice", DefaultVoice, DefaultTTSConfig().Voice)
	}
}
