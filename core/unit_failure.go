package core

import "context"

// UnitFailureReporter sits right after the TTS stage. A synthesis unit that
// fails before releasing any audio never gets a write completion, so the
// assistant context would wait for it forever (the reply stays active and
// uncommitted). On such an ErrorFrame it re-enters a Failed completion at the
// top of the pipeline. Errors a TTS failover stage swallows never reach it.
type UnitFailureReporter struct {
	session  SessionController
	maxAudio uint64 // highest unit id that produced audio; units play in order
}

func (r *UnitFailureReporter) Attach(s SessionController, _ int) error {
	r.session = s
	return nil
}

func (r *UnitFailureReporter) ProcessFrame(_ context.Context, f Frame, dir Direction, e Emitter) error {
	switch v := f.(type) {
	case *OutputAudioFrame:
		r.maxAudio = max(r.maxAudio, v.UnitID)
	case *ErrorFrame:
		if v.UnitID > r.maxAudio && dir == Downstream && r.session != nil {
			_ = r.session.Reenter(&WriteCompletionFrame{Turn: string(v.Turn), Gen: uint64(v.Gen), UnitID: v.UnitID, IsFinal: true, Failed: true}, Downstream, 0, uint64(v.Gen))
		}
	}
	return e.PushDirection(f, dir)
}
