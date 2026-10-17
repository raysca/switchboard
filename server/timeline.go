package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"govoice/core/resilience"
)

// TimelineEventType categorizes trace events for a session timeline.
type TimelineEventType string

const (
	EventConnect       TimelineEventType = "connect"
	EventAuthenticate  TimelineEventType = "authenticate"
	EventReady         TimelineEventType = "ready"
	EventSpeechStart   TimelineEventType = "speech_start"
	EventSpeechStop    TimelineEventType = "speech_stop"
	EventSTTResult     TimelineEventType = "stt_result"
	EventLLMFirstToken TimelineEventType = "llm_first_token"
	EventLLMComplete   TimelineEventType = "llm_complete"
	EventTTSUnit       TimelineEventType = "tts_unit"
	EventInterruption  TimelineEventType = "interruption"
	EventFlushAck      TimelineEventType = "flush_ack"
	EventProviderRetry TimelineEventType = "provider_retry"
	EventCircuitState  TimelineEventType = "circuit_state"
	EventLatency       TimelineEventType = "latency" // Details = stage, DurationMs = value
	EventError         TimelineEventType = "error"
	EventDisconnect    TimelineEventType = "disconnect"

	// Routed flows: the call moving between steps.
	EventStepPreparing TimelineEventType = "step_preparing" // Details = target step being built
	EventRoute         TimelineEventType = "route"          // Details = "from->to", DurationMs = commit to active
	EventStepActive    TimelineEventType = "step_active"    // Details = step now answering
	EventStepClosed    TimelineEventType = "step_closed"    // Details = step left and closed
	EventRouteFailed   TimelineEventType = "route_failed"   // Details = target step, Error = cause
	EventCueStart      TimelineEventType = "cue_start"      // the route cue began playing
	EventCueStop       TimelineEventType = "cue_stop"       // Details = why (first_audio, barge_in, failed, cap)
)

// TimelineEvent represents an immutable diagnostic record in a session trace.
type TimelineEvent struct {
	Timestamp  time.Time         `json:"timestamp"`
	SessionID  uint64            `json:"session_id"`
	TurnID     string            `json:"turn_id,omitempty"`
	Gen        uint64            `json:"gen,omitempty"`
	Type       TimelineEventType `json:"type"`
	Provider   string            `json:"provider,omitempty"`
	Step       string            `json:"step,omitempty"` // flow step that produced the event
	Details    string            `json:"details,omitempty"`
	DurationMs int64             `json:"duration_ms,omitempty"`
	Error      string            `json:"error,omitempty"`
}

// SessionTimeline stores a bounded circular ring of diagnostic events for a single call.
type SessionTimeline struct {
	sessionID uint64
	maxEvents int
	ttl       time.Duration

	step         atomic.Pointer[string] // flow step answering the call; stamped on events
	mu           sync.Mutex
	events       []TimelineEvent
	head         int
	count        int
	droppedCount atomic.Uint64
	createdAt    time.Time
	lastEventAt  time.Time // guarded by mu
}

// NewSessionTimeline initializes a bounded ring buffer for a call.
func NewSessionTimeline(sessionID uint64, maxEvents int, ttl time.Duration) *SessionTimeline {
	if maxEvents <= 0 {
		maxEvents = 500
	}
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	return &SessionTimeline{
		sessionID: sessionID,
		maxEvents: maxEvents,
		ttl:       ttl,
		events:    make([]TimelineEvent, maxEvents),
		createdAt: time.Now(),
	}
}

// Record appends an event to the ring buffer. It never blocks.
func (t *SessionTimeline) Record(e TimelineEvent) {
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}
	e.SessionID = t.sessionID
	if e.Step == "" {
		if n := t.step.Load(); n != nil {
			e.Step = *n
		}
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastEventAt = e.Timestamp

	idx := (t.head + t.count) % t.maxEvents
	if t.count < t.maxEvents {
		t.events[idx] = e
		t.count++
	} else {
		// Evict oldest
		t.events[t.head] = e
		t.head = (t.head + 1) % t.maxEvents
		t.droppedCount.Add(1)
	}
}

// SetStep records which flow step answers the call; later events carry it.
func (t *SessionTimeline) SetStep(id string) { t.step.Store(&id) }

// Events returns an ordered copy of all non-expired events.
func (t *SessionTimeline) Events() []TimelineEvent {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	result := make([]TimelineEvent, 0, t.count)
	for i := 0; i < t.count; i++ {
		idx := (t.head + i) % t.maxEvents
		ev := t.events[idx]
		if now.Sub(ev.Timestamp) <= t.ttl {
			result = append(result, ev)
		}
	}
	return result
}

// DroppedCount returns the number of events dropped due to buffer capacity.
func (t *SessionTimeline) DroppedCount() uint64 {
	return t.droppedCount.Load()
}

// TimelineRegistry manages active and recent call timelines.
type TimelineRegistry struct {
	mu        sync.RWMutex
	timelines map[uint64]*SessionTimeline
	maxEvents int
	ttl       time.Duration
}

// NewTimelineRegistry creates an empty registry.
func NewTimelineRegistry(maxEvents int, ttl time.Duration) *TimelineRegistry {
	return &TimelineRegistry{
		timelines: make(map[uint64]*SessionTimeline),
		maxEvents: maxEvents,
		ttl:       ttl,
	}
}

// GetOrCreate returns or initializes a timeline for a call.
func (r *TimelineRegistry) GetOrCreate(sessionID uint64) *SessionTimeline {
	r.mu.Lock()
	defer r.mu.Unlock()

	if tl, ok := r.timelines[sessionID]; ok {
		return tl
	}
	tl := NewSessionTimeline(sessionID, r.maxEvents, r.ttl)
	r.timelines[sessionID] = tl
	return tl
}

// Get returns the timeline if present.
func (r *TimelineRegistry) Get(sessionID uint64) *SessionTimeline {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.timelines[sessionID]
}

// PruneExpired removes timelines idle for longer than TTL, so a long live call
// keeps its timeline.
func (r *TimelineRegistry) PruneExpired() {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	for id, tl := range r.timelines {
		tl.mu.Lock()
		last := tl.lastEventAt
		tl.mu.Unlock()
		if last.IsZero() {
			last = tl.createdAt
		}
		if now.Sub(last) > tl.ttl {
			delete(r.timelines, id)
		}
	}
}

// Handler returns the HTTP handler for GET /debug/timeline?session=<id>.
func (r *TimelineRegistry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		sessionStr := req.URL.Query().Get("session")
		if sessionStr == "" {
			http.Error(w, `{"error":"session query parameter required"}`, http.StatusBadRequest)
			return
		}

		sessionID, err := strconv.ParseUint(sessionStr, 10, 64)
		if err != nil {
			http.Error(w, `{"error":"invalid session id"}`, http.StatusBadRequest)
			return
		}

		tl := r.Get(sessionID)
		if tl == nil {
			http.Error(w, `{"error":"timeline not found for session"}`, http.StatusNotFound)
			return
		}

		events := tl.Events()
		resp := map[string]any{
			"session_id":    sessionID,
			"event_count":   len(events),
			"dropped_count": tl.DroppedCount(),
			"events":        events,
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(resp)
	})
}

// recordCircuit puts a provider circuit's state change on the timeline of every
// live call whose start step uses that provider for that stage. Breakers report
// every success and failure, so the dedupe has its own lock and the server's
// mu is taken only for a real change; a circuit never seen before counts as
// closed. Breakers notify after unlocking, so reports from concurrent requests
// can arrive out of order and a timeline can end on "open" while the breaker is
// closed again (the circuit gauge has the same race).
// ponytail: matches the start step's candidates; a later step's providers are not matched.
func (s *Server) recordCircuit(stage, provider string, st resilience.State) {
	key := stage + "/" + provider
	s.circuitMu.Lock()
	prev, seen := s.circuitStates[key]
	if (seen && prev == st) || (!seen && st == resilience.Closed) {
		s.circuitMu.Unlock()
		return
	}
	if s.circuitStates == nil {
		s.circuitStates = map[string]resilience.State{}
	}
	s.circuitStates[key] = st
	s.circuitMu.Unlock()
	var timelines []*SessionTimeline
	s.mu.Lock()
	for _, c := range s.activeCalls {
		if c.providers[key] {
			timelines = append(timelines, c.timeline)
		}
	}
	s.mu.Unlock()
	for _, tl := range timelines {
		tl.Record(TimelineEvent{Type: EventCircuitState, Provider: provider, Details: stage + " " + strings.ToLower(string(st))})
	}
}
