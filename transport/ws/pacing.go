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
	ErrInvalidUnitID    = errors.New("ws: finality unit ID must be nonzero and session-monotonic")
	ErrActiveUnitLimit  = errors.New("ws: active synthesis unit identity limit exceeded")
)

type PacerConfig struct {
	MaxQueueDuration time.Duration
	MaxQueueBytes    int
	MaxQueueFrames   int
	WriteTimeout     time.Duration
	EnablePacing     bool
}

func DefaultPacerConfig() PacerConfig {
	// The lookahead must cover a TTS burst: enqueueOutput runs inline on the session
	// run loop, so a full queue stalls caller audio and VAD results (stale VAD chunks).
	// 2 s of 24 kHz PCM16 mono; a flush drops it all, so barge-in is unaffected.
	return PacerConfig{MaxQueueDuration: 2 * time.Second, MaxQueueBytes: 96000, MaxQueueFrames: 512, WriteTimeout: 2 * time.Second, EnablePacing: true}
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

type completedUnit struct {
	mediaEnd    time.Time
	disposition core.OutputDisposition
}

type finalDrain struct {
	key         turnFinalKey
	gen         uint64
	disposition core.OutputDisposition
	mediaEnd    time.Time
	abort       chan struct{}
	aborted     bool
	dropped     bool
}

const (
	maxPendingTurnFinals    = 64
	maxActiveUnitIdentities = 128
)

// PacedWriter is the single output pacing implementation. It owns immutable
// queue entries, strict accounting, flush barriers, and speaking transitions.
type PacedWriter struct {
	cfg     PacerConfig
	writeFn func(context.Context, []byte) error

	mu              sync.Mutex
	cond            *sync.Cond
	queue           []*pacedFrame
	queuedBytes     int
	queuedDur       time.Duration
	activeGen       uint64
	inflight        *pacedFrame
	dropInflight    bool
	inflightAbort   chan struct{}
	inflightDone    chan struct{}
	inWrite         bool
	writeCancel     context.CancelFunc
	closed          bool
	lastMediaEnd    time.Time
	pendingFinals   map[turnFinalKey]struct{}
	completedFinals map[turnFinalKey]completedUnit
	// Admission exceptions exist only until a unit's final chunk completes.
	// completedFinals retains late-marker metadata without reopening admission.
	activeUnits    map[turnFinalKey]core.OutputDisposition
	highestUnitID  uint64
	highestUnitKey turnFinalKey
	retiredUnitID  uint64
	drain          *finalDrain
	terminalErr    error
	room           chan struct{} // closed when queued audio shrinks; see WaitRoom

	speaking *core.SpeakingTracker
	eventMu  sync.Mutex
	events   []core.SpeakingStateFrame
	dispatch bool

	FramesWritten atomic.Uint64
	FramesDropped atomic.Uint64

	// Underlay, when set before Run, is mixed under every released speech
	// frame and emitted alone, as 20 ms NonSpeech frames on the pacer clock,
	// while the queue is empty (background ambience). Ambience frames are
	// Uninterruptible (flush never drops them), never reach OnWriteComplete
	// (recording and heard-text ignore them) and never change speaking state.
	// Requires EnablePacing; ignored otherwise. Driven only by Run.
	Underlay      Underlay
	AmbientFrames atomic.Uint64
	amb           *pacedFrame
	mixBuf        []byte

	OnWriteComplete func(*core.OutputAudioFrame, error)
	OnSpeakingState func(core.SpeakingStateFrame)

	// Test synchronization hooks. Production leaves these nil.
	afterSelect          func()
	beforeWriteAdmission func()
}

// Underlay is a background audio source mixed by the paced writer. Rate must
// equal the wire rate. Mix fills dst with src+ambience (src nil: ambience
// only); Active reports whether it still has audio (a finished one-shot does
// not keep the writer emitting silence).
type Underlay interface {
	Rate() int
	Active() bool
	Mix(dst, src []byte)
}

const ambientFrame = 20 * time.Millisecond

func NewPacedWriter(cfg PacerConfig, writeFn func(context.Context, []byte) error) *PacedWriter {
	defaults := DefaultPacerConfig()
	if cfg.MaxQueueDuration <= 0 {
		cfg.MaxQueueDuration = defaults.MaxQueueDuration
	}
	if cfg.MaxQueueBytes <= 0 {
		cfg.MaxQueueBytes = defaults.MaxQueueBytes
	}
	if cfg.MaxQueueFrames <= 0 {
		cfg.MaxQueueFrames = defaults.MaxQueueFrames
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = defaults.WriteTimeout
	}
	w := &PacedWriter{
		cfg: cfg, writeFn: writeFn, queue: make([]*pacedFrame, 0, 32),
		pendingFinals: make(map[turnFinalKey]struct{}), completedFinals: make(map[turnFinalKey]completedUnit),
		activeUnits: make(map[turnFinalKey]core.OutputDisposition),
		speaking:    core.NewSpeakingTracker(),
	}
	w.cond = sync.NewCond(&w.mu)
	return w
}

// MarkTurnFinal records bounded whole-turn finality for a synthesis unit that
// was already streamed before the LLM completion boundary was known.
func (w *PacedWriter) MarkTurnFinal(turn string, gen, unitID uint64) error {
	key := turnFinalKey{turn: turn, gen: gen, unitID: unitID}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return ErrWriterClosed
	}
	if unitID == 0 {
		w.mu.Unlock()
		return ErrInvalidUnitID
	}
	for _, entry := range w.queue {
		if frameKey(entry.frame) == key && entry.frame.IsFinal {
			entry.frame.TurnFinal = true
			delete(w.pendingFinals, key)
			w.mu.Unlock()
			return nil
		}
	}
	if w.inflight != nil && frameKey(w.inflight.frame) == key && w.inflight.frame.IsFinal {
		w.inflight.frame.TurnFinal = true
		delete(w.pendingFinals, key)
		w.mu.Unlock()
		return nil
	}
	if w.drain != nil && w.drain.key == key {
		w.mu.Unlock()
		return nil
	}
	if completed, ok := w.completedFinals[key]; ok {
		delete(w.completedFinals, key)
		if w.drain != nil && key.unitID < w.drain.key.unitID {
			delete(w.activeUnits, key)
			w.retireUnitLocked(key)
			w.mu.Unlock()
			return nil
		}
		drain := &finalDrain{key: key, gen: gen, disposition: completed.disposition, mediaEnd: completed.mediaEnd, abort: make(chan struct{})}
		var oldAbort chan struct{}
		if w.drain != nil {
			oldAbort = w.retireDrainLocked(w.drain)
		}
		w.drain = drain
		w.mu.Unlock()
		if oldAbort != nil {
			close(oldAbort)
		}
		go w.awaitFinalDrain(context.Background(), drain)
		return nil
	}
	activeDisposition, active := w.activeUnits[key]
	retainedCritical := active && activeDisposition == core.OutputUninterruptible
	if gen != 0 && gen < w.activeGen && !retainedCritical {
		w.mu.Unlock()
		return nil
	}
	if !retainedCritical && unitID <= w.retiredUnitID {
		w.mu.Unlock()
		return nil
	}
	if !retainedCritical && (unitID < w.highestUnitID || (unitID == w.highestUnitID && w.highestUnitID != 0 && key != w.highestUnitKey)) {
		w.mu.Unlock()
		return ErrInvalidUnitID
	}
	if _, exists := w.pendingFinals[key]; exists {
		w.mu.Unlock()
		return nil
	}
	if len(w.pendingFinals) >= maxPendingTurnFinals {
		w.mu.Unlock()
		return ErrFinalMarkerLimit
	}
	w.pendingFinals[key] = struct{}{}
	w.mu.Unlock()
	return nil
}

func frameKey(frame *core.OutputAudioFrame) turnFinalKey {
	return turnFinalKey{turn: frame.Turn, gen: frame.Gen, unitID: frame.UnitID}
}

func (w *PacedWriter) acceptUnitIdentityLocked(key turnFinalKey) error {
	if key.unitID == 0 {
		return ErrInvalidUnitID
	}
	if key.unitID <= w.retiredUnitID {
		return ErrInvalidUnitID
	}
	if key.unitID < w.highestUnitID || (key.unitID == w.highestUnitID && w.highestUnitID != 0 && key != w.highestUnitKey) {
		return ErrInvalidUnitID
	}
	if key.unitID > w.highestUnitID {
		w.highestUnitID = key.unitID
		w.highestUnitKey = key
	}
	return nil
}

func (w *PacedWriter) retireUnitLocked(key turnFinalKey) {
	delete(w.activeUnits, key)
	if key.unitID > w.retiredUnitID {
		w.retiredUnitID = key.unitID
	}
}

func (w *PacedWriter) recordCompletedLocked(key turnFinalKey, mediaEnd time.Time, disposition core.OutputDisposition) {
	if len(w.completedFinals) >= maxPendingTurnFinals {
		var oldest turnFinalKey
		for candidate := range w.completedFinals {
			if oldest.unitID == 0 || candidate.unitID < oldest.unitID {
				oldest = candidate
			}
		}
		delete(w.completedFinals, oldest)
		delete(w.activeUnits, oldest)
		w.retireUnitLocked(oldest)
	}
	w.completedFinals[key] = completedUnit{mediaEnd: mediaEnd, disposition: disposition}
}

func (w *PacedWriter) admitUnitLocked(key turnFinalKey, disposition core.OutputDisposition) error {
	if retained, ok := w.activeUnits[key]; ok {
		if retained != disposition {
			return ErrInvalidUnitID
		}
		return nil
	}
	if len(w.activeUnits) >= maxActiveUnitIdentities {
		return ErrActiveUnitLimit
	}
	if err := w.acceptUnitIdentityLocked(key); err != nil {
		return err
	}
	w.activeUnits[key] = disposition
	return nil
}

// WaitRoom blocks until the queue can take `need` more audio (duration and,
// at the queue's current byte rate, bytes), the writer closes, ctx ends or
// timeout passes. It is advisory: Enqueue still enforces the budgets.
func (w *PacedWriter) WaitRoom(ctx context.Context, need, timeout time.Duration) {
	var expired <-chan time.Time
	for {
		w.mu.Lock()
		fits := w.closed || need > w.cfg.MaxQueueDuration || w.queuedDur+need <= w.cfg.MaxQueueDuration
		if fits && !w.closed && w.queuedDur > 0 && w.cfg.MaxQueueBytes >= 0 {
			fits = w.queuedBytes+int(int64(w.queuedBytes)*int64(need)/int64(w.queuedDur)) <= w.cfg.MaxQueueBytes
		}
		if fits {
			w.mu.Unlock()
			return
		}
		if w.room == nil {
			w.room = make(chan struct{})
		}
		room := w.room
		w.mu.Unlock()
		if expired == nil {
			t := time.NewTimer(timeout)
			defer t.Stop()
			expired = t.C
		}
		select {
		case <-room:
		case <-ctx.Done():
			return
		case <-expired:
			return
		}
	}
}

func (w *PacedWriter) signalRoomLocked() {
	if w.room != nil {
		close(w.room)
		w.room = nil
	}
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
	nextBytes := w.queuedBytes + entry.bytes
	nextDuration := w.queuedDur + entry.duration
	if nextBytes < w.queuedBytes || nextDuration < w.queuedDur ||
		len(w.queue)+1 > w.cfg.MaxQueueFrames ||
		(w.cfg.MaxQueueBytes >= 0 && nextBytes > w.cfg.MaxQueueBytes) ||
		(w.cfg.MaxQueueDuration >= 0 && nextDuration > w.cfg.MaxQueueDuration) {
		return fmt.Errorf("%w: %w: queue duration %v > max %v or bytes %d > max %d", ErrSlowClient, ErrQueueBufferFull, nextDuration, w.cfg.MaxQueueDuration, nextBytes, w.cfg.MaxQueueBytes)
	}
	key := frameKey(entry.frame)
	if entry.frame.UnitID != 0 {
		if err := w.admitUnitLocked(key, entry.frame.Disposition); err != nil {
			return err
		}
	} else if entry.frame.IsFinal || entry.frame.TurnFinal {
		return ErrInvalidUnitID
	}
	if entry.frame.IsFinal {
		if _, marked := w.pendingFinals[key]; marked {
			entry.frame.TurnFinal = true
			delete(w.pendingFinals, key)
		}
	}
	w.queue = append(w.queue, entry)
	w.queuedBytes = nextBytes
	w.queuedDur = nextDuration
	if w.inflight != nil && w.inflight == w.amb && w.inflightAbort != nil {
		// An ambience frame is still waiting for its slot: give the slot to
		// speech. The cursor has not advanced (Mix runs after the wait).
		w.dropInflight = true
		close(w.inflightAbort)
		w.inflightAbort = nil
		w.lastMediaEnd = w.lastMediaEnd.Add(-ambientFrame)
	}
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
	wait, abort, dispatch := w.dropGeneration(gen)
	if abort != nil {
		close(abort)
	}
	if dispatch {
		w.dispatchSpeaking()
	}
	if done == nil {
		return
	}
	if wait == nil {
		go safeFlushCallback(done, w.flushResult())
		return
	}
	go func() {
		timer := time.NewTimer(w.flushTimeout())
		defer timer.Stop()
		select {
		case <-wait:
			safeFlushCallback(done, w.flushResult())
		case <-timer.C:
			err := fmt.Errorf("%w: flush timed out waiting for in-flight write cancellation", ErrSlowClient)
			w.closeWithError(err)
			safeFlushCallback(done, w.flushResult())
		}
	}()
}

// Flush is BeginFlush that waits for the acknowledgement or ctx.
func (w *PacedWriter) Flush(ctx context.Context, gen uint64) error {
	done := make(chan error, 1)
	w.BeginFlush(gen, func(err error) { done <- err })
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *PacedWriter) flushResult() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.terminalErr
}

func (w *PacedWriter) closeWithError(err error) {
	w.mu.Lock()
	if w.terminalErr == nil {
		w.terminalErr = err
	}
	w.mu.Unlock()
	w.Close()
}

func (w *PacedWriter) flushTimeout() time.Duration {
	if w.cfg.WriteTimeout > 0 && w.cfg.WriteTimeout < 200*time.Millisecond {
		return w.cfg.WriteTimeout
	}
	return 200 * time.Millisecond
}

func (w *PacedWriter) dropGeneration(gen uint64) (<-chan struct{}, chan struct{}, bool) {
	w.mu.Lock()
	scheduleChanged := false
	kept := w.queue[:0]
	for _, entry := range w.queue {
		if entry.frame.Gen <= gen && entry.frame.Disposition != core.OutputUninterruptible {
			w.queuedBytes -= entry.bytes
			w.queuedDur -= entry.duration
			w.signalRoomLocked()
			w.FramesDropped.Add(1)
			if entry.frame.IsFinal {
				delete(w.activeUnits, frameKey(entry.frame))
				w.retireUnitLocked(frameKey(entry.frame))
			}
			continue
		}
		kept = append(kept, entry)
	}
	w.queue = kept
	for key := range w.pendingFinals {
		disposition, active := w.activeUnits[key]
		if key.gen <= gen && (!active || disposition != core.OutputUninterruptible) {
			delete(w.pendingFinals, key)
			w.retireUnitLocked(key)
		}
	}
	for key, completed := range w.completedFinals {
		if key.gen <= gen && completed.disposition != core.OutputUninterruptible {
			delete(w.completedFinals, key)
			delete(w.activeUnits, key)
			w.retireUnitLocked(key)
		}
	}
	for key, disposition := range w.activeUnits {
		if key.gen <= gen && disposition != core.OutputUninterruptible {
			delete(w.activeUnits, key)
			w.retireUnitLocked(key)
		}
	}

	var wait <-chan struct{}
	var abort chan struct{}
	if entry := w.inflight; entry != nil && entry.frame.Gen <= gen && entry.frame.Disposition != core.OutputUninterruptible {
		if !w.dropInflight {
			w.dropInflight = true
			if !w.inWrite {
				abort = w.inflightAbort
				w.inflightAbort = nil
				scheduleChanged = true
			}
		}
		wait = w.inflightDone
	}
	if w.drain != nil && w.drain.gen <= gen && w.drain.disposition != core.OutputUninterruptible {
		w.drain.dropped = true
		if !w.drain.aborted {
			w.drain.aborted = true
			close(w.drain.abort)
		}
		scheduleChanged = true
		delete(w.activeUnits, w.drain.key)
		w.retireUnitLocked(w.drain.key)
	}
	if gen < math.MaxUint64 && gen+1 > w.activeGen {
		w.activeGen = gen + 1
	}
	if scheduleChanged {
		w.lastMediaEnd = time.Time{}
	}
	w.cond.Broadcast()
	dispatch := false
	if event, ok := w.speaking.StopThrough(gen, core.SpeakingStopFlushed); ok {
		dispatch = w.queueSpeakingLocked(event)
	}
	w.mu.Unlock()
	return wait, abort, dispatch
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
	defer w.Close()

	for {
		entry, waitDuration, mediaEnd, abort, err := w.next(ctx)
		if err != nil {
			return err
		}
		if entry == nil {
			return nil
		}
		if w.afterSelect != nil {
			w.afterSelect()
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
			dropped := w.finishEmpty(entry, mediaEnd)
			if !dropped && w.completionAllowed(entry) {
				w.safeWriteComplete(entry.frame, nil)
			}
			if entry.frame.TurnFinal {
				w.finishFinalDrain(ctx, entry, mediaEnd)
			}
			continue
		}
		wireBytes, err := EncodeAudioFrame(w.wireSamples(entry), entry.frame.SampleRate, entry.frame.Channels)
		if err != nil {
			dispatch := w.finishFatal(entry, core.SpeakingStopWriteError)
			if dispatch {
				w.dispatchSpeaking()
			}
			w.safeWriteComplete(entry.frame, err)
			return err
		}
		if w.beforeWriteAdmission != nil {
			w.beforeWriteAdmission()
		}
		writeCtx, writeCancel, admitted := w.beginWrite(ctx, entry)
		if !admitted {
			continue
		}
		err = w.callWrite(writeCtx, wireBytes)
		writeCancel()
		dropped, dispatch := w.finishWrite(entry, err, mediaEnd)
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
			if entry != w.amb {
				w.safeWriteComplete(entry.frame, err)
			}
			return err
		}
		if entry == w.amb {
			w.AmbientFrames.Add(1)
			continue
		}
		w.FramesWritten.Add(1)
		if w.completionAllowed(entry) {
			w.safeWriteComplete(entry.frame, nil)
		}
		if entry.frame.TurnFinal {
			w.finishFinalDrain(ctx, entry, mediaEnd)
		}
	}
}

// wake nudges an idle Run loop to re-check the underlay (a cue just started).
func (w *PacedWriter) wake() {
	w.mu.Lock()
	w.cond.Broadcast()
	w.mu.Unlock()
}

func (w *PacedWriter) next(ctx context.Context) (*pacedFrame, time.Duration, time.Time, chan struct{}, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for len(w.queue) == 0 && !w.closed && ctx.Err() == nil && !w.ambientLocked() {
		w.cond.Wait()
	}
	if ctx.Err() != nil {
		return nil, 0, time.Time{}, nil, ctx.Err()
	}
	if w.closed {
		return nil, 0, time.Time{}, nil, nil
	}
	var entry *pacedFrame
	if len(w.queue) == 0 {
		entry = w.amb // ambientLocked guaranteed the underlay is live
	} else {
		entry = w.queue[0]
		w.queue = w.queue[1:]
		w.queuedBytes -= entry.bytes
		w.queuedDur -= entry.duration
		w.signalRoomLocked()
	}
	w.inflight = entry
	w.dropInflight = false
	w.inflightAbort = make(chan struct{})
	w.inflightDone = make(chan struct{})
	abort := w.inflightAbort

	now := time.Now()
	mediaEnd := w.lastMediaEnd
	var waitDuration time.Duration
	if w.cfg.EnablePacing {
		late := 100 * time.Millisecond
		if entry == w.amb {
			late = ambientFrame // a stalled socket skips ambience, never bursts it to catch up
		}
		if mediaEnd.IsZero() || now.Sub(mediaEnd) > late {
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

// ambientLocked reports whether an ambience frame can fill an empty queue.
func (w *PacedWriter) ambientLocked() bool {
	if w.Underlay == nil || !w.cfg.EnablePacing || !w.Underlay.Active() {
		return false
	}
	if w.amb == nil {
		n := w.Underlay.Rate() / 50 * 2
		w.amb = &pacedFrame{bytes: n, duration: ambientFrame, frame: &core.OutputAudioFrame{
			SampleRate: w.Underlay.Rate(), Channels: 1, Duration: ambientFrame, NonSpeech: true,
			Disposition: core.OutputUninterruptible}}
	}
	return true
}

// wireSamples returns the bytes to encode: the frame's own samples, or the
// underlay mix in a reused buffer (the queued frame, which the recording tap
// sees, is never modified).
func (w *PacedWriter) wireSamples(entry *pacedFrame) []byte {
	u, f := w.Underlay, entry.frame
	if u == nil || (entry != w.amb && (!u.Active() || f.SampleRate != u.Rate() || f.Channels != 1 || len(f.Samples)%2 != 0 ||
		(f.Encoding != core.EncodingPCM16LE && f.Encoding != ""))) {
		return f.Samples
	}
	if cap(w.mixBuf) < entry.bytes {
		w.mixBuf = make([]byte, entry.bytes)
	}
	buf := w.mixBuf[:entry.bytes]
	if entry == w.amb {
		u.Mix(buf, nil)
	} else {
		u.Mix(buf, f.Samples)
	}
	return buf
}

func (w *PacedWriter) dropBeforeWrite(entry *pacedFrame) bool {
	w.mu.Lock()
	drop := w.inflight != entry || w.dropInflight || staleInterruptible(entry.frame, w.activeGen)
	if drop {
		w.clearInflightLocked(entry)
		if entry != w.amb {
			w.FramesDropped.Add(1)
		}
	}
	w.mu.Unlock()
	return drop
}

func (w *PacedWriter) beginWrite(ctx context.Context, entry *pacedFrame) (context.Context, context.CancelFunc, bool) {
	var writeCtx context.Context
	var cancel context.CancelFunc
	if w.cfg.WriteTimeout > 0 {
		writeCtx, cancel = context.WithTimeout(ctx, w.cfg.WriteTimeout)
	} else {
		writeCtx, cancel = context.WithCancel(ctx)
	}
	w.mu.Lock()
	if w.closed || w.inflight != entry || w.dropInflight || staleInterruptible(entry.frame, w.activeGen) {
		w.clearInflightLocked(entry)
		w.FramesDropped.Add(1)
		w.mu.Unlock()
		cancel()
		return nil, func() {}, false
	}
	w.inWrite = true
	w.writeCancel = cancel
	w.inflightAbort = nil
	w.mu.Unlock()
	return writeCtx, cancel, true
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

func (w *PacedWriter) finishWrite(entry *pacedFrame, writeErr error, mediaEnd time.Time) (bool, bool) {
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
		if entry.frame.NonSpeech {
			// DTMF tones are not bot speech.
		} else if event, ok := w.speaking.ObserveWrite(entry.frame.Turn, entry.frame.Gen, entry.frame.Disposition, entry.frame.UnitID); ok {
			dispatch = w.queueSpeakingLocked(event)
		}
		key := frameKey(entry.frame)
		if entry.frame.IsFinal || entry.frame.TurnFinal {
			w.retireUnitLocked(key)
		}
		if entry.frame.IsFinal && !entry.frame.TurnFinal {
			w.recordCompletedLocked(key, mediaEnd, entry.frame.Disposition)
		}
		w.clearInflightLocked(entry)
		if entry.frame.TurnFinal {
			if w.drain != nil {
				if abort := w.retireDrainLocked(w.drain); abort != nil {
					close(abort)
				}
			}
			w.drain = &finalDrain{key: key, gen: entry.frame.Gen, disposition: entry.frame.Disposition, mediaEnd: mediaEnd, abort: make(chan struct{})}
		}
	}
	w.mu.Unlock()
	return dropped, dispatch
}

func (w *PacedWriter) completionAllowed(entry *pacedFrame) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return !w.closed && !staleInterruptible(entry.frame, w.activeGen)
}

func (w *PacedWriter) finishEmpty(entry *pacedFrame, mediaEnd time.Time) bool {
	w.mu.Lock()
	dropped := w.inflight != entry || w.dropInflight || staleInterruptible(entry.frame, w.activeGen)
	w.clearInflightLocked(entry)
	if !dropped && (entry.frame.IsFinal || entry.frame.TurnFinal) {
		w.retireUnitLocked(frameKey(entry.frame))
	}
	if !dropped && entry.frame.IsFinal && !entry.frame.TurnFinal {
		w.recordCompletedLocked(frameKey(entry.frame), mediaEnd, entry.frame.Disposition)
	}
	if !dropped && entry.frame.TurnFinal {
		key := frameKey(entry.frame)
		if w.drain != nil {
			if abort := w.retireDrainLocked(w.drain); abort != nil {
				close(abort)
			}
		}
		w.drain = &finalDrain{key: key, gen: entry.frame.Gen, disposition: entry.frame.Disposition, mediaEnd: mediaEnd, abort: make(chan struct{})}
	}
	w.mu.Unlock()
	return dropped
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
		if entry.frame.IsFinal {
			delete(w.activeUnits, frameKey(entry.frame))
			w.retireUnitLocked(frameKey(entry.frame))
		}
	}
	w.mu.Unlock()
}

func (w *PacedWriter) finishFinalDrain(ctx context.Context, entry *pacedFrame, mediaEnd time.Time) {
	w.mu.Lock()
	drain := w.drain
	w.mu.Unlock()
	if drain != nil && drain.key == frameKey(entry.frame) {
		w.awaitFinalDrain(ctx, drain)
	}
}

func (w *PacedWriter) awaitFinalDrain(ctx context.Context, drain *finalDrain) {
	if w.cfg.EnablePacing {
		if wait := time.Until(drain.mediaEnd); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-drain.abort:
				stopAndDrainTimer(timer)
			case <-ctx.Done():
				stopAndDrainTimer(timer)
			}
		}
	}
	w.mu.Lock()
	dropped := w.closed || w.drain != drain || drain.dropped || (drain.gen != 0 && drain.gen < w.activeGen && drain.disposition != core.OutputUninterruptible)
	if w.drain == drain {
		w.drain = nil
	}
	w.retireUnitLocked(drain.key)
	delete(w.activeUnits, drain.key)
	dispatch := false
	if !dropped {
		if event, ok := w.speaking.StopOutput(drain.key.turn, drain.key.gen, core.SpeakingStopDrained); ok {
			dispatch = w.queueSpeakingLocked(event)
		}
	}
	w.mu.Unlock()
	if dispatch {
		w.dispatchSpeaking()
	}
}

func (w *PacedWriter) retireDrainLocked(drain *finalDrain) chan struct{} {
	drain.dropped = true
	w.retireUnitLocked(drain.key)
	delete(w.activeUnits, drain.key)
	if drain.aborted {
		return nil
	}
	drain.aborted = true
	return drain.abort
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
	w.signalRoomLocked()
	clear(w.pendingFinals)
	clear(w.completedFinals)
	clear(w.activeUnits)
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
	if w.drain != nil {
		w.drain.dropped = true
		if !w.drain.aborted {
			w.drain.aborted = true
			close(w.drain.abort)
		}
		w.drain = nil
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

func (w *PacedWriter) QueuedFrames() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.queue)
}
