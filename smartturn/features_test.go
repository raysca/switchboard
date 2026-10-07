package smartturn

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// sigSpec/signal mirror lcg_signal in testdata/gen_golden.py.
type sigSpec struct {
	N        int     `json:"n"`
	Amp1     float64 `json:"amp1"`
	Amp2     float64 `json:"amp2"`
	NoiseAmp *int64  `json:"noise_amp"`
	DC       float64 `json:"dc"`
	EnvHz    float64 `json:"env_hz"`
}

func signal(s sigSpec) []int16 {
	amp1, amp2, na := s.Amp1, s.Amp2, int64(1000)
	if amp1 == 0 {
		amp1 = 8000
	}
	if amp2 == 0 {
		amp2 = 3000
	}
	if s.NoiseAmp != nil {
		na = *s.NoiseAmp
	}
	pcm := make([]int16, s.N)
	state := int64(12345)
	for i := range pcm {
		state = (state*1103515245 + 12345) & 0x7FFFFFFF
		var noise float64
		if na != 0 {
			noise = float64((state>>16)%(2*na+1) - na)
		}
		env := 1.0
		if s.EnvHz != 0 {
			env = 0.5 + 0.5*math.Sin(2*math.Pi*s.EnvHz*float64(i)/16000)
		}
		v := s.DC + env*(amp1*math.Sin(2*math.Pi*220*float64(i)/16000)+amp2*math.Sin(2*math.Pi*1337*float64(i)/16000+0.5)) + noise
		pcm[i] = int16(min(32767, max(-32768, math.Floor(v+0.5))))
	}
	return pcm
}

func goldenPCM() []int16 { return signal(sigSpec{N: 24000}) }

type golden struct {
	Cols    []int       `json:"cols"`
	Values  [][]float64 `json:"values"`
	Sum     float64     `json:"sum"`
	Prob    float64     `json:"prob"`
	Signals []struct {
		Name   string      `json:"name"`
		Spec   sigSpec     `json:"spec"`
		Cols   []int       `json:"cols"`
		Values [][]float64 `json:"values"`
		Sum    float64     `json:"sum"`
		Prob   float64     `json:"prob"`
	} `json:"signals"`
}

func loadGolden(t testing.TB) golden {
	b, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var g golden
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

// TestFeaturesGolden compares against numbers produced by pipecat's
// compute_whisper_log_mel_features on the same synthetic signal.
func TestFeaturesGolden(t *testing.T) {
	g := loadGolden(t)
	f := Features(goldenPCM())
	if len(f) != FeatureSize {
		t.Fatalf("len %d", len(f))
	}
	var worst float64
	for ci, c := range g.Cols {
		for m := 0; m < nMels; m++ {
			worst = math.Max(worst, math.Abs(float64(f[m*NumFrames+c])-g.Values[ci][m]))
		}
	}
	var sum float64
	for _, v := range f {
		sum += float64(v)
	}
	if worst > 1e-5 || math.Abs(sum-g.Sum) > 0.01 {
		t.Fatalf("max abs diff %g, sum %g vs %g", worst, sum, g.Sum)
	}
	t.Logf("max abs diff %g, sum diff %g", worst, sum-g.Sum)
}

// TestFeaturesGoldenSignals covers the 8 s window exactly, truncation of a
// 10 s clip, an odd-length quiet clip with DC offset, and int16 saturation,
// against pipecat (truncate_audio_to_last_n_seconds + whisper features).
// With GOLDEN_DUMP set (see gen_golden.py) every one of the 64000 values is
// compared, not just the stored subsample.
func TestFeaturesGoldenSignals(t *testing.T) {
	g := loadGolden(t)
	if len(g.Signals) < 4 {
		t.Fatal("golden.json lacks the extra signals; rerun gen_golden.py")
	}
	for _, s := range g.Signals {
		f := Features(signal(s.Spec))
		var worst, sum float64
		for ci, c := range s.Cols {
			for m := 0; m < nMels; m++ {
				worst = math.Max(worst, math.Abs(float64(f[m*NumFrames+c])-s.Values[ci][m]))
			}
		}
		for _, v := range f {
			sum += float64(v)
		}
		if dir := os.Getenv("GOLDEN_DUMP"); dir != "" {
			raw, err := os.ReadFile(filepath.Join(dir, s.Name+".f32"))
			if err != nil || len(raw) != 4*FeatureSize {
				t.Fatalf("%s: dump %v", s.Name, err)
			}
			for i := range f {
				ref := math.Float32frombits(binary.LittleEndian.Uint32(raw[4*i:]))
				worst = math.Max(worst, math.Abs(float64(f[i]-ref)))
			}
		}
		if worst > 1e-5 || math.Abs(sum-s.Sum) > 0.01 {
			t.Errorf("%s: max abs diff %g, sum %g vs %g", s.Name, worst, sum, s.Sum)
		}
		t.Logf("%s: max abs diff %g, sum diff %g, python p=%.4f", s.Name, worst, sum-s.Sum, s.Prob)
	}
}

func TestFeaturesTruncatesToLastWindow(t *testing.T) {
	long := make([]int16, WindowLen+5000)
	for i := range long {
		long[i] = int16(i % 977)
	}
	a, b := Features(long), Features(long[5000:])
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("mismatch at %d", i)
		}
	}
}

func BenchmarkFeatures(b *testing.B) {
	pcm := goldenPCM()
	for i := 0; i < b.N; i++ {
		Features(pcm)
	}
}
