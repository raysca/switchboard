package core

// SessionID and TurnID are opaque, distinct identities. Generation is a
// session-scoped cancellation fence, not a turn or persistent identifier.
type SessionID string
type TurnID string
type Generation uint64
