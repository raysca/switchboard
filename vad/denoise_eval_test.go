package vad

import (
	"math"
	"math/rand/v2"
	"testing"

	"govoice/core/audio/filter"
)

// Evaluation for the P4-2 denoiser: Silero false positives on noise-only clips
// before vs after filtering, and speech recall on real clips in noise. Skips
// when onnxruntime or the speech assets are unavailable.

func denoised(t *testing.T, x []float32) []float32 {
	d, err := filter.NewDenoiser(filter.DefaultDnSt)
	if err != nil {
		t.Fatal(err)
	}
	pcm := make([]int16, len(x))
	for i, v := range x {
		pcm[i] = int16(max(-32768, min(32767, math.Round(float64(v)*32768))))
	}
	out, err := d.Process(pcm, 16000)
	if err != nil {
		t.Fatal(err)
	}
	y := make([]float32, len(out))
	for i, v := range out {
		y[i] = float32(v) / 32768
	}
	return y
}

func countHigh(p []float32) (n int) {
	for _, v := range p {
		if v > 0.5 {
			n++
		}
	}
	return
}

func TestDenoiseCutsVADFalsePositives(t *testing.T) {
	if err := InitORT(); err != nil {
		t.Skipf("onnxruntime unavailable: %v", err)
	}
	r, err := NewRunner(1, 1)
	if err != nil {
		t.Skipf("silero unavailable: %v", err)
	}
	defer r.Close()
	rng := rand.New(rand.NewPCG(7, 9))
	n := 10 * 16000
	// Machinery buzz: harmonics of 90 Hz under formant-like bumps, 4 Hz
	// amplitude modulation, light hiss. Voiced-looking, so Silero fires on it
	// (raw 311 chunks; denoised 0). With heavy hiss (0.03) the denoiser does
	// not help (299 -> 299), so this is not a claim about all noise.
	buzz := make([]float32, n)
	for i := range buzz {
		ti := float64(i) / 16000
		var s float64
		for h := 1; h < 30; h++ {
			f := 90 * float64(h)
			s += math.Sin(2*math.Pi*f*ti) / float64(h) * (1/(1+math.Pow((f-600)/250, 2)) + 1/(1+math.Pow((f-1500)/250, 2)))
		}
		am := 0.5 + 0.5*math.Sin(2*math.Pi*4*ti)
		buzz[i] = float32(0.1*s*am + 0.005*rng.NormFloat64())
	}
	white := make([]float32, n)
	for i := range white {
		white[i] = float32(0.03 * rng.NormFloat64())
	}
	totalRaw, totalDen := 0, 0
	for name, clip := range map[string][]float32{"buzz": buzz, "white": white} {
		raw := countHigh(streamProbs(t, r, clip))
		den := countHigh(streamProbs(t, r, denoised(t, clip)))
		t.Logf("noise-only %s: chunks>0.5 raw=%d denoised=%d (of %d)", name, raw, den, n/ChunkSamples)
		totalRaw += raw
		totalDen += den
	}
	if totalRaw == 0 {
		t.Fatal("test clips no longer trigger Silero; the evaluation proves nothing")
	}
	if totalDen > totalRaw/2 {
		t.Errorf("denoiser increased false positives: %d -> %d", totalRaw, totalDen)
	}

	// Recall: real speech in hiss at ~10 dB must not lose most speech chunks.
	L, err := loadTestLib("../assets/speech")
	if err != nil {
		t.Skipf("speech assets: %v", err)
	}
	clip := L.Clips[0].Samples
	noisy := make([]float32, len(clip))
	var e float64
	for _, v := range clip {
		e += float64(v) * float64(v)
	}
	sigma := math.Sqrt(e/float64(len(clip))) / math.Sqrt(10)
	for i, v := range clip {
		noisy[i] = v + float32(sigma*rng.NormFloat64())
	}
	raw := countHigh(streamProbs(t, r, noisy))
	den := countHigh(streamProbs(t, r, denoised(t, noisy)))
	t.Logf("speech+hiss 10 dB: chunks>0.5 raw=%d denoised=%d", raw, den)
	if den < raw*8/10 {
		t.Errorf("denoiser cost too much speech recall: %d -> %d", raw, den)
	}
}

// Quiet speech (-35 dBFS) in faint hiss, with a fresh denoiser per clip so the
// speech arrives before the noise estimate has converged (speech at call
// start). Denoising must keep Silero's speech chunks and must not delay the
// first detection (barge-in latency) beyond the filter's own 15.9 ms.
func TestDenoiseKeepsQuietSpeech(t *testing.T) {
	if err := InitORT(); err != nil {
		t.Skipf("onnxruntime unavailable: %v", err)
	}
	r, err := NewRunner(1, 1)
	if err != nil {
		t.Skipf("silero unavailable: %v", err)
	}
	defer r.Close()
	L, err := loadTestLib("../assets/speech")
	if err != nil {
		t.Skipf("speech assets: %v", err)
	}
	rng := rand.New(rand.NewPCG(3, 5))
	firstHigh := func(p []float32) int {
		for i, v := range p {
			if v > 0.5 {
				return i
			}
		}
		return -1
	}
	for _, noiseDB := range []float64{-70, -55} { // faint hiss; 20 dB SNR
		rawN, denN, lateChunks := 0, 0, 0
		for ci, c := range L.Clips {
			var e float64
			for _, v := range c.Samples {
				e += float64(v) * float64(v)
			}
			k := math.Pow(10, -35.0/20) / math.Sqrt(e/float64(len(c.Samples)))
			x := make([]float32, len(c.Samples))
			for i, v := range c.Samples {
				x[i] = float32(float64(v)*k + math.Pow(10, noiseDB/20)*rng.NormFloat64())
			}
			rp, dp := streamProbs(t, r, x), streamProbs(t, r, denoised(t, x))
			rawN += countHigh(rp)
			denN += countHigh(dp)
			if a, b := firstHigh(rp), firstHigh(dp); a >= 0 && (b < 0 || b > a+1) { // +1 chunk = the filter delay
				lateChunks++
				t.Logf("noise %v dBFS clip %d: first detection chunk %d raw vs %d denoised", noiseDB, ci, a, b)
			}
		}
		t.Logf("quiet speech -35 dBFS, noise %v dBFS: chunks>0.5 raw=%d denoised=%d over %d clips", noiseDB, rawN, denN, len(L.Clips))
		if rawN == 0 {
			t.Fatal("Silero does not detect the quiet clips at all; the test proves nothing")
		}
		if denN < rawN*9/10 {
			t.Errorf("denoiser cost quiet-speech recall: %d -> %d", rawN, denN)
		}
		if lateChunks > len(L.Clips)/10 {
			t.Errorf("first detection later than raw+1 chunk on %d of %d clips", lateChunks, len(L.Clips))
		}
	}
}
