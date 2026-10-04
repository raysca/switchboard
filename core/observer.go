package core

import "time"

// Observer receives values that contain no frame pointer or mutation hook.
// Delivery, ordering, and failure isolation are implemented by the later
// observer integration task.
type Observer interface{ Observe(ObserverEvent) }

type ObserverFunc func(ObserverEvent)

func (fn ObserverFunc) Observe(event ObserverEvent) { fn(event) }

type ObserverEvent struct {
	Name           string
	Time           time.Time
	SessionID      SessionID
	TurnID         TurnID
	Generation     Generation
	FrameKind      FrameType
	Direction      Direction
	Processor      string
	ProcessorIndex int
	attributes     map[string]any
}

func NewObserverEvent(name string, at time.Time, session SessionID, turn TurnID, gen Generation, frameKind FrameType, direction Direction, processor string, processorIndex int, attributes map[string]any) ObserverEvent {
	return ObserverEvent{Name: name, Time: at, SessionID: session, TurnID: turn, Generation: gen, FrameKind: frameKind, Direction: direction, Processor: processor, ProcessorIndex: processorIndex, attributes: safeEventAttributes(TracePolicy{}, attributes)}
}

func (e ObserverEvent) Attributes() map[string]any { return cloneAttributes(e.attributes) }
