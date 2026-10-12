package core

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
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

func (p *PassthroughProcessor) GetFrames() []Frame {
	p.mu.Lock()
	defer p.mu.Unlock()
	cp := make([]Frame, len(p.Frames))
	copy(cp, p.Frames)
	return cp
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
	EchoUser       bool // If true, append the user's message as a final chunk
	// FailWith, when set, makes a generation emit FailAfter chunks and then a
	// non-fatal ErrorFrame instead of the final LLMTextFrame (provider failure).
	FailWith  error
	FailAfter int
	// Label prefixes every reply (for example "[front] ") so a test can tell
	// which agent of a graph answered.
	Label string
	// ToolOnPhrase maps a lower-case phrase to a tool name. A user message that
	// contains the phrase makes the model call that tool through Executor and
	// answer AfterTool (default "OK.") instead of its normal reply, the way a
	// real model answers after a tool round.
	ToolOnPhrase map[string]string
	Executor     ToolExecutor
	AfterTool    string
	// SayResult maps a tool name to the name the reply uses for it. For those tools
	// the call takes its arguments from the first JSON object in the message ({} if
	// none), and the reply says what came back ("Tool <name> returned <json>" or
	// "Tool <name> failed: <why>") instead of AfterTool, so a test can see the
	// result or failure reach the model.
	SayResult map[string]string
}

// toolArguments is the first balanced {...} in msg if it is a JSON object, else {}.
func toolArguments(msg string) json.RawMessage {
	start := strings.IndexByte(msg, '{')
	if start < 0 {
		return json.RawMessage(`{}`)
	}
	depth := 0
	for i := start; i < len(msg); i++ {
		switch msg[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				var obj map[string]any
				if json.Unmarshal([]byte(msg[start:i+1]), &obj) == nil {
					return json.RawMessage(msg[start : i+1])
				}
				return json.RawMessage(`{}`)
			}
		}
	}
	return json.RawMessage(`{}`)
}

// toolReply is what the fake model says after a SayResult tool call.
func toolReply(name string, res ToolResult, err error) string {
	switch {
	case err != nil:
		return "Tool " + name + " failed: " + err.Error()
	case res.Status != "" && res.Status != ToolSucceeded:
		why := res.ErrorCategory
		if why == "" {
			why = string(res.Status)
		}
		return "Tool " + name + " failed: " + why
	default:
		return "Tool " + name + " returned " + string(res.Result)
	}
}

// toolFor is the tool the message asks the fake model to call (the first
// matching phrase in sorted order, so the choice is deterministic).
func (c FakeLLMConfig) toolFor(msg string) string {
	if c.Executor == nil {
		return ""
	}
	msg = strings.ToLower(msg)
	phrases := make([]string, 0, len(c.ToolOnPhrase))
	for p := range c.ToolOnPhrase {
		phrases = append(phrases, p)
	}
	sort.Strings(phrases)
	for _, p := range phrases {
		if strings.Contains(msg, p) {
			return c.ToolOnPhrase[p]
		}
	}
	return ""
}

// FakeLLMProcessor simulates streamed or non-streamed LLM responses.
type FakeLLMProcessor struct {
	session SessionController
	index   int
	cfg     FakeLLMConfig

	mu            sync.Mutex
	activeCancel  context.CancelFunc
	CanceledCount atomic.Uint64
	Started       atomic.Uint64 // generations begun
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
			if tool := p.cfg.toolFor(um.Content); tool != "" {
				spoken, say := p.cfg.SayResult[tool]
				args := json.RawMessage(`{}`)
				if say {
					args = toolArguments(um.Content)
				}
				// The tool runs on the turn: an interruption (taskCtx) cancels it, as the
				// production tool loop's per-call context does.
				callCtx, stopCall := context.WithCancel(c)
				unlink := context.AfterFunc(taskCtx, stopCall)
				res, err := p.cfg.Executor.Execute(callCtx, ToolCall{ID: "fake-" + um.Turn, Name: tool, Arguments: args,
					TurnID: TurnID(um.Turn), Generation: Generation(gen)})
				unlink()
				stopCall()
				after := p.cfg.AfterTool
				if after == "" {
					after = "OK."
				}
				if say {
					after = toolReply(spoken, res, err)
				}
				chunks = []string{after}
			}
			if p.cfg.Label != "" {
				chunks = append([]string{p.cfg.Label}, chunks...)
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

				_ = p.session.Reenter(&LLMTextDeltaFrame{
					Delta: chunk,
					Turn:  um.Turn,
					Gen:   gen,
				}, Downstream, 0, gen)
			}
			if p.cfg.FailWith != nil {
				_ = p.session.Reenter(&ErrorFrame{Err: p.cfg.FailWith}, Downstream, p.index, gen)
				return
			}
			_ = p.session.Reenter(&LLMTextFrame{
				Text: complete.String(),
				Turn: um.Turn,
				Gen:  gen,
			}, Downstream, 0, gen)
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
	// FailWith fails every unit with this error: before any audio when
	// FailAfterAudio is 0, else after that many audio frames, then closes the unit
	// out (empty final frame), as the real streaming providers do.
	FailWith       error
	FailAfterAudio int
	SampleRate     int // output rate (default 24000)
}

// FakeTTSProcessor simulates TTS synthesis from explicit synthesis units.
type FakeTTSProcessor struct {
	session SessionController
	index   int
	cfg     FakeTTSConfig
	queue   SerialQueue // one task for all units, like the real TTS stages (ttsunit)

	mu            sync.Mutex
	tail          chan struct{}
	nextTaskID    uint64
	cancels       map[uint64]context.CancelFunc
	Synthesized   atomic.Uint64
	CanceledCount atomic.Uint64
	Units         atomic.Uint64 // units this processor started synthesizing
}

func NewFakeTTSProcessor(cfg FakeTTSConfig) *FakeTTSProcessor {
	if cfg.FramesPerUnit <= 0 {
		cfg.FramesPerUnit = 2
	}
	ready := make(chan struct{})
	close(ready)
	return &FakeTTSProcessor{cfg: cfg, tail: ready, cancels: make(map[uint64]context.CancelFunc)}
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
				_ = p.session.Reenter(&ErrorFrame{Err: p.cfg.FailWith, Turn: TurnID(su.Turn), Gen: Generation(su.Gen), UnitID: su.UnitID}, Downstream, p.index+1, su.Gen)
			}
			if p.cfg.FailWith != nil && p.cfg.FailAfterAudio == 0 {
				fail()
				return
			}
			for i := 1; i <= p.cfg.FramesPerUnit; i++ {
				if p.cfg.FailWith != nil && i > p.cfg.FailAfterAudio {
					fail() // error first, then the closing frame (see the cartesia/elevenlabs providers)
					_ = p.session.Reenter(&OutputAudioFrame{Encoding: EncodingPCM16LE, SampleRate: rate, Channels: 1, Gen: su.Gen,
						Turn: su.Turn, UnitID: su.UnitID, IsFinal: true, TurnFinal: su.IsFinal}, Downstream, p.index+1, su.Gen)
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
				audioFrame := &OutputAudioFrame{
					Samples:    make([]byte, 640),
					Encoding:   EncodingPCM16LE,
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

				_ = p.session.Reenter(audioFrame, Downstream, p.index+1, su.Gen)
			}
		})

	case FrameInterruption:
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
		if p.session != nil && oaf.Gen != 0 && oaf.Gen < p.session.CurrentGeneration() && oaf.Disposition != OutputUninterruptible {
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
				Turn:        oaf.Turn,
				Gen:         oaf.Gen,
				UnitID:      oaf.UnitID,
				Seq:         oaf.Seq,
				IsFinal:     oaf.IsFinal,
				TurnFinal:   oaf.TurnFinal,
				Media:       oaf.MediaDuration(),
				WordTimings: oaf.WordTimings,
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
			if p.session != nil && oaf.Gen != 0 && oaf.Gen < p.session.CurrentGeneration() && oaf.Disposition != OutputUninterruptible {
				p.DiscardedFrames = append(p.DiscardedFrames, oaf)
				p.mu.Unlock()
				// Send write completion with old Gen to verify session rejects it
				_ = p.session.Reenter(&WriteCompletionFrame{
					Turn:      oaf.Turn,
					Gen:       oaf.Gen,
					UnitID:    oaf.UnitID,
					Seq:       oaf.Seq,
					IsFinal:   oaf.IsFinal,
					TurnFinal: oaf.TurnFinal,
				}, Downstream, 0, oaf.Gen)
				return
			}
			p.WrittenFrames = append(p.WrittenFrames, oaf)
			p.mu.Unlock()

			_ = p.session.Reenter(&WriteCompletionFrame{
				Turn:        oaf.Turn,
				Gen:         oaf.Gen,
				UnitID:      oaf.UnitID,
				Seq:         oaf.Seq,
				IsFinal:     oaf.IsFinal,
				TurnFinal:   oaf.TurnFinal,
				Media:       oaf.MediaDuration(),
				WordTimings: oaf.WordTimings,
			}, Downstream, 0, oaf.Gen)
		})

	case FrameInterruption:
		intr := f.(*InterruptionFrame)
		p.mu.Lock()
		// Discard pending frames belonging to interrupted generation
		active := make([]*OutputAudioFrame, 0, len(p.ReceivedFrames))
		for _, frame := range p.ReceivedFrames {
			if frame.Gen == intr.Gen && frame.Disposition != OutputUninterruptible {
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
			if frame.Gen == ff.Gen && frame.Disposition != OutputUninterruptible {
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
