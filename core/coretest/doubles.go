// Package coretest holds test doubles for pipelines: scripted LLM, TTS and STT processors with
// failure injection (FailWith, FailAfter, IgnoreCancel), pipeline sinks, failure-injecting
// processors and a recording transport output. They are not providers. A call that should behave
// like a real provider, in the UI, the harnesses or an end-to-end test, uses the simulated provider
// in providers/echo, which runs the real Google processors against simulated backends. These stay
// for the tests that need to break a stage on purpose, and for the framework-overhead benchmark,
// which must measure the pipeline and nothing else.
//
// core's own in-package tests cannot import this package (it imports core); they use package
// core_test, or small local helpers (core/helpers_test.go).
package coretest

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"govoice/core"
)

// PassthroughProcessor records frames received and forwards them.
type PassthroughProcessor struct {
	mu     sync.Mutex
	Frames []core.Frame
}

func (p *PassthroughProcessor) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
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

func (p *PassthroughProcessor) GetFrames() []core.Frame {
	p.mu.Lock()
	defer p.mu.Unlock()
	cp := make([]core.Frame, len(p.Frames))
	copy(cp, p.Frames)
	return cp
}

// LatencyProcessor sleeps for a configured duration before forwarding the frame.
type LatencyProcessor struct {
	Delay time.Duration
}

func (p *LatencyProcessor) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	select {
	case <-time.After(p.Delay):
		return e.PushDirection(f, dir)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// PanicProcessor panics when it encounters the configured frame type.
type PanicProcessor struct {
	PanicOn core.FrameType
}

func (p *PanicProcessor) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	if f.Type() == p.PanicOn {
		panic("deliberate inline processor panic")
	}
	return e.PushDirection(f, dir)
}

// ChildPanicProcessor initiates an async task via session.Go that panics.
type ChildPanicProcessor struct {
	session core.SessionController
}

func (p *ChildPanicProcessor) Attach(s core.SessionController, index int) error {
	p.session = s
	return nil
}

func (p *ChildPanicProcessor) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	return p.session.Go(func(ctx context.Context) {
		time.Sleep(10 * time.Millisecond)
		panic("deliberate child task panic in session.Go")
	})
}

// FloodProcessor attempts to emit count frames in response to a single frame.
type FloodProcessor struct {
	EmitCount int
}

func (p *FloodProcessor) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	for i := 0; i < p.EmitCount; i++ {
		if err := e.Push(&core.UserStartedSpeakingFrame{Seq: uint64(i)}); err != nil {
			return err
		}
	}
	return nil
}

// StallProcessor sleeps longer than the watchdog timeout to trigger watchdog failure.
type StallProcessor struct {
	Duration time.Duration
}

func (p *StallProcessor) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	time.Sleep(p.Duration)
	return e.Push(f)
}

// IgnoredCancellationProcessor ignores context cancellation in session.Go to test leaked task detection.
type IgnoredCancellationProcessor struct {
	session core.SessionController
}

func (p *IgnoredCancellationProcessor) Attach(s core.SessionController, index int) error {
	p.session = s
	return nil
}

func (p *IgnoredCancellationProcessor) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	return p.session.Go(func(ctx context.Context) {
		// Deliberately ignore ctx.Done() and sleep through teardown
		time.Sleep(10 * time.Second)
	})
}

// -------------------------------------------------------------------------
// Scripted LLM and TTS processors and a recording transport output
// -------------------------------------------------------------------------

// LLMConfig defines behavior for LLMProcessor.
type LLMConfig struct {
	ResponseChunks []string
	ChunkDelay     time.Duration
	IgnoreCancel   bool // If true, keep emitting after cancellation to test fencing
	EchoUser       bool // If true, append the user's message as a final chunk
	// FailWith, when set, makes a generation emit FailAfter chunks and then a
	// non-fatal ErrorFrame instead of the final LLMTextFrame (provider failure).
	FailWith  error
	FailAfter int
}

// LLMProcessor simulates streamed or non-streamed LLM responses.
type LLMProcessor struct {
	session core.SessionController
	index   int
	cfg     LLMConfig

	mu            sync.Mutex
	activeCancel  context.CancelFunc
	CanceledCount atomic.Uint64
	Started       atomic.Uint64 // generations begun
}

func NewLLMProcessor(cfg LLMConfig) *LLMProcessor {
	return &LLMProcessor{cfg: cfg}
}

func (p *LLMProcessor) Attach(s core.SessionController, index int) error {
	p.session = s
	p.index = index
	return nil
}

func (p *LLMProcessor) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	switch f.Type() {
	case core.FrameUserMessage:
		um := f.(*core.UserMessageFrame)
		// Preserve the same provider contract as production LLM processors: the
		// assistant context must observe the valid user-turn boundary before any
		// asynchronous model output can re-enter.
		if err := e.PushDirection(f, dir); err != nil {
			return err
		}
		p.mu.Lock()
		taskCtx, cancel := context.WithCancel(ctx)
		p.activeCancel = cancel
		p.mu.Unlock()

		gen := um.Gen
		if p.session != nil && gen == 0 {
			gen = p.session.CurrentGeneration()
		}

		p.Started.Add(1)
		return p.session.Go(func(c context.Context) {
			var complete strings.Builder
			chunks := p.cfg.ResponseChunks
			if p.cfg.EchoUser {
				chunks = append(chunks[:len(chunks):len(chunks)], um.Content)
			}
			for i, chunk := range chunks {
				if p.cfg.FailWith != nil && i == p.cfg.FailAfter {
					break
				}
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
				complete.WriteString(chunk)

				_ = p.session.Reenter(&core.LLMTextDeltaFrame{
					Delta: chunk,
					Turn:  um.Turn,
					Gen:   gen,
				}, core.Downstream, 0, gen)
			}
			if p.cfg.FailWith != nil {
				_ = p.session.Reenter(&core.ErrorFrame{Err: p.cfg.FailWith}, core.Downstream, p.index, gen)
				return
			}
			_ = p.session.Reenter(&core.LLMTextFrame{
				Text: complete.String(),
				Turn: um.Turn,
				Gen:  gen,
			}, core.Downstream, 0, gen)
		})

	case core.FrameInterruption:
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

// TTSConfig defines behavior for TTSProcessor.
type TTSConfig struct {
	FramesPerUnit int           // Output audio frames per unit (default: 2)
	FrameDelay    time.Duration // Delay between audio frame emissions
	IgnoreCancel  bool          // If true, keep emitting after cancellation to test fencing
	// FailWith fails every unit with this error: before any audio when
	// FailAfterAudio is 0, else after that many audio frames, then closes the unit
	// out (empty final frame), as the real streaming providers do.
	FailWith       error
	FailAfterAudio int
	SampleRate     int // output rate (default 24000)
}

// TTSProcessor simulates TTS synthesis from explicit synthesis units.
type TTSProcessor struct {
	session core.SessionController
	index   int
	cfg     TTSConfig
	queue   core.SerialQueue // one task for all units, like the real TTS stages (ttsunit)

	mu            sync.Mutex
	tail          chan struct{}
	nextTaskID    uint64
	cancels       map[uint64]context.CancelFunc
	Synthesized   atomic.Uint64
	CanceledCount atomic.Uint64
	Units         atomic.Uint64 // units this processor started synthesizing
}

func NewTTSProcessor(cfg TTSConfig) *TTSProcessor {
	if cfg.FramesPerUnit <= 0 {
		cfg.FramesPerUnit = 2
	}
	ready := make(chan struct{})
	close(ready)
	return &TTSProcessor{cfg: cfg, tail: ready, cancels: make(map[uint64]context.CancelFunc)}
}

func (p *TTSProcessor) Attach(s core.SessionController, index int) error {
	p.session = s
	p.index = index
	return nil
}

func (p *TTSProcessor) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	switch f.Type() {
	case core.FrameSynthesisUnit:
		su := f.(*core.SynthesisUnitFrame)
		p.mu.Lock()
		taskCtx, cancel := context.WithCancel(ctx)
		previous := p.tail
		done := make(chan struct{})
		p.tail = done
		p.nextTaskID++
		taskID := p.nextTaskID
		p.cancels[taskID] = cancel
		p.mu.Unlock()

		return p.queue.Go(p.session, func(c context.Context) {
			defer close(done)
			defer func() {
				p.mu.Lock()
				delete(p.cancels, taskID)
				p.mu.Unlock()
				cancel()
			}()
			if p.cfg.IgnoreCancel {
				select {
				case <-previous:
				case <-c.Done():
					return
				}
			} else {
				select {
				case <-previous:
				case <-taskCtx.Done():
					return
				case <-c.Done():
					return
				}
			}
			p.Units.Add(1)
			rate := p.cfg.SampleRate
			if rate == 0 {
				rate = 24000
			}
			fail := func() {
				_ = p.session.Reenter(&core.ErrorFrame{Err: p.cfg.FailWith, Turn: core.TurnID(su.Turn), Gen: core.Generation(su.Gen), UnitID: su.UnitID}, core.Downstream, p.index+1, su.Gen)
			}
			if p.cfg.FailWith != nil && p.cfg.FailAfterAudio == 0 {
				fail()
				return
			}
			for i := 1; i <= p.cfg.FramesPerUnit; i++ {
				if p.cfg.FailWith != nil && i > p.cfg.FailAfterAudio {
					fail() // error first, then the closing frame (see the cartesia/elevenlabs providers)
					_ = p.session.Reenter(&core.OutputAudioFrame{Encoding: core.EncodingPCM16LE, SampleRate: rate, Channels: 1, Gen: su.Gen,
						Turn: su.Turn, UnitID: su.UnitID, IsFinal: true, TurnFinal: su.IsFinal}, core.Downstream, p.index+1, su.Gen)
					return
				}
				if p.cfg.FrameDelay > 0 {
					timer := time.NewTimer(p.cfg.FrameDelay)
					if p.cfg.IgnoreCancel {
						select {
						case <-timer.C:
						case <-c.Done():
							if !timer.Stop() {
								<-timer.C
							}
							return
						}
					} else {
						select {
						case <-timer.C:
						case <-taskCtx.Done():
							if !timer.Stop() {
								select {
								case <-timer.C:
								default:
								}
							}
							return
						case <-c.Done():
							if !timer.Stop() {
								select {
								case <-timer.C:
								default:
								}
							}
							return
						}
					}
				}

				if !p.cfg.IgnoreCancel && taskCtx.Err() != nil {
					return
				}

				isFinal := (i == p.cfg.FramesPerUnit)
				audioFrame := &core.OutputAudioFrame{
					Samples:    make([]byte, 640),
					Encoding:   core.EncodingPCM16LE,
					SampleRate: rate,
					Channels:   1,
					Duration:   20 * time.Millisecond,
					Timestamp:  time.Now(),
					Seq:        p.Synthesized.Add(1),
					Gen:        su.Gen,
					Turn:       su.Turn,
					UnitID:     su.UnitID,
					IsFinal:    isFinal,
					TurnFinal:  su.IsFinal && isFinal,
				}

				_ = p.session.Reenter(audioFrame, core.Downstream, p.index+1, su.Gen)
			}
		})

	case core.FrameInterruption:
		p.mu.Lock()
		if len(p.cancels) > 0 {
			for _, cancel := range p.cancels {
				cancel()
			}
			p.CanceledCount.Add(1)
		}
		p.mu.Unlock()
		return e.PushDirection(f, dir)

	default:
		return e.PushDirection(f, dir)
	}
}

// TransportConfig defines behavior for TransportOutput.
type TransportConfig struct {
	WriteDelay        time.Duration // Simulated write latency
	SimulateFlushHang bool          // If true, withhold FlushProcessedFrame to test flush timeout
}

// TransportOutput simulates the client audio edge, paced writing, and write completions.
type TransportOutput struct {
	session core.SessionController
	index   int
	cfg     TransportConfig

	mu              sync.Mutex
	ReceivedFrames  []*core.OutputAudioFrame
	WrittenFrames   []*core.OutputAudioFrame
	DiscardedFrames []*core.OutputAudioFrame
	FlushCount      atomic.Uint64
	FlushAcksSent   atomic.Uint64
}

func NewTransportOutput(cfg TransportConfig) *TransportOutput {
	return &TransportOutput{
		cfg:             cfg,
		ReceivedFrames:  make([]*core.OutputAudioFrame, 0),
		WrittenFrames:   make([]*core.OutputAudioFrame, 0),
		DiscardedFrames: make([]*core.OutputAudioFrame, 0),
	}
}

func (p *TransportOutput) Attach(s core.SessionController, index int) error {
	p.session = s
	p.index = index
	return nil
}

func (p *TransportOutput) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	switch f.Type() {
	case core.FrameOutputAudio:
		oaf := f.(*core.OutputAudioFrame)
		p.mu.Lock()
		// Stale generation discard
		if p.session != nil && oaf.Gen != 0 && oaf.Gen < p.session.CurrentGeneration() && oaf.Disposition != core.OutputUninterruptible {
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
			return p.session.Reenter(&core.WriteCompletionFrame{
				Turn:        oaf.Turn,
				Gen:         oaf.Gen,
				UnitID:      oaf.UnitID,
				Seq:         oaf.Seq,
				IsFinal:     oaf.IsFinal,
				TurnFinal:   oaf.TurnFinal,
				Media:       oaf.MediaDuration(),
				WordTimings: oaf.WordTimings,
			}, core.Downstream, 0, oaf.Gen)
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
			if p.session != nil && oaf.Gen != 0 && oaf.Gen < p.session.CurrentGeneration() && oaf.Disposition != core.OutputUninterruptible {
				p.DiscardedFrames = append(p.DiscardedFrames, oaf)
				p.mu.Unlock()
				// Send write completion with old Gen to verify session rejects it
				_ = p.session.Reenter(&core.WriteCompletionFrame{
					Turn:      oaf.Turn,
					Gen:       oaf.Gen,
					UnitID:    oaf.UnitID,
					Seq:       oaf.Seq,
					IsFinal:   oaf.IsFinal,
					TurnFinal: oaf.TurnFinal,
				}, core.Downstream, 0, oaf.Gen)
				return
			}
			p.WrittenFrames = append(p.WrittenFrames, oaf)
			p.mu.Unlock()

			_ = p.session.Reenter(&core.WriteCompletionFrame{
				Turn:        oaf.Turn,
				Gen:         oaf.Gen,
				UnitID:      oaf.UnitID,
				Seq:         oaf.Seq,
				IsFinal:     oaf.IsFinal,
				TurnFinal:   oaf.TurnFinal,
				Media:       oaf.MediaDuration(),
				WordTimings: oaf.WordTimings,
			}, core.Downstream, 0, oaf.Gen)
		})

	case core.FrameInterruption:
		intr := f.(*core.InterruptionFrame)
		p.mu.Lock()
		// Discard pending frames belonging to interrupted generation
		active := make([]*core.OutputAudioFrame, 0, len(p.ReceivedFrames))
		for _, frame := range p.ReceivedFrames {
			if frame.Gen == intr.Gen && frame.Disposition != core.OutputUninterruptible {
				p.DiscardedFrames = append(p.DiscardedFrames, frame)
			} else {
				active = append(active, frame)
			}
		}
		p.ReceivedFrames = active
		p.mu.Unlock()
		return e.PushDirection(f, dir)

	case core.FrameFlush:
		ff := f.(*core.FlushFrame)
		p.mu.Lock()
		p.FlushCount.Add(1)
		// Discard all queued audio frames for the flushed generation
		remaining := make([]*core.OutputAudioFrame, 0, len(p.ReceivedFrames))
		for _, frame := range p.ReceivedFrames {
			if frame.Gen == ff.Gen && frame.Disposition != core.OutputUninterruptible {
				p.DiscardedFrames = append(p.DiscardedFrames, frame)
			} else {
				remaining = append(remaining, frame)
			}
		}
		p.ReceivedFrames = remaining
		p.mu.Unlock()

		if !p.cfg.SimulateFlushHang {
			p.FlushAcksSent.Add(1)
			return e.PushDirection(&core.FlushProcessedFrame{Gen: ff.Gen}, core.Upstream)
		}
		return nil

	default:
		return e.PushDirection(f, dir)
	}
}

func (p *TransportOutput) GetWrittenFrames() []*core.OutputAudioFrame {
	p.mu.Lock()
	defer p.mu.Unlock()
	res := make([]*core.OutputAudioFrame, len(p.WrittenFrames))
	copy(res, p.WrittenFrames)
	return res
}
