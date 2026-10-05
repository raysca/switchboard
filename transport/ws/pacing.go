package ws

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"govoice/core"
)

var (
	ErrSlowClient       = errors.New("ws: client buffer overflow or write deadline breach (slow client)")
	ErrWriterClosed     = errors.New("ws: paced writer closed")
	ErrQueueBufferFull  = errors.New("ws: output audio queue budget exceeded")
	ErrFinalMarkerLimit = errors.New("ws: pending whole-turn final marker limit exceeded")
)

type PacerConfig struct {
	MaxQueueDuration time.Duration
	MaxQueueBytes    int
	WriteTimeout     time.Duration
	EnablePacing     bool
}

func DefaultPacerConfig() PacerConfig {
	return PacerConfig{MaxQueueDuration: 100 * time.Millisecond, MaxQueueBytes: 4800, WriteTimeout: 2 * time.Second, EnablePacing: true}
}

type pacedFrame struct {
	frame    *core.OutputAudioFrame
	bytes    int
	duration time.Duration
}

type turnFinalKey struct {
	turn   string
	gen    uint64
	unitID uint64
}

const maxPendingTurnFinals = 64

// PacedWriter is the single output pacing implementation. It owns immutable
// queue entries, strict accounting, flush barriers, and speaking transitions.
type PacedWriter struct {
	cfg     PacerConfig
	writeFn func(context.Context, []byte) error

	mu            sync.Mutex
	cond          *sync.Cond
	queue         []*pacedFrame
	queuedBytes   int
	queuedDur     time.Duration
	activeGen     uint64
	inflight      *pacedFrame
	dropInflight  bool
	inflightAbort chan struct{}
	inflightDone  chan struct{}
	inWrite       bool
	writeCancel   context.CancelFunc
	closed        bool
	lastMediaEnd  time.Time
	pendingFinals map[turnFinalKey]struct{}

	speaking *core.SpeakingTracker
	eventMu  sync.Mutex
	events   []core.SpeakingStateFrame
	dispatch bool

	FramesWritten atomic.Uint64
	FramesDropped atomic.Uint64
	FlushesDone   atomic.Uint64

	OnWriteComplete func(*core.OutputAudioFrame, error)
	OnSpeakingState func(core.SpeakingStateFrame)
}

func NewPacedWriter(cfg PacerConfig, writeFn func(context.Context, []byte) error) *PacedWriter {
	w := &PacedWriter{cfg: cfg, writeFn: writeFn, queue: make([]*pacedFrame, 0, 32), pendingFinals: make(map[turnFinalKey]struct{}), speaking: core.NewSpeakingTracker()}
	w.cond = sync.NewCond(&w.mu)
	return w
}

// MarkTurnFinal records bounded whole-turn finality for a synthesis unit that
// was already streamed before the LLM completion boundary was known.
func (w *PacedWriter) MarkTurnFinal(turn string, gen, unitID uint64) error {
	key := turnFinalKey{turn: turn, gen: gen, unitID: unitID}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ErrWriterClosed
	}
	if gen != 0 && gen < w.activeGen {
		return nil
	}
	if _, exists := w.pendingFinals[key]; exists {
		return nil
	}
	if len(w.pendingFinals) >= maxPendingTurnFinals {
		return ErrFinalMarkerLimit
	}
	w.pendingFinals[key] = struct{}{}
	return nil
}

func (w *PacedWriter) SetActiveGeneration(gen uint64) {
	w.mu.Lock()
	if gen > w.activeGen {
		w.activeGen = gen
	}
	w.mu.Unlock()
}

// Enqueue copies and normalizes a frame before applying both budgets. A
// rejected enqueue leaves accounting and retained frames unchanged.
func (w *PacedWriter) Enqueue(f *core.OutputAudioFrame) error {
	entry, err := normalizePacedFrame(f)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ErrWriterClosed
	}
	if staleInterruptible(entry.frame, w.activeGen) {
		w.FramesDropped.Add(1)
		return nil
	}
	key := turnFinalKey{turn: entry.frame.Turn, gen: entry.frame.Gen, unitID: entry.frame.UnitID}
	if entry.frame.IsFinal {
		if _, marked := w.pendingFinals[key]; marked {
			entry.frame.TurnFinal = true
			delete(w.pendingFinals, key)
		}
	}
	nextBytes := w.queuedBytes + entry.bytes
	nextDuration := w.queuedDur + entry.duration
	if nextBytes < w.queuedBytes || nextDuration < w.queuedDur ||
		(w.cfg.MaxQueueBytes >= 0 && nextBytes > w.cfg.MaxQueueBytes) ||
		(w.cfg.MaxQueueDuration >= 0 && nextDuration > w.cfg.MaxQueueDuration) {
		return fmt.Errorf("%w: %w: queue duration %v > max %v or bytes %d > max %d", ErrSlowClient, ErrQueueBufferFull, nextDuration, w.cfg.MaxQueueDuration, nextBytes, w.cfg.MaxQueueBytes)
	}
	w.queue = append(w.queue, entry)
	w.queuedBytes = nextBytes
	w.queuedDur = nextDuration
	w.cond.Signal()
	return nil
}

func normalizePacedFrame(f *core.OutputAudioFrame) (*pacedFrame, error) {
	if f == nil {
		return nil, errors.New("ws: nil output audio frame")
	}
	clone := *f
	clone.Samples = append([]byte(nil), f.Samples...)
	n := len(clone.Samples)
	duration := clone.Duration
	if n == 0 {
		duration = 0
	} else if duration <= 0 {
		if clone.SampleRate <= 0 || clone.Channels <= 0 || n%(2*clone.Channels) != 0 {
			return nil, fmt.Errorf("ws: cannot derive PCM16 duration from %d bytes, rate %d, channels %d", n, clone.SampleRate, clone.Channels)
		}
		duration = time.Duration(n/(2*clone.Channels)) * time.Second / time.Duration(clone.SampleRate)
	}
	if duration < 0 {
		return nil, errors.New("ws: negative output duration")
	}
	clone.Duration = duration
	return &pacedFrame{frame: &clone, bytes: n, duration: duration}, nil
}

func staleInterruptible(f *core.OutputAudioFrame, active uint64) bool {
	return f.Gen != 0 && f.Gen < active && f.Disposition != core.OutputUninterruptible
}

// BeginFlush acknowledges asynchronously only after targeted interruptible
// in-flight work has quiesced. Repeated calls retain their own acknowledgements.
func (w *PacedWriter) BeginFlush(gen uint64, done func(error)) {
	wait, cancel, abort, dispatch := w.dropGeneration(gen)
	if abort != nil {
		close(abort)
	}
	if cancel != nil {
		cancel()
	}
	if dispatch {
		w.dispatchSpeaking()
	}
	if done == nil {
		return
	}
	if wait == nil {
		go safeFlushCallback(done, nil)
		return
	}
	go func() {
		timer := time.NewTimer(w.flushTimeout())
		defer timer.Stop()
		select {
		case <-wait:
			safeFlushCallback(done, nil)
		case <-timer.C:
			safeFlushCallback(done, fmt.Errorf("%w: flush timed out waiting for in-flight write cancellation", ErrSlowClient))
		}
	}()
}

func (w *PacedWriter) Flush(ctx context.Context, gen uint64) error {
	wait, cancel, abort, dispatch := w.dropGeneration(gen)
	if abort != nil {
		close(abort)
	}
	if cancel != nil {
		cancel()
	}
	if dispatch {
		w.dispatchSpeaking()
	}
	if wait == nil {
		return nil
	}
	timer := time.NewTimer(w.flushTimeout())
	defer timer.Stop()
	select {
	case <-wait:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return fmt.Errorf("%w: flush timed out waiting for in-flight write cancellation", ErrSlowClient)
	}
}

func (w *PacedWriter) flushTimeout() time.Duration {
	if w.cfg.WriteTimeout > 0 && w.cfg.WriteTimeout < 200*time.Millisecond {
		return w.cfg.WriteTimeout
	}
	return 200 * time.Millisecond
}

func (w *PacedWriter) dropGeneration(gen uint64) (<-chan struct{}, context.CancelFunc, chan struct{}, bool) {
	w.mu.Lock()
	kept := w.queue[:0]
	for _, entry := range w.queue {
		if entry.frame.Gen <= gen && entry.frame.Disposition != core.OutputUninterruptible {
			w.queuedBytes -= entry.bytes
			w.queuedDur -= entry.duration
			w.FramesDropped.Add(1)
			continue
		}
		kept = append(kept, entry)
	}
	w.queue = kept
	for key := range w.pendingFinals {
		if key.gen <= gen {
			delete(w.pendingFinals, key)
		}
	}

	var wait <-chan struct{}
	var cancel context.CancelFunc
	var abort chan struct{}
	if entry := w.inflight; entry != nil && entry.frame.Gen <= gen && entry.frame.Disposition != core.OutputUninterruptible {
		if !w.dropInflight {
			w.dropInflight = true
			abort = w.inflightAbort
			w.inflightAbort = nil
		}
		wait = w.inflightDone
		if w.inWrite {
			cancel = w.writeCancel
		}
	}
	if gen < math.MaxUint64 && gen+1 > w.activeGen {
		w.activeGen = gen + 1
	}
	w.lastMediaEnd = time.Time{}
	w.FlushesDone.Add(1)
	w.cond.Broadcast()
	dispatch := false
	if event, ok := w.speaking.StopThrough(gen, core.SpeakingStopFlushed); ok {
		dispatch = w.queueSpeakingLocked(event)
	}
	w.mu.Unlock()
	return wait, cancel, abort, dispatch
}

// Run paces and writes until cancellation or close. No queue lock is held
// during waits, encoding, writes, or callbacks.
func (w *PacedWriter) Run(ctx context.Context) error {
	wakeDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			w.mu.Lock()
			w.cond.Broadcast()
			w.mu.Unlock()
		case <-wakeDone:
		}
	}()
	defer close(wakeDone)

	for {
		entry, waitDuration, mediaEnd, abort, err := w.next(ctx)
		if err != nil {
			return err
		}
		if entry == nil {
			return nil
		}
		if waitDuration > 0 {
			timer := time.NewTimer(waitDuration)
			select {
			case <-timer.C:
			case <-abort:
				stopAndDrainTimer(timer)
			case <-ctx.Done():
				stopAndDrainTimer(timer)
				w.finishDropped(entry)
				return ctx.Err()
			}
		}
		if w.dropBeforeWrite(entry) {
			continue
		}

		if entry.bytes == 0 {
			w.safeWriteComplete(entry.frame, nil)
			w.finishFinalDrain(ctx, entry, mediaEnd, abort)
			continue
		}
		wireBytes, err := EncodeAudioFrame(entry.frame.Samples, entry.frame.SampleRate, entry.frame.Channels)
		if err != nil {
			dispatch := w.finishFatal(entry, core.SpeakingStopWriteError)
			if dispatch {
				w.dispatchSpeaking()
			}
			w.safeWriteComplete(entry.frame, err)
			return err
		}
		if w.dropBeforeWrite(entry) {
			continue
		}

		writeCtx, writeCancel := w.beginWrite(ctx, entry)
		err = w.callWrite(writeCtx, wireBytes)
		writeCancel()
		dropped, dispatch := w.finishWrite(entry, err)
		if dispatch {
			w.dispatchSpeaking()
		}
		if dropped {
			continue
		}
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				err = fmt.Errorf("%w: write deadline exceeded (%v)", ErrSlowClient, w.cfg.WriteTimeout)
			}
			w.safeWriteComplete(entry.frame, err)
			return err
		}
		w.FramesWritten.Add(1)
		w.safeWriteComplete(entry.frame, nil)
		if entry.frame.TurnFinal {
			w.finishFinalDrain(ctx, entry, mediaEnd, abort)
		}
	}
}

func (w *PacedWriter) next(ctx context.Context) (*pacedFrame, time.Duration, time.Time, chan struct{}, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for len(w.queue) == 0 && !w.closed && ctx.Err() == nil {
		w.cond.Wait()
	}
	if ctx.Err() != nil {
		return nil, 0, time.Time{}, nil, ctx.Err()
	}
	if w.closed {
		return nil, 0, time.Time{}, nil, nil
	}
	entry := w.queue[0]
	w.queue = w.queue[1:]
	w.queuedBytes -= entry.bytes
	w.queuedDur -= entry.duration
	w.inflight = entry
	w.dropInflight = false
	w.inflightAbort = make(chan struct{})
	w.inflightDone = make(chan struct{})
	abort := w.inflightAbort

	now := time.Now()
	mediaEnd := w.lastMediaEnd
	var waitDuration time.Duration
	if w.cfg.EnablePacing {
		if mediaEnd.IsZero() || now.Sub(mediaEnd) > 100*time.Millisecond {
			mediaEnd = now
		}
		if mediaEnd.After(now) {
			waitDuration = time.Until(mediaEnd)
		}
		mediaEnd = mediaEnd.Add(entry.duration)
		w.lastMediaEnd = mediaEnd
	}
	return entry, waitDuration, mediaEnd, abort, nil
}

func (w *PacedWriter) dropBeforeWrite(entry *pacedFrame) bool {
	w.mu.Lock()
	drop := w.inflight != entry || w.dropInflight || staleInterruptible(entry.frame, w.activeGen)
	if drop {
		w.clearInflightLocked(entry)
		w.FramesDropped.Add(1)
	}
	w.mu.Unlock()
	return drop
}

func (w *PacedWriter) beginWrite(ctx context.Context, entry *pacedFrame) (context.Context, context.CancelFunc) {
	var writeCtx context.Context
	var cancel context.CancelFunc
	if w.cfg.WriteTimeout > 0 {
		writeCtx, cancel = context.WithTimeout(ctx, w.cfg.WriteTimeout)
	} else {
		writeCtx, cancel = context.WithCancel(ctx)
	}
	w.mu.Lock()
	if w.inflight == entry {
		w.inWrite = true
		w.writeCancel = cancel
	}
	w.mu.Unlock()
	return writeCtx, cancel
}

func (w *PacedWriter) callWrite(ctx context.Context, data []byte) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("ws: writer panic: %v", recovered)
		}
	}()
	if w.writeFn == nil {
		return errors.New("ws: nil writer")
	}
	return w.writeFn(ctx, data)
}

func (w *PacedWriter) finishWrite(entry *pacedFrame, writeErr error) (bool, bool) {
	w.mu.Lock()
	w.inWrite = false
	w.writeCancel = nil
	dropped := w.inflight != entry || w.dropInflight || staleInterruptible(entry.frame, w.activeGen)
	dispatch := false
	if dropped {
		w.clearInflightLocked(entry)
		w.FramesDropped.Add(1)
	} else if writeErr != nil {
		w.clearInflightLocked(entry)
		if event, ok := w.speaking.Stop(core.SpeakingStopWriteError); ok {
			dispatch = w.queueSpeakingLocked(event)
		}
	} else {
		if event, ok := w.speaking.ObserveWrite(entry.frame.Turn, entry.frame.Gen, entry.frame.Disposition); ok {
			dispatch = w.queueSpeakingLocked(event)
		}
		if !entry.frame.TurnFinal {
			w.clearInflightLocked(entry)
		}
	}
	w.mu.Unlock()
	return dropped, dispatch
}

func (w *PacedWriter) finishFatal(entry *pacedFrame, reason core.SpeakingStopReason) bool {
	w.mu.Lock()
	w.clearInflightLocked(entry)
	dispatch := false
	if event, ok := w.speaking.Stop(reason); ok {
		dispatch = w.queueSpeakingLocked(event)
	}
	w.mu.Unlock()
	return dispatch
}

func (w *PacedWriter) finishDropped(entry *pacedFrame) {
	w.mu.Lock()
	if w.inflight == entry {
		w.clearInflightLocked(entry)
		w.FramesDropped.Add(1)
	}
	w.mu.Unlock()
}

func (w *PacedWriter) finishFinalDrain(ctx context.Context, entry *pacedFrame, mediaEnd time.Time, abort <-chan struct{}) {
	if entry.frame.TurnFinal && w.cfg.EnablePacing {
		if wait := time.Until(mediaEnd); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-abort:
				stopAndDrainTimer(timer)
			case <-ctx.Done():
				stopAndDrainTimer(timer)
			}
		}
	}
	w.mu.Lock()
	dropped := w.inflight != entry || w.dropInflight || staleInterruptible(entry.frame, w.activeGen)
	w.clearInflightLocked(entry)
	dispatch := false
	if !dropped && entry.frame.TurnFinal {
		if event, ok := w.speaking.Stop(core.SpeakingStopDrained); ok {
			dispatch = w.queueSpeakingLocked(event)
		}
	}
	w.mu.Unlock()
	if dispatch {
		w.dispatchSpeaking()
	}
}

func stopAndDrainTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func (w *PacedWriter) clearInflightLocked(entry *pacedFrame) {
	if w.inflight != entry {
		return
	}
	done := w.inflightDone
	w.inflight = nil
	w.dropInflight = false
	w.inflightAbort = nil
	w.inflightDone = nil
	w.inWrite = false
	w.writeCancel = nil
	if done != nil {
		close(done)
	}
}

func (w *PacedWriter) Close() {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	for range w.queue {
		w.FramesDropped.Add(1)
	}
	w.queue = nil
	w.queuedBytes = 0
	w.queuedDur = 0
	clear(w.pendingFinals)
	var abort chan struct{}
	var cancel context.CancelFunc
	if w.inflight != nil {
		w.dropInflight = true
		abort = w.inflightAbort
		w.inflightAbort = nil
		if w.inWrite {
			cancel = w.writeCancel
		}
	}
	dispatch := false
	if event, ok := w.speaking.Stop(core.SpeakingStopClosed); ok {
		dispatch = w.queueSpeakingLocked(event)
	}
	w.cond.Broadcast()
	w.mu.Unlock()
	if abort != nil {
		close(abort)
	}
	if cancel != nil {
		cancel()
	}
	if dispatch {
		w.dispatchSpeaking()
	}
}

// queueSpeakingLocked queues an event without invoking user code. The caller
// holds the pacing mutex so state changes are ordered with flush/write fences.
func (w *PacedWriter) queueSpeakingLocked(event core.SpeakingStateFrame) bool {
	w.eventMu.Lock()
	w.events = append(w.events, event)
	if w.dispatch {
		w.eventMu.Unlock()
		return false
	}
	w.dispatch = true
	w.eventMu.Unlock()
	return true
}

func (w *PacedWriter) dispatchSpeaking() {
	for {
		w.eventMu.Lock()
		if len(w.events) == 0 {
			w.dispatch = false
			w.eventMu.Unlock()
			return
		}
		event := w.events[0]
		w.events = w.events[1:]
		callback := w.OnSpeakingState
		w.eventMu.Unlock()
		if callback != nil {
			func() {
				defer func() { _ = recover() }()
				callback(event)
			}()
		}
	}
}

func (w *PacedWriter) safeWriteComplete(frame *core.OutputAudioFrame, err error) {
	callback := w.OnWriteComplete
	if callback == nil {
		return
	}
	defer func() { _ = recover() }()
	callback(frame, err)
}

func safeFlushCallback(callback func(error), err error) {
	defer func() { _ = recover() }()
	callback(err)
}

func (w *PacedWriter) QueuedDuration() time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.queuedDur
}

func (w *PacedWriter) QueuedBytes() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.queuedBytes
}
