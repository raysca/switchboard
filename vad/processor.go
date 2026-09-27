package vad

import (
	"context"
	"encoding/binary"
	"math"
	"sync/atomic"
	"time"

	"govoice/core"
)

// vadResultFrame re-enters the pipeline at the VAD processor when inference completes.
type vadResultFrame struct {
	j *Job
}

func (*vadResultFrame) Type() core.FrameType { return core.FrameInternal }

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
	samples [ChunkSamples]float32
	seq     uint64
	due     time.Time
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
	above    int
	below    int

	droppedCount atomic.Int64
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
		_ = s.Reenter(&vadResultFrame{j: j}, core.Downstream, index, s.CurrentGeneration())
	}
	return nil
}

func (p *Processor) Close() error {
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

		if p.cfg.PassAudio {
			if err := e.Push(f); err != nil {
				return err
			}
		}

		p.appendAudioSamples(v.Samples, v.Seq, v.Timestamp, e)
		return p.trySubmit(e)

	case *vadResultFrame:
		return p.onResult(v.j, e)
	}

	return e.PushDirection(f, dir)
}

func (p *Processor) appendAudioSamples(raw []byte, seq uint64, ts time.Time, e core.Emitter) {
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
			_ = p.detect(0, seq, ts, e)
			continue
		}

		if len(p.pending) >= p.maxPend {
			// Drop oldest pending chunk
			p.pending = p.pending[1:]
			p.droppedCount.Add(1)
		}

		p.pending = append(p.pending, pendingChunk{
			samples: chunk,
			seq:     seq,
			due:     ts,
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

		if err := p.st.Submit(j); err != nil {
			p.droppedCount.Add(1)
			continue
		}
		p.busy = true
	}
	return nil
}

func (p *Processor) onResult(j *Job, e core.Emitter) error {
	p.busy = false

	if j.Err != nil {
		// Recovery from parent-owned state checkpoint:
		// Unknown or crashed response cannot corrupt session detector state
		p.state = p.checkpointState
		p.droppedCount.Add(1)
	} else {
		p.state = j.State
		p.checkpointState = j.State // Update parent-owned checkpoint
		if err := p.detect(j.Prob, j.Seq, j.Due, e); err != nil {
			return err
		}
	}

	return p.trySubmit(e)
}

func (p *Processor) detect(prob float32, seq uint64, due time.Time, e core.Emitter) error {
	if !p.speaking {
		if prob >= p.cfg.StartThresh {
			p.above++
		} else {
			p.above = 0
		}
		if p.above >= p.cfg.StartChunks {
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
		p.speaking = false
		p.above = 0
		var gen uint64
		if p.s != nil {
			gen = p.s.CurrentGeneration()
		}
		return e.Push(&core.UserStoppedSpeakingFrame{
			Seq:       seq,
			Timestamp: due,
			Gen:       gen,
		})
	}
	return nil
}
