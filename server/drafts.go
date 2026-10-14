package server

import (
	"errors"
	"sync"
	"time"

	"govoice/server/flow"
)

// maxDrafts bounds the draft calls waiting for their caller to connect.
const maxDrafts = 32

var errTooManyDrafts = errors.New("too many draft calls are waiting to connect")

// draftCall is a validated draft held until its voice token connects. It is in
// memory only: a draft is never stored as a flow version.
type draftCall struct {
	flowID   string
	snapshot flow.Snapshot
	expires  time.Time
}

// draftCalls holds drafts by voice-token id (which is also the session id).
type draftCalls struct {
	mu      sync.Mutex
	pending map[string]draftCall
	issued  map[string]time.Time // ids ever issued as drafts, to tell a lost draft from an ordinary token
}

func newDraftCalls() *draftCalls {
	return &draftCalls{pending: map[string]draftCall{}, issued: map[string]time.Time{}}
}

func (d *draftCalls) sweepLocked(now time.Time) {
	for id, c := range d.pending {
		if !now.Before(c.expires) {
			delete(d.pending, id)
		}
	}
	for id, exp := range d.issued {
		if !now.Before(exp) {
			delete(d.issued, id)
		}
	}
}

func (d *draftCalls) put(id string, c draftCall, now time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sweepLocked(now)
	if len(d.pending) >= maxDrafts {
		return errTooManyDrafts
	}
	d.pending[id] = c
	d.issued[id] = c.expires
	return nil
}

// take consumes the draft for a token. lost reports a token that was issued as
// a draft whose entry is gone (used, expired): it must fail, never fall back
// to the published version.
func (d *draftCalls) take(id string, now time.Time) (c draftCall, found, lost bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sweepLocked(now)
	if c, ok := d.pending[id]; ok {
		delete(d.pending, id)
		return c, true, false
	}
	_, wasIssued := d.issued[id]
	return draftCall{}, false, wasIssued
}
