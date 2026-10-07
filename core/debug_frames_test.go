package core

import (
	"testing"
	"time"
)

func newDebugSession(t testing.TB) *Session {
	pipe, err := NewPipeline(NewFakeTransportOutput(FakeTransportConfig{}))
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewSession(1, pipe, DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestEmitFrameDebugDedup(t *testing.T) {
	s := newDebugSession(t)
	defer s.Close()
	ch := make(chan DebugEvent, 16)
	if _, _, err := s.Subscribe(16, func(e DebugEvent) { ch <- e }); err != nil {
		t.Fatal(err)
	}
	stop1, stop2 := &UserStoppedSpeakingFrame{}, &UserStoppedSpeakingFrame{} // equal values, distinct stops
	for _, f := range []Frame{
		stop1, stop1, stop2,
		&OutputAudioFrame{Turn: "t1", IsFinal: true}, // empty marker: not audio
		&OutputAudioFrame{Turn: "t1", Samples: []byte{1, 2}},
		&OutputAudioFrame{Turn: "t1", Samples: []byte{1, 2}},
	} {
		s.emitFrameDebug(f)
	}
	want := []string{DebugUserStopped, DebugUserStopped, DebugBotFirstAudio}
	for i, w := range want {
		select {
		case e := <-ch:
			if e.Name != w {
				t.Fatalf("[%d] = %s, want %s", i, e.Name, w)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("missing event %d (%s)", i, w)
		}
	}
	select {
	case e := <-ch:
		t.Fatalf("unexpected %s", e.Name)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestEmitFrameDebugNoObserversNoAllocs(t *testing.T) {
	s := newDebugSession(t)
	defer s.Close()
	f := &OutputAudioFrame{Turn: "t1", Samples: make([]byte, 640)}
	if n := testing.AllocsPerRun(1000, func() { s.emitFrameDebug(f) }); n != 0 {
		t.Fatalf("allocs = %v", n)
	}
	_, cancel, err := s.Subscribe(0, func(DebugEvent) {})
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	s.emitFrameDebug(f) // first sighting emits; repeats must not allocate
	if n := testing.AllocsPerRun(1000, func() { s.emitFrameDebug(f) }); n != 0 {
		t.Fatalf("allocs with observer = %v", n)
	}
}

func BenchmarkEmitFrameDebugAudio(b *testing.B) {
	s := newDebugSession(b)
	defer s.Close()
	f := &OutputAudioFrame{Turn: "t1", Samples: make([]byte, 640)}
	b.Run("no_observers", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			s.emitFrameDebug(f)
		}
	})
	_, cancel, _ := s.Subscribe(0, func(DebugEvent) {})
	defer cancel()
	b.Run("observer", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			s.emitFrameDebug(f)
		}
	})
}
