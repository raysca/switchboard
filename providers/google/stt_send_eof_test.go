package google

import (
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
)

type eofSendStream struct {
	*mockSTTStream
	eof *atomic.Bool
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
}

func (o *eofOpener) OpenStream(context.Context, STTConfig) (STTStream, error) {
	o.opened.Add(1)
	return eofSendStream{newMockSTTStream(), &o.eof}, nil
}

// gRPC's Send returns io.EOF once the stream has ended (server close, or the
// call's context cancelled at hang-up); the real status belongs to Recv. It
// must not fail the session ("session failed: stt: EOF"): the next audio frame
// opens a new stream.
func TestGoogleSTTSendEOFIsNotSessionFailure(t *testing.T) {
	opener := &eofOpener{}
	cfg := DefaultSTTConfig()
	cfg.StreamOpener = opener
	proc, _ := NewSTTProcessor(cfg)
	pipe, _ := core.NewPipeline(proc, &core.PassthroughProcessor{})
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
	_ = session.Post(&core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1})
	waitFor(func() bool { return opener.opened.Load() == 2 }, "no new stream after the ended one")
	if err := session.Err(); err != nil {
		t.Fatalf("session failed on a send to an ended stream: %v", err)
	}
}
