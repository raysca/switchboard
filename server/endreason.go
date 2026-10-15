package server

import (
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
