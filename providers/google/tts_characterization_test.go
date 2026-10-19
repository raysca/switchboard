package google

import (
	"context"
	"errors"
	"regexp"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/resilience"
)

// Characterization of the unary scheduler through its public surface only
// (NewTTSProcessor, core.NewSession, Post, OnOutput): these rows must keep
// passing unchanged when the scheduler moves onto ttsunit.

type charLog struct {
	mu     sync.Mutex
	frames []core.Frame
}

func (l *charLog) add(f core.Frame) {
	l.mu.Lock()
	l.frames = append(l.frames, f)
	l.mu.Unlock()
}

func (l *charLog) snapshot() []core.Frame {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]core.Frame(nil), l.frames...)
}

// waitFor polls the log until ok holds (then returns true) or 3 s pass.
func (l *charLog) waitFor(ok func([]core.Frame) bool) bool {
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(2 * time.Millisecond) {
		if ok(l.snapshot()) {
			return true
		}
	}
	return ok(l.snapshot())
}

// charStart builds a started session around a unary processor.
func charStart(t *testing.T, cfg TTSConfig) (*TTSProcessor, *core.Session, *charLog) {
	t.Helper()
	proc, err := NewTTSProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	pipe, _ := core.NewPipeline(proc)
	session, _ := core.NewSession(1, pipe, core.DefaultResourceBudget())
	log := &charLog{}
	session.OnOutput = log.add
	_ = proc.Attach(session, 0)
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return proc, session, log
}

func charCfg(client TTSClient) TTSConfig {
	cfg := DefaultTTSConfig()
	cfg.Client = client
	cfg.RetryPolicy = resilience.Policy{MaxAttempts: 1, AttemptTimeout: 5 * time.Second}
	return cfg
}

// charMarks renders the frame sequence of one unit as letters: U unit, S
// started, A audio, T stopped, P usage, E error.
func charMarks(frames []core.Frame, unit uint64) string {
	var b []byte
	var lastStopped uint64
	for _, f := range frames {
		switch v := f.(type) {
		case *core.SynthesisUnitFrame:
			if v.UnitID == unit {
				b = append(b, 'U')
			}
		case *core.TTSStartedFrame:
			if v.UnitID == unit {
				b = append(b, 'S')
			}
		case *core.OutputAudioFrame:
			if v.UnitID == unit {
				b = append(b, 'A')
			}
		case *core.TTSStoppedFrame:
			lastStopped = v.UnitID
			if v.UnitID == unit {
				b = append(b, 'T')
			}
		case *core.UsageMetricsFrame:
			if lastStopped == unit { // usage carries no unit id: it follows its unit's TTSStopped
				b = append(b, 'P')
			}
		case *core.ErrorFrame:
			if v.UnitID == unit {
				b = append(b, 'E')
			}
		}
	}
	return string(b)
}

func charAudio(frames []core.Frame, unit uint64) (out []*core.OutputAudioFrame) {
	for _, f := range frames {
		if a, ok := f.(*core.OutputAudioFrame); ok && a.UnitID == unit {
			out = append(out, a)
		}
	}
	return out
}

func charStopped(frames []core.Frame) int {
	n := 0
	for _, f := range frames {
		if _, ok := f.(*core.TTSStoppedFrame); ok {
			n++
		}
	}
	return n
}

func TestTTSCharacterization_FramesPerUnit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		bytes int
		rate  int
		chunk int
	}{
		{"empty", 0, 24000, 960},
		{"odd byte past a whole chunk", 961, 24000, 960},
		{"exactly three chunks", 2880, 24000, 960},
		{"16 kHz", 1600, 16000, 640},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotRate atomic.Int64
			cfg := charCfg(&mockTTSClient{synthesizeFn: func(_ context.Context, _, _ string, rate int) ([]byte, error) {
				gotRate.Store(int64(rate))
				return make([]byte, tc.bytes), nil
			}})
			cfg.SampleRate = tc.rate
			_, session, log := charStart(t, cfg)
			_ = session.Post(&core.SynthesisUnitFrame{Turn: "t", Gen: 1, UnitID: 7, Text: "Hello there.", IsFinal: true})
			if !log.waitFor(func(fs []core.Frame) bool { return unitDone.MatchString(charMarks(fs, 7)) }) {
				t.Fatalf("unit never completed: %q", charMarks(log.snapshot(), 7))
			}
			frames := log.snapshot()
			marks := charMarks(frames, 7)
			if !unitDone.MatchString(marks) {
				t.Fatalf("sequence = %q, want U S A+ T P", marks)
			}
			if gotRate.Load() != int64(tc.rate) {
				t.Fatalf("client sample rate = %d, want %d", gotRate.Load(), tc.rate)
			}
			audio := charAudio(frames, 7)
			total, finals := 0, 0
			for i, a := range audio {
				total += len(a.Samples)
				if a.IsFinal {
					finals++
					if i != len(audio)-1 {
						t.Fatalf("final frame is #%d of %d, want the last", i, len(audio))
					}
				} else if len(a.Samples) != tc.chunk {
					t.Fatalf("non-final frame #%d has %d bytes, want %d", i, len(a.Samples), tc.chunk)
				}
				if len(a.Samples) > tc.chunk {
					t.Fatalf("frame #%d has %d bytes, over the %d-byte chunk", i, len(a.Samples), tc.chunk)
				}
				if a.TurnFinal != a.IsFinal { // the unit is the turn's last
					t.Fatalf("frame #%d: TurnFinal=%v IsFinal=%v", i, a.TurnFinal, a.IsFinal)
				}
				if a.SampleRate != tc.rate || a.Channels != 1 || a.Encoding != core.EncodingPCM16LE || a.Turn != "t" || a.Gen != 1 {
					t.Fatalf("frame #%d carries %+v", i, a)
				}
			}
			if finals != 1 {
				t.Fatalf("%d final frames, want exactly one", finals)
			}
			// A lone odd byte may be dropped or kept (R6d): only sample-aligned bytes count.
			if total&^1 != tc.bytes&^1 {
				t.Fatalf("audio bytes = %d, want %d aligned", total, tc.bytes&^1)
			}
			var usage *core.UsageMetricsFrame
			for _, f := range frames {
				if u, ok := f.(*core.UsageMetricsFrame); ok {
					usage = u
				}
			}
			wantMs := int64(tc.bytes / 2 * 1000 / tc.rate)
			if usage == nil || usage.Provider != "google" || usage.Service != "tts" || usage.Model != cfg.Voice ||
				*usage.Characters != int64(len("Hello there.")) || *usage.AudioDurationMs != wantMs || *usage.Requests != 1 {
				t.Fatalf("usage = %+v (want %d ms)", usage, wantMs)
			}
		})
	}
}

var unitDone = regexp.MustCompile(`^USA+TP$`)

func TestTTSCharacterization_ClientErrors(t *testing.T) {
	type want struct{ timeout, circuit, canceled, retryable, clientErr, asProvider bool }
	for _, tc := range []struct {
		name    string
		err     error
		trip    bool // trip the breaker first: the client is never called
		timeout bool // the client blocks until the attempt deadline
		want    want
	}{
		{name: "non-retryable provider error", err: &ProviderError{Capability: "tts", BaseErr: ErrProviderBadRequest},
			want: want{clientErr: true, asProvider: true}},
		{name: "deadline exceeded", err: context.DeadlineExceeded,
			want: want{timeout: true, retryable: true, asProvider: true}},
		{name: "plain error", err: errors.New("x"),
			want: want{asProvider: true}}, // MapError: an unknown error is an internal, non-retryable provider error
		{name: "attempt timeout", timeout: true,
			want: want{timeout: true, retryable: true}},
		{name: "breaker open", trip: true,
			want: want{circuit: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			client := &mockTTSClient{synthesizeFn: func(ctx context.Context, text, _ string, _ int) ([]byte, error) {
				calls.Add(1)
				if text == "good" {
					return make([]byte, 960), nil
				}
				if tc.timeout {
					<-ctx.Done()
					return nil, ctx.Err()
				}
				return nil, tc.err
			}}
			cfg := charCfg(client)
			if tc.timeout {
				cfg.RetryPolicy.AttemptTimeout = 40 * time.Millisecond
			}
			if tc.trip {
				cfg.CircuitBreaker = resilience.NewBreaker(resilience.Config{FailureThreshold: 1, Cooldown: time.Hour})
				cfg.CircuitBreaker.RecordFailure()
			}
			_, session, log := charStart(t, cfg)
			_ = session.Post(&core.SynthesisUnitFrame{Turn: "t", Gen: 1, UnitID: 1, Text: "bad"})
			_ = session.Post(&core.SynthesisUnitFrame{Turn: "t", Gen: 1, UnitID: 2, Text: "good", IsFinal: true})
			if tc.trip {
				// A tripped breaker refuses "good" too: both units fail, none speaks.
				if !log.waitFor(func(fs []core.Frame) bool { return charMarks(fs, 1) == "UE" && charMarks(fs, 2) == "UE" }) {
					t.Fatalf("marks = %q / %q", charMarks(log.snapshot(), 1), charMarks(log.snapshot(), 2))
				}
				if calls.Load() != 0 {
					t.Fatalf("client called %d times through an open breaker", calls.Load())
				}
			} else if !log.waitFor(func(fs []core.Frame) bool { return charMarks(fs, 2) == "USATP" }) {
				t.Fatalf("unit 2 after a failed unit 1: %q (unit 1: %q)", charMarks(log.snapshot(), 2), charMarks(log.snapshot(), 1))
			}
			frames := log.snapshot()
			if m := charMarks(frames, 1); m != "UE" {
				t.Fatalf("failed unit marks = %q, want UE (no audio, no stop)", m)
			}
			var ef *core.ErrorFrame
			for _, f := range frames {
				if e, ok := f.(*core.ErrorFrame); ok && e.UnitID == 1 {
					ef = e
				}
			}
			if ef == nil || ef.Fatal || ef.Err == nil {
				t.Fatalf("error frame = %+v, want non-fatal with an error", ef)
			}
			got := want{
				timeout:    errors.Is(ef.Err, ErrProviderTimeout),
				circuit:    errors.Is(ef.Err, ErrCircuitOpen),
				canceled:   errors.Is(ef.Err, context.Canceled),
				retryable:  classify(ef.Err).Retryable,
				clientErr:  classify(ef.Err).ClientError,
				asProvider: func() bool { var pe *ProviderError; return errors.As(ef.Err, &pe) }(),
			}
			if tc.trip || tc.timeout { // raised by resilience.Execute itself: the concrete type is not pinned (R5)
				got.asProvider, tc.want.asProvider = false, false
			}
			if got != tc.want {
				t.Fatalf("error %v: got %+v, want %+v", ef.Err, got, tc.want)
			}
		})
	}
}

func TestTTSCharacterization_ClientCanceledIsSilent(t *testing.T) {
	cfg := charCfg(&mockTTSClient{synthesizeFn: func(_ context.Context, text, _ string, _ int) ([]byte, error) {
		if text == "bad" {
			return nil, context.Canceled
		}
		return make([]byte, 960), nil
	}})
	_, session, log := charStart(t, cfg)
	_ = session.Post(&core.SynthesisUnitFrame{Turn: "t", Gen: 1, UnitID: 1, Text: "bad"})
	_ = session.Post(&core.SynthesisUnitFrame{Turn: "t", Gen: 1, UnitID: 2, Text: "good", IsFinal: true})
	if !log.waitFor(func(fs []core.Frame) bool { return charMarks(fs, 2) == "USATP" }) {
		t.Fatalf("unit 2: %q", charMarks(log.snapshot(), 2))
	}
	if m := charMarks(log.snapshot(), 1); m != "U" {
		t.Fatalf("canceled unit marks = %q, want only the unit (no error frame, no audio)", m)
	}
}

func TestTTSCharacterization_BargeInStopsMidEmission(t *testing.T) {
	const chunks = 400
	cfg := charCfg(&mockTTSClient{synthesizeFn: func(context.Context, string, string, int) ([]byte, error) {
		return make([]byte, 960*chunks), nil
	}})
	_, session, log := charStart(t, cfg)
	var audio atomic.Int32
	first := make(chan struct{})
	var once sync.Once
	session.OnOutput = func(f core.Frame) {
		log.add(f)
		if _, ok := f.(*core.OutputAudioFrame); ok {
			audio.Add(1)
			once.Do(func() { close(first) })
			time.Sleep(time.Millisecond) // keep emission slower than the interruption
		}
	}
	_ = session.Post(&core.SynthesisUnitFrame{Turn: "t", Gen: 1, UnitID: 1, Text: "Long reply.", IsFinal: true})
	select {
	case <-first:
	case <-time.After(3 * time.Second):
		t.Fatal("no audio")
	}
	_ = session.Post(&core.InterruptionFrame{Turn: "t", Gen: session.CurrentGeneration()}) // no generation bump: the cancelled unit context alone must stop the unit
	time.Sleep(300 * time.Millisecond)                                                     // in-flight frames drain
	settled := audio.Load()
	time.Sleep(300 * time.Millisecond)
	if n := audio.Load(); n != settled || n >= chunks {
		t.Fatalf("audio frames: %d at settle, %d later, of %d; want emission to stop early and stay stopped", settled, n, chunks)
	}
	if m := charMarks(log.snapshot(), 1); m[len(m)-1] == 'P' || charStopped(log.snapshot()) != 0 {
		t.Fatalf("interrupted unit completed: %q", m)
	}
}

func TestTTSCharacterization_LookAheadIsBoundedAndOrdered(t *testing.T) {
	release := make(chan struct{})
	var started atomic.Int32
	cfg := charCfg(&mockTTSClient{synthesizeFn: func(_ context.Context, text, _ string, _ int) ([]byte, error) {
		started.Add(1)
		if text == "u1" {
			<-release
		}
		return make([]byte, 960), nil
	}})
	_, session, log := charStart(t, cfg)
	const n = 8
	for i := 1; i <= n; i++ {
		_ = session.Post(&core.SynthesisUnitFrame{Turn: "t", Gen: 1, UnitID: uint64(i), Text: "u" + string(rune('0'+i)), IsFinal: i == n})
	}
	time.Sleep(100 * time.Millisecond)
	// As measured on base: the slow unit plus two units ahead, no more.
	if got := started.Load(); got != 3 {
		t.Fatalf("units synthesizing while u1 is pending = %d, want 3", got)
	}
	if stopped := charStopped(log.snapshot()); stopped != 0 {
		t.Fatalf("%d units emitted before u1 finished", stopped)
	}
	close(release)
	if !log.waitFor(func(fs []core.Frame) bool { return charStopped(fs) == n }) {
		t.Fatalf("stopped = %d of %d", charStopped(log.snapshot()), n)
	}
	var order []uint64
	for _, f := range log.snapshot() {
		if s, ok := f.(*core.TTSStoppedFrame); ok {
			order = append(order, s.UnitID)
		}
	}
	for i, id := range order {
		if id != uint64(i+1) {
			t.Fatalf("emission order = %v", order)
		}
	}
}

func TestTTSCharacterization_StaleBehindSlowUnitKeepsOrder(t *testing.T) {
	release := make(chan struct{})
	cfg := charCfg(&mockTTSClient{synthesizeFn: func(_ context.Context, text, _ string, _ int) ([]byte, error) {
		if text == "slow" {
			<-release
		}
		return make([]byte, 960), nil
	}})
	_, session, log := charStart(t, cfg)
	_ = session.Post(&core.SynthesisUnitFrame{Turn: "t", Gen: 1, UnitID: 1, Text: "slow"})
	_ = session.Post(&core.SynthesisUnitFrame{Turn: "t", Gen: 1, UnitID: 2, Text: "queued"})
	time.Sleep(50 * time.Millisecond)
	session.IncrementGeneration() // no interruption: units 1 and 2 go stale
	_ = session.Post(&core.SynthesisUnitFrame{Turn: "t2", Gen: 2, UnitID: 3, Text: "new a"})
	_ = session.Post(&core.SynthesisUnitFrame{Turn: "t2", Gen: 2, UnitID: 4, Text: "new b", IsFinal: true})
	close(release)
	if !log.waitFor(func(fs []core.Frame) bool { return charStopped(fs) >= 2 }) {
		t.Fatalf("stopped = %d", charStopped(log.snapshot()))
	}
	time.Sleep(50 * time.Millisecond)
	var order []uint64
	for _, f := range log.snapshot() {
		if s, ok := f.(*core.TTSStoppedFrame); ok {
			order = append(order, s.UnitID)
		}
	}
	if len(order) != 2 || order[0] != 3 || order[1] != 4 {
		t.Fatalf("stopped units = %v, want [3 4]", order)
	}
}

type closableTTSClient struct {
	mockTTSClient
	closes atomic.Int32
}

func (c *closableTTSClient) Close() error { c.closes.Add(1); return nil }

func TestTTSCharacterization_CloseClosesTheClientOnce(t *testing.T) {
	client := &closableTTSClient{}
	proc, _, _ := charStart(t, charCfg(client))
	if err := proc.Close(); err != nil {
		t.Fatal(err)
	}
	if n := client.closes.Load(); n != 1 {
		t.Fatalf("client closed %d times by processor Close, want 1", n)
	}
}

func TestTTSCharacterization_CloseWhileWaitingReturnsPromptly(t *testing.T) {
	base := runtime.NumGoroutine()
	cfg := charCfg(&mockTTSClient{synthesizeFn: func(ctx context.Context, text, _ string, _ int) ([]byte, error) {
		if text == "u1" {
			<-ctx.Done() // u1 never finishes, so u2 and u3 wait for their emission turn
			return nil, ctx.Err()
		}
		return make([]byte, 960), nil
	}})
	proc, session, _ := charStart(t, cfg)
	for i, txt := range []string{"u1", "u2", "u3"} {
		_ = session.Post(&core.SynthesisUnitFrame{Turn: "t", Gen: 1, UnitID: uint64(i + 1), Text: txt})
	}
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	_ = proc.Close()
	session.Close()
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Close took %v", d)
	}
	for i := 0; i < 100 && runtime.NumGoroutine() > base+3; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > base+3 {
		t.Fatalf("goroutines %d after Close, %d before", n, base)
	}
}

// A blank final unit still closes the unit (Started, one empty final frame,
// Stopped, Usage), whatever the number of synthesis requests it makes.
func TestTTSCharacterization_BlankTextUnit(t *testing.T) {
	var calls atomic.Int64
	cfg := charCfg(&mockTTSClient{synthesizeFn: func(_ context.Context, _, _ string, _ int) ([]byte, error) {
		calls.Add(1)
		return nil, nil
	}})
	_, session, log := charStart(t, cfg)
	_ = session.Post(&core.SynthesisUnitFrame{Turn: "t", Gen: 1, UnitID: 7, Text: "  ", IsFinal: true})
	if !log.waitFor(func(fs []core.Frame) bool { return charMarks(fs, 7) == "USATP" }) {
		t.Fatalf("sequence = %q, want USATP", charMarks(log.snapshot(), 7))
	}
	audio := charAudio(log.snapshot(), 7)
	if len(audio) != 1 || len(audio[0].Samples) != 0 || !audio[0].IsFinal || !audio[0].TurnFinal {
		t.Fatalf("audio = %+v, want one empty final turn-final frame", audio)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("client calls = %d, want 0 (same as base)", n)
	}
}
