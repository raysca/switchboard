package server

import (
	"errors"
	"sync/atomic"
	"time"

	"govoice/core"
	coreturn "govoice/core/turn"
	"govoice/server/storage"
)

// endReason holds why one call ended. The first cause named wins (an idle hang-up followed by the socket
// closing stays "silent caller"); without one, the transport's own close cause is used, else the caller
// hung up. It is lock-free and a nil holder is allowed.
type endReason struct {
	v         atomic.Pointer[string]
	transport func() string // the connection's CloseCause, set when the call has one
}

func (e *endReason) set(reason string) {
	if e != nil && reason != "" {
		e.v.CompareAndSwap(nil, &reason)
	}
}

func (e *endReason) get() string {
	if e == nil {
		return storage.EndCallerHungUp
	}
	if p := e.v.Load(); p != nil {
		return *p
	}
	if e.transport != nil {
		if r := reasonFromTransport(e.transport()); r != "" {
			return r
		}
	}
	return storage.EndCallerHungUp
}

// reasonFromEnd maps what the idle controller says ended a call to the stored vocabulary. (The
// max-duration timer and the drain name their reasons directly.)
func reasonFromEnd(reason string) string {
	switch reason {
	case core.IdleReasonUser:
		return storage.EndSilentCaller
	case core.IdleReasonBot:
		return storage.EndAgentStuck
	case EndReasonFlow:
		return storage.EndedByFlow
	}
	return storage.EndCallerHungUp
}

// reasonFromTransport maps ws.Connection.CloseCause; empty when the transport named no cause.
func reasonFromTransport(cause string) string {
	switch cause {
	case "peer_unresponsive":
		return storage.EndConnectionLost
	case "session_start_failed":
		return storage.EndSetupFailed
	}
	return ""
}

// sttUnavailableLimit is how many STT-unreachable errors, at least one no-transcript window apart
// with no transcript between them, end a call. An STT failover recovers or swallows such errors until
// its chain is exhausted, so only a stage with nothing left to try reaches this count. Not
// configurable on purpose.
const sttUnavailableLimit = 2

// sttOutage counts STT-unreachable error frames. STT keys its errors by VAD segment, and the turn
// processor merges several segments into one logical turn that STT never sees, so a failure counts
// only once a full no-transcript window (turn.no_transcript_timeout) has passed since the last
// counted one: segments inside one window are one lost turn. It is used from the one goroutine
// that observes the call's pipeline output, so it needs no lock.
type sttOutage struct {
	window time.Duration    // 0: turn.DefaultNoTranscriptTimeout
	now    func() time.Time // nil: time.Now (monotonic); tests inject a clock
	n      int
	last   *core.ErrorFrame // OnFrame sees one frame at every pipeline hop: count it once
	at     time.Time        // when the last failure was counted
}

// observe reports true once, when f is the error that reaches sttUnavailableLimit.
func (o *sttOutage) observe(f core.Frame) bool {
	switch fr := f.(type) {
	case *core.TranscriptCandidateFrame, *core.UserMessageFrame:
		o.n, o.last = 0, nil
	case *core.ErrorFrame:
		// A frame with no turn is an unbound pre-onset dial: no turn lost its transcript.
		if !errors.Is(fr.Err, core.ErrSTTUnavailable) || fr.Turn == "" || fr == o.last {
			return false
		}
		o.last = fr
		now := time.Now
		if o.now != nil {
			now = o.now
		}
		window := o.window
		if window <= 0 {
			window = coreturn.DefaultNoTranscriptTimeout
		}
		t := now()
		if o.n > 0 && t.Sub(o.at) < window {
			return false
		}
		o.at = t
		o.n++
		return o.n == sttUnavailableLimit
	}
	return false
}
