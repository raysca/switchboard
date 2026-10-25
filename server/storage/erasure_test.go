package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// seedCall makes an ended call of caller with content in every table an erasure must clear, including rows that reach
// the call only through a span or a turn (no session_id), which the restrict constraints make hard to delete.
func seedCall(t *testing.T, s *Store, caller string, endedAgo time.Duration) string {
	t.Helper()
	ctx := context.Background()
	id := seedSession(t, s, time.Now().Add(-endedAgo-time.Minute), "ended")
	ended := time.Now().Add(-endedAgo)
	if _, err := s.DB().ExecContext(ctx, s.bind("UPDATE sessions SET ended_at=?, caller=?, safe_summary=? WHERE id=?"), utc(ended), caller, "summary of "+caller, id); err != nil {
		t.Fatal(err)
	}
	turn := NewID()
	if err := s.Sessions().AddTurn(ctx, TurnRecord{ID: turn, SessionID: id, Ordinal: 1, StartedAt: ended, EndedAt: ended, Transcript: "secret words", AssistantText: "reply", LatenciesJSON: `{}`}); err != nil {
		t.Fatal(err)
	}
	span, linkedSpan := NewID(), NewID()
	for _, sp := range []TraceSpan{
		{ID: span, SessionID: id, TurnID: turn, Name: "turn", StartedAt: ended, AttributesJSON: `{}`},
		{ID: linkedSpan, TurnID: turn, Name: "linked by turn only", StartedAt: ended, AttributesJSON: `{}`},
	} {
		if err := s.Traces().AddSpan(ctx, sp); err != nil {
			t.Fatal(err)
		}
	}
	for _, ev := range []TraceEvent{
		{ID: NewID(), SpanID: span, SessionID: id, Name: "e1", OccurredAt: ended, AttributesJSON: `{}`},
		{ID: NewID(), SpanID: linkedSpan, Name: "linked by span only", OccurredAt: ended, AttributesJSON: `{}`},
		{ID: NewID(), TurnID: turn, Name: "linked by turn only", OccurredAt: ended, AttributesJSON: `{}`},
	} {
		if err := s.Traces().AddEvent(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Traces().AddToolCall(ctx, ToolCallRecord{ID: NewID(), SessionID: id, TurnID: turn, RemoteName: "t", SchemaHash: "h", StartedAt: ended, EndedAt: ended, Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	return id
}

func count(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRowContext(context.Background(), s.bind(query), args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// rowsOf counts every row in every table that could hold the call's id or its caller label.
func rowsOf(t *testing.T, s *Store, id, caller string) int {
	t.Helper()
	n := 0
	for _, q := range []string{
		"SELECT count(*) FROM sessions WHERE id=?", "SELECT count(*) FROM turns WHERE session_id=?", "SELECT count(*) FROM tool_calls WHERE session_id=?",
		"SELECT count(*) FROM trace_spans WHERE session_id=?", "SELECT count(*) FROM trace_events WHERE session_id=?", "SELECT count(*) FROM session_mcp_tools WHERE session_id=?",
	} {
		n += count(t, s, q, id)
	}
	n += count(t, s, "SELECT count(*) FROM sessions WHERE caller=?", caller)
	// rows that reach the call only through a turn or a span
	n += count(t, s, "SELECT count(*) FROM trace_spans WHERE name LIKE 'linked%'")
	n += count(t, s, "SELECT count(*) FROM trace_events WHERE name LIKE 'linked%'")
	return n
}

func TestEraseSessionRemovesEveryRowAndAuditsIt(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		id := seedCall(t, s, "caller-"+NewID(), time.Hour)
		keep := seedCall(t, s, "someone-else-"+NewID(), time.Hour)
		caller := sessionOf(t, s, id).Caller
		if rowsOf(t, s, id, caller) == 0 {
			t.Fatal("the seed made nothing")
		}
		got, err := s.Retention().EraseSession(ctx, id, "req-1")
		if err != nil {
			t.Fatal(err)
		}
		if got.Turns != 1 || got.ToolCalls != 1 || got.Spans != 2 || got.Events != 3 {
			t.Fatalf("erased %+v", got)
		}
		// the call's own rows are gone; the linked rows belonged to it too (2 spans and 3 events above), the other call is intact
		if n := count(t, s, "SELECT count(*) FROM sessions WHERE id=?", id) + count(t, s, "SELECT count(*) FROM turns WHERE session_id=?", id) + count(t, s, "SELECT count(*) FROM sessions WHERE caller=?", caller); n != 0 {
			t.Fatalf("%d rows of the erased call remain", n)
		}
		if count(t, s, "SELECT count(*) FROM turns WHERE session_id=?", keep) != 1 || count(t, s, "SELECT count(*) FROM trace_spans WHERE session_id=?", keep) != 1 {
			t.Fatal("another call lost rows")
		}
		actions, _ := s.Retention().RecentActions(ctx, 10)
		if len(actions) != 1 || actions[0].Action != AuditEraseCall || actions[0].Subject != id || actions[0].Request != "req-1" || strings.Contains(actions[0].Detail, "secret words") || strings.Contains(actions[0].Detail, caller) {
			t.Fatalf("audit = %+v", actions)
		}
	})
}

func TestEraseSessionRefusesHeldActiveAndUnknownCallsWithoutChangingAnything(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		held := seedCall(t, s, "h-"+NewID(), time.Hour)
		if _, err := s.Retention().SetHold(ctx, held, true, "legal", "r"); err != nil {
			t.Fatal(err)
		}
		active := seedSession(t, s, time.Now().Add(-time.Hour), "active")
		for name, tc := range map[string]struct {
			id   string
			want error
		}{
			"held":      {held, ErrSessionHeld},
			"active":    {active, ErrSessionActive},
			"unknown":   {NewID(), sql.ErrNoRows},
			"not an id": {"nope", sql.ErrNoRows},
		} {
			if _, err := s.Retention().EraseSession(ctx, tc.id, "req"); !errors.Is(err, tc.want) {
				t.Fatalf("%s: %v", name, err)
			}
		}
		if count(t, s, "SELECT count(*) FROM turns WHERE session_id=?", held) != 1 {
			t.Fatal("a refused erasure changed the held call")
		}
		// only the hold itself was audited
		actions, _ := s.Retention().RecentActions(ctx, 10)
		if len(actions) != 1 || actions[0].Action != AuditHold {
			t.Fatalf("audit = %+v", actions)
		}
		// released, it can be erased
		if _, err := s.Retention().SetHold(ctx, held, false, "", "r2"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Retention().EraseSession(ctx, held, "r3"); err != nil {
			t.Fatalf("after release: %v", err)
		}
	})
}

func TestHoldReasonIsReplacedOnlyAFlagChangeIsAuditedAndTheCallRecordShowsIt(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		id := seedCall(t, s, "c-"+NewID(), time.Hour)
		for i, reason := range []string{"first", "second"} {
			changed, err := s.Retention().SetHold(ctx, id, true, reason, fmt.Sprint("r", i))
			if err != nil || changed != (i == 0) {
				t.Fatalf("hold %d: changed=%v err=%v", i, changed, err)
			}
		}
		if rec := sessionOf(t, s, id); !rec.Held || rec.HoldReason != "second" {
			t.Fatalf("record = %+v", rec)
		}
		if held, _ := s.Retention().IsHeld(ctx, id); !held {
			t.Fatal("IsHeld")
		}
		if n, _ := s.Retention().HeldCount(ctx); n != 1 {
			t.Fatalf("held count %d", n)
		}
		if changed, err := s.Retention().SetHold(ctx, id, false, "ignored", "r3"); err != nil || !changed {
			t.Fatalf("release: %v %v", changed, err)
		}
		if changed, _ := s.Retention().SetHold(ctx, id, false, "", "r4"); changed {
			t.Fatal("releasing a call that is not held reported a change")
		}
		if rec := sessionOf(t, s, id); rec.Held || rec.HoldReason != "" {
			t.Fatalf("after release: %+v", rec)
		}
		actions, _ := s.Retention().RecentActions(ctx, 10)
		if len(actions) != 2 || actions[0].Action != AuditRelease || actions[1].Action != AuditHold {
			t.Fatalf("audit = %+v", actions)
		}
		if _, err := s.Retention().SetHold(ctx, NewID(), true, "", "r"); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("unknown: %v", err)
		}
		if _, err := s.Retention().SetHold(ctx, id, true, strings.Repeat("x", MaxHoldReason+1), "r"); err == nil {
			t.Fatal("an over-long reason was accepted")
		}
		held := true
		list, _ := s.Sessions().List(ctx, SessionFilter{Held: &held})
		if len(list) != 0 {
			t.Fatalf("held list = %d", len(list))
		}
	})
}

// The retention job and its dry run leave held calls alone, and the dry run still equals what a run removes.
func TestPurgeAndPreviewSkipHeldCalls(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		old1 := seedCall(t, s, "a-"+NewID(), 60*24*time.Hour)
		old2 := seedCall(t, s, "b-"+NewID(), 60*24*time.Hour)
		if _, err := s.Retention().SetHold(ctx, old2, true, "", "r"); err != nil {
			t.Fatal(err)
		}
		content, metadata := time.Now().Add(-7*24*time.Hour), time.Now().Add(-30*24*time.Hour)
		preview, err := s.Retention().Preview(ctx, content, metadata)
		if err != nil {
			t.Fatal(err)
		}
		if preview.ContentSessions < 1 || preview.MetadataSessions < 1 {
			t.Fatalf("preview = %+v", preview)
		}
		// a hold placed after the preview is respected by the run
		third := seedCall(t, s, "c-"+NewID(), 60*24*time.Hour)
		if _, err := s.Retention().SetHold(ctx, third, true, "", "r"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Retention().PurgeContent(ctx, content, 500); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Retention().PurgeMetadata(ctx, metadata, 500); err != nil {
			t.Fatal(err)
		}
		if count(t, s, "SELECT count(*) FROM sessions WHERE id=?", old1) != 0 {
			t.Fatal("an unheld old call survived the purge")
		}
		for _, id := range []string{old2, third} {
			if count(t, s, "SELECT count(*) FROM sessions WHERE id=?", id) != 1 || count(t, s, "SELECT count(*) FROM turns WHERE session_id=? AND transcript IS NOT NULL", id) != 1 {
				t.Fatalf("held call %s lost rows or content", id)
			}
		}
		after, _ := s.Retention().Preview(ctx, content, metadata)
		if after.MetadataSessions != 0 || after.ContentSessions != 0 { // only held calls remain, and they are not counted
			// held calls past the cutoff must not be counted
			t.Fatalf("held calls are counted: %+v", after)
		}
		// released, the next run removes it
		if _, err := s.Retention().SetHold(ctx, old2, false, "", "r"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Retention().PurgeMetadata(ctx, metadata, 500); err != nil {
			t.Fatal(err)
		}
		if count(t, s, "SELECT count(*) FROM sessions WHERE id=?", old2) != 0 {
			t.Fatal("a released call was not purged")
		}
	})
}

func TestEraseCallerCountsBatchesAndNeverTouchesHeldOrActiveCalls(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		label := "caller-" + NewID()
		var ids []string
		for i := 0; i < 30; i++ { // more than one batch of 25
			ids = append(ids, seedCall(t, s, label, time.Hour))
		}
		held := seedCall(t, s, label, time.Hour)
		_, _ = s.Retention().SetHold(ctx, held, true, "", "r")
		active := seedSession(t, s, time.Now().Add(-time.Hour), "active")
		_, _ = s.DB().ExecContext(ctx, s.bind("UPDATE sessions SET caller=? WHERE id=?"), label, active)
		other := seedCall(t, s, "other-"+NewID(), time.Hour)

		dry, err := s.Retention().EraseCaller(ctx, label, "req-dry", true)
		if err != nil || dry.Erased != 0 || dry.Remaining != 30 || dry.Held != 1 || dry.Active != 1 {
			t.Fatalf("dry run = %+v %v", dry, err)
		}
		if n := count(t, s, "SELECT count(*) FROM sessions WHERE caller=?", label); n != 32 {
			t.Fatalf("a dry run deleted: %d left", n)
		}
		if a, _ := s.Retention().RecentActions(ctx, 10); len(a) != 1 || a[0].Action != AuditHold {
			t.Fatalf("a dry run was audited: %+v", a)
		}

		got, err := s.Retention().EraseCaller(ctx, label, "req-erase", false)
		if err != nil || got.Erased != 30 || len(got.IDs) != 30 || got.Remaining != 0 || got.Held != 1 || got.Active != 1 || got.Removed.Turns != 30 {
			t.Fatalf("erase = %+v %v", got, err)
		}
		for _, id := range ids {
			if count(t, s, "SELECT count(*) FROM sessions WHERE id=?", id) != 0 {
				t.Fatalf("%s survived", id)
			}
		}
		for _, id := range []string{held, active, other} {
			if count(t, s, "SELECT count(*) FROM sessions WHERE id=?", id) != 1 {
				t.Fatalf("%s was erased", id)
			}
		}
		actions, _ := s.Retention().RecentActions(ctx, 10)
		var erase *RetentionAction
		for i := range actions {
			if actions[i].Action == AuditEraseCaller {
				erase = &actions[i]
			}
		}
		if erase == nil || erase.Subject != CallerHash(label) || strings.Contains(erase.Subject+erase.Detail, label) || erase.Request != "req-erase" {
			t.Fatalf("audit = %+v", actions)
		}
		again, _ := s.Retention().EraseCaller(ctx, label, "req-again", false)
		if again.Erased != 0 {
			t.Fatalf("a second request erased %d", again.Erased)
		}
	})
}

func TestEraseCallerLimitsOneRequestAndTheClientRepeats(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		label := "many-" + NewID()
		a := Agent{ID: NewID(), Name: "A", Enabled: true, CreatedAt: time.Now()}
		if err := s.Agents().Create(ctx, a); err != nil {
			t.Fatal(err)
		}
		v, err := publishTestVersion(ctx, s, a.ID, []byte(`{}`), nil)
		if err != nil {
			t.Fatal(err)
		}
		total := EraseCallerLimit + 5
		for i := 0; i < total; i++ {
			if err := s.Sessions().Create(ctx, SessionRecord{ID: NewID(), AgentID: a.ID, AgentVersionID: v.ID, ConfigHash: v.ConfigHash, StartedAt: time.Now().Add(-time.Hour), Status: "ended", EndedAt: time.Now().Add(-time.Minute), Caller: label}); err != nil {
				t.Fatal(err)
			}
		}
		first, err := s.Retention().EraseCaller(ctx, label, "r1", false)
		if err != nil || first.Erased != EraseCallerLimit || first.Remaining != 5 {
			t.Fatalf("first = %d erased, %d remaining, %v", first.Erased, first.Remaining, err)
		}
		second, err := s.Retention().EraseCaller(ctx, label, "r2", false)
		if err != nil || second.Erased != 5 || second.Remaining != 0 {
			t.Fatalf("second = %+v %v", second, err)
		}
	})
}

func TestNoTableHoldsAnErasedCallOrItsCaller(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		label := "gone-" + NewID()
		id := seedCall(t, s, label, time.Hour)
		if _, err := s.Retention().EraseCaller(ctx, label, "r", false); err != nil {
			t.Fatal(err)
		}
		if n := rowsOf(t, s, id, label); n != 0 {
			t.Fatalf("%d rows still reach the erased call or its caller", n)
		}
		// the audit trail holds neither the id of the caller label nor transcript text
		var leaks int
		for _, a := range func() []RetentionAction { r, _ := s.Retention().RecentActions(ctx, 100); return r }() {
			if strings.Contains(a.Subject+a.Detail, label) || strings.Contains(a.Subject+a.Detail, "secret words") {
				leaks++
			}
		}
		if leaks != 0 {
			t.Fatalf("%d audit entries carry the label or content", leaks)
		}
	})
}

// A call held while it was live, then ended by the restart cleanup, is still protected from the purge.
func TestHeldCallSurvivesOrphanCleanupAndAPurge(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		id := seedSession(t, s, time.Now().Add(-90*24*time.Hour), "active")
		if _, err := s.Retention().SetHold(ctx, id, true, "live", "r"); err != nil {
			t.Fatal(err)
		}
		if n, err := s.Sessions().EndOrphans(ctx, time.Now()); err != nil || n != 1 {
			t.Fatalf("orphans ended %d, %v", n, err)
		}
		if _, err := s.Retention().PurgeMetadata(ctx, time.Now(), 500); err != nil {
			t.Fatal(err)
		}
		if rec := sessionOf(t, s, id); rec.Status != "ended" || !rec.Held {
			t.Fatalf("record = %+v", rec)
		}
	})
}

// The guard inside the delete is what makes a hold placed after a call was selected win: the delete then does nothing.
func TestDeleteSessionDoesNothingToACallHeldAfterItWasSelected(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		id := seedCall(t, s, "late-"+NewID(), time.Hour)
		if _, err := s.Retention().SetHold(ctx, id, true, "", "r"); err != nil {
			t.Fatal(err)
		}
		err := s.WithTx(ctx, func(tx *sql.Tx) error {
			_, deleted, err := s.Retention().deleteSession(ctx, tx, id)
			if deleted {
				t.Error("a held call was deleted")
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if count(t, s, "SELECT count(*) FROM turns WHERE session_id=?", id) != 1 || count(t, s, "SELECT count(*) FROM trace_events WHERE session_id=?", id) != 1 {
			t.Fatal("the guarded delete changed a held call")
		}
	})
}

// A hold placed after PurgeContent chose its calls, before it blanked them, still protects the call.
func TestPurgeContentLeavesACallHeldAfterItWasSelected(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		id := seedCall(t, s, "race-"+NewID(), 60*24*time.Hour)
		s.purgeContentHook = func() {
			if _, err := s.Retention().SetHold(ctx, id, true, "", "r"); err != nil {
				t.Error(err)
			}
		}
		defer func() { s.purgeContentHook = nil }()
		if _, err := s.Retention().PurgeContent(ctx, time.Now().Add(-time.Hour), 500); err != nil {
			t.Fatal(err)
		}
		if count(t, s, "SELECT count(*) FROM turns WHERE session_id=? AND transcript IS NOT NULL", id) != 1 {
			t.Fatal("a call held after selection was blanked")
		}
	})
}

// An eval result's reason can quote its call; erasing the call blanks it and keeps the score, and another call's result
// is untouched. Fails if deleteSession stops clearing eval_results.
func TestEraseSessionBlanksTheReasonOfItsEvalResults(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		resetEval(t, s)
		ctx := context.Background()
		id := seedCall(t, s, "caller-"+NewID(), time.Hour)
		keep := seedCall(t, s, "caller-"+NewID(), time.Hour)
		erased, _ := finishedRun(t, s, time.Now())
		kept, _ := finishedRun(t, s, time.Now())
		for run, session := range map[string]string{erased.ID: id, kept.ID: keep} {
			if _, err := s.DB().ExecContext(ctx, s.bind(`UPDATE eval_results SET session_id=?, reason='never_say: matched "secret words"' WHERE run_id=?`), session, run); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.Retention().EraseSession(ctx, id, "r"); err != nil {
			t.Fatal(err)
		}
		got, err := s.Eval().RunResults(ctx, erased.ID)
		if err != nil || len(got) != 1 || got[0].Reason != "" || got[0].Outcome != "pass" {
			t.Fatalf("erased call's result = %+v %v", got, err)
		}
		if got, _ := s.Eval().RunResults(ctx, kept.ID); len(got) != 1 || !strings.Contains(got[0].Reason, "secret words") {
			t.Fatalf("another call's result changed: %+v", got)
		}
	})
}
