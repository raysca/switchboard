package nvidia

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/status"

	"govoice/core"
	"govoice/core/coretest"
	"govoice/core/resilience"
	"govoice/providers/nvidia/rivapb"
)

// stallRPC is a Riva RPC whose Send blocks until the RPC is cancelled, and whose Recv fails
// with the Canceled status the same cancel produces. sendWaitsForRecv orders the two outcomes.
type stallRPC struct {
	rivapb.RivaSpeechRecognition_StreamingRecognizeClient
	ctx              context.Context
	recvDone         chan struct{}
	sendWaitsForRecv bool
}

func (r *stallRPC) Send(*rivapb.StreamingRecognizeRequest) error {
	<-r.ctx.Done()
	if r.sendWaitsForRecv {
		<-r.recvDone
	}
	return status.FromContextError(r.ctx.Err()).Err()
}

func (r *stallRPC) Recv() (*rivapb.StreamingRecognizeResponse, error) {
	<-r.ctx.Done()
	defer close(r.recvDone)
	return nil, status.FromContextError(r.ctx.Err()).Err()
}

// A stalled Send is a timeout whichever of Send and Recv reports it first: both are
// retryable, breaker-charged and in the timeout failover class, never a client Canceled.
func TestSTTConn_StalledSendIsATimeoutInEitherOrder(t *testing.T) {
	for _, sendWaits := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		rpc := &stallRPC{ctx: ctx, recvDone: make(chan struct{}), sendWaitsForRecv: sendWaits}
		c := &sttConn{rpc: rpc, cancel: cancel, br: resilience.NewBreaker(resilience.Config{FailureThreshold: 1, Cooldown: time.Hour})}
		recvErr := make(chan error, 1)
		go func() { _, err := c.Recv(ctx); recvErr <- err }()
		sctx, scancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		sendErr := c.Send(sctx, []byte{1, 0})
		scancel()
		for name, err := range map[string]error{"Send": sendErr, "Recv": <-recvErr} {
			var pe *ProviderError
			if !errors.As(err, &pe) || !errors.Is(err, resilience.ErrTimeout) || !pe.Retryable || pe.ClientError {
				t.Errorf("sendWaitsForRecv=%v: %s returned %v, want a retryable timeout", sendWaits, name, err)
			}
			if v := classify(err); v.ClientError || !v.Retryable {
				t.Errorf("sendWaitsForRecv=%v: %s verdict %+v is not breaker-charged", sendWaits, name, v)
			}
		}
	}
}

// A Riva that stops reading (flow-control stall) behind a real STTFailover: the turn moves to
// the fallback and the breaker is charged, whichever of Send and Recv notices first.
func TestSTTFailover_StalledRivaJumpsToTheFallbackAndChargesTheBreaker(t *testing.T) {
	f, cc := newFake(t, func(ctx context.Context, s rivapb.RivaSpeechRecognition_StreamingRecognizeServer) error {
		<-ctx.Done() // never reads again
		return nil
	})
	cfg := f.cfg(cc)
	cfg.SendTimeout = 150 * time.Millisecond
	cfg.CircuitBreaker = resilience.NewBreaker(resilience.Config{FailureThreshold: 1, Cooldown: time.Hour})
	primary, err := NewSTTProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	fb := coretest.NewSTTProcessor(coretest.STTConfig{Provider: "fallback", Words: []string{"hello", "there"}})
	w, err := core.NewSTTFailover(core.STTFailoverConfig{
		Candidates: []core.STTCandidate{{Provider: "riva", Proc: primary}, {Provider: "fallback", Proc: fb}},
		Classify: func(err error) string {
			var pe *ProviderError
			if errors.As(err, &pe) && pe.ClientError {
				return "" // client errors never fail over
			}
			return "server_error"
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sink := &coretest.PassthroughProcessor{}
	pipe, _ := core.NewPipeline(w, sink)
	sess, _ := core.NewSession(9, pipe, core.DefaultResourceBudget())
	if err := sess.Start(); err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	audio := func(n int) {
		_ = sess.Post(&core.InputAudioFrame{Samples: make([]byte, n), SampleRate: 16000, Channels: 1, Gen: 1})
	}
	_ = sess.Post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	audio(640)
	for deadline := time.Now().Add(3 * time.Second); f.opens.Load() < 1; time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the RPC never opened")
		}
	}
	time.Sleep(100 * time.Millisecond) // the pre-roll flushed, live sends from here
	for i := 0; i < 6; i++ {
		audio(128 << 10) // overruns the HTTP/2 window the server never reads
	}
	_ = sess.Post(&core.UserStoppedSpeakingFrame{})
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := false
		for _, fr := range sink.GetFrames() {
			if c, ok := fr.(*core.TranscriptCandidateFrame); ok && c.Provider == "fallback" {
				got = true
			}
		}
		if got {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the fallback never received the turn")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !cfg.CircuitBreaker.WouldRefuse() {
		t.Fatal("the stalled Riva was not charged to the breaker")
	}
}
