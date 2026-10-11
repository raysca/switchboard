package ws

import (
	"context"
	"encoding/binary"
	"sync/atomic"
	"testing"
	"time"
)

// tone is an always-active Underlay producing a constant sample value.
type tone struct{ v int16 }

func (tone) Rate() int    { return 16000 }
func (tone) Active() bool { return true }
func (t tone) Mix(dst, src []byte) {
	for i := 0; i+1 < len(dst); i += 2 {
		s := int32(t.v)
		if src != nil {
			s += int32(int16(binary.LittleEndian.Uint16(src[i:])))
		}
		binary.LittleEndian.PutUint16(dst[i:], uint16(int16(s)))
	}
}

func sample(b []byte, i int) int16 { return int16(binary.LittleEndian.Uint16(b[2*i:])) }

func TestCueMixesWithAndWithoutBackground(t *testing.T) {
	c := NewCueUnderlay(tone{1000})
	dst := make([]byte, 640)
	c.Mix(dst, nil)
	base := sample(dst, 0)
	c.Start(tone{500})
	c.Mix(dst, nil)
	if sample(dst, 0) != base+500 {
		t.Fatalf("cue must add to the background, got %d want %d", sample(dst, 0), base+500)
	}
	c.Stop(0)
	c.Mix(dst, nil)
	if sample(dst, 0) != base {
		t.Fatal("cue must be gone after Stop")
	}
}

func TestCueMixesUnderSpeech(t *testing.T) {
	c := NewCueUnderlay(nil)
	c.Start(tone{500})
	src, dst := make([]byte, 640), make([]byte, 640)
	binary.LittleEndian.PutUint16(src, uint16(int16(200)))
	c.Mix(dst, src)
	if sample(dst, 0) != 700 {
		t.Fatalf("speech+cue = %d, want 700", sample(dst, 0))
	}
}

func TestCueActiveOnlyWhileSomethingPlays(t *testing.T) {
	c := NewCueUnderlay(nil)
	if c.Active() {
		t.Fatal("no background, no cue: inactive")
	}
	c.Start(tone{500})
	if !c.Active() {
		t.Fatal("cue must make it active")
	}
	c.Stop(0)
	if c.Active() {
		t.Fatal("stopped cue, no background: inactive")
	}
}

func TestCueFadeOutHasNoStep(t *testing.T) {
	c := NewCueUnderlay(nil)
	c.Start(tone{1000})
	c.Stop(40 * time.Millisecond) // 40 ms at 16 kHz = 640 samples = 2 frames of 320
	dst := make([]byte, 640)      // 20 ms mono PCM16
	prev := int16(1000)
	for f := 0; f < 3; f++ {
		c.Mix(dst, nil)
		for i := 0; i < len(dst)/2; i++ {
			v := sample(dst, i)
			if v > prev || prev-v > 10 { // monotonic, no step bigger than the ramp
				t.Fatalf("frame %d sample %d: %d after %d", f, i, v, prev)
			}
			prev = v
		}
	}
	if prev != 0 || c.Active() {
		t.Fatalf("fade must end silent and inactive: last=%d active=%v", prev, c.Active())
	}
}

func TestCueStartWakesIdlePacer(t *testing.T) {
	var wrote atomic.Int64
	cue := NewCueUnderlay(nil)
	cfg := DefaultPacerConfig()
	cfg.EnablePacing = true
	w := NewPacedWriter(cfg, func(context.Context, []byte) error { wrote.Add(1); return nil })
	w.Underlay = cue
	cue.wake = w.wake
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	time.Sleep(50 * time.Millisecond) // the writer is idle: nothing to play
	if wrote.Load() != 0 {
		t.Fatal("an idle writer with no cue must write nothing")
	}
	cue.Start(tone{500})
	deadline := time.Now().Add(200 * time.Millisecond)
	for wrote.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if wrote.Load() == 0 {
		t.Fatal("a cue started on an idle writer must reach the wire within 200 ms")
	}
}
