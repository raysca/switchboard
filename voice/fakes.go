package voice

import (
	"context"
	"errors"
	"sync/atomic"
	"time"
)

type Mode int32

const (
	ModeNormal      Mode = iota
	ModeSlow             // async latency x10
	ModeSlowSync         // ProcessFrame sleeps 200ms on every frame (stops consuming)
	ModeStall            // async task blocks until ctx cancelled
	ModeHang             // async task ignores ctx (leaks until HangRelease)
	ModeError            // ProcessFrame returns error
	ModePanic            // ProcessFrame panics
	ModeChildPanic       // managed task panics
	ModeFlood            // ProcessFrame emits frames in a loop (runaway)
	ModeOverflow         // managed task floods the session inbox
	ModeBadTeardown      // Close never returns
)

var modeNames = map[string]Mode{
	"normal": ModeNormal, "slow": ModeSlow, "slowsync": ModeSlowSync, "stall": ModeStall,
	"hang": ModeHang, "error": ModeError, "panic": ModePanic, "childpanic": ModeChildPanic,
	"flood": ModeFlood, "overflow": ModeOverflow, "badteardown": ModeBadTeardown,
}

func ParseMode(s string) (Mode, bool) { m, ok := modeNames[s]; return m, ok }

// HangRelease lets tests reclaim goroutines deliberately leaked by ModeHang/ModeBadTeardown.
var HangRelease = make(chan struct{})

var ErrInjected = errors.New("injected processor error")

type FakeKind int

const (
	FakeSTT FakeKind = iota // SpeechStopped -> Text
	FakeLLM                 // Text -> Text
	FakeTTS                 // Text -> N output AudioFrames
)

// Fake is a fake provider processor. Normal behaviour: pass everything through;
// on its trigger frame, do "provider work" in a managed task and re-enter the
// pipeline with the result.
type Fake struct {
	Name    string
	Kind    FakeKind
	Latency time.Duration
	mode    atomic.Int32
	armed   atomic.Bool // async fault modes fire once

	s   *Session
	idx int
}

func (f *Fake) SetMode(m Mode) { f.mode.Store(int32(m)); f.armed.Store(true) }
func (f *Fake) Mode() Mode     { return Mode(f.mode.Load()) }

func (f *Fake) Attach(s *Session, i int) error { f.s, f.idx = s, i; return nil }

func (f *Fake) Close() error {
	if f.Mode() == ModeBadTeardown {
		<-HangRelease
	}
	return nil
}

var silence = make([]float32, 512)

func (f *Fake) ProcessFrame(ctx context.Context, fr Frame, dir Direction, e Emitter) error {
	switch f.Mode() {
	case ModeError:
		return ErrInjected
	case ModePanic:
		panic("injected panic in " + f.Name)
	case ModeSlowSync: // slow but cancellable
		select {
		case <-time.After(200 * time.Millisecond):
		case <-ctx.Done():
			return nil
		}
	case ModeFlood:
		for {
			if err := e.Push(&TextFrame{Text: "flood"}); err != nil {
				return err
			}
		}
	case ModeChildPanic, ModeStall, ModeHang, ModeOverflow:
		if f.armed.CompareAndSwap(true, false) {
			if err := f.s.Go(f.faultTask(f.Mode())); err != nil {
				return err
			}
		}
	}

	var in string
	var at time.Time
	switch v := fr.(type) {
	case *SpeechStoppedFrame:
		if f.Kind == FakeSTT {
			in, at = "user said something", v.EmittedAt
		}
	case *TextFrame:
		if f.Kind != FakeSTT {
			in, at = v.Text, v.At
		}
	}
	if err := e.PushDirection(fr, dir); err != nil {
		return err
	}
	if in == "" {
		return nil
	}
	lat := f.Latency
	if f.Mode() == ModeSlow {
		lat *= 10
	}
	kind, next := f.Kind, f.idx+1
	s := f.s
	return s.Go(func(ctx context.Context) error {
		select {
		case <-time.After(lat):
		case <-ctx.Done():
			return nil
		}
		if kind == FakeTTS {
			for i := 0; i < 10; i++ {
				if err := s.Post(&AudioFrame{Samples: silence, Output: true, Due: at}, Downstream, next); err != nil {
					return err
				}
			}
			return nil
		}
		return s.Post(&TextFrame{Text: in + " -> ok", At: at}, Downstream, next)
	})
}

func (f *Fake) faultTask(m Mode) func(context.Context) error {
	s, next := f.s, f.idx+1
	return func(ctx context.Context) error {
		switch m {
		case ModeChildPanic:
			panic("injected panic in managed task of " + f.Name)
		case ModeStall:
			<-ctx.Done()
		case ModeHang:
			<-HangRelease // ignores ctx: cannot be reclaimed by Go supervision
		case ModeOverflow:
			for ctx.Err() == nil {
				if err := s.Post(&TextFrame{Text: "overflow"}, Downstream, next); err != nil {
					return err
				}
			}
		}
		return nil
	}
}
