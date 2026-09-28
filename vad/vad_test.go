package vad

import (
	"encoding/binary"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

type testClip struct {
	Samples []float32
}

type testLib struct {
	Clips []testClip
	Noise []float32
}

func lib(t *testing.T) *testLib {
	l, err := loadTestLib(filepath.Join(Root(), "assets", "speech"))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func loadTestLib(dir string) (*testLib, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.wav"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	lib := &testLib{}
	for _, fn := range files {
		pcm, err := readWavPCM(fn)
		if err != nil {
			return nil, err
		}
		lib.Clips = append(lib.Clips, testClip{Samples: pcm})
	}
	if len(lib.Clips) == 0 {
		return nil, os.ErrNotExist
	}
	r := rand.New(rand.NewPCG(1, 2))
	lib.Noise = make([]float32, 30*16000)
	for i := range lib.Noise {
		lib.Noise[i] = float32(r.NormFloat64() * 0.0005)
	}
	return lib, nil
}

func readWavPCM(fn string) ([]float32, error) {
	b, err := os.ReadFile(fn)
	if err != nil {
		return nil, err
	}
	if len(b) < 12 || string(b[:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return nil, os.ErrInvalid
	}
	for p := 12; p+8 <= len(b); {
		id, sz := string(b[p:p+4]), int(binary.LittleEndian.Uint32(b[p+4:]))
		p += 8
		if id == "fmt " {
			ch := binary.LittleEndian.Uint16(b[p+2:])
			sr := binary.LittleEndian.Uint32(b[p+4:])
			bits := binary.LittleEndian.Uint16(b[p+14:])
			if ch != 1 || sr != 16000 || bits != 16 {
				return nil, os.ErrInvalid
			}
		}
		if id == "data" {
			sz = min(sz, len(b)-p)
			out := make([]float32, sz/2)
			for i := range out {
				out[i] = float32(int16(binary.LittleEndian.Uint16(b[p+2*i:]))) / 32768
			}
			return out, nil
		}
		p += sz + sz&1
	}
	return nil, os.ErrInvalid
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
