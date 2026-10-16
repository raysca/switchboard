//go:build xaispeech

package xai

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/coretest"
)

type slowXAIOpener struct {
	delay  time.Duration
	stream *mockSTTStream
}

func (o slowXAIOpener) OpenStream(ctx context.Context, _ STTConfig) (STTStream, error) {
	select {
	case <-time.After(o.delay):
		return o.stream, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func startXAI(t *testing.T, opener STTStreamOpener) (*core.Session, *coretest.PassthroughProcessor) {
	t.Helper()
	cfg := DefaultSTTConfig()
	cfg.StreamOpener = opener
	proc, err := NewSTTProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sink := &coretest.PassthroughProcessor{}
	pipe, _ := core.NewPipeline(proc, sink)
	session, _ := core.NewSession(41, pipe, core.DefaultResourceBudget())
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session, sink
}

// A1: an open longer than the 2 s frame watchdog runs off the run loop; the
// audio held meanwhile goes out in order after transcript.created.
func TestXAISTTOpenRunsOffTheRunLoop(t *testing.T) {
	ms := &mockSTTStream{messages: []*STTMessage{{Type: "transcript.created"}}}
	session, _ := startXAI(t, slowXAIOpener{3 * time.Second, ms})
	_ = session.Post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	for b := byte(1); b <= 2; b++ {
		_ = session.Post(&core.InputAudioFrame{Samples: bytes.Repeat([]byte{b}, 640), Gen: 1})
	}
	time.Sleep(2500 * time.Millisecond)
	if err := session.Err(); err != nil {
		t.Fatalf("session failed while the open hung: %v", err)
	}
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		ms.mu.Lock()
		n := len(ms.audioIn)
		ordered := n >= 2 && ms.audioIn[0][0] == 1 && ms.audioIn[1][0] == 2
		ms.mu.Unlock()
		if n >= 2 {
			if !ordered {
				t.Fatal("held audio sent out of order")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("held audio never sent: %d chunks", n)
		}
	}
}

func TestXAISTTOpenFailureReportsOncePerTurn(t *testing.T) {
	var opens atomic.Int32
	session, sink := startXAI(t, &countOpener{n: &opens, err: errors.New("dial refused")})
	errorsSeen := func() (n int) {
		for _, f := range sink.GetFrames() {
			if _, ok := f.(*core.ErrorFrame); ok {
				n++
			}
		}
		return n
	}
	_ = session.Post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	for i := 0; i < 3; i++ {
		_ = session.Post(&core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1})
	}
	for deadline := time.Now().Add(time.Second); errorsSeen() != 1; time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("error frames = %d, want 1", errorsSeen())
		}
	}
	time.Sleep(50 * time.Millisecond)
	if n := opens.Load(); n != 1 || errorsSeen() != 1 {
		t.Fatalf("opens = %d, errors = %d: want one of each per turn", n, errorsSeen())
	}
	_ = session.Post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 2})
	for deadline := time.Now().Add(time.Second); opens.Load() != 2; time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the next turn did not reopen")
		}
	}
	if err := session.Err(); err != nil {
		t.Fatalf("a failed open ended the call: %v", err)
	}
}

// bareSession gives a directly driven processor the session its open task needs.
func bareSession(p *STTProcessor) *mockSessionController {
	s := newMockSessionController(1, 1)
	_ = p.Attach(s, 0)
	return s
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); !cond(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// waitReady waits until the active stream has seen transcript.created.
func waitReady(t *testing.T, p *STTProcessor) {
	t.Helper()
	eventually(t, "stream ready", func() bool {
		p.mu.Lock()
		st := p.active
		p.mu.Unlock()
		if st == nil {
			return false
		}
		st.mu.Lock()
		defer st.mu.Unlock()
		return st.isReady
	})
}

// waitAttached waits until the active stream's open has completed (stream set).
func waitAttached(t *testing.T, p *STTProcessor) {
	t.Helper()
	eventually(t, "stream attached", func() bool {
		p.mu.Lock()
		st := p.active
		p.mu.Unlock()
		if st == nil {
			return false
		}
		st.mu.Lock()
		defer st.mu.Unlock()
		return st.stream != nil
	})
}
