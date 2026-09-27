package vad

import (
	"context"
	"errors"
	"math"
	"sync/atomic"
	"time"

	"govoice/voice"
)

// resultFrame re-enters the pipeline at the VAD processor when inference completes.
type resultFrame struct{ j *Job }

func (*resultFrame) Type() voice.FrameType { return voice.FrameInternal }

type ProcessorConfig struct {
	StaleAfter  time.Duration // job deadline = due + StaleAfter
	EnergyGate  float32       // RMS below this skips inference (0 = off)
	StartThresh float32
	StopThresh  float32
	StartChunks int
	StopChunks  int
	PassAudio   bool // forward inbound audio downstream (to STT)
}

func DefaultProcessorConfig() ProcessorConfig {
	return ProcessorConfig{StaleAfter: 100 * time.Millisecond, StartThresh: 0.5, StopThresh: 0.35,
		StartChunks: 2, StopChunks: 25, PassAudio: true}
}

// Processor is an ordinary FrameProcessor from the pipeline's point of view;
// inference is delegated to the shared Executor.
type Processor struct {
	cfg  ProcessorConfig
	exec *Executor
	s    *voice.Session
	idx  int
	st   *Stream

	job     Job
	busy    bool
	pending []*voice.AudioFrame // ring-ish FIFO, bounded by MaxBufferedAudio
	maxPend int
	ctx     [ContextSamples]float32
	state   [StateLen]float32

	speaking   bool
	above      int
	below      int
	abuseOn    atomic.Bool // fault: flood the executor with extra submissions
	abuseFails int

	// Host-wide instrumentation (optional).
	Host *HostMetrics
}

// HostMetrics decomposes latency (spec §9.2) across all sessions.
type HostMetrics struct {
	SubmitLag voice.Hist // due -> submitted (feeder + inbox + buffering)
	Deliver   voice.Hist // inference finished -> result applied in session
	Total     voice.Hist // due -> result applied
	Inferred  voice.Hist // count only
	Gated     voice.Hist
	Dropped   voice.Hist // stale or rejected, count only
}

func NewProcessor(exec *Executor, cfg ProcessorConfig, host *HostMetrics) *Processor {
	return &Processor{exec: exec, cfg: cfg, Host: host}
}

func (p *Processor) Attach(s *voice.Session, i int) error {
	p.s, p.idx = s, i
	p.st = p.exec.Register(s.ID)
	p.maxPend = max(1, int(s.Budget.MaxBufferedAudio/(32*time.Millisecond)))
	p.job.Done = func(j *Job) { s.Post(&resultFrame{j}, voice.Downstream, i) }
	return nil
}

func (p *Processor) Close() error {
	if p.st != nil {
		p.st.Close()
	}
	return nil
}

func (p *Processor) ProcessFrame(ctx context.Context, f voice.Frame, dir voice.Direction, e voice.Emitter) error {
	switch v := f.(type) {
	case *voice.AudioFrame:
		if v.Output || dir != voice.Downstream {
			return e.PushDirection(f, dir)
		}
		if len(p.pending) == p.maxPend { // stale: drop oldest
			p.pending = p.pending[1:]
			p.drop()
		}
		p.pending = append(p.pending, v)
		if p.cfg.PassAudio {
			if err := e.Push(f); err != nil {
				return err
			}
		}
		if p.abuseOn.Load() {
			if err := p.abuse(); err != nil {
				return err
			}
		}
		return p.trySubmit(e)
	case *resultFrame:
		return p.onResult(v.j, e)
	}
	return e.PushDirection(f, dir)
}

func (p *Processor) drop() {
	p.s.VADDropped.Add(1)
	p.s.Manager().CountVADDrop()
	if p.Host != nil {
		p.Host.Dropped.Record(0)
	}
}

func rms(x []float32) float32 {
	var s float64
	for _, v := range x {
		s += float64(v) * float64(v)
	}
	return float32(math.Sqrt(s / float64(len(x))))
}

func (p *Processor) trySubmit(e voice.Emitter) error {
	for !p.busy && len(p.pending) > 0 {
		a := p.pending[0]
		p.pending = p.pending[1:]
		chunk := a.Samples
		if p.cfg.EnergyGate > 0 && rms(chunk) < p.cfg.EnergyGate {
			// Silent line: skip inference, reset recurrent state (as after a long pause).
			p.state = [StateLen]float32{}
			copy(p.ctx[:], chunk[ChunkSamples-ContextSamples:])
			if p.Host != nil {
				p.Host.Gated.Record(0)
			}
			if err := p.detect(0, a.Seq, a.Due, e); err != nil {
				return err
			}
			continue
		}
		j := &p.job
		j.Seq, j.Due, j.Deadline = a.Seq, a.Due, a.Due.Add(p.cfg.StaleAfter)
		copy(j.Input[:ContextSamples], p.ctx[:])
		copy(j.Input[ContextSamples:], chunk)
		j.State = p.state
		copy(p.ctx[:], chunk[ChunkSamples-ContextSamples:])
		if err := p.st.Submit(j); err != nil {
			p.drop()
			continue
		}
		p.busy = true
	}
	return nil
}

func (p *Processor) onResult(j *Job, e voice.Emitter) error {
	p.busy = false
	now := time.Now()
	if j.Err != nil {
		p.drop()
	} else {
		p.state = j.State
		lat := now.Sub(j.Due)
		p.s.VADLatency.Record(lat)
		p.s.Manager().VADLatency.Record(lat)
		if h := p.Host; h != nil {
			h.SubmitLag.Record(j.Submitted.Sub(j.Due))
			h.Deliver.Record(now.Sub(j.Finished))
			h.Total.Record(lat)
			h.Inferred.Record(0)
		}
		if err := p.detect(j.Prob, j.Seq, j.Due, e); err != nil {
			return err
		}
	}
	return p.trySubmit(e)
}

func (p *Processor) detect(prob float32, seq uint64, due time.Time, e voice.Emitter) error {
	if !p.speaking {
		if prob >= p.cfg.StartThresh {
			p.above++
		} else {
			p.above = 0
		}
		if p.above >= p.cfg.StartChunks {
			p.speaking, p.below = true, 0
			return e.Push(&voice.SpeechStartedFrame{Seq: seq, Due: due, EmittedAt: time.Now()})
		}
		return nil
	}
	if prob < p.cfg.StopThresh {
		p.below++
	} else {
		p.below = 0
	}
	if p.below >= p.cfg.StopChunks {
		p.speaking, p.above = false, 0
		return e.Push(&voice.SpeechStoppedFrame{Seq: seq, EmittedAt: time.Now()})
	}
	return nil
}

func (p *Processor) SetAbuse() { p.abuseOn.Store(true) }

var ErrVADAbuse = errors.New("session exceeded VAD submission budget")

// abuse simulates a buggy processor bypassing the one-outstanding rule. The
// executor rejects excess work; repeated rejections fail the session.
func (p *Processor) abuse() error {
	for i := 0; i < 20; i++ {
		j := &Job{Due: time.Now(), Deadline: time.Now().Add(time.Second), Done: func(*Job) {}}
		if err := p.st.Submit(j); err != nil {
			p.abuseFails++
		}
	}
	if p.abuseFails > 200 {
		return ErrVADAbuse
	}
	return nil
}
