package google

import (
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/coretest"
)

type eofSendStream struct {
	*mockSTTStream
	eof *atomic.Bool
	end chan struct{} // closed: the server ends the stream (Recv returns io.EOF)
}

func (s eofSendStream) Recv() ([]*STTResult, error) {
	select {
	case <-s.end:
		return nil, io.EOF
	case <-s.closed:
		return nil, context.Canceled
	}
}

func (s eofSendStream) Send(audio []byte) error {
	if s.eof.CompareAndSwap(true, false) { // one-shot
		return io.EOF
	}
	return s.mockSTTStream.Send(audio)
}

type eofOpener struct {
	opened atomic.Int32
	eof    atomic.Bool
	end    chan struct{}
}

func (o *eofOpener) OpenStream(context.Context, STTConfig) (STTStream, error) {
	o.opened.Add(1)
	return eofSendStream{newMockSTTStream(), &o.eof, o.end}, nil
}

// gRPC's Send returns io.EOF once the stream has ended (server close, or the
// call's context cancelled at hang-up); the real status belongs to Recv. It
// must not fail the session ("session failed: stt: EOF"): the next audio frame
// opens a new stream.
func TestGoogleSTTSendEOFIsNotSessionFailure(t *testing.T) {
	opener := &eofOpener{end: make(chan struct{})}
	cfg := DefaultSTTConfig()
	cfg.StreamOpener = opener
	proc, _ := NewSTTProcessor(cfg)
	pipe, _ := core.NewPipeline(proc, &coretest.PassthroughProcessor{})
	session, _ := core.NewSession(36, pipe, core.DefaultResourceBudget())
	_ = session.Start()
	defer session.Close()

	_ = session.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	_ = session.Post(&core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1})
	waitFor := func(cond func() bool, what string) {
		t.Helper()
		for deadline := time.Now().Add(time.Second); !cond(); time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal(what)
			}
		}
	}
	waitFor(func() bool { return opener.opened.Load() == 1 }, "stream did not open")
	opener.eof.Store(true)
	_ = session.Post(&core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1}) // Send -> io.EOF
	time.Sleep(20 * time.Millisecond)
	// The receiver, not Send, retires the ended stream: the server's end reaches Recv.
	if n := opener.opened.Load(); n != 1 {
		t.Fatalf("opens = %d before the receiver saw the end, want 1", n)
	}
	close(opener.end)
	// Wait for the receiver to retire the ended stream (it counts the lost turn):
	// a turn that starts first binds to the dead stream and, by design, is not redialled.
	waitFor(func() bool { return proc.Errors.Load() == 1 }, "receiver did not see the stream end")
	// The ended turn is lost (no redial within it); the next turn opens a new stream.
	_ = session.Post(&core.UserStartedSpeakingFrame{Seq: 2, Gen: 1})
	waitFor(func() bool {
		_ = session.Post(&core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1})
		return opener.opened.Load() == 2
	}, "no new stream after the ended one")
	if err := session.Err(); err != nil {
		t.Fatalf("session failed on a send to an ended stream: %v", err)
	}
}
