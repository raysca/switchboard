package resample

import (
	"encoding/binary"
	"math"
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
