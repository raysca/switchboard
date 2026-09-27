// Package vad is the shared Silero VAD execution runtime.
package vad

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	ort "github.com/yalue/onnxruntime_go"
)

const (
	SampleRate     = 16000
	ChunkSamples   = 512
	ContextSamples = 64
	InputSamples   = ChunkSamples + ContextSamples
	StateLen       = 2 * 128
)

var initOnce sync.Once
var initErr error

// Root finds the repo root (dir containing third_party/) from cwd or GOVOICE_ROOT.
func Root() string {
	if r := os.Getenv("GOVOICE_ROOT"); r != "" {
		return r
	}
	dir, _ := os.Getwd()
	for d := dir; d != "/"; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "third_party", "silero_vad.onnx")); err == nil {
			return d
		}
	}
	return dir
}

func ModelPath() string { return filepath.Join(Root(), "third_party", "silero_vad.onnx") }

func InitORT() error {
	initOnce.Do(func() {
		ort.SetSharedLibraryPath(filepath.Join(Root(), "third_party", "onnxruntime-osx-arm64-1.29.0", "lib", "libonnxruntime.dylib"))
		initErr = ort.InitializeEnvironment()
	})
	return initErr
}

type tensors struct {
	in, state, out, stateN *ort.Tensor[float32]
}

// Runner owns one ORT session and pre-allocated tensors for batch sizes 1..maxBatch.
// Not safe for concurrent use: one Runner per worker.
type Runner struct {
	sess *ort.DynamicAdvancedSession
	sr   *ort.Scalar[int64]
	bufs []tensors // index = batch-1
}

func NewRunner(maxBatch, intraOp int) (*Runner, error) {
	if err := InitORT(); err != nil {
		return nil, err
	}
	opts, err := ort.NewSessionOptions()
	if err != nil {
		return nil, err
	}
	defer opts.Destroy()
	opts.SetIntraOpNumThreads(intraOp)
	opts.SetInterOpNumThreads(1)
	opts.SetExecutionMode(ort.ExecutionModeSequential)
	s, err := ort.NewDynamicAdvancedSession(ModelPath(), []string{"input", "state", "sr"}, []string{"output", "stateN"}, opts)
	if err != nil {
		return nil, err
	}
	sr, _ := ort.NewScalar(int64(SampleRate))
	r := &Runner{sess: s, sr: sr}
	for b := 1; b <= maxBatch; b++ {
		var t tensors
		if t.in, err = ort.NewEmptyTensor[float32](ort.NewShape(int64(b), InputSamples)); err != nil {
			return nil, err
		}
		t.state, _ = ort.NewEmptyTensor[float32](ort.NewShape(2, int64(b), 128))
		t.out, _ = ort.NewEmptyTensor[float32](ort.NewShape(int64(b), 1))
		t.stateN, _ = ort.NewEmptyTensor[float32](ort.NewShape(2, int64(b), 128))
		r.bufs = append(r.bufs, t)
	}
	return r, nil
}

// Run infers a batch in place: reads j.Input/j.State, writes j.Prob/j.State.
// State layout per job is [2][128]; batched ORT layout is [2][B][128].
func (r *Runner) Run(jobs []*Job) error {
	b := len(jobs)
	if b == 0 {
		return nil
	}
	if b > len(r.bufs) {
		return fmt.Errorf("batch %d > max %d", b, len(r.bufs))
	}
	t := r.bufs[b-1]
	in, st := t.in.GetData(), t.state.GetData()
	for i, j := range jobs {
		copy(in[i*InputSamples:], j.Input[:])
		copy(st[i*128:], j.State[:128])
		copy(st[(b+i)*128:], j.State[128:])
	}
	if err := r.sess.Run([]ort.Value{t.in, t.state, r.sr}, []ort.Value{t.out, t.stateN}); err != nil {
		return err
	}
	out, sn := t.out.GetData(), t.stateN.GetData()
	for i, j := range jobs {
		j.Prob = out[i]
		copy(j.State[:128], sn[i*128:(i+1)*128])
		copy(j.State[128:], sn[(b+i)*128:(b+i+1)*128])
	}
	return nil
}

func (r *Runner) Close() {
	for _, t := range r.bufs {
		t.in.Destroy()
		t.state.Destroy()
		t.out.Destroy()
		t.stateN.Destroy()
	}
	r.sr.Destroy()
	r.sess.Destroy()
}
