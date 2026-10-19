package google

import (
	"context"
	"testing"

	"govoice/core"
)

// BenchmarkUnaryUnit measures one unary unit end to end: 1 s of PCM16 at
// 24 kHz in, 50 audio frames out, until its TTSStoppedFrame.
func BenchmarkUnaryUnit(b *testing.B) {
	pcm := make([]byte, 48000)
	cfg := DefaultTTSConfig()
	cfg.Client = &mockTTSClient{synthesizeFn: func(context.Context, string, string, int) ([]byte, error) { return pcm, nil }}
	proc, err := NewTTSProcessor(cfg)
	if err != nil {
		b.Fatal(err)
	}
	pipe, _ := core.NewPipeline(proc)
	session, _ := core.NewSession(1, pipe, core.DefaultResourceBudget())
	stopped := make(chan struct{}, 1)
	session.OnOutput = func(f core.Frame) {
		if _, ok := f.(*core.TTSStoppedFrame); ok {
			stopped <- struct{}{}
		}
	}
	_ = proc.Attach(session, 0)
	_ = session.Start()
	defer session.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = session.Post(&core.SynthesisUnitFrame{Turn: "t", Gen: 1, UnitID: uint64(i + 1), Text: "Hello there.", IsFinal: true})
		<-stopped
	}
}
