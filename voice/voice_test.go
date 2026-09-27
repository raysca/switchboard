package voice

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// tag appends its name to text frames; "echo" bounces text upstream; "drop" consumes.
type tag struct {
	name string
	log  *[]string
	mu   *sync.Mutex
}

func (t tag) ProcessFrame(_ context.Context, f Frame, dir Direction, e Emitter) error {
	tf, ok := f.(*TextFrame)
	if !ok {
		return e.PushDirection(f, dir)
	}
	t.mu.Lock()
	*t.log = append(*t.log, t.name+map[Direction]string{Downstream: "v", Upstream: "^"}[dir])
	t.mu.Unlock()
	switch {
	case t.name == "drop" && dir == Downstream:
		return nil
	case t.name == "echo" && dir == Downstream:
		if err := e.Push(tf); err != nil { // multi-emit: down and up
			return err
		}
		return e.PushDirection(tf, Upstream)
	}
	return e.PushDirection(f, dir)
}

func mk(t *testing.T, m *Manager, procs ...Processor) *Session {
	t.Helper()
	s, _, err := m.Create(Spec{Build: func(*Session) ([]Processor, error) { return procs, nil }})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRouting(t *testing.T) {
	m := NewManager()
	defer m.Close()
	var log []string
	var mu sync.Mutex
	out, up := make(chan Frame, 4), make(chan Frame, 4)
	s := mk(t, m, tag{"a", &log, &mu}, tag{"echo", &log, &mu}, tag{"c", &log, &mu})
	s.OnOutput = func(f Frame) { out <- f }
	s.OnUpstream = func(f Frame) { up <- f }
	s.Post(&TextFrame{Text: "x"}, Downstream, 0)
	<-out
	<-up
	mu.Lock()
	got := strings.Join(log, ",")
	mu.Unlock()
	if got != "av,echov,cv,a^" {
		t.Fatalf("routing order = %s", got)
	}
	// Async re-entry at an index skips earlier processors.
	log = nil
	s.Post(&TextFrame{Text: "y"}, Downstream, 2)
	<-out
	if strings.Join(log, ",") != "cv" {
		t.Fatalf("re-entry = %v", log)
	}
	if st := s.Stop(); st != StateStopped {
		t.Fatalf("state %v", st)
	}
}

func fakeSession(t *testing.T, m *Manager) (*Session, *Fake) {
	f := &Fake{Name: "stt", Kind: FakeSTT, Latency: time.Millisecond}
	b := DefaultBudget()
	b.TeardownTimeout = 200 * time.Millisecond
	s, _, err := m.Create(Spec{Budget: b, Build: func(*Session) ([]Processor, error) {
		return []Processor{f, &Fake{Name: "llm", Kind: FakeLLM, Latency: time.Millisecond}}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	return s, f
}

func waitDone(t *testing.T, s *Session) State {
	t.Helper()
	select {
	case <-s.Done():
		return s.State()
	case <-time.After(3 * time.Second):
		t.Fatal("session did not terminate")
	}
	return 0
}

func TestFaultContainment(t *testing.T) {
	cases := []struct {
		mode    Mode
		want    State
		wantErr error
	}{
		{ModePanic, StateFailed, nil},
		{ModeChildPanic, StateFailed, nil},
		{ModeError, StateFailed, ErrInjected},
		{ModeFlood, StateFailed, nil}, // emit guard or task budget, whichever trips first
		{ModeOverflow, StateFailed, nil},
		{ModeSlowSync, StateFailed, ErrAudioShedding},
	}
	m := NewManager()
	defer m.Close()
	healthy, _ := fakeSession(t, m)
	for _, c := range cases {
		s, f := fakeSession(t, m)
		f.SetMode(c.mode)
		done := time.After(3 * time.Second)
		pace := time.Millisecond
		if c.mode == ModeSlowSync { // real-time audio rate: budgets must trip at 31 frames/s
			pace, done = 32*time.Millisecond, time.After(8*time.Second)
		}
	feed:
		for i := 0; ; i++ {
			select {
			case <-s.Done():
				break feed
			case <-done:
				t.Fatalf("mode %d: not contained", c.mode)
			default:
			}
			s.PushAudio(&AudioFrame{Samples: silence})
			healthy.PushAudio(&AudioFrame{Samples: silence})
			time.Sleep(pace)
		}
		if st := s.State(); st != c.want {
			t.Fatalf("mode %d: state %v err %v", c.mode, st, s.Err())
		}
		if c.wantErr != nil && !errors.Is(s.Err(), c.wantErr) {
			t.Fatalf("mode %d: err %v", c.mode, s.Err())
		}
	}
	if healthy.State() != StateRunning {
		t.Fatalf("healthy session affected: %v", healthy.State())
	}
	healthy.Stop()
	if st := m.Stats(); st.ManagedGoroutines != 0 || st.RunLoops != 0 || st.Sessions != 0 {
		t.Fatalf("resources not reclaimed: %+v", st)
	}
}

func TestLeakIsSurfaced(t *testing.T) {
	m := NewManager()
	defer m.Close()
	for _, mode := range []Mode{ModeHang, ModeStall, ModeBadTeardown} {
		s, f := fakeSession(t, m)
		f.SetMode(mode)
		s.PushAudio(&AudioFrame{Samples: silence})
		time.Sleep(20 * time.Millisecond)
		st := s.Stop()
		want := StateStopped
		if mode != ModeStall {
			want = StateLeaked
		}
		if st != want {
			t.Fatalf("mode %d: %v", mode, st)
		}
	}
	if st := m.Stats(); st.LeakedSessions != 2 || st.ManagedGoroutines != 1 {
		t.Fatalf("leak accounting wrong: %+v", st)
	}
	close(HangRelease)
	time.Sleep(50 * time.Millisecond)
	if st := m.Stats(); st.ManagedGoroutines != 0 {
		t.Fatalf("hang goroutine not released: %+v", st)
	}
}

func TestHist(t *testing.T) {
	var h Hist
	for i := 1; i <= 1000; i++ {
		h.Record(time.Duration(i) * time.Millisecond)
	}
	for _, q := range []float64{0.5, 0.99} {
		got, want := h.Quantile(q), time.Duration(q*1000)*time.Millisecond
		if d := float64(got-want) / float64(want); d < -0.01 || d > 0.07 {
			t.Fatalf("q%.2f = %v want ~%v", q, got, want)
		}
	}
}
