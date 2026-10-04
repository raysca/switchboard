package core

import "time"

// DebugEvent is a protocol-neutral semantic value. A later adapter may map it
// to a selected debug wire format without introducing that protocol into core.
type DebugEvent struct {
	Name       string
	Time       time.Time
	SessionID  SessionID
	TurnID     TurnID
	Generation Generation
	attributes map[string]any
}

func NewDebugEvent(name string, at time.Time, session SessionID, turn TurnID, gen Generation, attributes map[string]any) DebugEvent {
	return DebugEvent{Name: name, Time: at, SessionID: session, TurnID: turn, Generation: gen, attributes: safeEventAttributes(TracePolicy{}, attributes)}
}

func (e DebugEvent) Attributes() map[string]any { return cloneAttributes(e.attributes) }
