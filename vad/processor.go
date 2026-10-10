package vad

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"govoice/core"
)

// vadResultFrame re-enters the pipeline at the VAD processor when inference completes.
type vadResultFrame struct {
	j     *Job
	trace *vadJobTrace
}

type vadJobTrace struct {
	span      core.Span
	policy    core.TracePolicy
	session   core.SessionID
	gen       core.Generation
	submitted time.Time
	once      sync.Once
}

func (t *vadJobTrace) event(name string, attrs map[string]any) {
	if t != nil && t.span != nil {
		t.span.AddEvent(core.NewTraceEvent(name, time.Now(), t.session, "", t.gen, t.policy, attrs))
	}
}
func (t *vadJobTrace) finish(err error) {
	if t == nil {
		return
	}
	t.once.Do(func() {
		name := "vad.result"
		if err != nil {
			name = "vad.failure"
		}
		t.event(name, map[string]any{"latency_ms": time.Since(t.submitted).Milliseconds()})
		if t.span != nil {
			t.span.End(err)
		}
	})
}

func (*vadResultFrame) Type() core.FrameType { return core.FrameInternal }

// SurvivesGenerationFence: a result dropped as stale would leave busy set and
// VAD would never submit again.
func (*vadResultFrame) SurvivesGenerationFence() {}

// ProcessorConfig defines VAD thresholds and behavior.
type ProcessorConfig struct {
	StaleAfter  time.Duration // job deadline = due + StaleAfter
	EnergyGate  float32       // RMS below this skips inference (0 = off)
	StartThresh float32       // probability threshold for speech start
	StopThresh  float32       // probability threshold for speech stop
	StartChunks int           // consecutive chunks above StartThresh to trigger start
	StopChunks  int           // consecutive chunks below StopThresh to trigger stop
	PassAudio   bool          // forward inbound audio downstream (to STT)
}

// DefaultProcessorConfig returns recommended Silero VAD settings.
func DefaultProcessorConfig() ProcessorConfig {
	return ProcessorConfig{
		StaleAfter:  100 * time.Millisecond,
		EnergyGate:  0.0,
		StartThresh: 0.5,
		StopThresh:  0.35,
		StartChunks: 2,
		StopChunks:  25,
		PassAudio:   true,
	}
}

type pendingChunk struct {
	samples  [ChunkSamples]float32
	seq      uint64
	due      time.Time
	traceCtx context.Context
}

// Processor is an ordinary core.Processor that delegates inference to a shared Executor.
// Streaming recurrent state is session-scoped.
type Processor struct {
	cfg  ProcessorConfig
	exec SharedExecutor
	s    core.SessionController
	idx  int
	st   Stream

	job             Job
	busy            bool
	sampleBuf       []float32
	pending         []pendingChunk
	maxPend         int
	ctx             [ContextSamples]float32
	state           [StateLen]float32
	checkpointState [StateLen]float32 // parent-owned state checkpoint for crash recovery

	speaking bool
	muted    bool // input muted: ignore audio and results, so no segment opens
	discard  bool // the in-flight result was submitted before a mute: drop it
	above    int
	below    int

	droppedCount atomic.Int64
	traceMu      sync.Mutex
	jobTrace     *vadJobTrace
	closed       bool
}

// NewProcessor creates a VAD Processor backed by the given Executor.
func NewProcessor(exec SharedExecutor, cfg ProcessorConfig) *Processor {
	return &Processor{
		exec:    exec,
		cfg:     cfg,
		maxPend: 16, // allows buffering up to ~500ms before dropping oldest
	}
}

func (p *Processor) Attach(s core.SessionController, index int) error {
	p.s = s
	p.idx = index
	p.st = p.exec.Register(s.ID())
	p.job.Done = func(j *Job) {
		p.traceMu.Lock()
		trace := p.jobTrace
		p.traceMu.Unlock()
		p.reenterResult(&vadResultFrame{j: j, trace: trace})
	}
	return nil
}

// reenterResult delivers a finished job. A full inbox refuses internal frames;
// the result must not be lost (busy is cleared only when it is processed), so
// retry until the session takes it or closes.
func (p *Processor) reenterResult(f *vadResultFrame) {
	err := p.s.Reenter(f, core.Downstream, p.idx, p.s.CurrentGeneration())
	if errors.Is(err, core.ErrQueueFull) {
		time.AfterFunc(resultRetryDelay, func() { p.reenterResult(f) })
		return
	}
	if err != nil {
		f.trace.finish(errors.New("canceled"))
	}
}

const resultRetryDelay = 5 * time.Millisecond

func (p *Processor) Close() error {
	p.traceMu.Lock()
	p.closed = true
	trace := p.jobTrace
	p.jobTrace = nil
	p.traceMu.Unlock()
	trace.finish(errors.New("canceled"))
	if p.st != nil {
		p.st.Close()
	}
	return nil
}

func (p *Processor) DroppedCount() int64 {
	return p.droppedCount.Load()
}

func (p *Processor) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	switch v := f.(type) {
	case *core.InputAudioFrame:
		if dir != core.Downstream {
			return e.PushDirection(f, dir)
		}
		if p.muted {
			return nil // queued before the mute; the session drops later audio itself
		}

		if p.cfg.PassAudio {
			if err := e.Push(f); err != nil {
				return err
			}
		}

		p.appendAudioSamples(ctx, v.Samples, v.Seq, v.Timestamp, e)
		return p.trySubmit(e)

	case *core.InputMutedStateFrame:
		if err := p.setMuted(v.Muted, e); err != nil {
			return err
		}
		return e.PushDirection(f, dir)

	case *vadResultFrame:
		return p.onTraceResult(v, e)
	}

	return e.PushDirection(f, dir)
}

// setMuted ends an open speech segment when the input is muted: muted audio
// never arrives, so the quiet chunks that would end it never come and the
// user's turn would hang. While muted nothing is buffered or detected.
func (p *Processor) setMuted(muted bool, e core.Emitter) error {
	p.muted = muted
	if !muted {
		// Start fresh: mid-speech recurrent state would open a phantom segment.
		p.state, p.checkpointState, p.ctx = [StateLen]float32{}, [StateLen]float32{}, [ContextSamples]float32{}
		return nil
	}
	p.discard = p.discard || p.busy
	p.sampleBuf, p.pending, p.above, p.below = p.sampleBuf[:0], nil, 0, 0
	if !p.speaking {
		return nil
	}
	p.speaking = false
	var gen uint64
	if p.s != nil {
		gen = p.s.CurrentGeneration()
	}
	return e.Push(&core.UserStoppedSpeakingFrame{Timestamp: time.Now(), Gen: gen})
}

func (p *Processor) appendAudioSamples(ctx context.Context, raw []byte, seq uint64, ts time.Time, e core.Emitter) {
	numSamples := len(raw) / 2
	if numSamples == 0 {
		return
	}

	// Convert PCM16LE to float32 normalized in [-1.0, 1.0]
	for i := 0; i < numSamples; i++ {
		s16 := int16(binary.LittleEndian.Uint16(raw[i*2 : i*2+2]))
		f := float32(s16) / 32768.0
		p.sampleBuf = append(p.sampleBuf, f)
	}

	for len(p.sampleBuf) >= ChunkSamples {
		var chunk [ChunkSamples]float32
		copy(chunk[:], p.sampleBuf[:ChunkSamples])
		p.sampleBuf = p.sampleBuf[ChunkSamples:]

		if p.cfg.EnergyGate > 0 && rms(chunk[:]) < p.cfg.EnergyGate {
			// Silent line: skip inference, reset recurrent state
			p.state = [StateLen]float32{}
			p.checkpointState = [StateLen]float32{}
			copy(p.ctx[:], chunk[ChunkSamples-ContextSamples:])
			_ = p.detect(0, seq, ts, e, nil)
			continue
		}

		if len(p.pending) >= p.maxPend {
			// Drop oldest pending chunk
			p.pending = p.pending[1:]
			p.droppedCount.Add(1)
		}

		p.pending = append(p.pending, pendingChunk{
			samples:  chunk,
			seq:      seq,
			due:      ts,
			traceCtx: ctx,
		})
	}
}

func rms(x []float32) float32 {
	var s float64
	for _, v := range x {
		s += float64(v) * float64(v)
	}
	return float32(math.Sqrt(s / float64(len(x))))
}

func (p *Processor) trySubmit(e core.Emitter) error {
	for !p.busy && len(p.pending) > 0 {
		p.traceMu.Lock()
		closed := p.closed
		p.traceMu.Unlock()
		if closed {
			return nil
		}
		pc := p.pending[0]
		p.pending = p.pending[1:]

		j := &p.job
		j.Seq = pc.seq
		j.Due = pc.due
		if p.cfg.StaleAfter > 0 {
			j.Deadline = pc.due.Add(p.cfg.StaleAfter)
		} else {
			j.Deadline = time.Time{}
		}

		copy(j.Input[:ContextSamples], p.ctx[:])
		copy(j.Input[ContextSamples:], pc.samples[:])
		j.State = p.state
		copy(p.ctx[:], pc.samples[ChunkSamples-ContextSamples:])

		if p.st == nil {
			return nil
		}
		// Every chunk passes here (~31/s per call), so build the trace only when
		// the session is actually tracing; vadJobTrace methods are nil-safe.
		var trace *vadJobTrace
		if tc := core.TraceContextFromContext(pc.traceCtx); p.s != nil && !isNoopTracer(tc.Tracer) {
			session := core.SessionID(strconv.FormatUint(p.s.ID(), 10))
			gen := core.Generation(p.s.CurrentGeneration())
			trace = &vadJobTrace{span: core.StartContextSpan(pc.traceCtx, "vad.inference", session, "", gen, map[string]any{"component": "vad"}), policy: tc.Policy, session: session, gen: gen, submitted: time.Now()}
			trace.event("vad.job", map[string]any{"sequence": pc.seq})
		}
		p.traceMu.Lock()
		if p.closed {
			p.traceMu.Unlock()
			trace.finish(errors.New("canceled"))
			return nil
		}
		p.jobTrace = trace
		p.traceMu.Unlock()

		submitErr := func() (err error) {
			defer func() {
				if v := recover(); v != nil {
					trace.finish(errors.New("vad_failure"))
					panic(v)
				}
			}()
			return p.st.Submit(j)
		}()
		if submitErr != nil {
			trace.finish(errors.New("vad_failure"))
			p.traceMu.Lock()
			p.jobTrace = nil
			p.traceMu.Unlock()
			p.droppedCount.Add(1)
			continue
		}
		p.busy = true
	}
	return nil
}

func (p *Processor) onTraceResult(frame *vadResultFrame, e core.Emitter) error {
	j, trace := frame.j, frame.trace
	p.traceMu.Lock()
	if p.jobTrace == trace {
		p.jobTrace = nil
	}
	p.traceMu.Unlock()
	var traceFailure error
	defer func() {
		if v := recover(); v != nil {
			trace.finish(errors.New("vad_failure"))
			panic(v)
		}
		trace.finish(traceFailure)
	}()
	p.busy = false

	if p.discard {
		// Submitted before a mute: its state is from speech the user muted.
		p.discard = false
		return p.trySubmit(e)
	}
	if j.Err != nil {
		traceFailure = errors.New("vad_failure")
		// Recovery from parent-owned state checkpoint:
		// Unknown or crashed response cannot corrupt session detector state
		p.state = p.checkpointState
		p.droppedCount.Add(1)
	} else {
		p.state = j.State
		p.checkpointState = j.State // Update parent-owned checkpoint
		if err := p.detect(j.Prob, j.Seq, j.Due, e, trace); err != nil {
			traceFailure = errors.New("vad_failure")
			return err
		}
	}

	return p.trySubmit(e)
}

func (p *Processor) detect(prob float32, seq uint64, due time.Time, e core.Emitter, trace *vadJobTrace) error {
	if p.muted {
		return nil // a result for a chunk submitted before the mute
	}
	if !p.speaking {
		if prob >= p.cfg.StartThresh {
			p.above++
		} else {
			p.above = 0
		}
		if p.above >= p.cfg.StartChunks {
			trace.event("vad.speech_start", map[string]any{"sequence": seq})
			p.speaking = true
			p.below = 0
			var gen uint64
			if p.s != nil {
				gen = p.s.CurrentGeneration()
			}
			return e.Push(&core.UserStartedSpeakingFrame{
				Seq:       seq,
				Timestamp: due,
				Gen:       gen,
			})
		}
		return nil
	}

	if prob < p.cfg.StopThresh {
		p.below++
	} else {
		p.below = 0
	}
	if p.below >= p.cfg.StopChunks {
		trace.event("vad.speech_stop", map[string]any{"sequence": seq})
		p.speaking = false
		p.above = 0
		var gen uint64
		if p.s != nil {
			gen = p.s.CurrentGeneration()
		}
		chunkDuration := time.Duration(ChunkSamples) * time.Second / time.Duration(SampleRate)
		hangover := time.Duration(p.cfg.StopChunks) * chunkDuration
		return e.Push(&core.UserStoppedSpeakingFrame{
			Seq:             seq,
			Timestamp:       due,
			Gen:             gen,
			SilenceDuration: hangover,
		})
	}
	return nil
}

func isNoopTracer(t core.Tracer) bool {
	_, noop := t.(core.NoopTracer)
	return noop
}
