package storage

import (
	"context"
	"testing"
	"time"
)

func TestSessionDraftFlagRoundTripsAndDefaultsToFalse(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		real := seedSession(t, s, time.Now(), "active")
		if got, err := s.Sessions().Get(ctx, real); err != nil || got.Draft {
			t.Fatalf("a normal session must not be a draft: %+v err %v", got, err)
		}
		a := Flow{ID: NewID(), Name: "D", Enabled: true, CreatedAt: time.Now()}
		if err := s.Flows().Create(ctx, a); err != nil {
			t.Fatal(err)
		}
		v, err := publishTestVersion(ctx, s, a.ID, []byte(`{}`), nil)
		if err != nil {
			t.Fatal(err)
		}
		hash, _, err := ConfigHash([]byte(`{"draft":true}`))
		if err != nil {
			t.Fatal(err)
		}
		id := NewID()
		if err := s.Sessions().Create(ctx, SessionRecord{ID: id, FlowID: a.ID, FlowVersionID: v.ID, ConfigHash: hash, StartedAt: time.Now(), Status: "active", Draft: true}); err != nil {
			t.Fatal(err)
		}
		got, err := s.Sessions().Get(ctx, id)
		if err != nil || !got.Draft || got.FlowVersionID != v.ID || got.ConfigHash != hash {
			t.Fatalf("draft session %+v err %v", got, err)
		}
		list, err := s.Sessions().List(ctx, SessionFilter{FlowID: a.ID})
		if err != nil || len(list) != 1 || !list[0].Draft {
			t.Fatalf("list %+v err %v", list, err)
		}
	})
}

func TestConfigHashMatchesAPublishedVersionsHash(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := Flow{ID: NewID(), Name: "H", Enabled: true, CreatedAt: time.Now()}
	if err := s.Flows().Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	v, err := publishTestVersion(ctx, s, a.ID, []byte(`{"b":2,"a":1}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if h, _, err := ConfigHash([]byte(`{ "a": 1, "b": 2 }`)); err != nil || h != v.ConfigHash {
		t.Fatalf("hash %q err %v, want the published %q (same canonical form)", h, err, v.ConfigHash)
	}
}

// A normal session still must match its version's hash: the integrity check is only relaxed for drafts.
func TestOnlyDraftSessionsMayCarryAnotherHash(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		a := Flow{ID: NewID(), Name: "P", Enabled: true, CreatedAt: time.Now()}
		if err := s.Flows().Create(ctx, a); err != nil {
			t.Fatal(err)
		}
		v, err := publishTestVersion(ctx, s, a.ID, []byte(`{}`), nil)
		if err != nil {
			t.Fatal(err)
		}
		other, _, _ := ConfigHash([]byte(`{"x":1}`))
		err = s.Sessions().Create(ctx, SessionRecord{ID: NewID(), FlowID: a.ID, FlowVersionID: v.ID, ConfigHash: other, StartedAt: time.Now(), Status: "active"})
		if err == nil {
			t.Fatal("a normal session with a foreign config hash was accepted")
		}
		// And a draft still has to name a real version of that flow.
		other2 := Flow{ID: NewID(), Name: "Q", Enabled: true, CreatedAt: time.Now()}
		if err := s.Flows().Create(ctx, other2); err != nil {
			t.Fatal(err)
		}
		if err := s.Sessions().Create(ctx, SessionRecord{ID: NewID(), FlowID: other2.ID, FlowVersionID: v.ID, ConfigHash: other, StartedAt: time.Now(), Status: "active", Draft: true}); err == nil {
			t.Fatal("a draft naming another flow's version was accepted")
		}
	})
}
