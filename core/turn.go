package core

// TurnState is a one-way lifecycle for a conversational turn.
type TurnState uint8

const (
	TurnIdle TurnState = iota
	TurnActive
	TurnCompleted
	TurnInterrupted
)

// TurnLifecycle serializes start and terminal decisions on a session run loop.
// It does not mint identity; the caller supplies a stable TurnID.
type TurnLifecycle struct {
	ID         TurnID
	Generation Generation
	State      TurnState
}

func (t *TurnLifecycle) Start(id TurnID, gen Generation) bool {
	if id == "" || gen == 0 || t.State == TurnActive || t.ID == id {
		return false
	}
	t.ID, t.Generation, t.State = id, gen, TurnActive
	return true
}
func (t *TurnLifecycle) Complete(id TurnID) bool {
	if t.State != TurnActive || t.ID != id {
		return false
	}
	t.State = TurnCompleted
	return true
}
func (t *TurnLifecycle) Interrupt(id TurnID) bool {
	if t.State != TurnActive || t.ID != id {
		return false
	}
	t.State = TurnInterrupted
	return true
}
