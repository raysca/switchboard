package google

import (
	"context"
	"testing"
	"time"

	"govoice/core"
)

type ctxRecordingOpener struct {
	mockSTTOpener
	ctxs []context.Context
}

func (o *ctxRecordingOpener) OpenStream(ctx context.Context, cfg STTConfig) (STTStream, error) {
	o.mu.Lock()
	o.ctxs = append(o.ctxs, ctx)
	o.mu.Unlock()
	return o.mockSTTOpener.OpenStream(ctx, cfg)
}

// A detached stream only gets CloseSend; once the provider ends it, its
// context must be released rather than held until the session closes.
func TestGoogleSTTDetachedStreamContextIsCancelledWhenItEnds(t *testing.T) {
	opener := &ctxRecordingOpener{}
	cfg := DefaultSTTConfig()
	cfg.StreamOpener = opener
	proc, _ := NewSTTProcessor(cfg)
	pipe, _ := core.NewPipeline(proc, &core.PassthroughProcessor{})
	session, _ := core.NewSession(35, pipe, core.DefaultResourceBudget())
	_ = session.Start()
	defer session.Close()

	_ = session.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	_ = session.Post(&core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1})
	_ = session.Post(&core.UserStoppedSpeakingFrame{})
	deadline := time.Now().Add(time.Second)
	for {
		opener.mu.Lock()
		n := len(opener.streams)
		opener.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stream did not open")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // let the stop frame detach it
	opener.mu.Lock()
	st, ctx := opener.streams[0], opener.ctxs[0]
	opener.mu.Unlock()
	_ = st.Close() // provider ends the half-closed stream

	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("detached stream's context was never cancelled")
	}
}
