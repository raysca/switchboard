package ws_test

import (
	"context"
	"encoding/binary"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"govoice/core"
	"govoice/transport/ws"
)

// dcUnder adds a constant to every sample (a trivially checkable ambience).
type dcUnder struct{ v int16 }

func (dcUnder) Rate() int    { return 24000 }
func (dcUnder) Active() bool { return true }
func (u dcUnder) Mix(dst, src []byte) {
	for i := 0; i+1 < len(dst); i += 2 {
		s := u.v
		if src != nil {
			s += int16(binary.LittleEndian.Uint16(src[i:]))
		}
		binary.LittleEndian.PutUint16(dst[i:], uint16(s))
	}
}

type wireLog struct {
	mu    sync.Mutex
	first []int16 // first sample of every written frame
}

func (l *wireLog) write(_ context.Context, raw []byte) error {
	f, err := ws.DecodeBinaryFrame(raw, 0, time.Now())
	if err != nil {
		return err
	}
	a := f.(*core.InputAudioFrame)
	l.mu.Lock()
	l.first = append(l.first, int16(binary.LittleEndian.Uint16(a.Samples)))
	l.mu.Unlock()
	return nil
}

func (l *wireLog) snapshot() []int16 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]int16(nil), l.first...)
}

func speechFrame(gen uint64, v int16) *core.OutputAudioFrame {
	f := outputFrame(0, gen, core.OutputInterruptible)
	for i := 0; i < len(f.Samples); i += 2 {
		binary.LittleEndian.PutUint16(f.Samples[i:], uint16(v))
	}
	return f
}

func runUnderlay(t *testing.T) (*ws.PacedWriter, *wireLog) {
	t.Helper()
	log := &wireLog{}
	p := ws.NewPacedWriter(ws.DefaultPacerConfig(), log.write)
	p.Underlay = dcUnder{1000}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = p.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); p.Close(); <-done })
	return p, log
}

func TestUnderlayEmitsAmbienceOnPacerClockWithoutSpeechSignals(t *testing.T) {
	p, log := runUnderlay(t)
	var completes, transitions atomic.Int32
	p.OnWriteComplete = func(*core.OutputAudioFrame, error) { completes.Add(1) }
	p.OnSpeakingState = func(core.SpeakingStateFrame) { transitions.Add(1) }
	time.Sleep(410 * time.Millisecond)
	got := log.snapshot()
	if len(got) < 15 || len(got) > 24 { // 20 ms cadence: ~20 frames in 410 ms
		t.Fatalf("%d ambience frames in 410 ms", len(got))
	}
	for _, v := range got {
		if v != 1000 {
			t.Fatalf("ambience frame sample %d", v)
		}
	}
	if completes.Load() != 0 || transitions.Load() != 0 || p.FramesWritten.Load() != 0 {
		t.Fatalf("ambience leaked into speech signals: complete=%d speaking=%d written=%d", completes.Load(), transitions.Load(), p.FramesWritten.Load())
	}
}

func TestUnderlayMixesUnderSpeechAndLeavesTapFrameClean(t *testing.T) {
	p, log := runUnderlay(t)
	taps := make(chan int16, 4)
	p.OnWriteComplete = func(f *core.OutputAudioFrame, err error) {
		taps <- int16(binary.LittleEndian.Uint16(f.Samples))
	}
	var started atomic.Int32
	p.OnSpeakingState = func(e core.SpeakingStateFrame) { started.Add(1) }
	if err := p.Enqueue(speechFrame(1, 2000)); err != nil {
		t.Fatal(err)
	}
	select {
	case v := <-taps:
		if v != 2000 {
			t.Fatalf("tap sees %d: recording must get speech without ambience", v)
		}
	case <-time.After(time.Second):
		t.Fatal("no completion")
	}
	found := false
	for _, v := range log.snapshot() {
		found = found || v == 3000
	}
	if !found {
		t.Fatalf("no mixed frame on the wire: %v", log.snapshot())
	}
	if started.Load() == 0 {
		t.Fatal("speech did not report speaking")
	}
}

func TestUnderlayFlushClearsSpeechKeepsAmbienceAndQueueBound(t *testing.T) {
	p, log := runUnderlay(t)
	for i := 0; i < 5; i++ { // 100 ms of speech, the configured bound
		if err := p.Enqueue(speechFrame(1, 2000)); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.Flush(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if n := p.QueuedFrames(); n != 0 {
		t.Fatalf("%d frames queued after flush", n)
	}
	log.mu.Lock()
	mark := len(log.first)
	log.mu.Unlock()
	before := p.AmbientFrames.Load()
	time.Sleep(200 * time.Millisecond)
	if p.AmbientFrames.Load() < before+5 {
		t.Fatalf("ambience stopped after flush: %d -> %d", before, p.AmbientFrames.Load())
	}
	for _, v := range log.snapshot()[mark:] {
		if v != 1000 {
			t.Fatalf("speech sample %d after flush", v)
		}
	}
}

// Speech arriving while an ambience frame waits for its slot must take that
// slot: at most one 20 ms frame (already on the wire) delays it.
func TestUnderlaySpeechPreemptsWaitingAmbience(t *testing.T) {
	var mu sync.Mutex
	var at time.Time
	p := ws.NewPacedWriter(ws.DefaultPacerConfig(), func(_ context.Context, raw []byte) error {
		f, _ := ws.DecodeBinaryFrame(raw, 0, time.Now())
		if int16(binary.LittleEndian.Uint16(f.(*core.InputAudioFrame).Samples)) == 3000 {
			mu.Lock()
			if at.IsZero() {
				at = time.Now()
			}
			mu.Unlock()
		}
		return nil
	})
	p.Underlay = dcUnder{1000}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = p.Run(ctx); close(done) }()
	defer func() { cancel(); p.Close(); <-done }()
	worst := time.Duration(0)
	for i := 0; i < 10; i++ {
		time.Sleep(time.Duration(37+i*3) * time.Millisecond) // land at varied phases of the 20 ms clock
		mu.Lock()
		at = time.Time{}
		mu.Unlock()
		t0 := time.Now()
		if err := p.Enqueue(speechFrame(0, 2000)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(60 * time.Millisecond)
		mu.Lock()
		d := at.Sub(t0)
		mu.Unlock()
		if at.IsZero() {
			t.Fatal("speech never written")
		}
		worst = max(worst, d)
	}
	if worst > 30*time.Millisecond { // unfixed: ~40 ms (a second ambience frame queued ahead)
		t.Fatalf("speech waited %v behind ambience (want <= 20 ms + scheduling slack)", worst)
	}
}

// BenchmarkUnderlayIdleWriters reports the CPU that ambience costs per idle
// session (50 encoded writes/s each, no-op socket):
// go test -run=^$ -bench=UnderlayIdle -benchtime=1x ./ws/
func BenchmarkUnderlayIdleWriters(b *testing.B) {
	const n, window = 1000, 2 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	ps := make([]*ws.PacedWriter, n)
	for i := range ps {
		ps[i] = ws.NewPacedWriter(ws.DefaultPacerConfig(), func(context.Context, []byte) error { return nil })
		ps[i].Underlay = dcUnder{1}
		wg.Add(1)
		go func(p *ws.PacedWriter) { defer wg.Done(); _ = p.Run(ctx) }(ps[i])
	}
	time.Sleep(200 * time.Millisecond)
	count := func() (t uint64) {
		for _, p := range ps {
			t += p.AmbientFrames.Load()
		}
		return t
	}
	var r0, r1 syscall.Rusage
	f0 := count()
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &r0)
	b.ResetTimer()
	time.Sleep(window)
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &r1)
	cpu := time.Duration(r1.Utime.Nano()+r1.Stime.Nano()-r0.Utime.Nano()-r0.Stime.Nano()) * time.Nanosecond
	frames := count() - f0
	cancel()
	wg.Wait()
	b.ReportMetric(float64(cpu)/float64(window)*100, "cpu%")
	b.ReportMetric(float64(frames)/window.Seconds()/n, "frames/s/session")
	b.ReportMetric(float64(cpu.Microseconds())/float64(frames), "us/frame")
}

// After the socket stalls, ambience resumes at real time: no catch-up burst.
func TestUnderlayDoesNotBurstAfterStall(t *testing.T) {
	var mu sync.Mutex
	var times []time.Time
	stall := make(chan struct{})
	var once sync.Once
	p := ws.NewPacedWriter(ws.DefaultPacerConfig(), func(context.Context, []byte) error {
		once.Do(func() { <-stall })
		mu.Lock()
		times = append(times, time.Now())
		mu.Unlock()
		return nil
	})
	p.Underlay = dcUnder{1}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = p.Run(ctx); close(done) }()
	defer func() { cancel(); p.Close(); <-done }()
	time.Sleep(80 * time.Millisecond) // first write blocked for 80 ms (< the 100 ms speech catch-up window)
	close(stall)
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	for i := 3; i < len(times); i++ { // times[0] is the stalled write itself
		if times[i].Sub(times[i-2]) < 30*time.Millisecond {
			t.Fatalf("burst after stall: writes %d..%d within %v", i-2, i, times[i].Sub(times[i-2]))
		}
	}
}
