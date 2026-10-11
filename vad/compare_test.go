package vad

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"govoice/core"
)

// segLog records VAD segment boundaries as chunk indices.
type segLog struct {
	chunk       int
	starts, end []int
}

func (l *segLog) Push(f core.Frame) error {
	switch f.(type) {
	case *core.UserStartedSpeakingFrame:
		l.starts = append(l.starts, l.chunk)
	case *core.UserStoppedSpeakingFrame:
		l.end = append(l.end, l.chunk)
	}
	return nil
}
func (l *segLog) PushDirection(f core.Frame, _ core.Direction) error { return l.Push(f) }

// modelProbs runs one Silero model file over a clip, framed exactly like Processor.trySubmit.
func modelProbs(t *testing.T, r *Runner, pcm []float32) []float32 {
	var ctx [ContextSamples]float32
	var state [StateLen]float32
	var out []float32
	for i := 0; i+ChunkSamples <= len(pcm); i += ChunkSamples {
		j := &Job{}
		copy(j.Input[:ContextSamples], ctx[:])
		copy(j.Input[ContextSamples:], pcm[i:i+ChunkSamples])
		j.State = state
		if err := r.Run([]*Job{j}); err != nil {
			t.Fatal(err)
		}
		state = j.State
		copy(ctx[:], pcm[i+ChunkSamples-ContextSamples:i+ChunkSamples])
		out = append(out, j.Prob)
	}
	return out
}

// trailingSilence (ms) is appended to each clip: longer than the 800 ms stop, so both stops fire.
const trailingSilence = 1200

// segments runs our detector over per-chunk probabilities at a VAD stop.
func segments(probs []float32, stop time.Duration) (starts, ends []int) {
	cfg := DefaultProcessorConfig()
	chunkMS := ChunkSamples * 1000 / SampleRate
	cfg.StopChunks = (int(stop/time.Millisecond) + chunkMS - 1) / chunkMS
	p := NewProcessor(nil, cfg)
	l := &segLog{}
	for i, pr := range probs {
		l.chunk = i
		_ = p.detect(pr, uint64(i), time.Time{}, l, nil)
	}
	return l.starts, l.end
}

// TestCompareSileroModels (VAD_COMPARE=1, VAD_COMPARE_PIPECAT_MODEL=<pipecat silero_vad.onnx>,
// optional VAD_COMPARE_PIPECAT_PROBS=<json from tools/vadcompare/pipecat_probs.py>,
// VAD_COMPARE_OUT=<markdown path>) compares our model file with Pipecat's on assets/speech.
// Offline and free; skipped unless VAD_COMPARE is set.
func TestCompareSileroModels(t *testing.T) {
	if os.Getenv("VAD_COMPARE") == "" {
		t.Skip("set VAD_COMPARE=1 (offline, no cost)")
	}
	theirs := os.Getenv("VAD_COMPARE_PIPECAT_MODEL")
	if theirs == "" {
		t.Fatal("VAD_COMPARE_PIPECAT_MODEL is required")
	}
	files, _ := filepath.Glob("../assets/speech/*.wav")
	sort.Strings(files)
	run := func(model string) map[string][]float32 {
		ConfigureArtifacts(model, "")
		r, err := NewRunner(1, 1)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		out := map[string][]float32{}
		for _, f := range files {
			pcm, err := readWavPCM(f)
			if err != nil {
				t.Fatal(err)
			}
			// The clips end mid-speech: pad trailing silence so every utterance gets its stop.
			pcm = append(pcm, make([]float32, trailingSilence*SampleRate/1000)...)
			out[filepath.Base(f)] = modelProbs(t, r, pcm)
		}
		return out
	}
	oursPath := filepath.Join(Root(), "third_party", "silero_vad.onnx")
	ours := run(oursPath)
	pc := run(theirs)
	ConfigureArtifacts(oursPath, "")

	chunkMS := ChunkSamples * 1000 / SampleRate
	stops := []time.Duration{200 * time.Millisecond, 800 * time.Millisecond}
	var b strings.Builder
	fmt.Fprintf(&b, "| clip | model | stop | segments | first start (ms) | last end (ms) |\n|---|---|---|---|---|---|\n")
	splits := map[string]map[time.Duration]int{"ours": {}, "pipecat": {}}
	endSum := map[string]map[time.Duration]int{"ours": {}, "pipecat": {}}
	for _, f := range files {
		name := filepath.Base(f)
		for _, m := range []struct {
			n string
			p []float32
		}{{"ours", ours[name]}, {"pipecat", pc[name]}} {
			for _, stop := range stops {
				s, e := segments(m.p, stop)
				first, last := -1, -1
				if len(s) > 0 {
					first = s[0] * chunkMS
				}
				if len(e) > 0 {
					last = e[len(e)-1] * chunkMS
					endSum[m.n][stop] += last
				}
				if len(s) > 1 {
					splits[m.n][stop] += len(s) - 1
				}
				fmt.Fprintf(&b, "| %s | %s | %v | %d | %d | %d |\n", name, m.n, stop, len(s), first, last)
			}
		}
	}
	fmt.Fprintf(&b, "\nEach clip is padded with %d ms of silence, so the last end is the stop after the utterance.\n\n| model | stop | extra segments (mid-utterance splits) | summed last end (ms) |\n|---|---|---|---|\n", trailingSilence)
	for _, n := range []string{"ours", "pipecat"} {
		for _, stop := range stops {
			fmt.Fprintf(&b, "| %s | %v | %d | %d |\n", n, stop, splits[n][stop], endSum[n][stop])
		}
	}
	if path := os.Getenv("VAD_COMPARE_PIPECAT_PROBS"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var py map[string][]float64
		if err := json.Unmarshal(data, &py); err != nil {
			t.Fatal(err)
		}
		worst, n := 0.0, 0
		for name, ps := range py {
			g := pc[name]
			for i := 0; i < len(ps) && i < len(g); i++ {
				worst = math.Max(worst, math.Abs(ps[i]-float64(g[i])))
				n++
			}
		}
		fmt.Fprintf(&b, "\nInference parity (our Go runner with Pipecat's file vs Pipecat's Python wrapper): max |Δp| = %.4f over %d chunks in %d clips.\n", worst, n, len(py))
	}
	if out := os.Getenv("VAD_COMPARE_OUT"); out != "" {
		if err := os.WriteFile(out, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Log("\n" + b.String())
}
