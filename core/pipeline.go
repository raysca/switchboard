package core

import (
	"context"
	"errors"
	"fmt"
)

var (
	ErrPipelineEmpty = errors.New("pipeline is empty")
)

// Processor processes incoming frames on the session run loop.
// Processors are executed inline sequentially per session, never concurrently with themselves.
type Processor interface {
	ProcessFrame(ctx context.Context, f Frame, dir Direction, e Emitter) error
}

// Emitter allows a processor to emit zero, one, or multiple frames either downstream or upstream.
type Emitter interface {
	Push(f Frame) error
	PushDirection(f Frame, dir Direction) error
}

// Attacher is an optional interface: called at session initialization to give the
// processor access to its index in the pipeline and the session supervisor.
type Attacher interface {
	Attach(s SessionController, index int) error
}

// Closer is an optional interface: called during orderly session teardown.
type Closer interface {
	Close() error
}

// SessionController is the restricted interface exposed to processors via Attacher.
type SessionController interface {
	ID() uint64
	CurrentGeneration() uint64
	Go(fn func(ctx context.Context)) error
	Reenter(f Frame, dir Direction, resumeIndex int, gen uint64) error
	IncrementGeneration() uint64
}

// Pipeline represents an ordered sequence of processors.
type Pipeline struct {
	processors []Processor
}

// NewPipeline creates a new ordered Pipeline from the given slice of processors.
func NewPipeline(procs ...Processor) (*Pipeline, error) {
	if len(procs) == 0 {
		return nil, ErrPipelineEmpty
	}
	return &Pipeline{
		processors: procs,
	}, nil
}

// Processors returns a shallow copy of the processors slice.
func (p *Pipeline) Processors() []Processor {
	cp := make([]Processor, len(p.processors))
	copy(cp, p.processors)
	return cp
}

// Len returns the count of processors in the pipeline.
func (p *Pipeline) Len() int {
	return len(p.processors)
}

// At returns the processor at the given index.
func (p *Pipeline) At(index int) Processor {
	if index < 0 || index >= len(p.processors) {
		return nil
	}
	return p.processors[index]
}

// Validate ensures all processors in the pipeline are non-nil.
func (p *Pipeline) Validate() error {
	for i, proc := range p.processors {
		if proc == nil {
			return fmt.Errorf("nil processor at index %d", i)
		}
	}
	return nil
}
