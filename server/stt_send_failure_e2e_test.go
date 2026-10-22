//go:build xaispeech

package server

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/sttunit"
	"govoice/server/flow"
	"govoice/server/storage"
)

// A failed live write to the STT provider is that stream's failure, not the call's. These tests run a
// real server, the real VAD and a caller's recorded audio against a fake streaming provider whose
// writes fail: the call survives and the caller's later speech is still transcribed; a provider that
// stays down still ends the call through the existing stt_unavailable hang-up.

// flakySTT is a streaming provider (the shared sttunit runner every Deepgram-style provider uses)
// whose first `failStreams` streams each fail their next write once the test arms it and the stream
// is 100 ms old (the pre-roll flush right after a dial is not a live write). A stream's final
// transcript names the stream, so a reply proves which stream the caller's speech reached.
type flakySTT struct {
	failStreams int32
	deadAfter   bool // every dial fails once a write has failed
	armed       atomic.Bool
	dials       atomic.Int32
	failed      atomic.Int32
}

var errFlakyWrite = errors.New("write: connection reset by peer")

func (p *flakySTT) runner() *sttunit.Runner {
	return sttunit.New(sttunit.Config{Provider: "echo", Dial: func(context.Context) (sttunit.Conn, error) {
		if p.deadAfter && p.failed.Load() > 0 {
			return nil, errors.New("dial: connection refused")
		}
		return &flakyConn{p: p, idx: p.dials.Add(1), born: time.Now(), finish: make(chan struct{})}, nil
	}})
}

type flakyConn struct {
	p         *flakySTT
	idx       int32
	born      time.Time
	finish    chan struct{}
	finishing sync.Once
	delivered atomic.Bool
}

func (c *flakyConn) Send(context.Context, []byte) error {
	if c.p.armed.Load() && c.idx <= c.p.failStreams && time.Since(c.born) > 100*time.Millisecond {
		c.p.failed.Add(1)
		return errFlakyWrite
	}
	return nil
}

func (c *flakyConn) Finish(context.Context) error {
	c.finishing.Do(func() { close(c.finish) })
	return nil
}

// Recv blocks until Finish, delivers the stream's one final, then reports the provider's close.
func (c *flakyConn) Recv(ctx context.Context) (sttunit.Result, error) {
	select {
	case <-c.finish:
		if c.delivered.CompareAndSwap(false, true) {
			return sttunit.Result{Text: fmt.Sprintf("hello from stream %d", c.idx), Final: true, EndOfUtterance: true}, nil
		}
		return sttunit.Result{}, sttunit.ErrStreamEnded
	case <-ctx.Done():
		return sttunit.Result{}, ctx.Err()
	}
}

func (c *flakyConn) Close() {}

// sttSendRig is a flow rig whose echo STT is a flaky provider chosen by the flow's STT model, with the
// real VAD.
func sttSendRig(t *testing.T, byModel map[string]*flakySTT, mods ...func(*Config)) *flowRigE2E {
	t.Helper()
	r := newFlowRigE2E(t, newProbe(), mods...)
	realVAD(t, r.s)
	r.s.registry.Register("echo.stt", func(_ *Config, spec StageSpec) (core.Processor, error) {
		return byModel[spec.Model].runner(), nil
	})
	return r
}

func sttSendFlow(t *testing.T, r *flowRigE2E, stt string) string {
	t.Helper()
	cfg := `{"stt":{"provider":"echo",` + stt + `},"llm":{"provider":"echo","model":"m","system_prompt":"STTSend"},"tts":{"provider":"echo","voice":"sttsendvoice"}}`
	f, err := r.s.flows.Create(t.Context(), flow.CreateRequest{Name: "sttsend", Config: []byte(cfg)})
	if err != nil || f.Readiness.State != flow.ReadinessReady {
		t.Fatalf("flow: %+v err=%v", f.Readiness, err)
	}
	return f.ID
}

// armOnSpeech arms the provider's write failures once the caller's speech has begun.
func armOnSpeech(t *testing.T, r *flowRigE2E, ps ...*flakySTT) {
	t.Helper()
	eventually(t, "the caller's speech to start", func() bool { return r.has(EventSpeechStart, "") })
	for _, p := range ps {
		p.armed.Store(true)
	}
}

func timelineErrors(r *flowRigE2E) (n int) {
	for _, e := range r.timeline() {
		if e.Type == EventError {
			n++
		}
	}
	return n
}

// A write to the provider fails twice mid-speech: each failure is a timeline error and the next audio
// opens a fresh stream; the call stays up and the third stream's transcript reaches the model.
func TestFlowE2E_STTSendFailureKeepsTheCall(t *testing.T) {
	p := &flakySTT{failStreams: 2}
	r := sttSendRig(t, map[string]*flakySTT{"s": p})
	c := r.dial(sttSendFlow(t, r, `"model":"s"`))
	go c.sendAudioLoose(callerSpeech(t))
	armOnSpeech(t, r, p)
	c.waitReplies("hello from stream 3", 1)
	if c.ended() {
		t.Fatal("a failed write to the STT provider ended the call")
	}
	if n := p.failed.Load(); n != 2 {
		t.Fatalf("failed writes = %d, want 2", n)
	}
	if n := timelineErrors(r); n < 2 {
		t.Fatalf("timeline errors = %d, want a failed write each", n)
	}
	if rec := r.session(c.sessionID); rec.Status != "active" {
		t.Fatalf("session record %+v", rec)
	}
}

// The provider stays down after the failed write: every redial is refused, the turn loses its transcript
// (a local, non-fatal error), and a second lost turn a window later ends the call as stt_unavailable.
func TestFlowE2E_STTSendFailureThenDeadProviderHangsUp(t *testing.T) {
	p := &flakySTT{failStreams: 1, deadAfter: true}
	r := sttSendRig(t, map[string]*flakySTT{"s": p}, func(c *Config) { c.Turn.NoTranscriptTimeout = time.Second })
	c := r.dial(sttSendFlow(t, r, `"model":"s"`))
	go c.sendAudioLoose(callerSpeech(t))
	armOnSpeech(t, r, p)
	eventually(t, "the failed write and the refused redial on the timeline", func() bool { return timelineErrors(r) >= 2 })
	time.Sleep(1200 * time.Millisecond) // a miscounted first failure would hang up here; and the window has passed
	if c.ended() {
		t.Fatal("the call ended after the first lost turn")
	}
	c.sendAudioLoose(callerSpeech(t))
	eventually(t, "the call to end after its STT stayed down", c.ended)
	r.waitEnded(c.sessionID, storage.EndError)
	if !r.has(EventDisconnect, "terminated: stt unavailable") {
		t.Fatalf("timeline lacks the stt_unavailable hang-up: %+v", r.timeline())
	}
	if countOf(c.wireLog(), "msg:error") == 0 {
		t.Fatal("the caller got no RTVI error naming the hang-up")
	}
}

// The failing stream is the LAST candidate of an STT failover chain: the primary's failed write moves the
// utterance to the fallback, whose own failed write used to end the call because the chain had nowhere
// left to go. Now it is a reported error and the next audio reopens the fallback.
func TestFlowE2E_STTSendFailureFailsOverOnLastCandidate(t *testing.T) {
	primary, fallback := &flakySTT{failStreams: 99}, &flakySTT{failStreams: 1}
	r := sttSendRig(t, map[string]*flakySTT{"s": primary, "f": fallback})
	c := r.dial(sttSendFlow(t, r, `"model":"s","fallbacks":[{"provider":"echo","model":"f"}]`))
	go c.sendAudioLoose(callerSpeech(t))
	armOnSpeech(t, r, primary, fallback)
	c.waitReplies("hello from stream 2", 1)
	if c.ended() {
		t.Fatal("the last STT candidate's failed write ended the call")
	}
	if n := fallback.failed.Load(); n != 1 {
		t.Fatalf("the fallback's failed writes = %d, want 1", n)
	}
	if primary.failed.Load() == 0 {
		t.Fatal("the primary never failed, so the fallback was not the last candidate in play")
	}
}
