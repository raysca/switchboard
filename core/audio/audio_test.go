package audio

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"testing"
	"time"

	"govoice/core"
)

func pcm(samples []int16) []byte {
	b := make([]byte, len(samples)*2)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(s))
	}
	return b
}

func TestAudioFilterFormatLifecycleAndPartition(t *testing.T) {
	fmt := core.AudioFormat{Encoding: core.PCM16LE, SampleRate: 16000, Channels: 1}
	badFormats := []core.AudioFormat{
		{Encoding: core.PCM16LE, SampleRate: 12345, Channels: 1},
		{Encoding: core.Float32LE, SampleRate: 16000, Channels: 1},
		{Encoding: core.PCM16LE, SampleRate: 16000, Channels: 2},
	}
	for _, bad := range badFormats {
		if _, err := NewSpeexDenoiser(bad); err == nil {
			t.Fatalf("unsupported format accepted: %+v", bad)
		}
	}
	a, err := NewSpeexDenoiser(fmt)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if a.InputFormat() != fmt || a.OutputFormat() != fmt || a.Latency() != 10*time.Millisecond {
		t.Fatal("format or latency mismatch")
	}
	if _, err := a.Process(context.Background(), []byte{1}); err == nil {
		t.Fatal("odd PCM accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.Process(ctx, pcm(make([]int16, 160))); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	signal := make([]int16, 160*4+37)
	for i := range signal {
		signal[i] = int16(4000*math.Sin(2*math.Pi*330*float64(i)/16000) + 500*math.Sin(2*math.Pi*1700*float64(i)/16000))
	}
	all, err := a.Process(context.Background(), pcm(signal))
	if err != nil {
		t.Fatal(err)
	}
	tail, err := a.Flush(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	all = append(all, tail...)
	if len(all) != len(signal)*2 {
		t.Fatalf("length %d", len(all))
	}
	if err := a.Reset(); err != nil {
		t.Fatal(err)
	}
	var parts []byte
	for _, p := range [][]int16{signal[:13], signal[13:173], signal[173:421], signal[421:]} {
		b, err := a.Process(context.Background(), pcm(p))
		if err != nil {
			t.Fatal(err)
		}
		parts = append(parts, b...)
		if a.PendingSamples() >= 160 {
			t.Fatal("unbounded partial frame")
		}
	}
	tail, err = a.Flush(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	parts = append(parts, tail...)
	if !bytes.Equal(all, parts) {
		t.Fatal("chunk partition changed signal")
	}
	if err := a.Reset(); err != nil {
		t.Fatal(err)
	}
	again, err := a.Process(context.Background(), pcm(signal))
	if err != nil {
		t.Fatal(err)
	}
	againTail, err := a.Flush(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(all, append(again, againTail...)) {
		t.Fatal("reset leaked prior stream")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Process(context.Background(), pcm(signal)); !errors.Is(err, ErrClosed) {
		t.Fatalf("post-close: %v", err)
	}
}

func TestAudioFilterConcurrentStreamsAreIsolated(t *testing.T) {
	format := core.AudioFormat{Encoding: core.PCM16LE, SampleRate: 16000, Channels: 1}
	input := pcm(make([]int16, 160*3))
	type result struct {
		output []byte
		err    error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			filter, err := NewSpeexDenoiser(format)
			if err != nil {
				results <- result{err: err}
				return
			}
			defer filter.Close()
			output, err := filter.Process(context.Background(), input)
			if err == nil {
				var tail []byte
				tail, err = filter.Flush(context.Background())
				output = append(output, tail...)
			}
			results <- result{output: output, err: err}
		}()
	}
	first, second := <-results, <-results
	if first.err != nil || second.err != nil {
		t.Fatalf("concurrent streams: %v, %v", first.err, second.err)
	}
	if !bytes.Equal(first.output, second.output) {
		t.Fatal("independent streams shared mutable signal state")
	}
}

func TestAudioConversionAndChannels(t *testing.T) {
	b, err := Float32ToPCM16LE([]float32{-2, -1, 0, 0.5, math.Nextafter32(1, 0), 1, 2})
	if err != nil {
		t.Fatal(err)
	}
	want := pcm([]int16{-32768, -32768, 0, 16384, 32767, 32767, 32767})
	if !bytes.Equal(b, want) {
		t.Fatalf("conversion %v", b)
	}
	if _, err := Float32ToPCM16LE([]float32{float32(math.NaN())}); err == nil {
		t.Fatal("NaN accepted")
	}
	if _, err := PCM16LEToFloat32([]byte{1}); err == nil {
		t.Fatal("odd PCM accepted")
	}
	mono, err := ConvertChannels(pcm([]int16{1000, 3000, -1000, 1000}), 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mono, pcm([]int16{2000, 0})) {
		t.Fatalf("downmix %v", mono)
	}
	stereo, err := ConvertChannels(mono, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stereo, pcm([]int16{2000, 2000, 0, 0})) {
		t.Fatalf("upmix %v", stereo)
	}
}

func TestPreRollAndRechunkBounds(t *testing.T) {
	p, err := NewPreRoll(4, OverflowDropOldest)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Push([]byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	if err := p.Push([]byte{4, 5, 6}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(p.Bytes(), []byte{3, 4, 5, 6}) {
		t.Fatalf("pre-roll %v", p.Bytes())
	}

	r, err := NewRechunker(3, 6, OverflowRejectNewest)
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := r.Push([]byte{1, 2, 3, 4, 5})
	if err != nil || len(chunks) != 1 || !bytes.Equal(chunks[0], []byte{1, 2, 3}) {
		t.Fatalf("rechunk %v %v", chunks, err)
	}
	if _, err := r.Push([]byte{6, 7, 8, 9, 10, 11, 12}); err == nil {
		t.Fatal("overflow accepted")
	}
	if !bytes.Equal(r.Flush(), []byte{4, 5}) {
		t.Fatal("rechunk remainder lost")
	}

	drop, err := NewRechunker(4, 6, OverflowDropOldest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := drop.Push([]byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	chunks, err = drop.Push([]byte{4, 5, 6, 7, 8})
	if err != nil || len(chunks) != 1 || !bytes.Equal(chunks[0], []byte{3, 4, 5, 6}) {
		t.Fatalf("drop-oldest rechunk %v %v", chunks, err)
	}
	if !bytes.Equal(drop.Flush(), []byte{7, 8}) {
		t.Fatal("drop-oldest did not retain newest bytes")
	}
}

func TestAudioBufferExactTurnBoundaries(t *testing.T) {
	disabled, err := NewTurnBuffer(false, 4, OverflowDropOldest)
	if err != nil {
		t.Fatal(err)
	}
	disabled.Start(core.TurnID("one"))
	if err := disabled.Push([]byte{1, 2}); err != nil {
		t.Fatal(err)
	}
	if got, err := disabled.End(core.TurnID("one")); err != nil || got != nil {
		t.Fatalf("disabled buffer retained audio: %v %v", got, err)
	}

	b, err := NewTurnBuffer(true, 4, OverflowDropOldest)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Push([]byte{0}); err != nil {
		t.Fatal(err)
	}
	b.Start(core.TurnID("one"))
	if err := b.Push([]byte{1, 2}); err != nil {
		t.Fatal(err)
	}
	b.Start(core.TurnID("two"))
	if err := b.Push([]byte{3, 4, 5}); err != nil {
		t.Fatal(err)
	}
	if got, err := b.End(core.TurnID("one")); err == nil || got != nil {
		t.Fatal("wrong turn ended")
	}
	got, err := b.End(core.TurnID("two"))
	if err != nil || !bytes.Equal(got, []byte{3, 4, 5}) {
		t.Fatalf("turn %v %v", got, err)
	}
	b.Start(core.TurnID("three"))
	if err := b.Push([]byte{1, 2, 3, 4, 5}); err != nil {
		t.Fatal(err)
	}
	got, err = b.End(core.TurnID("three"))
	if err != nil || !bytes.Equal(got, []byte{2, 3, 4, 5}) {
		t.Fatalf("overflow %v %v", got, err)
	}

	reject, err := NewTurnBuffer(true, 4, OverflowRejectNewest)
	if err != nil {
		t.Fatal(err)
	}
	reject.Start(core.TurnID("reject"))
	if err := reject.Push([]byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	if err := reject.Push([]byte{4, 5}); err == nil {
		t.Fatal("reject-newest overflow accepted")
	}
	got, err = reject.End(core.TurnID("reject"))
	if err != nil || !bytes.Equal(got, []byte{1, 2, 3}) {
		t.Fatalf("rejected bytes changed prior content: %v %v", got, err)
	}
	if _, err := NewTurnBuffer(true, 4, OverflowPolicy(99)); err == nil {
		t.Fatal("invalid turn-buffer overflow policy accepted")
	}
}

func TestResamplerStreamingCountAndAlias(t *testing.T) {
	if _, err := NewResampler(0, 16000); err == nil {
		t.Fatal("zero input rate accepted")
	}
	r, err := NewResampler(48000, 16000)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	in := make([]int16, 48000)
	for i := range in {
		in[i] = int16(12000*math.Sin(2*math.Pi*1000*float64(i)/48000) + 8000*math.Sin(2*math.Pi*12000*float64(i)/48000))
	}
	var out []int16
	for pos := 0; pos < len(in); {
		n := pos + 137
		if n > len(in) {
			n = len(in)
		}
		p, err := r.Process(context.Background(), in[pos:n])
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, p...)
		pos = n
	}
	tail, err := r.Flush(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out = append(out, tail...)
	if len(out) != 16000 {
		t.Fatalf("sample count %d", len(out))
	}
	var wanted, alias float64
	for i, s := range out {
		wanted += float64(s) * math.Sin(2*math.Pi*1000*float64(i)/16000)
		alias += float64(s) * math.Sin(2*math.Pi*4000*float64(i)/16000)
	}
	if math.Abs(wanted) < 1e7 {
		t.Fatalf("wanted tone was not preserved: amplitude=%g", wanted)
	}
	if math.Abs(alias) > math.Abs(wanted)*0.1 {
		t.Fatalf("alias too high: wanted=%g alias=%g", wanted, alias)
	}
	if err := r.Reset(); err != nil {
		t.Fatal(err)
	}
	one, err := r.Process(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	tail, err = r.Flush(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	one = append(one, tail...)
	if len(one) != len(out) {
		t.Fatalf("partition count %d vs %d", len(one), len(out))
	}
	for i := range one {
		if d := int(one[i]) - int(out[i]); d > 1 || d < -1 {
			t.Fatalf("partition difference at %d: %d", i, d)
		}
	}
	if err := r.Reset(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Process(ctx, in[:1]); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Process(context.Background(), in[:1]); !errors.Is(err, ErrClosed) {
		t.Fatalf("post-close %v", err)
	}
}
