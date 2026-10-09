package core

import (
	"context"
	"errors"
	"time"
)

// UnitFailureReporter sits right after the TTS stage. A synthesis unit that
// fails before releasing any audio never gets a write completion, so the
// assistant context would wait for it forever (the reply stays active and
// uncommitted). On such an ErrorFrame it re-enters a Failed completion at the
// top of the pipeline. Errors a TTS failover stage swallows never reach it.
type UnitFailureReporter struct {
	session  SessionController
	index    int
	maxAudio uint64 // highest unit id that produced audio; units play in order
}

// unitFailureCheck re-asks "did this unit play audio?" behind the data queue:
// an ErrorFrame is control and overtakes the unit's audio still queued as data.
type unitFailureCheck struct{ err *ErrorFrame }

func (*unitFailureCheck) Type() FrameType { return FrameInternal }

func (r *UnitFailureReporter) Attach(s SessionController, index int) error {
	r.session, r.index = s, index
	return nil
}

func (r *UnitFailureReporter) ProcessFrame(_ context.Context, f Frame, dir Direction, e Emitter) error {
	switch v := f.(type) {
	case *OutputAudioFrame:
		r.maxAudio = max(r.maxAudio, v.UnitID)
	case *ErrorFrame:
		if v.UnitID > r.maxAudio && dir == Downstream && r.session != nil {
			r.check(&unitFailureCheck{v})
		}
	case *unitFailureCheck:
		if v.err.UnitID > r.maxAudio {
			_ = r.session.Reenter(&WriteCompletionFrame{Turn: string(v.err.Turn), Gen: uint64(v.err.Gen), UnitID: v.err.UnitID, IsFinal: true, Failed: true}, Downstream, 0, uint64(v.err.Gen))
		}
		return nil
	}
	return e.PushDirection(f, dir)
}

// check queues c behind the data already admitted. A full inbox refuses
// internal frames, so retry until the session takes it or closes.
func (r *UnitFailureReporter) check(c *unitFailureCheck) {
	if errors.Is(r.session.Reenter(c, Downstream, r.index, uint64(c.err.Gen)), ErrQueueFull) {
		time.AfterFunc(5*time.Millisecond, func() { r.check(c) })
	}
}
