package vad

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"govoice/voice"
)

// legacyResultFrame re-enters the pipeline at the legacy VAD processor when inference completes.
type legacyResultFrame struct{ j *Job }

func (*legacyResultFrame) Type() voice.FrameType { return voice.FrameInternal }

// LegacyProcessor is the prototype processor from the POC for use with govoice/voice.
type LegacyProcessor struct {
	cfg  ProcessorConfig
	exec *Executor
	s    *voice.Session
	idx  int
	st   Stream

	job     Job
	busy    bool
	pending []*voice.AudioFrame
	maxPend int
	ctx     [ContextSamples]float32
	state   [StateLen]float32

	speaking   bool
	above      int
	below      int
	abuseOn    atomic.Bool
	abuseFails int

	Host *HostMetrics
}

// HostMetrics decomposes latency across sessions for POC benchmarks.
type HostMetrics struct {
	SubmitLag voice.Hist
	Deliver   voice.Hist
	Total     voice.Hist
	Inferred  voice.Hist
	Gated     voice.Hist
	Dropped   voice.Hist
}

func NewLegacyProcessor(exec *Executor, cfg ProcessorConfig, host *HostMetrics) *LegacyProcessor {
	return &LegacyProcessor{exec: exec, cfg: cfg, Host: host}
}

func (p *LegacyProcessor) Attach(s *voice.Session, i int) error {
	p.s, p.idx = s, i
	p.st = p.exec.Register(s.ID)
	p.maxPend = max(1, int(s.Budget.MaxBufferedAudio/(32*time.Millisecond)))
	p.job.Done = func(j *Job) { s.Post(&legacyResultFrame{j}, voice.Downstream, i) }
	return nil
}

func (p *LegacyProcessor) Close() error {
	if p.st != nil {
		p.st.Close()
	}
	return nil
}

func (p *LegacyProcessor) ProcessFrame(ctx context.Context, f voice.Frame, dir voice.Direction, e voice.Emitter) error {
	switch v := f.(type) {
	case *voice.AudioFrame:
		if v.Output || dir != voice.Downstream {
			return e.PushDirection(f, dir)
		}
		if len(p.pending) == p.maxPend {
			p.pending = p.pending[1:]
			p.drop()
		}
		p.pending = append(p.pending, v)
		if p.cfg.PassAudio {
			if err := e.Push(f); err != nil {
				return err
			}
		}
		return p.trySubmit(e)
	case *legacyResultFrame:
		return p.onResult(v.j, e)
	}
	return e.PushDirection(f, dir)
}

func (p *LegacyProcessor) drop() {
	p.s.VADDropped.Add(1)
	p.s.Manager().CountVADDrop()
	if p.Host != nil {
		p.Host.Dropped.Record(0)
	}
}

func (p *LegacyProcessor) trySubmit(e voice.Emitter) error {
	for !p.busy && len(p.pending) > 0 {
		a := p.pending[0]
		p.pending = p.pending[1:]
		chunk := a.Samples
		if p.cfg.EnergyGate > 0 && rms(chunk) < p.cfg.EnergyGate {
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

func (p *LegacyProcessor) onResult(j *Job, e voice.Emitter) error {
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

func (p *LegacyProcessor) detect(prob float32, seq uint64, due time.Time, e voice.Emitter) error {
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

var ErrVADAbuse = errors.New("session exceeded VAD submission budget")

func (p *LegacyProcessor) SetAbuse() { p.abuseOn.Store(true) }

func (p *LegacyProcessor) abuse() error {
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
