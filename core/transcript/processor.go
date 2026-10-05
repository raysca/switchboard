package transcript

import (
	"context"
	"fmt"
	"sync"
	"time"

	"govoice/core"
)

// PolicyTimer and PolicyClock make receipt ordering and the deadline driver
// deterministic in tests without consulting provider/media timestamps.
type PolicyTimer interface {
	C() <-chan time.Time
	Stop() bool
}

type PolicyClock interface {
	Now() time.Time
	NewTimer(time.Duration) PolicyTimer
}

type realPolicyClock struct{}
type realPolicyTimer struct{ timer *time.Timer }

func (realPolicyClock) Now() time.Time { return time.Now() }
func (realPolicyClock) NewTimer(d time.Duration) PolicyTimer {
	return realPolicyTimer{timer: time.NewTimer(d)}
}
func (t realPolicyTimer) C() <-chan time.Time { return t.timer.C }
func (t realPolicyTimer) Stop() bool          { return t.timer.Stop() }

// Processor adapts typed pipeline frames to Finalizer operations. It consumes
// raw candidates/advance frames and emits only stable interims or marked
// committed finals; already-committed frames are forwarded unchanged.
type Processor struct {
	finalizer  *Finalizer
	legacyTurn core.TurnID
	legacyGen  core.Generation
	clock      PolicyClock
	policyMu   sync.Mutex
	lastPolicy time.Time
	session    core.SessionController
	index      int
	wake       chan struct{}
	startOnce  sync.Once
}

func NewProcessor(cfg Config) (*Processor, error) {
	finalizer, err := NewFinalizer(cfg)
	if err != nil {
		return nil, err
	}
	clock := cfg.PolicyClock
	if clock == nil {
		clock = realPolicyClock{}
	}
	return &Processor{finalizer: finalizer, clock: clock, wake: make(chan struct{}, 1)}, nil
}

// Attach starts exactly one session-owned deadline driver. It is woken when
// policy state changes and never invokes pipeline callbacks under finalizer locks.
func (p *Processor) Attach(session core.SessionController, index int) error {
	p.session, p.index = session, index
	var err error
	p.startOnce.Do(func() { err = session.Go(p.runDeadlineDriver) })
	return err
}

func (p *Processor) nextPolicyTime() time.Time {
	p.policyMu.Lock()
	defer p.policyMu.Unlock()
	now := p.clock.Now()
	if now.IsZero() {
		now = time.Unix(0, 1)
	}
	if !p.lastPolicy.IsZero() && !now.After(p.lastPolicy) {
		now = p.lastPolicy.Add(time.Nanosecond)
	}
	p.lastPolicy = now
	return now
}

func (p *Processor) policyTimeForDelay() time.Time {
	p.policyMu.Lock()
	defer p.policyMu.Unlock()
	now := p.clock.Now()
	if !p.lastPolicy.IsZero() && now.Before(p.lastPolicy) {
		return p.lastPolicy
	}
	return now
}

func (p *Processor) signalDeadlineDriver() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *Processor) runDeadlineDriver(ctx context.Context) {
	for {
		deadline, ok := p.finalizer.NextDeadline()
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-p.wake:
				continue
			}
		}
		d := deadline.Sub(p.policyTimeForDelay())
		if d < 0 {
			d = 0
		}
		timer := p.clock.NewTimer(d)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-p.wake:
			timer.Stop()
			continue
		case <-timer.C():
			if p.session != nil {
				_ = p.session.Reenter(&core.TranscriptAdvanceFrame{At: deadline}, core.Downstream, p.index, 0)
			}
			// Wait until the serialized pipeline has applied the advance before
			// inspecting the same deadline again.
			select {
			case <-ctx.Done():
				return
			case <-p.wake:
			}
		}
	}
}

func (p *Processor) ProcessFrame(_ context.Context, frame core.Frame, dir core.Direction, emitter core.Emitter) error {
	if dir != core.Downstream {
		return emitter.PushDirection(frame, dir)
	}
	var (
		outputs []Output
		err     error
		forward = true
	)
	policyAt := p.nextPolicyTime()
	switch value := frame.(type) {
	case *core.TranscriptCandidateFrame:
		forward = false
		outputs, err = p.finalizer.Observe(Candidate{
			Turn: value.Turn, Generation: value.Gen, Text: value.Text, At: policyAt, SourceAt: value.Timestamp,
			Final: value.Final, EndOfUtterance: value.EndOfUtterance,
			Stability: value.Stability, HasStability: value.HasStability,
		})
	case *core.TranscriptAdvanceFrame:
		forward = false
		outputs, err = p.finalizer.Advance(policyAt)
	case *core.TurnEventFrame:
		switch value.Kind {
		case core.TurnEventStarted:
			p.legacyTurn, p.legacyGen = value.Turn, value.Gen
		case core.TurnEventCompleted:
			outputs, err = p.finalizer.Complete(value.Turn, value.Gen, policyAt)
		case core.TurnEventInterrupted:
			err = p.finalizer.Interrupt(value.Turn, value.Gen, policyAt)
		}
	case *core.UserStartedSpeakingFrame:
		// Explicit V1 adapter: legacy speech frames predate typed TurnID, and
		// both shipped STT providers historically derive this exact identity.
		p.legacyTurn = core.TurnID(fmt.Sprintf("turn-%d-%d", value.Gen, value.Seq))
		p.legacyGen = core.Generation(value.Gen)
	case *core.UserStoppedSpeakingFrame:
		if p.legacyTurn != "" && p.legacyGen == core.Generation(value.Gen) {
			outputs, err = p.finalizer.Complete(p.legacyTurn, p.legacyGen, policyAt)
		}
	}
	if err != nil {
		return err
	}
	if forward {
		if err := emitter.PushDirection(frame, dir); err != nil {
			return err
		}
	}
	p.signalDeadlineDriver()
	for _, output := range outputs {
		if output.Kind == Interim {
			if err := emitter.Push(&core.InterimTranscriptionFrame{
				Text: output.Text, Turn: string(output.Turn), Gen: uint64(output.Generation), Timestamp: output.SourceAt,
			}); err != nil {
				return err
			}
			continue
		}
		if err := emitter.Push(&core.FinalTranscriptionFrame{
			Text: output.Text, Turn: string(output.Turn), Gen: uint64(output.Generation), Timestamp: output.SourceAt, Committed: true,
		}); err != nil {
			return err
		}
	}
	return nil
}
