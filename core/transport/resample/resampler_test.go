package resample

import (
	"bytes"
	"encoding/binary"
	"math"
	"math/rand"
	"sync"
	"testing"
)

func makeSinePCM(sampleRate int, freqHz float64, durationSec float64) []byte {
	numSamples := int(float64(sampleRate) * durationSec)
	buf := make([]byte, numSamples*2)
	for i := 0; i < numSamples; i++ {
		t := float64(i) / float64(sampleRate)
		val := int16(16000 * math.Sin(2*math.Pi*freqHz*t))
		binary.LittleEndian.PutUint16(buf[i*2:i*2+2], uint16(val))
	}
	return buf
}

func TestResampler_ChunkContinuity(t *testing.T) {
	// Compare streaming 20ms chunks with continuous stream for 16kHz -> 24kHz
	r, err := NewResampler(16000, 24000)
	if err != nil {
		t.Fatalf("NewResampler error: %v", err)
	}

	totalDuration := 1.0 // 1 second
	fullAudio := makeSinePCM(16000, 440.0, totalDuration)
	chunkSize := 16000 * 20 / 1000 * 2 // 20ms = 320 samples = 640 bytes

	var streamedOutput []byte
	for offset := 0; offset+chunkSize <= len(fullAudio); offset += chunkSize {
		chunk := fullAudio[offset : offset+chunkSize]
		resampled, err := r.ResamplePCM16(chunk)
		if err != nil {
			t.Fatalf("ResamplePCM16 chunk error: %v", err)
		}
		streamedOutput = append(streamedOutput, resampled...)
	}

	// 1 second of 24kHz audio should have ~24000 samples = 48000 bytes
	expectedSamples := 24000
	actualSamples := len(streamedOutput) / 2
	diff := math.Abs(float64(actualSamples - expectedSamples))
	// Allow small delta due to edge interpolation boundary (< 5 samples)
	if diff > 5 {
		t.Errorf("Sample count drift: expected ~%d samples, got %d (diff %.1f)", expectedSamples, actualSamples, diff)
	}
}

func TestResampler_Downsample48To16(t *testing.T) {
	r, err := NewResampler(48000, 16000)
	if err != nil {
		t.Fatalf("NewResampler error: %v", err)
	}

	// 20ms @ 48kHz = 960 samples = 1920 bytes
	chunk48 := makeSinePCM(48000, 440.0, 0.02)
	resampled16, err := r.ResamplePCM16(chunk48)
	if err != nil {
		t.Fatalf("ResamplePCM16 error: %v", err)
	}

	// Expect 20ms @ 16kHz = 320 samples = 640 bytes
	if len(resampled16) != 640 {
		t.Errorf("Expected 640 bytes (320 samples), got %d bytes", len(resampled16))
	}
}

func TestResampler_Concurreny(t *testing.T) {
	r, _ := NewResampler(16000, 24000)
	var wg sync.WaitGroup
	chunk := makeSinePCM(16000, 440.0, 0.02)

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_, err := r.ResamplePCM16(chunk)
				if err != nil {
					t.Errorf("Resample error: %v", err)
				}
			}
		}()
	}
	wg.Wait()
}

func BenchmarkResampler_20msChunk(b *testing.B) {
	r, _ := NewResampler(16000, 24000)
	chunk := makeSinePCM(16000, 440.0, 0.02)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = r.ResamplePCM16(chunk)
	}
}

func randPCM(rng *rand.Rand, samples int) []byte {
	b := make([]byte, samples*2)
	for i := 0; i < samples; i++ {
		binary.LittleEndian.PutUint16(b[i*2:], uint16(int16(rng.Intn(65536)-32768)))
	}
	return b
}

func TestDownsample48To16_ChunkingInvariant(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	const total = 48000
	stream := randPCM(rng, total)

	whole, err := NewResampler(48000, 16000)
	if err != nil {
		t.Fatal(err)
	}
	want, err := whole.ResamplePCM16(stream)
	if err != nil {
		t.Fatal(err)
	}
	if len(want) != total/3*2 {
		t.Fatalf("single-shot length = %d, want %d", len(want), total/3*2)
	}

	chunked, _ := NewResampler(48000, 16000)
	var got []byte
	for off := 0; off < total; {
		n := 1 + rng.Intn(960)
		if off+n > total {
			n = total - off
		}
		out, err := chunked.ResamplePCM16(stream[off*2 : (off+n)*2])
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, out...)
		off += n
	}
	if len(got) != len(want) {
		t.Fatalf("chunked length = %d, want %d", len(got), len(want))
	}
	if !bytes.Equal(got, want) {
		t.Fatal("chunked output differs from single-shot output")
	}
}

func TestDownsample48To16_AllocsPerRun(t *testing.T) {
	r, _ := NewResampler(48000, 16000)
	chunk := makeSinePCM(48000, 440, 0.02)
	_, _ = r.ResamplePCM16(chunk)
	// One alloc: the exact-size result, which callers may retain.
	if a := testing.AllocsPerRun(100, func() { _, _ = r.ResamplePCM16(chunk) }); a > 1 {
		t.Fatalf("allocs/op = %v, want <= 1", a)
	}
}

func BenchmarkDownsample48To16(b *testing.B) {
	r, _ := NewResampler(48000, 16000)
	chunk := makeSinePCM(48000, 440, 0.02)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = r.ResamplePCM16(chunk)
	}
}

// refResampler is the general path as it was before it read straight from the input bytes
// (slice of samples, append-grown output): the reference the allocation-free path must match.
type refResampler struct {
	ratio, phase float64
	lastSample   int16
	hasHistory   bool
}

func (r *refResampler) resample(pcmIn []byte) []byte {
	samplesIn := make([]int16, len(pcmIn)/2)
	for i := range samplesIn {
		samplesIn[i] = int16(binary.LittleEndian.Uint16(pcmIn[i*2 : i*2+2]))
	}
	var samplesOut []int16
	phase := r.phase
	for {
		idx := int(math.Floor(phase))
		frac := phase - float64(idx)
		var s0, s1 float64
		if idx < 0 {
			if r.hasHistory {
				s0 = float64(r.lastSample)
			} else if len(samplesIn) > 0 {
				s0 = float64(samplesIn[0])
			}
			if len(samplesIn) > 0 {
				s1 = float64(samplesIn[0])
			}
		} else if idx >= len(samplesIn)-1 {
			break
		} else {
			s0 = float64(samplesIn[idx])
			s1 = float64(samplesIn[idx+1])
		}
		interpolated := s0 + frac*(s1-s0)
		if interpolated > 32767 {
			interpolated = 32767
		} else if interpolated < -32768 {
			interpolated = -32768
		}
		samplesOut = append(samplesOut, int16(math.Round(interpolated)))
		phase += r.ratio
	}
	r.phase = phase - float64(len(samplesIn))
	if len(samplesIn) > 0 {
		r.lastSample = samplesIn[len(samplesIn)-1]
		r.hasHistory = true
	}
	out := make([]byte, len(samplesOut)*2)
	for i, s := range samplesOut {
		binary.LittleEndian.PutUint16(out[i*2:i*2+2], uint16(s))
	}
	return out
}

func TestResamplerGeneralPathChunkingInvariant(t *testing.T) {
	for _, rates := range [][2]int{{16000, 24000}, {24000, 16000}} {
		rng := rand.New(rand.NewSource(7))
		const total = 24000
		stream := randPCM(rng, total)
		r, _ := NewResampler(rates[0], rates[1])
		ref := &refResampler{ratio: float64(rates[0]) / float64(rates[1])}
		for off := 0; off < total; {
			n := 1 + rng.Intn(960)
			if rng.Intn(4) == 0 {
				n = 1 // a single sample after history: the phase < 0 branch
			}
			if off+n > total {
				n = total - off
			}
			chunk := stream[off*2 : (off+n)*2]
			got, err := r.ResamplePCM16(chunk)
			if err != nil {
				t.Fatal(err)
			}
			if want := ref.resample(chunk); !bytes.Equal(got, want) {
				t.Fatalf("%d->%d chunk at sample %d (%d samples): %d bytes differ from the reference's %d", rates[0], rates[1], off, n, len(got), len(want))
			}
			if r.phase != ref.phase {
				t.Fatalf("%d->%d phase %v, reference %v", rates[0], rates[1], r.phase, ref.phase)
			}
			off += n
		}
	}
}

func TestResamplerGeneralPathAllocs(t *testing.T) {
	r, _ := NewResampler(16000, 24000)
	chunk := makeSinePCM(16000, 440, 0.02)
	_, _ = r.ResamplePCM16(chunk)
	// One alloc: the exact-size result, which callers may retain.
	if a := testing.AllocsPerRun(100, func() { _, _ = r.ResamplePCM16(chunk) }); a > 1 {
		t.Fatalf("allocs/op = %v, want <= 1", a)
	}
}
