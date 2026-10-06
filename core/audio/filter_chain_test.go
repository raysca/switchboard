package audio

import (
	"context"
	"testing"
	"time"

	"govoice/core"
)

type dummyFilter struct {
	inFmt   core.AudioFormat
	outFmt  core.AudioFormat
	latency time.Duration
	gain    byte
	closed  bool
}

func (d *dummyFilter) InputFormat() core.AudioFormat  { return d.inFmt }
func (d *dummyFilter) OutputFormat() core.AudioFormat { return d.outFmt }
func (d *dummyFilter) Latency() time.Duration         { return d.latency }
func (d *dummyFilter) Process(ctx context.Context, in []byte) ([]byte, error) {
	out := make([]byte, len(in))
	for i, b := range in {
		out[i] = b + d.gain
	}
	return out, nil
}
func (d *dummyFilter) Flush(ctx context.Context) ([]byte, error) { return nil, nil }
func (d *dummyFilter) Reset() error                              { return nil }
func (d *dummyFilter) Close() error {
	d.closed = true
	return nil
}

func TestFilterChain_ExecutionAndBypass(t *testing.T) {
	fmtMono16k := core.AudioFormat{
		Encoding:   core.PCM16LE,
		SampleRate: 16000,
		Channels:   1,
	}

	fc := NewFilterChain()

	f1 := &dummyFilter{inFmt: fmtMono16k, outFmt: fmtMono16k, latency: 5 * time.Millisecond, gain: 1}
	f2 := &dummyFilter{inFmt: fmtMono16k, outFmt: fmtMono16k, latency: 10 * time.Millisecond, gain: 2}

	if err := fc.AddFilter("f1", f1); err != nil {
		t.Fatalf("AddFilter f1 failed: %v", err)
	}
	if err := fc.AddFilter("f2", f2); err != nil {
		t.Fatalf("AddFilter f2 failed: %v", err)
	}

	if fc.TotalLatency() != 15*time.Millisecond {
		t.Errorf("expected 15ms latency, got %v", fc.TotalLatency())
	}

	ctx := context.Background()
	in := []byte{10, 20}
	out, err := fc.Process(ctx, in)
	if err != nil {
		t.Fatalf("Process failed: %v", err)
	}
	// 10 + 1 + 2 = 13, 20 + 1 + 2 = 23
	if out[0] != 13 || out[1] != 23 {
		t.Errorf("unexpected output: %v", out)
	}

	// Bypass f1
	if err := fc.SetBypass("f1", true); err != nil {
		t.Fatalf("SetBypass failed: %v", err)
	}

	if fc.TotalLatency() != 10*time.Millisecond {
		t.Errorf("expected 10ms latency after bypass, got %v", fc.TotalLatency())
	}

	outBypassed, err := fc.Process(ctx, in)
	if err != nil {
		t.Fatalf("Process failed: %v", err)
	}
	// 10 + 2 = 12, 20 + 2 = 22
	if outBypassed[0] != 12 || outBypassed[1] != 22 {
		t.Errorf("unexpected output when bypassed: %v", outBypassed)
	}

	if err := fc.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	if !f1.closed || !f2.closed {
		t.Errorf("expected underlying filters to be closed")
	}
}

func TestFilterChain_IncompatibleFormatRejection(t *testing.T) {
	fmtMono16k := core.AudioFormat{Encoding: core.PCM16LE, SampleRate: 16000, Channels: 1}
	fmtMono24k := core.AudioFormat{Encoding: core.PCM16LE, SampleRate: 24000, Channels: 1}

	fc := NewFilterChain()
	f1 := &dummyFilter{inFmt: fmtMono16k, outFmt: fmtMono16k}
	f2 := &dummyFilter{inFmt: fmtMono24k, outFmt: fmtMono24k}

	if err := fc.AddFilter("f1", f1); err != nil {
		t.Fatalf("failed to add f1: %v", err)
	}

	err := fc.AddFilter("f2", f2)
	if err == nil {
		t.Fatalf("expected error adding incompatible filter format")
	}
}
