// Package trace provides framework-to-server trace persistence.
package trace

import (
	"context"
	"encoding/json"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"govoice/core"
	"govoice/server/storage"
)

const (
	queueSize                  = 2048
	maxLabelLength             = 256
	maxStructuredMetadataNodes = 1024
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

	parentSpan := core.SpanFromContext(ctx)
	parentID := extractSpanID(parentSpan)

	sessionID := string(event.SessionID)
	if !isValidUUIDv7(sessionID) {
		sessionID = ""
		if pInfo, ok := parentSpan.(sessionInfo); ok && isValidUUIDv7(pInfo.SessionID()) {
			sessionID = pInfo.SessionID()
		}
	}

	turnID := string(event.TurnID)
	if !isValidUUIDv7(turnID) {
		turnID = ""
		if pInfo, ok := parentSpan.(sessionInfo); ok && isValidUUIDv7(pInfo.TurnID()) {
			turnID = pInfo.TurnID()
		}
	}

	rawAttrs := event.Attributes()
	attrs := a.filterAttributes(rawAttrs)
	if !isValidUUIDv7(string(event.SessionID)) && string(event.SessionID) != "" {
		attrs["session_label"] = string(event.SessionID)
	}
	if !isValidUUIDv7(string(event.TurnID)) && string(event.TurnID) != "" {
		attrs["turn_label"] = string(event.TurnID)
	}

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
	// ponytail: metadata-only; transcript/prompt/text/tool content never persists.
	out := make(map[string]any)
	var customKeys []string
	tooManyCustom := false

	for k, v := range input {
		switch k {
		case "count", "sample_rate", "channels", "duration_ms", "latency_ms", "sequence", "model", "provider", "format", "status", "error.category", "unit", "quantity", "reason", "component", "frame", "direction":
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
		default:
			if strings.HasPrefix(k, "custom.") && safeKey(k) {
				if len(customKeys) == maxStructuredMetadataNodes {
					tooManyCustom = true
				} else {
					customKeys = append(customKeys, k)
				}
			}
		}
	}

	if tooManyCustom {
		return out
	}

	sort.Strings(customKeys)
	remaining := maxStructuredMetadataNodes
	for _, key := range customKeys {
		if copied, ok := copyStructuredMetadata(input[key], 0, &remaining); ok {
			out[key] = copied
		}
	}
	return out
}

func safeKey(key string) bool {
	if key == "" || len(key) > 64 {
		return false
	}
	lower := strings.ToLower(key)
	for _, forbidden := range []string{
		"secret", "token", "password", "credential", "api_key", "audio",
		"transcript", "prompt", "argument", "result", "text",
	} {
		if strings.Contains(lower, forbidden) {
			return false
		}
	}
	for _, r := range key {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

// copyStructuredMetadata accepts bounded JSON-like numeric/boolean metadata.
// Strings, bytes, pointers, and unsupported collections fail closed.
func copyStructuredMetadata(value any, depth int, remaining *int) (any, bool) {
	if depth > 8 || *remaining == 0 {
		return nil, false
	}
	*remaining--
	switch v := value.(type) {
	case bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return v, true
	case float32:
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return nil, false
		}
		return v, true
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, false
		}
		return v, true
	case map[string]any:
		if len(v) > 64 {
			return nil, false
		}
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make(map[string]any, len(v))
		for _, key := range keys {
			if !safeKey(key) {
				return nil, false
			}
			copied, ok := copyStructuredMetadata(v[key], depth+1, remaining)
			if !ok {
				return nil, false
			}
			out[key] = copied
		}
		return out, true
	case []any:
		if len(v) > 64 {
			return nil, false
		}
		out := make([]any, len(v))
		for i, child := range v {
			copied, ok := copyStructuredMetadata(child, depth+1, remaining)
			if !ok {
				return nil, false
			}
			out[i] = copied
		}
		return out, true
	default:
		return nil, false
	}
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

type sessionInfo interface {
	SessionID() string
	TurnID() string
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

func (s *adapterSpan) SpanID() string    { return s.id }
func (s *adapterSpan) SessionID() string { return s.sessionID }
func (s *adapterSpan) TurnID() string    { return s.turnID }

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
		s.attributes["error.category"] = err.Error()
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
