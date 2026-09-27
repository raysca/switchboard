package ws

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"govoice/core"
)

var (
	ErrSlowClient      = errors.New("ws: client buffer overflow or write deadline breach (slow client)")
	ErrWriterClosed    = errors.New("ws: paced writer closed")
	ErrQueueBufferFull = errors.New("ws: output audio queue budget exceeded")
)

// PacerConfig configures media clock pacing and queue limits.
type PacerConfig struct {
	MaxQueueDuration time.Duration // Maximum queued audio duration (100ms residual bound)
	MaxQueueBytes    int           // Maximum queued audio bytes at 24 kHz PCM16 mono
	WriteTimeout     time.Duration // Maximum duration per socket write (e.g. 2s)
	EnablePacing     bool          // If true, pace against real media time (default: true)
}

// DefaultPacerConfig returns production-tuned pacer parameters.
func DefaultPacerConfig() PacerConfig {
	return PacerConfig{
		MaxQueueDuration: 100 * time.Millisecond,
		MaxQueueBytes:    4800, // 100ms of 24 kHz 16-bit mono
		WriteTimeout:     2 * time.Second,
		EnablePacing:     true,
	}
}

// PacedWriter queues output audio frames, enforces bounded memory, discards stale
// generations, paces writes against real media time, and terminates slow clients explicitly.
type PacedWriter struct {
	cfg     PacerConfig
	writeFn func(ctx context.Context, data []byte) error

	mu           sync.Mutex
	cond         *sync.Cond
	queue        []*core.OutputAudioFrame
	queuedBytes  int
	queuedDur    time.Duration
	activeGen    uint64
	inflight     *core.OutputAudioFrame
	dropInflight bool
	abortCh      chan struct{}
	inWrite      bool
	writeGen     uint64
	writeCancel  context.CancelFunc
	writeDoneCh  chan struct{}
	closed       bool
	lastMediaEnd time.Time

	// Metrics
	FramesWritten atomic.Uint64
	FramesDropped atomic.Uint64
	FlushesDone   atomic.Uint64

	OnWriteComplete func(f *core.OutputAudioFrame, err error)
}

// NewPacedWriter creates an initialized PacedWriter.
func NewPacedWriter(cfg PacerConfig, writeFn func(ctx context.Context, data []byte) error) *PacedWriter {
	pw := &PacedWriter{
		cfg:     cfg,
		writeFn: writeFn,
		queue:   make([]*core.OutputAudioFrame, 0, 32),
		abortCh: make(chan struct{}),
	}
	pw.cond = sync.NewCond(&pw.mu)
	return pw
}

// SetActiveGeneration updates the current generation ID. Older frames will be discarded.
func (w *PacedWriter) SetActiveGeneration(gen uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.activeGen = gen
}

// Enqueue adds an output audio frame to the pacing queue.
// Stale generations are discarded. Current-generation overflow triggers ErrSlowClient.
func (w *PacedWriter) Enqueue(f *core.OutputAudioFrame) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return ErrWriterClosed
	}

	// 1. Stale generation discard
	if f.Gen != 0 && f.Gen < w.activeGen {
		w.FramesDropped.Add(1)
		return nil
	}

	// 2. Current-generation queue limits check (NO silent truncation)
	frameBytes := len(f.Samples)
	frameDur := f.Duration
	if frameDur <= 0 && f.SampleRate > 0 {
		samples := frameBytes / 2
		frameDur = time.Duration(samples) * time.Second / time.Duration(f.SampleRate)
	}

	if w.queuedDur+frameDur > w.cfg.MaxQueueDuration || w.queuedBytes+frameBytes > w.cfg.MaxQueueBytes {
		return fmt.Errorf("%w: queue duration %v > max %v or bytes %d > max %d",
			ErrSlowClient, w.queuedDur+frameDur, w.cfg.MaxQueueDuration, w.queuedBytes+frameBytes, w.cfg.MaxQueueBytes)
	}

	w.queue = append(w.queue, f)
	w.queuedBytes += frameBytes
	w.queuedDur += frameDur
	w.cond.Signal()

	return nil
}

// Flush removes all queued audio frames belonging to the specified generation.
func (w *PacedWriter) Flush(gen uint64) {
	w.mu.Lock()

	active := make([]*core.OutputAudioFrame, 0, len(w.queue))
	for _, f := range w.queue {
		if f.Gen == gen {
			w.queuedBytes -= len(f.Samples)
			w.queuedDur -= f.Duration
			w.FramesDropped.Add(1)
		} else {
			active = append(active, f)
		}
	}
	w.queue = active
	if w.inflight != nil && w.inflight.Gen == gen {
		w.dropInflight = true
	}
	if gen+1 > w.activeGen {
		w.activeGen = gen + 1
	}
	w.lastMediaEnd = time.Time{} // Reset media clock
	oldAbort := w.abortCh
	w.abortCh = make(chan struct{})
	w.FlushesDone.Add(1)
	w.cond.Broadcast()

	var cancelFn context.CancelFunc
	var waitCh chan struct{}
	if w.inWrite && w.writeGen <= gen {
		cancelFn = w.writeCancel
		waitCh = w.writeDoneCh
	}
	w.mu.Unlock()
	close(oldAbort)

	if cancelFn != nil {
		cancelFn()
	}
	if waitCh != nil {
		select {
		case <-waitCh:
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Run executes the media-clock pacing loop until the context is canceled or the writer is closed.
func (w *PacedWriter) Run(ctx context.Context) error {
	for {
		w.mu.Lock()
		for len(w.queue) == 0 && !w.closed && ctx.Err() == nil {
			w.cond.Wait()
		}

		if ctx.Err() != nil {
			w.mu.Unlock()
			return ctx.Err()
		}
		if w.closed {
			w.mu.Unlock()
			return nil
		}

		// Pop next frame
		frame := w.queue[0]
		w.queue = w.queue[1:]
		w.queuedBytes -= len(frame.Samples)
		w.queuedDur -= frame.Duration
		w.inflight = frame
		w.dropInflight = false
		abortCh := w.abortCh
		activeGen := w.activeGen

		var waitDur time.Duration
		if w.cfg.EnablePacing && frame.Duration > 0 {
			now := time.Now()
			if w.lastMediaEnd.IsZero() || now.Sub(w.lastMediaEnd) > 100*time.Millisecond {
				// Gap or first frame: schedule from now
				w.lastMediaEnd = now.Add(frame.Duration)
			} else {
				// Consecutive frames: schedule smoothly against media clock
				waitDur = time.Until(w.lastMediaEnd)
				w.lastMediaEnd = w.lastMediaEnd.Add(frame.Duration)
			}
		}
		w.mu.Unlock()

		dropFrame := func() bool {
			w.mu.Lock()
			defer w.mu.Unlock()
			drop := w.dropInflight || (frame.Gen != 0 && frame.Gen < w.activeGen)
			if w.inflight == frame {
				w.inflight = nil
				w.dropInflight = false
			}
			return drop
		}

		if frame.Gen != 0 && frame.Gen < activeGen {
			w.FramesDropped.Add(1)
			_ = dropFrame()
			continue
		}

		// Media clock pacing: wait until media time arrives. Flush closes abortCh.
		if waitDur > 0 {
			timer := time.NewTimer(waitDur)
			select {
			case <-timer.C:
			case <-abortCh:
				timer.Stop()
			case <-ctx.Done():
				timer.Stop()
				_ = dropFrame()
				return ctx.Err()
			}
		}

		if dropFrame() {
			w.FramesDropped.Add(1)
			continue
		}

		// Encode audio to protobuf
		wireBytes, err := EncodeAudioFrame(frame.Samples, frame.SampleRate, frame.Channels)
		if err != nil {
			if w.OnWriteComplete != nil {
				w.OnWriteComplete(frame, err)
			}
			return err
		}

		w.mu.Lock()
		if w.dropInflight || (frame.Gen != 0 && frame.Gen < w.activeGen) {
			w.mu.Unlock()
			w.FramesDropped.Add(1)
			continue
		}
		writeCtx, writeCancel := context.WithTimeout(ctx, w.cfg.WriteTimeout)
		doneCh := make(chan struct{})
		w.inWrite = true
		w.writeGen = frame.Gen
		w.writeCancel = writeCancel
		w.writeDoneCh = doneCh
		w.mu.Unlock()

		err = w.writeFn(writeCtx, wireBytes)
		writeCancel()

		w.mu.Lock()
		w.inWrite = false
		w.writeCancel = nil
		w.writeDoneCh = nil
		close(doneCh)
		w.mu.Unlock()

		if err != nil {
			if errors.Is(err, context.Canceled) {
				w.FramesDropped.Add(1)
				continue
			}
			if errors.Is(err, context.DeadlineExceeded) {
				err = fmt.Errorf("%w: write deadline exceeded (%v)", ErrSlowClient, w.cfg.WriteTimeout)
			}
			if w.OnWriteComplete != nil {
				w.OnWriteComplete(frame, err)
			}
			return err
		}

		w.FramesWritten.Add(1)
		if w.OnWriteComplete != nil {
			w.OnWriteComplete(frame, nil)
		}
	}
}

// Close gracefully closes the writer and unblocks waiting goroutines.
func (w *PacedWriter) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	w.cond.Broadcast()
}

// QueuedDuration returns the total audio duration currently waiting in queue.
func (w *PacedWriter) QueuedDuration() time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.queuedDur
}

// QueuedBytes returns the total audio bytes currently waiting in queue.
func (w *PacedWriter) QueuedBytes() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.queuedBytes
}
