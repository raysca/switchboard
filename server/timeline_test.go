package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestTimeline_OrderingAndCapacityEviction(t *testing.T) {
	// Ring buffer with small capacity (5 events)
	tl := NewSessionTimeline(101, 5, 10*time.Minute)

	// Record 7 events
	for i := 1; i <= 7; i++ {
		tl.Record(TimelineEvent{
			TurnID:  "turn-1",
			Gen:     1,
			Type:    EventSpeechStart,
			Details: "event",
		})
	}

	events := tl.Events()
	if len(events) != 5 {
		t.Fatalf("expected 5 bounded events, got %d", len(events))
	}

	// Verify dropped count = 2
	if tl.DroppedCount() != 2 {
		t.Fatalf("expected 2 dropped events, got %d", tl.DroppedCount())
	}
}

func TestTimeline_TranscriptOmittedByDefault(t *testing.T) {
	reg := NewTimelineRegistry(10, 10*time.Minute)
	tl := reg.GetOrCreate(102)

	tl.Record(TimelineEvent{
		TurnID:  "turn-1",
		Gen:     1,
		Type:    EventSTTResult,
		Details: "STT completed (transcript omitted)",
	})

	handler := reg.Handler()
	req := httptest.NewRequest("GET", "/debug/timeline?session=102", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var resp struct {
		SessionID    uint64          `json:"session_id"`
		EventCount   int             `json:"event_count"`
		DroppedCount uint64          `json:"dropped_count"`
		Events       []TimelineEvent `json:"events"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal json: %v", err)
	}

	if resp.SessionID != 102 || resp.EventCount != 1 {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if resp.Events[0].Type != EventSTTResult {
		t.Fatalf("expected event type stt_result, got: %s", resp.Events[0].Type)
	}
}

func TestTimeline_NonBlockingConcurrentRingEviction(t *testing.T) {
	const capacity = 50
	const goroutines = 10
	const eventsPerGoroutine = 100

	tl := NewSessionTimeline(201, capacity, 10*time.Minute)

	var wg sync.WaitGroup
	startCh := make(chan struct{})

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			<-startCh
			for i := 0; i < eventsPerGoroutine; i++ {
				tl.Record(TimelineEvent{
					TurnID:  "turn-1",
					Gen:     1,
					Type:    EventTTSAudio,
					Details: "audio chunk",
				})
			}
		}(g)
	}

	close(startCh)
	wg.Wait()

	events := tl.Events()
	if len(events) != capacity {
		t.Fatalf("expected bounded event count %d, got %d", capacity, len(events))
	}

	expectedDropped := uint64(goroutines*eventsPerGoroutine - capacity)
	if tl.DroppedCount() != expectedDropped {
		t.Fatalf("expected dropped count %d, got %d", expectedDropped, tl.DroppedCount())
	}
}

func TestTimeline_FaultAndMetricIncrement(t *testing.T) {
	metrics := NewMetrics()
	reg := NewTimelineRegistry(100, 10*time.Minute)
	tl := reg.GetOrCreate(301)

	// Simulate fault
	tl.Record(TimelineEvent{
		TurnID:   "turn-2",
		Gen:      3,
		Type:     EventError,
		Provider: "google.llm",
		Error:    "rate limit exceeded",
	})
	metrics.LLMErrorsTotal.Add(1)
	metrics.LLMRetriesTotal.Add(1)

	// Verify timeline
	events := tl.Events()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	ev := events[0]
	if ev.Type != EventError || ev.Provider != "google.llm" || ev.TurnID != "turn-2" || ev.Gen != 3 {
		t.Fatalf("unexpected event: %+v", ev)
	}

	// Verify Prometheus metrics
	var buf bytes.Buffer
	metrics.WritePrometheus(&buf)
	metricsOutput := buf.String()
	if !bytes.Contains(buf.Bytes(), []byte("voice_llm_errors_total 1")) {
		t.Fatalf("missing voice_llm_errors_total metric:\n%s", metricsOutput)
	}
}

// A timeline that is still receiving events outlives its TTL; an idle one goes.
func TestTimelinePruneKeepsActiveTimelines(t *testing.T) {
	r := NewTimelineRegistry(10, 50*time.Millisecond)
	live, idle := r.GetOrCreate(1), r.GetOrCreate(2)
	idle.Record(TimelineEvent{Type: EventConnect})
	time.Sleep(80 * time.Millisecond)
	live.Record(TimelineEvent{Type: EventReady})
	r.PruneExpired()
	if r.Get(1) == nil {
		t.Fatal("live call's timeline was pruned")
	}
	if r.Get(2) != nil {
		t.Fatal("idle timeline was kept")
	}
}
