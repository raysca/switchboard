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

	const (
		frameSamples   = 160
		trainingFrames = 100
		totalFrames    = 400
	)
	wanted := make([]int16, frameSamples*totalFrames)
	mixed := make([]int16, len(wanted))
	state := uint32(7)
	phase := 0.0
	for i := range wanted {
		frame := i / frameSamples
		if frame >= trainingFrames {
			syllableFrame := (frame - trainingFrames) % 40
			if syllableFrame < 26 {
				syllable := (frame - trainingFrames) / 40
				position := float64(syllableFrame*frameSamples+i%frameSamples) / float64(26*frameSamples)
				envelope := math.Sin(math.Pi * position)
				fundamental := 125 + 18*math.Sin(2*math.Pi*position) + float64(13*(syllable%5))
				phase += 2 * math.Pi * fundamental / 16000
				voice := envelope * (7200*math.Sin(phase) +
					2600*math.Sin(2*phase+0.3) + 1300*math.Sin(3*phase+0.7) +
					700*math.Sin(5*phase+0.2))
				wanted[i] = int16(math.Round(voice))
			}
		}
		state = state*1664525 + 1013904223
		noise := int16(int32(state>>16)%1801 - 900)
		mixed[i] = wanted[i] + noise
	}
	var output []byte
	for offset := 0; offset < len(mixed); offset += frameSamples {
		chunk, err := filter.Process(context.Background(), pcm(mixed[offset:offset+frameSamples]))
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

	evidence := measureSuppression(wanted, mixed, processed, trainingFrames*frameSamples)
	if !evidence.accepted() {
		t.Fatalf("wanted/noise evidence failed: gain=%g improvement=%g dB", evidence.gain, evidence.improvementDB)
	}
	t.Logf("Speex evidence: gain=%0.3f improvement=%0.2f dB", evidence.gain, evidence.improvementDB)

	negativeControls := []struct {
		name   string
		output []int16
	}{
		{"bypass", append([]int16(nil), mixed...)},
		{"fixed attenuation 0.9", scalePCM16(mixed, 0.9)},
		{"fixed attenuation 0.1", scalePCM16(mixed, 0.1)},
		{"all silence", make([]int16, len(mixed))},
		{"threshold gate", thresholdGatePCM16(mixed, 901)},
	}
	for _, tt := range negativeControls {
		t.Run(tt.name, func(t *testing.T) {
			control := measureSuppression(wanted, mixed, tt.output, trainingFrames*frameSamples)
			t.Logf("negative-control evidence: gain=%0.3f improvement=%0.2f dB", control.gain, control.improvementDB)
			if control.accepted() {
				t.Fatalf("negative control unexpectedly accepted: gain=%g improvement=%g dB", control.gain, control.improvementDB)
			}
		})
	}
}

type suppressionEvidence struct {
	gain          float64
	improvementDB float64
}

func (e suppressionEvidence) accepted() bool {
	// A 3 dB improvement halves equivalent residual-noise power. The 0.5 gain
	// floor allows at most about 6 dB wanted-signal attenuation and independently
	// rejects outputs that improve SNR by erasing the wanted signal.
	return e.gain >= 0.5 && e.improvementDB >= 3
}

func measureSuppression(wanted, mixed, output []int16, start int) suppressionEvidence {
	var wantedPower, projection, inputNoisePower float64
	for i := start; i < len(wanted); i++ {
		w := float64(wanted[i])
		wantedPower += w * w
		projection += float64(output[i]) * w
		d := float64(int(mixed[i]) - int(wanted[i]))
		inputNoisePower += d * d
	}
	gain := projection / wantedPower
	var outputResidualPower float64
	for i := start; i < len(wanted); i++ {
		d := float64(output[i]) - gain*float64(wanted[i])
		outputResidualPower += d * d
	}
	improvementDB := math.Inf(-1)
	if gain > 0 {
		equivalentResidualPower := outputResidualPower / (gain * gain)
		improvementDB = 10 * math.Log10(inputNoisePower/equivalentResidualPower)
	}
	return suppressionEvidence{
		gain:          gain,
		improvementDB: improvementDB,
	}
}

func scalePCM16(input []int16, scale float64) []int16 {
	output := make([]int16, len(input))
	for i, sample := range input {
		output[i] = int16(math.Round(float64(sample) * scale))
	}
	return output
}

func thresholdGatePCM16(input []int16, threshold int) []int16 {
	output := make([]int16, len(input))
	for i, sample := range input {
		if absInt(int(sample)) >= threshold {
			output[i] = sample
		}
	}
	return output
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
