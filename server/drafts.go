package server

import (
	"errors"
	"sync"
	"time"

	"govoice/server/agent"
)

// maxDrafts bounds the draft calls waiting for their caller to connect.
const maxDrafts = 32

var errTooManyDrafts = errors.New("too many draft calls are waiting to connect")

// draftGrace keeps an entry past its expiry: the token validator already refused an expired
// token, and one accepted in its last instant must still find its draft rather than fall
// through to the published version.
const draftGrace = time.Minute

// draftCall is a validated draft held until its voice token connects. It is in
// memory only: a draft is never stored as a flow version.
type draftCall struct {
	snapshot agent.Snapshot
	expires  time.Time
}

// draftCalls holds drafts by voice-token id (which is also the session id). A restart
// drops them, and the token epoch check refuses the tokens that named them.
type draftCalls struct {
	mu      sync.Mutex
	pending map[string]draftCall
}

func newDraftCalls() *draftCalls { return &draftCalls{pending: map[string]draftCall{}} }

func (d *draftCalls) put(id string, c draftCall, now time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for k, e := range d.pending {
		if now.After(e.expires.Add(draftGrace)) {
			delete(d.pending, k)
		}
	}
	if len(d.pending) >= maxDrafts {
		return errTooManyDrafts
	}
	d.pending[id] = c
	return nil
}

// take consumes the draft for a token; found is false for an ordinary token.
func (d *draftCalls) take(id string) (draftCall, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, ok := d.pending[id]
	delete(d.pending, id)
	return c, ok
}
