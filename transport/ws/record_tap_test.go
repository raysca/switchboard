package ws_test

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/record"
	"govoice/transport/ws"
)

// A barge-in flush drops queued and in-flight audio; the recording's bot track
// must contain only what the pacer released successfully.
func TestRecorderBotTrackExcludesFlushedTail(t *testing.T) {
	dir := t.TempDir()
	sink, err := record.NewLocalSink(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec := record.New(sink, record.Meta{ID: "flush-test"}, record.Config{})

	calls := 0
	release := make(chan struct{})
	pacer := ws.NewPacedWriter(ws.PacerConfig{
		MaxQueueDuration: time.Second, MaxQueueBytes: 1 << 20, WriteTimeout: 5 * time.Second,
	}, func(ctx context.Context, _ []byte) error {
		calls++
		if calls == 1 {
			return nil
		}
		<-release // second write is in flight when the flush arrives, then "succeeds" late
		return nil
	})
	released := 0
	pacer.OnWriteComplete = func(f *core.OutputAudioFrame, err error) {
		if err == nil {
			released++
			rec.Bot(f)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = pacer.Run(ctx) }()

	pcm := make([]byte, 480*2) // 20ms @ 24k, constant non-zero
	for i := 0; i < len(pcm); i += 2 {
		binary.LittleEndian.PutUint16(pcm[i:], 3000)
	}
	for i := 0; i < 6; i++ {
		f := &core.OutputAudioFrame{Samples: append([]byte(nil), pcm...), Encoding: core.EncodingPCM16LE, SampleRate: 24000, Channels: 1, Gen: 1, Turn: "t", UnitID: 1}
		if err := pacer.Enqueue(f); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for pacer.FramesWritten.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // frame 2 is now blocked in write
	flushed := make(chan error, 1)
	pacer.BeginFlush(1, func(err error) { flushed <- err })
	close(release)
	if err := <-flushed; err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	if released != 1 {
		t.Fatalf("released=%d, want exactly the pre-flush frame", released)
	}
	b, err := os.ReadFile(filepath.Join(dir, "flush-test.wav"))
	if err != nil {
		t.Fatal(err)
	}
	botSamples := 0
	for i := 44; i+4 <= len(b); i += 4 {
		if binary.LittleEndian.Uint16(b[i+2:]) != 0 {
			botSamples++
		}
	}
	if botSamples < 300 || botSamples > 330 { // one 20ms frame = 320 samples @16k
		t.Fatalf("bot samples=%d, want ~320 (one frame)", botSamples)
	}
}

type countTap struct{ bot atomic.Int32 }

func (*countTap) User(*core.InputAudioFrame)   {}
func (c *countTap) Bot(*core.OutputAudioFrame) { c.bot.Add(1) }

// The Connection must tap output after pacing: frames queued behind the paced
// writer and flushed by barge-in never reach the tap.
func TestConnectionTapsOnlyReleasedOutput(t *testing.T) {
	tap := &countTap{}
	done := make(chan struct{})
	frame := func(gen uint64) *core.OutputAudioFrame {
		return &core.OutputAudioFrame{Samples: make([]byte, 480*2), Encoding: core.EncodingPCM16LE,
			SampleRate: ws.FixedPlayerSampleRate, Channels: 1, Gen: gen}
	}
	_, _ = dialReady(t, "jti-record-tap", func(conn *ws.Connection) {
		defer close(done)
		conn.SetAudioTap(tap)
		for i := 0; i < 5; i++ { // 5 x 20ms: at most one is written, the rest wait their media slot
			if err := conn.ProcessFrame(context.Background(), frame(1), core.Downstream, nil); err != nil {
				t.Error(err)
				return
			}
		}
		conn.Pacer().BeginFlush(1, nil)
		before := tap.bot.Load()
		// Post-flush audio is released and must be tapped (proves the wiring).
		if err := conn.ProcessFrame(context.Background(), frame(2), core.Downstream, nil); err != nil {
			t.Error(err)
			return
		}
		deadline := time.Now().Add(2 * time.Second)
		for tap.bot.Load() == before && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
	})
	<-done
	time.Sleep(100 * time.Millisecond)
	if n := tap.bot.Load(); n < 1 || n > 3 {
		t.Fatalf("tapped %d frames, want the post-flush frame plus at most the 1-2 released before the flush", n)
	}
}
