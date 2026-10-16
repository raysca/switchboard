package server

import (
	"errors"
	"sync/atomic"

	"govoice/core"
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
	case "connection_lifetime":
		return storage.EndTimeLimit
	case "session_start_failed":
		return storage.EndSetupFailed
	}
	return ""
}

// sttUnavailableLimit is how many STT-unreachable errors in a row (no transcript between them)
// end a call. An STT failover recovers or swallows such errors until its chain is exhausted, so
// only a stage with nothing left to try reaches this count. Not configurable on purpose.
const sttUnavailableLimit = 2

// sttOutage counts consecutive STT-unreachable error frames. It is used from the one goroutine that
// observes the call's pipeline output, so it needs no lock.
type sttOutage struct {
	n    int
	last *core.ErrorFrame // OnFrame sees one frame at every pipeline hop: count it once (like lastSTTTurn)
}

// observe reports true once, when f is the error that reaches sttUnavailableLimit.
func (o *sttOutage) observe(f core.Frame) bool {
	switch fr := f.(type) {
	case *core.TranscriptCandidateFrame, *core.UserMessageFrame:
		o.n = 0
	case *core.ErrorFrame:
		if errors.Is(fr.Err, core.ErrSTTUnavailable) && fr != o.last {
			o.last = fr
			o.n++
			return o.n == sttUnavailableLimit
		}
	}
	return false
}
