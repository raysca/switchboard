package vad

import (
	"math"
	"path/filepath"
	"testing"
	"time"

	"govoice/loadgen"
	"govoice/voice"
)

func lib(t *testing.T) *loadgen.Library {
	l, err := loadgen.Load(filepath.Join(Root(), "assets", "speech"), 0.0005)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// streamProbs runs one clip through a streaming state with batch size 1.
func streamProbs(t *testing.T, r *Runner, pcm []float32) []float32 {
	var ctx [ContextSamples]float32
	var st [StateLen]float32
	var out []float32
	for i := 0; i+ChunkSamples <= len(pcm); i += ChunkSamples {
		j := &Job{State: st}
		copy(j.Input[:], ctx[:])
		copy(j.Input[ContextSamples:], pcm[i:i+ChunkSamples])
		if err := r.Run([]*Job{j}); err != nil {
			t.Fatal(err)
		}
		st = j.State
		copy(ctx[:], pcm[i+ChunkSamples-ContextSamples:i+ChunkSamples])
		out = append(out, j.Prob)
	}
	return out
}

func TestStreamingAndBatching(t *testing.T) {
	L := lib(t)
	r, err := NewRunner(8, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	// Silence stays low, speech goes high.
	var noiseMax float32
	for _, p := range streamProbs(t, r, L.Noise[:16000*3]) {
		noiseMax = max(noiseMax, p)
	}
	speechHi := 0
	probs := streamProbs(t, r, L.Clips[0].Samples)
	for _, p := range probs {
		if p > 0.5 {
			speechHi++
		}
	}
	t.Logf("noise max prob %.3f, speech chunks >0.5: %d/%d", noiseMax, speechHi, len(probs))
	if noiseMax > 0.2 || speechHi < len(probs)/3 {
		t.Fatal("streaming VAD output implausible")
	}
	// Batched streams of different clips must equal their unbatched runs.
	const B = 6
	want := make([][]float32, B)
	for b := 0; b < B; b++ {
		want[b] = streamProbs(t, r, L.Clips[b].Samples)
	}
	jobs := make([]*Job, B)
	for b := range jobs {
		jobs[b] = &Job{}
	}
	var ctx [B][ContextSamples]float32
	var maxDiff float64
	for c := 0; ; c++ {
		live := jobs[:0]
		idx := []int{}
		for b := 0; b < B; b++ {
			pcm := L.Clips[b].Samples
			if (c+1)*ChunkSamples > len(pcm) {
				continue
			}
			j := jobs[b]
			copy(j.Input[:], ctx[b][:])
			copy(j.Input[ContextSamples:], pcm[c*ChunkSamples:(c+1)*ChunkSamples])
			copy(ctx[b][:], pcm[(c+1)*ChunkSamples-ContextSamples:(c+1)*ChunkSamples])
			live = append(live, j)
			idx = append(idx, b)
		}
		if len(live) == 0 {
			break
		}
		if err := r.Run(live); err != nil {
			t.Fatal(err)
		}
		for k, j := range live {
			maxDiff = math.Max(maxDiff, math.Abs(float64(j.Prob-want[idx[k]][c])))
		}
		jobs = []*Job{jobs[0], jobs[1], jobs[2], jobs[3], jobs[4], jobs[5]}
	}
	t.Logf("batched vs unbatched max |dprob| = %g", maxDiff)
	if maxDiff > 1e-4 {
		t.Fatal("batched inference diverges from unbatched")
	}
}

// End-to-end: sessions + VAD processor + shared executor + real-time feeder.
func TestEndToEndDetection(t *testing.T) {
	for _, mode := range []string{"global", "sharded", "process"} {
		t.Run(mode, func(t *testing.T) {
			L := lib(t)
			ex, err := New(Config{Mode: mode, Workers: 2, Batch: 4, QueueCap: 256})
			if err != nil {
				t.Fatal(err)
			}
			m := voice.NewManager()
			pool := loadgen.NewPool(1)
			var streams []*loadgen.Stream
			for i := 0; i < 10; i++ {
				st := &loadgen.Stream{TL: loadgen.NewTimeline(L, uint64(i+1), 0.5)}
				s, _, err := m.Create(voice.Spec{Build: func(s *voice.Session) ([]voice.Processor, error) {
					return []voice.Processor{NewProcessor(ex, DefaultProcessorConfig(), nil)}, nil
				}})
				if err != nil {
					t.Fatal(err)
				}
				s.OnOutput = func(f voice.Frame) {
					if sf, ok := f.(*voice.SpeechStartedFrame); ok {
						st.Detections = append(st.Detections, sf.EmittedAt)
					}
				}
				st.S, st.Start = s, time.Now()
				streams = append(streams, st)
				pool.Add(st)
			}
			from := time.Now()
			time.Sleep(12 * time.Second)
			until := time.Now()
			pool.Stop()
			var tot loadgen.Score
			for _, st := range streams {
				st.S.Stop()
				sc := st.Score(from, until)
				tot.Onsets += sc.Onsets
				tot.Hits += sc.Hits
				tot.False += sc.False
			}
			ex.Close()
			m.Close()
			p99 := m.VADLatency.Quantile(0.99)
			t.Logf("%s: onsets %d hits %d false %d; VAD p99 %v; exec %+v", mode, tot.Onsets, tot.Hits, tot.False, p99, ex.Stats())
			if tot.Onsets < 10 || tot.Hits*100 < tot.Onsets*95 || tot.False > 2 {
				t.Fatal("detection accuracy too low")
			}
			if st := ex.Stats(); st.Streams != 0 {
				t.Fatalf("stream registrations leaked: %d", st.Streams)
			}
		})
	}
}
