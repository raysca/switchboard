//go:build xaispeech

package xai

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"govoice/core"
)

type recvItem struct {
	msg *STTMessage
	err error
}

// wireSTTStream records the order of client writes and serves scripted reads.
type wireSTTStream struct {
	mu   sync.Mutex
	ops  []string
	recv chan recvItem
}

func newWireSTTStream() *wireSTTStream { return &wireSTTStream{recv: make(chan recvItem, 8)} }

func (w *wireSTTStream) SendAudio(context.Context, []byte) error {
	w.mu.Lock()
	w.ops = append(w.ops, "audio")
	w.mu.Unlock()
	return nil
}
func (w *wireSTTStream) CloseSend(context.Context) error {
	w.mu.Lock()
	w.ops = append(w.ops, "done")
	w.mu.Unlock()
	return nil
}
func (w *wireSTTStream) Recv(ctx context.Context) (*STTMessage, error) {
	select {
	case it := <-w.recv:
		return it.msg, it.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (w *wireSTTStream) Close() error { return nil }
func (w *wireSTTStream) snapshot() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.ops...)
}

type wireOpener struct{ s *wireSTTStream }

func (o wireOpener) OpenStream(context.Context, STTConfig) (STTStream, error) { return o.s, nil }

type sttHarness struct {
	p      *STTProcessor
	ws     *wireSTTStream
	mu     sync.Mutex
	errs   []*core.ErrorFrame
	inv    int
	finals []string
}

func newSTTHarness(t *testing.T) *sttHarness {
	t.Helper()
	h := &sttHarness{ws: newWireSTTStream()}
	cfg := DefaultSTTConfig()
	cfg.StreamOpener = wireOpener{h.ws}
	p, err := NewSTTProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.p = p
	pipe, _ := core.NewPipeline(p, &core.PassthroughProcessor{})
	session, _ := core.NewSession(4, pipe, core.DefaultResourceBudget())
	session.OnOutput = func(f core.Frame) {
		h.mu.Lock()
		defer h.mu.Unlock()
		switch v := f.(type) {
		case *core.ErrorFrame:
			h.errs = append(h.errs, v)
		case *core.TranscriptInvalidationFrame:
			h.inv++
		case *core.TranscriptCandidateFrame:
			if v.Final {
				h.finals = append(h.finals, v.Text)
			}
		}
	}
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return h
}

func (h *sttHarness) send(t *testing.T, f core.Frame) {
	t.Helper()
	if err := h.p.ProcessFrame(context.Background(), f, core.Downstream, nopEmit{}); err != nil {
		t.Fatal(err)
	}
}

func (h *sttHarness) errCount() (int, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.errs), h.inv
}

func (h *sttHarness) finalTexts() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.finals...)
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timeout: " + what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// M3-02: the VAD stop arrives before transcript.created (an STT failover
// replay). audio.done must go out after every buffered chunk, never before.
func TestXAISTTDoneFollowsBufferedAudioWhenStopBeatsHandshake(t *testing.T) {
	h := newSTTHarness(t)
	h.send(t, &core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	for i := 0; i < 50; i++ {
		h.send(t, &core.InputAudioFrame{Samples: make([]byte, 640), SampleRate: 16000})
	}
	h.send(t, &core.UserStoppedSpeakingFrame{})
	if ops := h.ws.snapshot(); len(ops) != 0 {
		t.Fatalf("wrote %d ops before transcript.created", len(ops))
	}
	h.ws.recv <- recvItem{msg: &STTMessage{Type: "transcript.created"}}
	waitFor(t, "audio then done", func() bool { return len(h.ws.snapshot()) == 51 })
	ops := h.ws.snapshot()
	for i, op := range ops[:50] {
		if op != "audio" {
			t.Fatalf("op %d = %s; audio.done jumped ahead of buffered audio", i, op)
		}
	}
	if ops[50] != "done" {
		t.Fatalf("last op = %s, want done", ops[50])
	}
}

// M3-09: the connection drops mid-utterance (EOF, no audio.done sent): the
// partial is retired and a transcript-invalidating error lets STTFailover act.
func TestXAISTTDropMidUtteranceRaisesError(t *testing.T) {
	h := newSTTHarness(t)
	h.send(t, &core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	h.send(t, &core.InputAudioFrame{Samples: make([]byte, 640), SampleRate: 16000})
	h.ws.recv <- recvItem{msg: &STTMessage{Type: "transcript.created"}}
	h.ws.recv <- recvItem{err: io.ErrUnexpectedEOF}
	waitFor(t, "error frame", func() bool { e, _ := h.errCount(); return e > 0 })
	if _, inv := h.errCount(); inv != 1 {
		t.Fatalf("invalidations = %d, want 1", inv)
	}
}

// A drop after audio.done (draining) is the normal end, not a failure.
func TestXAISTTEOFWhileDrainingIsClean(t *testing.T) {
	h := newSTTHarness(t)
	h.send(t, &core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	h.send(t, &core.InputAudioFrame{Samples: make([]byte, 640), SampleRate: 16000})
	h.ws.recv <- recvItem{msg: &STTMessage{Type: "transcript.created"}}
	waitFor(t, "audio", func() bool { return len(h.ws.snapshot()) == 1 })
	h.send(t, &core.UserStoppedSpeakingFrame{})
	waitFor(t, "done", func() bool { return len(h.ws.snapshot()) == 2 })
	h.ws.recv <- recvItem{err: io.EOF}
	time.Sleep(100 * time.Millisecond)
	if e, _ := h.errCount(); e != 0 {
		t.Fatalf("clean drain end raised %d errors", e)
	}
}

// M3-09 follow-up: a drop after audio.done but before transcript.done, with a partial heard and
// no speech_final yet, loses the utterance and must raise the failover error.
func TestXAISTTDropAfterDoneWithOpenUtteranceRaisesError(t *testing.T) {
	h := newSTTHarness(t)
	h.send(t, &core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	h.send(t, &core.InputAudioFrame{Samples: make([]byte, 640), SampleRate: 16000})
	h.ws.recv <- recvItem{msg: &STTMessage{Type: "transcript.created"}}
	h.ws.recv <- recvItem{msg: &STTMessage{Type: "transcript.partial", Text: "hello there"}}
	waitFor(t, "audio", func() bool { return len(h.ws.snapshot()) == 1 })
	time.Sleep(50 * time.Millisecond) // the partial is consumed
	h.send(t, &core.UserStoppedSpeakingFrame{})
	waitFor(t, "done", func() bool { return len(h.ws.snapshot()) == 2 })
	h.ws.recv <- recvItem{err: io.EOF}
	waitFor(t, "error frame", func() bool { e, _ := h.errCount(); return e > 0 })
	if _, inv := h.errCount(); inv != 1 {
		t.Fatalf("invalidations = %d, want 1", inv)
	}
}

func TestXAISTTInterruptionLeavesDrainingUtterance(t *testing.T) {
	h := newSTTHarness(t)
	h.send(t, &core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	h.send(t, &core.InputAudioFrame{Samples: make([]byte, 640), SampleRate: 16000})
	h.ws.recv <- recvItem{msg: &STTMessage{Type: "transcript.created"}}
	h.ws.recv <- recvItem{msg: &STTMessage{Type: "transcript.partial", Text: "hello there"}}
	waitFor(t, "audio", func() bool { return len(h.ws.snapshot()) == 1 })
	time.Sleep(50 * time.Millisecond)
	h.send(t, &core.UserStoppedSpeakingFrame{})
	waitFor(t, "done", func() bool { return len(h.ws.snapshot()) == 2 })
	h.send(t, &core.InterruptionFrame{Gen: 2})
	h.p.mu.Lock()
	n := len(h.p.draining)
	h.p.mu.Unlock()
	if n != 1 {
		t.Fatalf("draining = %d, want 1 after interruption", n)
	}
	h.ws.recv <- recvItem{msg: &STTMessage{Type: "transcript.done", Text: "hello there"}}
	waitFor(t, "final", func() bool {
		for _, text := range h.finalTexts() {
			if text == "hello there" {
				return true
			}
		}
		return false
	})
}
