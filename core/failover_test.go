package core

import (
	"errors"
	"testing"
)

type failoverSess struct {
	SessionController
	events []DebugEvent
}

func (s *failoverSess) EmitDebug(e DebugEvent) { s.events = append(s.events, e) }

type attachProc struct {
	nopTTS
	got SessionController
	err error
}

func (p *attachProc) Attach(s SessionController, _ int) error { p.got = s; return p.err }

func TestFailoverClass(t *testing.T) {
	boom := errors.New("boom")
	if got := failoverClass(nil, boom); got != "" {
		t.Fatalf("nil classifier: %q", got)
	}
	if got := failoverClass(func(err error) string {
		if errors.Is(err, boom) {
			return "server_error"
		}
		return "x"
	}, boom); got != "server_error" {
		t.Fatalf("class: %q", got)
	}
}

func TestNoteFailoverCallsHookAndEmitsDebug(t *testing.T) {
	s := &failoverSess{}
	var from, to, class string
	noteFailover(s, func(f, t, c string) { from, to, class = f, t, c }, DebugTTSFailover, "a", "b", "server_error", "turn-1", 7)
	if from != "a" || to != "b" || class != "server_error" {
		t.Fatalf("hook: %q %q %q", from, to, class)
	}
	if len(s.events) != 1 {
		t.Fatalf("events: %d", len(s.events))
	}
	e := s.events[0]
	if e.Name != DebugTTSFailover || e.TurnID != "turn-1" || e.Generation != 7 {
		t.Fatalf("event: %+v", e)
	}
	attrs := e.Attributes()
	if attrs["component"] != "a" || attrs["provider"] != "b" || attrs["reason"] != "server_error" {
		t.Fatalf("attrs: %v", attrs)
	}
	// No hook and a session without EmitDebug are both fine.
	noteFailover(&nopSess{}, nil, DebugLLMFailover, "a", "b", "c", "", 0)
}

type nopSess struct{ SessionController }

func TestAttachCandidatesRoutesReenterAndStopsOnError(t *testing.T) {
	p0, p1, p2 := &attachProc{}, &attachProc{err: errors.New("attach")}, &attachProc{}
	procs := []Processor{p0, nopTTS{}, p1, p2}
	var calls []int
	err := attachCandidates(&nopSess{}, 3, len(procs), func(i int) Processor { return procs[i] },
		func(idx int, _ Frame, _ Direction, _ int, _ uint64) error { calls = append(calls, idx); return nil })
	if err == nil || err.Error() != "attach" {
		t.Fatalf("err: %v", err)
	}
	if p2.got != nil {
		t.Fatal("attached past the failing candidate")
	}
	_ = p0.got.Reenter(nil, Downstream, 0, 0)
	_ = p1.got.Reenter(nil, Downstream, 0, 0)
	if len(calls) != 2 || calls[0] != 0 || calls[1] != 2 {
		t.Fatalf("candidate indices: %v", calls)
	}
}
