package ttsunit

import (
	"bytes"
	"testing"

	"govoice/core"
)

type nopSpan struct{}

func (nopSpan) SetAttribute(string, any) {}
func (nopSpan) AddEvent(core.TraceEvent) {}
func (nopSpan) End(error)                {}

// sinkSession swallows re-entered frames, handing each to onFrame.
type sinkSession struct {
	core.SessionController
	onFrame func(core.Frame)
}

func (s *sinkSession) Reenter(f core.Frame, _ core.Direction, _ int, _ uint64) error {
	if s.onFrame != nil {
		s.onFrame(f)
	}
	return nil
}
func (*sinkSession) CurrentGeneration() uint64 { return 1 }
func (*sinkSession) ID() uint64                { return 1 }

func newBenchUnit(s core.SessionController) *Unit {
	r := New(Config{Provider: "fake", Model: "m", SampleRate: 24000})
	return &Unit{
		Frame: &core.SynthesisUnitFrame{Turn: "t", Gen: 1, UnitID: 1, Text: "hi", IsFinal: true},
		r:     r, s: s, span: nopSpan{},
	}
}

// writeSecond streams 1 s of 24 kHz PCM16 (50 chunks) in 4 provider-sized pieces.
func writeSecond(u *Unit, pcm []byte) {
	q := len(pcm) / 4
	for i := 0; i < 4; i++ {
		_ = u.Write(pcm[i*q : (i+1)*q])
	}
	_ = u.Finish()
}

func BenchmarkUnitWrite(b *testing.B) {
	pcm := make([]byte, 48000)
	s := &sinkSession{}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		writeSecond(newBenchUnit(s), pcm)
	}
}

// R3: a chunk is a sub-slice of the unit's buffer, not a per-chunk copy.
func TestUnitEmitAllocatesOncePerFrame(t *testing.T) {
	pcm := make([]byte, 48000)
	s := &sinkSession{}
	got := testing.AllocsPerRun(50, func() { writeSecond(newBenchUnit(s), pcm) })
	// Measured 64 after (114 before: one extra copy per chunk): 50 frames plus
	// started/stopped/usage, buffer growth and the unit itself.
	if got > 68 {
		t.Fatalf("allocs per 1 s unit = %v, want one per frame (<= 68)", got)
	}
}

func TestUnitFramesDoNotShareCapacity(t *testing.T) {
	var frames []*core.OutputAudioFrame
	s := &sinkSession{onFrame: func(f core.Frame) {
		if a, ok := f.(*core.OutputAudioFrame); ok {
			frames = append(frames, a)
		}
	}}
	pcm := bytes.Repeat([]byte{1, 2}, 24000)
	writeSecond(newBenchUnit(s), pcm)
	if len(frames) < 3 {
		t.Fatalf("frames = %d", len(frames))
	}
	for i, f := range frames {
		if cap(f.Samples) != len(f.Samples) {
			t.Fatalf("frame %d: cap %d != len %d", i, cap(f.Samples), len(f.Samples))
		}
	}
	before := append([]byte(nil), frames[1].Samples...)
	_ = append(frames[0].Samples, 0xFF, 0xFF, 0xFF)
	if !bytes.Equal(frames[1].Samples, before) {
		t.Fatal("append to frame 0 overwrote frame 1")
	}
}

// The provider's buffer is never retained: reusing it after Write must not
// change an emitted frame (Write copies into the unit's own buffer once).
func TestUnitEmittedFramesDoNotAliasProviderBuffer(t *testing.T) {
	var frames []*core.OutputAudioFrame
	s := &sinkSession{onFrame: func(f core.Frame) {
		if a, ok := f.(*core.OutputAudioFrame); ok {
			frames = append(frames, a)
		}
	}}
	u := newBenchUnit(s)
	prov := bytes.Repeat([]byte{7}, 4000)
	_ = u.Write(prov)
	for i := range prov {
		prov[i] = 9
	}
	_ = u.Finish()
	for i, f := range frames {
		if bytes.IndexByte(f.Samples, 9) >= 0 {
			t.Fatalf("frame %d aliases the provider buffer", i)
		}
	}
}
