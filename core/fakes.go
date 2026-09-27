package core

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrFakeProcessor = errors.New("fake: configured processor error")
)

// PassthroughProcessor records frames received and forwards them.
type PassthroughProcessor struct {
	mu     sync.Mutex
	Frames []Frame
}

func (p *PassthroughProcessor) ProcessFrame(ctx context.Context, f Frame, dir Direction, e Emitter) error {
	p.mu.Lock()
	p.Frames = append(p.Frames, f)
	p.mu.Unlock()
	return e.PushDirection(f, dir)
}

func (p *PassthroughProcessor) Count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.Frames)
}

// LatencyProcessor sleeps for a configured duration before forwarding the frame.
type LatencyProcessor struct {
	Delay time.Duration
}

func (p *LatencyProcessor) ProcessFrame(ctx context.Context, f Frame, dir Direction, e Emitter) error {
	select {
	case <-time.After(p.Delay):
		return e.PushDirection(f, dir)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ErrorProcessor returns an error when it encounters the configured frame type.
type ErrorProcessor struct {
	FailOn FrameType
}

func (p *ErrorProcessor) ProcessFrame(ctx context.Context, f Frame, dir Direction, e Emitter) error {
	if f.Type() == p.FailOn {
		return ErrFakeProcessor
	}
	return e.PushDirection(f, dir)
}

// PanicProcessor panics when it encounters the configured frame type.
type PanicProcessor struct {
	PanicOn FrameType
}

func (p *PanicProcessor) ProcessFrame(ctx context.Context, f Frame, dir Direction, e Emitter) error {
	if f.Type() == p.PanicOn {
		panic("deliberate inline processor panic")
	}
	return e.PushDirection(f, dir)
}

// ChildPanicProcessor initiates an async task via session.Go that panics.
type ChildPanicProcessor struct {
	session SessionController
}

func (p *ChildPanicProcessor) Attach(s SessionController, index int) error {
	p.session = s
	return nil
}

func (p *ChildPanicProcessor) ProcessFrame(ctx context.Context, f Frame, dir Direction, e Emitter) error {
	return p.session.Go(func(ctx context.Context) {
		time.Sleep(10 * time.Millisecond)
		panic("deliberate child task panic in session.Go")
	})
}

// FloodProcessor attempts to emit count frames in response to a single frame.
type FloodProcessor struct {
	EmitCount int
}

func (p *FloodProcessor) ProcessFrame(ctx context.Context, f Frame, dir Direction, e Emitter) error {
	for i := 0; i < p.EmitCount; i++ {
		if err := e.Push(&UserStartedSpeakingFrame{Seq: uint64(i)}); err != nil {
			return err
		}
	}
	return nil
}

// StallProcessor sleeps longer than the watchdog timeout to trigger watchdog failure.
type StallProcessor struct {
	Duration time.Duration
}

func (p *StallProcessor) ProcessFrame(ctx context.Context, f Frame, dir Direction, e Emitter) error {
	time.Sleep(p.Duration)
	return e.Push(f)
}

// LateCompletionProcessor starts an async task that finishes with an old generation.
type LateCompletionProcessor struct {
	session SessionController
	index   int
	Delay   time.Duration
}

func (p *LateCompletionProcessor) Attach(s SessionController, index int) error {
	p.session = s
	p.index = index
	return nil
}

func (p *LateCompletionProcessor) ProcessFrame(ctx context.Context, f Frame, dir Direction, e Emitter) error {
	gen := p.session.CurrentGeneration()
	return p.session.Go(func(ctx context.Context) {
		time.Sleep(p.Delay)
		// Reenter with the captured old generation
		_ = p.session.Reenter(&FinalTranscriptionFrame{
			Text: "late transcript",
			Gen:  gen,
		}, Downstream, p.index+1, gen)
	})
}

// IgnoredCancellationProcessor ignores context cancellation in session.Go to test leaked task detection.
type IgnoredCancellationProcessor struct {
	session SessionController
}

func (p *IgnoredCancellationProcessor) Attach(s SessionController, index int) error {
	p.session = s
	return nil
}

func (p *IgnoredCancellationProcessor) ProcessFrame(ctx context.Context, f Frame, dir Direction, e Emitter) error {
	return p.session.Go(func(ctx context.Context) {
		// Deliberately ignore ctx.Done() and sleep through teardown
		time.Sleep(10 * time.Second)
	})
}

// -------------------------------------------------------------------------
// Phase 2 Deterministic Fakes: LLM, TTS, and TransportOutput
// -------------------------------------------------------------------------

// FakeLLMConfig defines behavior for FakeLLMProcessor.
type FakeLLMConfig struct {
	ResponseChunks []string
	ChunkDelay     time.Duration
	IgnoreCancel   bool // If true, keep emitting after cancellation to test fencing
}

// FakeLLMProcessor simulates streamed or non-streamed LLM responses.
type FakeLLMProcessor struct {
	session SessionController
	index   int
	cfg     FakeLLMConfig

	mu            sync.Mutex
	activeCancel  context.CancelFunc
	CanceledCount atomic.Uint64
}

func NewFakeLLMProcessor(cfg FakeLLMConfig) *FakeLLMProcessor {
	return &FakeLLMProcessor{cfg: cfg}
}

func (p *FakeLLMProcessor) Attach(s SessionController, index int) error {
	p.session = s
	p.index = index
	return nil
}

func (p *FakeLLMProcessor) ProcessFrame(ctx context.Context, f Frame, dir Direction, e Emitter) error {
	switch f.Type() {
	case FrameUserMessage:
		um := f.(*UserMessageFrame)
		p.mu.Lock()
		taskCtx, cancel := context.WithCancel(ctx)
		p.activeCancel = cancel
		p.mu.Unlock()

		gen := um.Gen
		if p.session != nil && gen == 0 {
			gen = p.session.CurrentGeneration()
		}

		return p.session.Go(func(c context.Context) {
			for _, chunk := range p.cfg.ResponseChunks {
				if p.cfg.ChunkDelay > 0 {
					select {
					case <-taskCtx.Done():
						if !p.cfg.IgnoreCancel {
							return
						}
					case <-time.After(p.cfg.ChunkDelay):
					}
				}

				if !p.cfg.IgnoreCancel && taskCtx.Err() != nil {
					return
				}

				_ = p.session.Reenter(&LLMTextDeltaFrame{
					Delta: chunk,
					Turn:  um.Turn,
					Gen:   gen,
				}, Downstream, 0, gen)
			}
		})

	case FrameInterruption:
		p.mu.Lock()
		if p.activeCancel != nil {
			p.activeCancel()
			p.CanceledCount.Add(1)
		}
		p.mu.Unlock()
		return e.PushDirection(f, dir)

	default:
		return e.PushDirection(f, dir)
	}
}

// FakeTTSConfig defines behavior for FakeTTSProcessor.
type FakeTTSConfig struct {
	FramesPerUnit int           // Output audio frames per unit (default: 2)
	FrameDelay    time.Duration // Delay between audio frame emissions
	IgnoreCancel  bool          // If true, keep emitting after cancellation to test fencing
}

// FakeTTSProcessor simulates TTS synthesis from explicit synthesis units.
type FakeTTSProcessor struct {
	session SessionController
	index   int
	cfg     FakeTTSConfig

	mu            sync.Mutex
	activeCancel  context.CancelFunc
	Synthesized   atomic.Uint64
	CanceledCount atomic.Uint64
}

func NewFakeTTSProcessor(cfg FakeTTSConfig) *FakeTTSProcessor {
	if cfg.FramesPerUnit <= 0 {
		cfg.FramesPerUnit = 2
	}
	return &FakeTTSProcessor{cfg: cfg}
}

func (p *FakeTTSProcessor) Attach(s SessionController, index int) error {
	p.session = s
	p.index = index
	return nil
}

func (p *FakeTTSProcessor) ProcessFrame(ctx context.Context, f Frame, dir Direction, e Emitter) error {
	switch f.Type() {
	case FrameSynthesisUnit:
		su := f.(*SynthesisUnitFrame)
		p.mu.Lock()
		taskCtx, cancel := context.WithCancel(ctx)
		p.activeCancel = cancel
		p.mu.Unlock()

		return p.session.Go(func(c context.Context) {
			for i := 1; i <= p.cfg.FramesPerUnit; i++ {
				if p.cfg.FrameDelay > 0 {
					select {
					case <-taskCtx.Done():
						if !p.cfg.IgnoreCancel {
							return
						}
					case <-time.After(p.cfg.FrameDelay):
					}
				}

				if !p.cfg.IgnoreCancel && taskCtx.Err() != nil {
					return
				}

				isFinal := (i == p.cfg.FramesPerUnit)
				audioFrame := &OutputAudioFrame{
					Samples:    make([]byte, 640),
					Encoding:   EncodingPCM16LE,
					SampleRate: 24000,
					Channels:   1,
					Duration:   20 * time.Millisecond,
					Timestamp:  time.Now(),
					Seq:        p.Synthesized.Add(1),
					Gen:        su.Gen,
					Turn:       su.Turn,
					UnitID:     su.UnitID,
					IsFinal:    isFinal,
				}

				_ = p.session.Reenter(audioFrame, Downstream, p.index+1, su.Gen)
			}
		})

	case FrameInterruption:
		p.mu.Lock()
		if p.activeCancel != nil {
			p.activeCancel()
			p.CanceledCount.Add(1)
		}
		p.mu.Unlock()
		return e.PushDirection(f, dir)

	default:
		return e.PushDirection(f, dir)
	}
}

// FakeTransportConfig defines behavior for FakeTransportOutput.
type FakeTransportConfig struct {
	WriteDelay        time.Duration // Simulated write latency
	SimulateFlushHang bool          // If true, withhold FlushProcessedFrame to test flush timeout
}

// FakeTransportOutput simulates the client audio edge, paced writing, and write completions.
type FakeTransportOutput struct {
	session SessionController
	index   int
	cfg     FakeTransportConfig

	mu              sync.Mutex
	ReceivedFrames  []*OutputAudioFrame
	WrittenFrames   []*OutputAudioFrame
	DiscardedFrames []*OutputAudioFrame
	FlushCount      atomic.Uint64
	FlushAcksSent   atomic.Uint64
}

func NewFakeTransportOutput(cfg FakeTransportConfig) *FakeTransportOutput {
	return &FakeTransportOutput{
		cfg:             cfg,
		ReceivedFrames:  make([]*OutputAudioFrame, 0),
		WrittenFrames:   make([]*OutputAudioFrame, 0),
		DiscardedFrames: make([]*OutputAudioFrame, 0),
	}
}

func (p *FakeTransportOutput) Attach(s SessionController, index int) error {
	p.session = s
	p.index = index
	return nil
}

func (p *FakeTransportOutput) ProcessFrame(ctx context.Context, f Frame, dir Direction, e Emitter) error {
	switch f.Type() {
	case FrameOutputAudio:
		oaf := f.(*OutputAudioFrame)
		p.mu.Lock()
		// Stale generation discard
		if p.session != nil && oaf.Gen != 0 && oaf.Gen < p.session.CurrentGeneration() {
			p.DiscardedFrames = append(p.DiscardedFrames, oaf)
			p.mu.Unlock()
			return nil
		}
		p.ReceivedFrames = append(p.ReceivedFrames, oaf)
		p.mu.Unlock()

		if p.cfg.WriteDelay == 0 {
			p.mu.Lock()
			p.WrittenFrames = append(p.WrittenFrames, oaf)
			p.mu.Unlock()

			// Report write completion immediately back to session
			return p.session.Reenter(&WriteCompletionFrame{
				Turn:    oaf.Turn,
				Gen:     oaf.Gen,
				UnitID:  oaf.UnitID,
				Seq:     oaf.Seq,
				IsFinal: oaf.IsFinal,
			}, Downstream, 0, oaf.Gen)
		}

		// Asynchronous write simulation
		return p.session.Go(func(ctx context.Context) {
			select {
			case <-time.After(p.cfg.WriteDelay):
			case <-ctx.Done():
				return
			}

			p.mu.Lock()
			// Check if generation changed during write
			if p.session != nil && oaf.Gen != 0 && oaf.Gen < p.session.CurrentGeneration() {
				p.DiscardedFrames = append(p.DiscardedFrames, oaf)
				p.mu.Unlock()
				// Send write completion with old Gen to verify session rejects it
				_ = p.session.Reenter(&WriteCompletionFrame{
					Turn:    oaf.Turn,
					Gen:     oaf.Gen,
					UnitID:  oaf.UnitID,
					Seq:     oaf.Seq,
					IsFinal: oaf.IsFinal,
				}, Downstream, 0, oaf.Gen)
				return
			}
			p.WrittenFrames = append(p.WrittenFrames, oaf)
			p.mu.Unlock()

			_ = p.session.Reenter(&WriteCompletionFrame{
				Turn:    oaf.Turn,
				Gen:     oaf.Gen,
				UnitID:  oaf.UnitID,
				Seq:     oaf.Seq,
				IsFinal: oaf.IsFinal,
			}, Downstream, 0, oaf.Gen)
		})

	case FrameInterruption:
		intr := f.(*InterruptionFrame)
		p.mu.Lock()
		// Discard pending frames belonging to interrupted generation
		active := make([]*OutputAudioFrame, 0, len(p.ReceivedFrames))
		for _, frame := range p.ReceivedFrames {
			if frame.Gen == intr.Gen {
				p.DiscardedFrames = append(p.DiscardedFrames, frame)
			} else {
				active = append(active, frame)
			}
		}
		p.ReceivedFrames = active
		p.mu.Unlock()
		return e.PushDirection(f, dir)

	case FrameFlush:
		ff := f.(*FlushFrame)
		p.mu.Lock()
		p.FlushCount.Add(1)
		// Discard all queued audio frames for the flushed generation
		remaining := make([]*OutputAudioFrame, 0, len(p.ReceivedFrames))
		for _, frame := range p.ReceivedFrames {
			if frame.Gen == ff.Gen {
				p.DiscardedFrames = append(p.DiscardedFrames, frame)
			} else {
				remaining = append(remaining, frame)
			}
		}
		p.ReceivedFrames = remaining
		p.mu.Unlock()

		if !p.cfg.SimulateFlushHang {
			p.FlushAcksSent.Add(1)
			return e.PushDirection(&FlushProcessedFrame{Gen: ff.Gen}, Upstream)
		}
		return nil

	default:
		return e.PushDirection(f, dir)
	}
}

func (p *FakeTransportOutput) GetWrittenFrames() []*OutputAudioFrame {
	p.mu.Lock()
	defer p.mu.Unlock()
	res := make([]*OutputAudioFrame, len(p.WrittenFrames))
	copy(res, p.WrittenFrames)
	return res
}

func (p *FakeTransportOutput) GetDiscardedFrames() []*OutputAudioFrame {
	p.mu.Lock()
	defer p.mu.Unlock()
	res := make([]*OutputAudioFrame, len(p.DiscardedFrames))
	copy(res, p.DiscardedFrames)
	return res
}
