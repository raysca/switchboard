package eval

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestScriptCaller(t *testing.T) {
	next := ScriptCaller([]string{"one", "two"})
	for _, want := range []string{"one", "two"} {
		line, done, err := next(context.Background(), nil)
		if line != want || done || err != nil {
			t.Fatalf("got %q done=%v err=%v, want %q", line, done, err, want)
		}
	}
	for range 2 { // stays done
		if line, done, err := next(context.Background(), nil); line != "" || !done || err != nil {
			t.Fatalf("after the script: %q done=%v err=%v", line, done, err)
		}
	}
	if _, _, err := ScriptCaller([]string{"x"})(canceled(), nil); err == nil {
		t.Error("a canceled context did not stop the caller")
	}
}

func canceled() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// fakeAsk replays replies and records every prompt it was given.
type fakeAsk struct {
	replies        []string
	systems, users []string
	err            error
}

func (f *fakeAsk) ask(_ context.Context, system, user string) (string, error) {
	f.systems, f.users = append(f.systems, system), append(f.users, user)
	if f.err != nil {
		return "", f.err
	}
	r := f.replies[min(len(f.users)-1, len(f.replies)-1)]
	return r, nil
}

func TestPersonaCaller(t *testing.T) {
	p := Persona{Who: "Jane Doe", Goal: "book a cleaning", MaxTurns: 5}
	f := &fakeAsk{replies: []string{"\"Hi, I'm Jane.\"", "Next week please.\nSecond line ignored", "  done. "}}
	next := PersonaCaller(p, "Smile Dental", f.ask)
	hist := []Line{}
	for _, want := range []string{"Hi, I'm Jane.", "Next week please."} {
		line, done, err := next(context.Background(), hist)
		if line != want || done || err != nil {
			t.Fatalf("got %q done=%v err=%v, want %q", line, done, err, want)
		}
		hist = append(hist, Line{"caller", line}, Line{"agent", "What day suits you?"})
	}
	if line, done, err := next(context.Background(), hist); line != "" || !done || err != nil {
		t.Fatalf("DONE marker: %q done=%v err=%v", line, done, err)
	}
	for _, s := range f.systems {
		if !strings.Contains(s, "Jane Doe") || !strings.Contains(s, "book a cleaning") || !strings.Contains(s, "Smile Dental") || !strings.Contains(s, "DONE") {
			t.Errorf("system prompt lacks persona, goal, agent or marker: %q", s)
		}
	}
	if !strings.Contains(f.users[2], "What day suits you?") || !strings.Contains(f.users[2], "Next week please.") {
		t.Errorf("transcript not in the user prompt: %q", f.users[2])
	}
}

func TestPersonaCallerMaxTurnsCap(t *testing.T) { // mutation: a persona that never says DONE still stops
	f := &fakeAsk{replies: []string{"I still want something"}}
	next := PersonaCaller(Persona{Who: "w", Goal: "g", MaxTurns: 3}, "A", f.ask)
	n := 0
	for {
		_, done, err := next(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			break
		}
		if n++; n > 10 {
			t.Fatal("max_turns not enforced")
		}
	}
	if n != 3 || len(f.users) != 3 {
		t.Errorf("typed %d lines with %d model calls, want 3 and 3", n, len(f.users))
	}
}

func TestPersonaCallerErrors(t *testing.T) {
	for name, f := range map[string]*fakeAsk{
		"empty":     {replies: []string{" \n "}},
		"quotes":    {replies: []string{`""`}},
		"model err": {err: errors.New("boom")},
	} {
		if _, _, err := PersonaCaller(Persona{MaxTurns: 2}, "A", f.ask)(context.Background(), nil); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if _, _, err := PersonaCaller(Persona{MaxTurns: 2}, "A", (&fakeAsk{replies: []string{"x"}}).ask)(canceled(), nil); err == nil {
		t.Error("a canceled context did not stop the caller")
	}
}

func TestPersonaCallerBounds(t *testing.T) {
	f := &fakeAsk{replies: []string{strings.Repeat("é", 1000)}}
	line, _, err := PersonaCaller(Persona{MaxTurns: 2}, "A", f.ask)(context.Background(), nil)
	if err != nil || len([]rune(line)) != maxCallerLine {
		t.Errorf("line of %d runes (err %v), want %d", len([]rune(line)), err, maxCallerLine)
	}
	var hist []Line
	for i := range 100 {
		hist = append(hist, Line{"agent", fmt.Sprintf("line-%03d %s", i, strings.Repeat("x", 2000))})
	}
	f = &fakeAsk{replies: []string{"ok"}}
	if _, _, err := PersonaCaller(Persona{MaxTurns: 2}, "A", f.ask)(context.Background(), hist); err != nil {
		t.Fatal(err)
	}
	if u := f.users[0]; len(u) > 20000 || strings.Contains(u, "line-000") || !strings.Contains(u, "line-099") {
		t.Errorf("history not bounded to the latest lines: %d bytes", len(u))
	}
}
