package audio

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"

	"govoice/core"
)

type cancelAfterErrChecks struct {
	context.Context
	calls    int
	cancelAt int
}

func (c *cancelAfterErrChecks) Err() error {
	c.calls++
	if c.calls >= c.cancelAt {
		return context.Canceled
	}
	return nil
}

func TestAudioFilterMidProcessCancellationCanBeReused(t *testing.T) {
	format := core.AudioFormat{Encoding: core.PCM16LE, SampleRate: 16000, Channels: 1}
	filter, err := NewSpeexDenoiser(format)
	if err != nil {
		t.Fatal(err)
	}
	defer filter.Close()

	ctx := &cancelAfterErrChecks{Context: context.Background(), cancelAt: 3}
	if _, err := filter.Process(ctx, pcm(make([]int16, 320))); !errors.Is(err, context.Canceled) {
		t.Fatalf("mid-process cancellation: %v", err)
	}
	pending := filter.PendingSamples()
	if pending >= 160 {
		t.Fatalf("cancellation stranded %d samples", pending)
	}
	if _, err := filter.Process(context.Background(), pcm(make([]int16, 160-pending))); err != nil {
		t.Fatalf("reuse after cancellation: %v", err)
	}
	tail, err := filter.Flush(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) != 160*2 {
		t.Fatalf("reused stream returned %d bytes, want %d", len(tail), 160*2)
	}
}

func TestAudioFilterConcurrentCallsAreSerialized(t *testing.T) {
	format := core.AudioFormat{Encoding: core.PCM16LE, SampleRate: 16000, Channels: 1}
	filter, err := NewSpeexDenoiser(format)
	if err != nil {
		t.Fatal(err)
	}
	defer filter.Close()

	results := make(chan []byte, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for value := int16(1000); value <= 2000; value += 1000 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := filter.Process(context.Background(), pcm(make([]int16, 160)))
			results <- out
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	total := 0
	for out := range results {
		total += len(out)
	}
	tail, err := filter.Flush(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	total += len(tail)
	if total != 320*2 || filter.PendingSamples() >= 160 {
		t.Fatalf("serialized output=%d pending=%d", total, filter.PendingSamples())
	}
}

func TestAudioFilterLatencyDrainPreservesBoundaries(t *testing.T) {
	tests := []struct {
		name       string
		length     int
		impulses   []int
		chunkSizes []int
	}{
		{name: "frame-aligned", length: 320, impulses: []int{40, 280}, chunkSizes: []int{320}},
		{name: "partial-tail", length: 197, impulses: []int{13, 180}, chunkSizes: []int{17, 143, 37}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			format := core.AudioFormat{Encoding: core.PCM16LE, SampleRate: 16000, Channels: 1}
			filter, err := NewSpeexDenoiser(format)
			if err != nil {
				t.Fatal(err)
			}
			defer filter.Close()
			input := make([]int16, tt.length)
			for _, index := range tt.impulses {
				input[index] = 20000
			}
			var output []byte
			offset := 0
			for _, size := range tt.chunkSizes {
				chunk, err := filter.Process(context.Background(), pcm(input[offset:offset+size]))
				if err != nil {
					t.Fatal(err)
				}
				output = append(output, chunk...)
				offset += size
			}
			tail, err := filter.Flush(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			output = append(output, tail...)
			if len(output) != len(input)*2 {
				t.Fatalf("finite output length %d, want %d", len(output), len(input)*2)
			}
			decoded := decodePCM16(t, output)
			for _, index := range tt.impulses {
				peak := 0
				for i := max(0, index-3); i <= min(len(decoded)-1, index+3); i++ {
					if value := absInt(int(decoded[i])); value > peak {
						peak = value
					}
				}
				if peak < 1000 {
					t.Fatalf("impulse near sample %d was lost; peak=%d", index, peak)
				}
			}
		})
	}
}

func TestAudioFilterPreservesWantedSignalAndSuppressesNoise(t *testing.T) {
	format := core.AudioFormat{Encoding: core.PCM16LE, SampleRate: 16000, Channels: 1}
	filter, err := NewSpeexDenoiser(format)
	if err != nil {
		t.Fatal(err)
	}
	defer filter.Close()

	wanted := make([]int16, 160*200)
	mixed := make([]int16, len(wanted))
	state := uint32(7)
	for i := range wanted {
		frame := i / 160
		fundamental := 170 + 37*(frame%7)
		envelope := 0.55 + 0.45*math.Sin(math.Pi*float64(i%160)/160)
		voice := envelope * (7000*math.Sin(2*math.Pi*float64(fundamental)*float64(i)/16000) +
			2500*math.Sin(2*math.Pi*float64(2*fundamental)*float64(i)/16000))
		wanted[i] = int16(math.Round(voice))
		state = state*1664525 + 1013904223
		noise := int16(int32(state>>16)%1201 - 600)
		mixed[i] = wanted[i] + noise
	}
	var output []byte
	for offset := 0; offset < len(mixed); offset += 160 {
		chunk, err := filter.Process(context.Background(), pcm(mixed[offset:offset+160]))
		if err != nil {
			t.Fatal(err)
		}
		output = append(output, chunk...)
	}
	tail, err := filter.Flush(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	output = append(output, tail...)
	processed := decodePCM16(t, output)

	start := len(wanted) / 2
	var wantedPower, projection, inputNoisePower float64
	for i := start; i < len(wanted); i++ {
		w := float64(wanted[i])
		wantedPower += w * w
		projection += float64(processed[i]) * w
		d := float64(int(mixed[i]) - int(wanted[i]))
		inputNoisePower += d * d
	}
	gain := projection / wantedPower
	if gain < 0.08 {
		t.Fatalf("wanted signal not preserved: gain=%g", gain)
	}
	var outputNoisePower float64
	for i := start; i < len(wanted); i++ {
		d := float64(processed[i]) - gain*float64(wanted[i])
		outputNoisePower += d * d
	}
	if outputNoisePower >= inputNoisePower*0.9 {
		t.Fatalf("noise not reduced: input=%g output=%g gain=%g", inputNoisePower, outputNoisePower, gain)
	}
}

func TestResamplerRejectsUnsupportedRatesAndOversizeInput(t *testing.T) {
	for _, rate := range []int{8000, 16000, 24000, 48000} {
		r, err := NewResampler(rate, rate)
		if err != nil {
			t.Fatalf("supported rate %d rejected: %v", rate, err)
		}
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
	}
	for _, rates := range [][2]int{{0, 16000}, {1, int(^uint32(0))}, {8000, 48001}, {44100, 16000}} {
		if _, err := NewResampler(rates[0], rates[1]); err == nil {
			t.Fatalf("unsupported rates accepted: %d to %d", rates[0], rates[1])
		}
	}
	r, err := NewResampler(8000, 48000)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.latency <= 0 || r.latency > maxResamplerLatency {
		t.Fatalf("native latency %d outside bound", r.latency)
	}
	if _, err := r.Process(context.Background(), make([]int16, MaxResamplerInputSamples+1)); !errors.Is(err, ErrResamplerInputTooLarge) {
		t.Fatalf("oversize input: %v", err)
	}
}

func decodePCM16(t *testing.T, data []byte) []int16 {
	t.Helper()
	if len(data)%2 != 0 {
		t.Fatalf("odd PCM length %d", len(data))
	}
	out := make([]int16, len(data)/2)
	for i := range out {
		out[i] = int16(binaryLittleEndian(data[i*2:]))
	}
	return out
}

func binaryLittleEndian(data []byte) uint16 {
	return uint16(data[0]) | uint16(data[1])<<8
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
