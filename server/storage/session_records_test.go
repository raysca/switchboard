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
	v, err := s.Flows().PublishVersion(ctx, a.ID, 1, []byte(`{}`), nil)
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

func TestACallerOverTheLimitIsRefusedAndEmptyIsAllowed(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		if _, err := seedWithCaller(t, s, strings.Repeat("a", 128)); err != nil {
			t.Fatalf("128: %v", err)
		}
		if _, err := seedWithCaller(t, s, ""); err != nil {
			t.Fatalf("empty: %v", err)
		}
		if _, err := seedWithCaller(t, s, strings.Repeat("a", 129)); err == nil {
			t.Fatal("129 characters accepted")
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
