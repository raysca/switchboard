// Package trace provides framework-to-server trace persistence.
package trace

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"govoice/core"
	"govoice/server/storage"
)

const (
	queueSize      = 2048
	maxLabelLength = 256
)

type traceOpKind uint8

const (
	opStartSpan traceOpKind = iota + 1
	opEndSpan
	opAddEvent
)

type traceOp struct {
	kind       traceOpKind
	spanID     string
	parentID   string
	sessionID  string
	turnID     string
	name       string
	startedAt  time.Time
	endedAt    time.Time
	occurredAt time.Time
	attributes string
}

// Adapter translates framework traces and events to server trace persistence.
type Adapter struct {
	repo      storage.TraceRepository
	queue     chan traceOp
	closed    atomic.Bool
	done      chan struct{}
	closeOnce sync.Once
	closeMu   sync.RWMutex
}

// NewAdapter creates a new non-blocking trace adapter.
func NewAdapter(repo storage.TraceRepository) *Adapter {
	a := &Adapter{
		repo:  repo,
		queue: make(chan traceOp, queueSize),
		done:  make(chan struct{}),
	}
	go a.worker()
	return a
}

// StartSpan implements core.Tracer.
func (a *Adapter) StartSpan(ctx context.Context, event core.TraceEvent) core.Span {
	spanID := storage.NewID()
	startedAt := event.Time
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}

	parentID := extractSpanID(core.SpanFromContext(ctx))

	sessionID := string(event.SessionID)
	if !isValidUUIDv7(sessionID) {
		sessionID = ""
	}
	turnID := string(event.TurnID)
	if !isValidUUIDv7(turnID) {
		turnID = ""
	}

	attrs := a.filterAttributes(event.Attributes())

	span := &adapterSpan{
		adapter:    a,
		id:         spanID,
		parentID:   parentID,
		sessionID:  sessionID,
		turnID:     turnID,
		name:       event.Name,
		startedAt:  startedAt,
		attributes: attrs,
	}

	attrJSON, _ := json.Marshal(attrs)
	a.enqueue(traceOp{
		kind:       opStartSpan,
		spanID:     spanID,
		parentID:   parentID,
		sessionID:  sessionID,
		turnID:     turnID,
		name:       event.Name,
		startedAt:  startedAt,
		attributes: string(attrJSON),
	})

	return span
}

func (a *Adapter) enqueue(op traceOp) (sent bool) {
	if a.closed.Load() {
		return false
	}
	defer func() {
		if recover() != nil {
			sent = false
		}
	}()

	a.closeMu.RLock()
	defer a.closeMu.RUnlock()

	if a.closed.Load() {
		return false
	}
	select {
	case a.queue <- op:
		return true
	default:
		return false
	}
}

// Close stops accepting new operations, drains queued work, and waits for the worker to exit.
func (a *Adapter) Close() error {
	a.closeOnce.Do(func() {
		a.closed.Store(true)
		a.closeMu.Lock()
		close(a.queue)
		a.closeMu.Unlock()
		<-a.done
	})
	return nil
}

func (a *Adapter) worker() {
	defer close(a.done)
	ctx := context.Background()

	for op := range a.queue {
		switch op.kind {
		case opStartSpan:
			err := a.repo.AddSpan(ctx, storage.TraceSpan{
				ID:             op.spanID,
				SessionID:      op.sessionID,
				TurnID:         op.turnID,
				ParentID:       op.parentID,
				Name:           op.name,
				StartedAt:      op.startedAt,
				AttributesJSON: op.attributes,
			})
			if err != nil && op.parentID != "" {
				// Retry without parent if parent failed foreign key constraint
				_ = a.repo.AddSpan(ctx, storage.TraceSpan{
					ID:             op.spanID,
					SessionID:      op.sessionID,
					TurnID:         op.turnID,
					ParentID:       "",
					Name:           op.name,
					StartedAt:      op.startedAt,
					AttributesJSON: op.attributes,
				})
			}
		case opEndSpan:
			_ = a.repo.EndSpan(ctx, op.spanID, op.endedAt, op.attributes)
		case opAddEvent:
			ev := storage.TraceEvent{
				ID:             op.spanID,
				SpanID:         op.parentID,
				SessionID:      op.sessionID,
				TurnID:         op.turnID,
				Name:           op.name,
				OccurredAt:     op.occurredAt,
				AttributesJSON: op.attributes,
			}
			_ = a.repo.AddEvent(ctx, ev)
		}
	}
}

func (a *Adapter) filterAttributes(input map[string]any) map[string]any {
	// ponytail: metadata-only; an exact whitelist of keys the server sends, so transcript/prompt/text/tool content never persists.
	out := make(map[string]any)
	for k, v := range input {
		switch k {
		case "status", "error.category", "reason", "from_step", "to_step", "mcp.server_id", "mcp.tool_name", "mcp.duration_ms", "mcp.tool_count":
			switch val := v.(type) {
			case bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
				out[k] = val
			case string:
				if len(val) <= maxLabelLength {
					out[k] = val
				}
			case float32:
				if !math.IsNaN(float64(val)) && !math.IsInf(float64(val), 0) {
					out[k] = val
				}
			case float64:
				if !math.IsNaN(val) && !math.IsInf(val, 0) {
					out[k] = val
				}
			}
		}
	}
	return out
}

func isValidUUIDv7(s string) bool {
	u, err := uuid.Parse(s)
	return err == nil && u.Version() == 7 && u.Variant() == uuid.RFC4122 && u.String() == s
}

type spanWithID interface {
	SpanID() string
}

type spanUnwrapper interface {
	Unwrap() core.Span
}

func extractSpanID(s core.Span) string {
	for s != nil {
		if withID, ok := s.(spanWithID); ok {
			return withID.SpanID()
		}
		if unwrapper, ok := s.(spanUnwrapper); ok {
			s = unwrapper.Unwrap()
			continue
		}
		break
	}
	return ""
}

type adapterSpan struct {
	adapter    *Adapter
	id         string
	parentID   string
	sessionID  string
	turnID     string
	name       string
	startedAt  time.Time
	mu         sync.Mutex
	ended      bool
	attributes map[string]any
}

func (s *adapterSpan) SpanID() string { return s.id }

func (s *adapterSpan) SetAttribute(key string, value any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return
	}
	temp := map[string]any{key: value}
	filtered := s.adapter.filterAttributes(temp)
	for k, v := range filtered {
		s.attributes[k] = v
	}
}

func (s *adapterSpan) AddEvent(event core.TraceEvent) {
	s.mu.Lock()
	ended := s.ended
	sessID := s.sessionID
	turnID := s.turnID
	s.mu.Unlock()

	if ended {
		return
	}

	eventID := storage.NewID()
	occurredAt := event.Time
	if occurredAt.IsZero() {
		occurredAt = time.Now().UTC()
	}

	evSess := string(event.SessionID)
	if isValidUUIDv7(evSess) {
		sessID = evSess
	}
	evTurn := string(event.TurnID)
	if isValidUUIDv7(evTurn) {
		turnID = evTurn
	}

	attrs := s.adapter.filterAttributes(event.Attributes())
	attrJSON, _ := json.Marshal(attrs)

	s.adapter.enqueue(traceOp{
		kind:       opAddEvent,
		spanID:     eventID,
		parentID:   s.id, // span ID stored in parentID field for event
		sessionID:  sessID,
		turnID:     turnID,
		name:       event.Name,
		occurredAt: occurredAt,
		attributes: string(attrJSON),
	})
}

func (s *adapterSpan) End(err error) {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	now := time.Now().UTC()

	if err != nil {
		s.attributes["error.category"] = fmt.Sprintf("%T", err) // type only: messages can carry credentials
		s.attributes["status"] = "error"
	} else if _, ok := s.attributes["status"]; !ok {
		s.attributes["status"] = "ok"
	}

	// Copy attributes
	copied := make(map[string]any, len(s.attributes))
	for k, v := range s.attributes {
		copied[k] = v
	}
	s.mu.Unlock()

	attrJSON, _ := json.Marshal(copied)
	s.adapter.enqueue(traceOp{
		kind:       opEndSpan,
		spanID:     s.id,
		parentID:   s.parentID,
		sessionID:  s.sessionID,
		turnID:     s.turnID,
		name:       s.name,
		startedAt:  s.startedAt,
		endedAt:    now,
		attributes: string(attrJSON),
	})
}

// Compile-time interface checks.
var (
	_ core.Tracer = (*Adapter)(nil)
	_ core.Span   = (*adapterSpan)(nil)
)
