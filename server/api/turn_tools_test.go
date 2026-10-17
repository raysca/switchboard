package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"govoice/server/api"
	"govoice/server/storage"
)

func TestTurnsCarryTheirToolCalls(t *testing.T) {
	s, handler, sessID, turnID := setupTracesHandler(t)
	at := time.Now().UTC().Truncate(time.Millisecond).Add(2 * time.Second)
	for _, tc := range []storage.ToolCallRecord{
		{ID: storage.NewID(), SessionID: sessID, TurnID: turnID, RemoteName: "lookup_order", SchemaHash: "h", StartedAt: at, EndedAt: at.Add(150 * time.Millisecond), Status: "succeeded"},
		{ID: storage.NewID(), SessionID: sessID, RemoteName: "end_call", StartedAt: at, Status: "cancelled"}, // on no turn
	} {
		if err := s.Traces().AddToolCall(context.Background(), tc); err != nil {
			t.Fatal(err)
		}
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+sessID+"/turns", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Data []api.TurnDTO `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || len(resp.Data) != 1 {
		t.Fatalf("turns %s: %v", w.Body.String(), err)
	}
	// setupTracesHandler stores get_weather (status completed, no end) on the same turn, earlier.
	want := []api.TurnToolDTO{{Name: "get_weather", Status: "completed", DurationMS: 0}, {Name: "lookup_order", Status: "succeeded", DurationMS: 150}}
	if !reflect.DeepEqual(resp.Data[0].Tools, want) {
		t.Fatalf("tools %+v, want %+v", resp.Data[0].Tools, want)
	}
}
