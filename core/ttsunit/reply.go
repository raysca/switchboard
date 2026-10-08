package ttsunit

import (
	"sync"

	"govoice/core"
)

// ReplySlot holds one provider session (typically a websocket) for the reply
// (Turn, Gen) whose units are being synthesized. Units run strictly in order,
// so a provider reuses the session across the units of a reply; a unit of a
// different reply replaces it. Drop is safe to call concurrently (barge-in).
type ReplySlot[S any] struct {
	mu    sync.Mutex
	turn  string
	gen   uint64
	s     S
	ok    bool
	Close func(S) // abort and close; called for stale, dropped or canceled sessions
}

// Get returns the live session for f's reply; a session of another reply is closed.
func (r *ReplySlot[S]) Get(f *core.SynthesisUnitFrame) (s S, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ok && (r.turn != f.Turn || r.gen != f.Gen) {
		r.drop()
	}
	return r.s, r.ok
}

func (r *ReplySlot[S]) Put(f *core.SynthesisUnitFrame, s S) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ok {
		r.drop()
	}
	r.turn, r.gen, r.s, r.ok = f.Turn, f.Gen, s, true
}

// Drop aborts and closes the session, if any; it does not block on the close.
func (r *ReplySlot[S]) Drop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.drop()
}

// Release forgets the session without closing it (the caller closes it).
func (r *ReplySlot[S]) Release() {
	r.mu.Lock()
	defer r.mu.Unlock()
	var zero S
	r.s, r.ok = zero, false
}

func (r *ReplySlot[S]) drop() {
	if !r.ok {
		return
	}
	s := r.s
	var zero S
	r.s, r.ok = zero, false
	go r.Close(s) // a goodbye write may take a moment: never under r.mu or the runner lock
}
