package filter

import (
	"context"
	"encoding/binary"
	"sync/atomic"

	"govoice/core"
)

// Processor runs a Filter over InputAudioFrames. Only 16-bit little-endian mono
// frames at Rate are filtered; everything else (other rates, stereo, odd byte
// counts, empty frames, other frame types) passes through untouched. A filter
// error also fails open: the original frame is forwarded and Errors counts it.
//
// Frames are copy-on-write: the incoming frame and its buffer (which other
// stages may hold) are never mutated; a new frame with a new buffer is pushed.
// Build one Processor per session. When no filters are configured, do not
// insert a Processor at all.
type Processor struct {
	chain  Filter
	rate   int
	Errors atomic.Uint64
}

func NewProcessor(f Filter, rate int) *Processor { return &Processor{chain: f, rate: rate} }

func (p *Processor) ProcessFrame(_ context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	a, ok := f.(*core.InputAudioFrame)
	if !ok || dir != core.Downstream || a.SampleRate != p.rate || a.Channels > 1 ||
		(a.Encoding != core.EncodingPCM16LE && a.Encoding != "") || len(a.Samples) == 0 || len(a.Samples)%2 != 0 {
		return e.PushDirection(f, dir)
	}
	pcm := make([]int16, len(a.Samples)/2)
	for i := range pcm {
		pcm[i] = int16(binary.LittleEndian.Uint16(a.Samples[2*i:]))
	}
	out, err := p.chain.Process(pcm, a.SampleRate)
	if err != nil {
		p.Errors.Add(1)
		return e.PushDirection(f, dir)
	}
	b := make([]byte, 2*len(out))
	for i, s := range out {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(s))
	}
	cp := *a
	cp.Samples = b
	return e.PushDirection(&cp, dir)
}

// Close releases the filter chain; the pipeline closes processors on teardown.
func (p *Processor) Close() error { return p.chain.Close() }
