package storage

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func seedWithCaller(t *testing.T, s *Store, caller string) (string, error) {
	t.Helper()
	ctx := context.Background()
	a := Flow{ID: NewID(), Name: "A", Enabled: true, CreatedAt: time.Now()}
	if err := s.Flows().Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	v, err := publishTestVersion(ctx, s, a.ID, []byte(`{}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	id := NewID()
	return id, s.Sessions().Create(ctx, SessionRecord{ID: id, FlowID: a.ID, FlowVersionID: v.ID, ConfigHash: v.ConfigHash, StartedAt: time.Now(), Status: "active", Caller: caller})
}

func TestASessionStoresItsCallerAndTheReasonItEnded(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		id, err := seedWithCaller(t, s, "  alice  ")
		if err != nil {
			t.Fatal(err)
		}
		if r := sessionOf(t, s, id); r.Caller != "alice" || r.EndReason != "" {
			t.Fatalf("active: caller %q reason %q (trimmed caller, no reason yet)", r.Caller, r.EndReason)
		}
		if err := s.Sessions().End(context.Background(), id, EndSilentCaller); err != nil {
			t.Fatal(err)
		}
		r := sessionOf(t, s, id)
		if r.Status != "ended" || r.EndedAt.IsZero() || r.EndReason != EndSilentCaller {
			t.Fatalf("ended: %+v", r)
		}
		if err := s.Sessions().End(context.Background(), id, EndCallerHungUp); err != nil {
			t.Fatal(err)
		}
		if got := sessionOf(t, s, id).EndReason; got != EndSilentCaller {
			t.Fatalf("ending twice must keep the first reason, got %q", got)
		}
		listed, err := s.Sessions().List(context.Background(), SessionFilter{Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, l := range listed {
			if l.ID == id {
				found = l.Caller == "alice" && l.EndReason == EndSilentCaller
			}
		}
		if !found {
			t.Fatal("list must return caller and end reason")
		}
	})
}

func TestEveryEndReasonIsKnownAndAnythingElseIsRefused(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		for _, reason := range EndReasons {
			id, err := seedWithCaller(t, s, "")
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Sessions().End(context.Background(), id, reason); err != nil {
				t.Fatalf("%s: %v", reason, err)
			}
		}
		id, _ := seedWithCaller(t, s, "")
		for _, bad := range []string{"", "because", "CALLER_HUNG_UP"} {
			if err := s.Sessions().End(context.Background(), id, bad); !errors.Is(err, ErrInvalidEndReason) {
				t.Fatalf("%q: %v", bad, err)
			}
		}
		if sessionOf(t, s, id).Status != "active" {
			t.Fatal("a refused reason must leave the session active")
		}
	})
}

func TestACallerOverTheLimitIsCutNotRefusedAndEmptyIsAllowed(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		for _, c := range []struct{ in, want string }{
			{strings.Repeat("a", 128), strings.Repeat("a", 128)},
			{"", ""},
			{strings.Repeat("a", 200), strings.Repeat("a", 128)},
			{strings.Repeat("é", 129), strings.Repeat("é", 128)},
		} {
			id, err := seedWithCaller(t, s, c.in)
			if err != nil {
				t.Fatalf("%d runes: %v", len([]rune(c.in)), err)
			}
			if got := sessionOf(t, s, id).Caller; got != c.want {
				t.Fatalf("stored %d runes, want %d", len([]rune(got)), len([]rune(c.want)))
			}
		}
	})
}

func TestEndOrphansRecordsServerRestartAndKeepsTheNote(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		id := seedSession(t, s, time.Now().Add(-2*time.Hour), "active")
		if _, err := s.Sessions().EndOrphans(context.Background(), time.Now()); err != nil {
			t.Fatal(err)
		}
		r := sessionOf(t, s, id)
		if r.EndReason != EndServerRestart || r.SafeSummary != orphanNote {
			t.Fatalf("%+v", r)
		}
	})
}

func TestTurnLatenciesAreFetchedForAPageOfSessionsInOneQuery(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		a, _ := seedWithCaller(t, s, "")
		b, _ := seedWithCaller(t, s, "")
		none, _ := seedWithCaller(t, s, "")
		for i, l := range []string{`{"voice_to_voice_ms":1}`, `{"voice_to_voice_ms":2}`} {
			if err := s.Sessions().AddTurn(ctx, TurnRecord{ID: NewID(), SessionID: a, Ordinal: i + 1, StartedAt: time.Now(), LatenciesJSON: l}); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Sessions().AddTurn(ctx, TurnRecord{ID: NewID(), SessionID: b, Ordinal: 1, StartedAt: time.Now(), LatenciesJSON: `{}`}); err != nil {
			t.Fatal(err)
		}
		got, err := s.Sessions().TurnLatencies(ctx, []string{a, b, none})
		if err != nil {
			t.Fatal(err)
		}
		if len(got[a]) != 2 || got[a][0] != `{"voice_to_voice_ms":1}` || got[a][1] != `{"voice_to_voice_ms":2}` || len(got[b]) != 1 {
			t.Fatalf("%v", got)
		}
		if _, ok := got[none]; ok {
			t.Fatal("a session with no turns is absent")
		}
		if m, err := s.Sessions().TurnLatencies(ctx, nil); err != nil || len(m) != 0 {
			t.Fatalf("empty ids: %v %v", m, err)
		}
	})
}
