package main

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/rand"
	"os"

	"govoice/tools/internal/wav"
)

const (
	sampleRate = 16000
	chunkMS    = 20
	chunkBytes = sampleRate * chunkMS / 1000 * 2 // 640
)

// loadClip returns 16 kHz mono PCM16 little-endian speech. path "synthetic"
// generates a deterministic voiced signal; otherwise a 16 kHz mono 16-bit WAV
// is read and leading/trailing digital silence is trimmed (the repo fixture
// e2e/fixtures/voice-debug.wav is 22 s with ~1.6 s of speech).
func loadClip(path string) ([]byte, error) {
	if path == "synthetic" {
		return syntheticSpeech(1800), nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pcm, err := parseWAV(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return trimSilence(pcm), nil
}

func parseWAV(b []byte) ([]byte, error) {
	pcm, r, err := wav.ReadWAV(b)
	if err != nil {
		return nil, err
	}
	if r != sampleRate {
		return nil, fmt.Errorf("need PCM16 mono %d Hz, got %d Hz", sampleRate, r)
	}
	return pcm, nil
}

// trimSilence drops 20 ms frames below a small RMS at both ends, keeping 100 ms of margin.
func trimSilence(pcm []byte) []byte {
	frames := len(pcm) / chunkBytes
	loud := func(i int) bool {
		var sum float64
		for j := i * chunkBytes; j < (i+1)*chunkBytes; j += 2 {
			s := float64(int16(binary.LittleEndian.Uint16(pcm[j:])))
			sum += s * s
		}
		return math.Sqrt(sum/(chunkBytes/2)) > 50
	}
	first, last := -1, -1
	for i := 0; i < frames; i++ {
		if loud(i) {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < 0 {
		return pcm
	}
	const margin = 5
	return pcm[max(first-margin, 0)*chunkBytes : min(last+1+margin, frames)*chunkBytes]
}

// syntheticSpeech makes a speech-like proxy: a glottal pulse train with a
// gliding f0 (110-150 Hz), three formant resonators moving between vowel
// targets, 4 Hz syllable amplitude modulation and light breath noise.
// Deterministic (fixed seed). Whether Silero fires on it is checked by the
// harness itself (turns time out when it does not); see capacity.md.
func syntheticSpeech(ms int) []byte {
	n := sampleRate * ms / 1000
	rng := rand.New(rand.NewSource(1))
	type res struct{ y1, y2 float64 }
	var st [3]res
	out := make([]byte, 2*n)
	phase := 0.0
	vowels := [][3]float64{{730, 1090, 2440}, {270, 2290, 3010}, {570, 840, 2410}, {300, 870, 2240}}
	for i := 0; i < n; i++ {
		t := float64(i) / sampleRate
		f0 := 130 + 20*math.Sin(2*math.Pi*0.9*t)
		phase += f0 / sampleRate
		pulse := 0.0
		if phase >= 1 {
			phase -= 1
			pulse = 1
		}
		src := pulse*40 + rng.NormFloat64()*0.05
		vi := int(t*3) % len(vowels)
		sum := 0.0
		for k := 0; k < 3; k++ {
			fc, bw := vowels[vi][k], 60.0+40*float64(k)
			r := math.Exp(-math.Pi * bw / sampleRate)
			a1, a2 := 2*r*math.Cos(2*math.Pi*fc/sampleRate), -r*r
			y := (1-r)*src + a1*st[k].y1 + a2*st[k].y2
			st[k].y2, st[k].y1 = st[k].y1, y
			sum += y * (1 / float64(k+1))
		}
		env := 0.55 + 0.45*math.Sin(2*math.Pi*4*t-math.Pi/2)
		v := sum * env * 6000
		binary.LittleEndian.PutUint16(out[2*i:], uint16(int16(max(min(v, 32000), -32000))))
	}
	return out
}
