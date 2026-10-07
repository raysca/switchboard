package smartturn

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"

	ort "github.com/yalue/onnxruntime_go"

	"govoice/vad"
)

// PinnedModelSHA256 is the sha256 of smart-turn-v3.2-cpu.onnx (BSD-2-Clause,
// Daily); it is also recorded in docs/compatibility/v1-lock.json.
const PinnedModelSHA256 = "2bb026316b14a660486a75b1733cd3fbab8c2fd0314dc9af7be49f8cca967e4f"

var ErrModelHashMismatch = errors.New("smartturn: model SHA-256 mismatch")

// ModelPath is the configured model, else third_party/ under the repo root.
func ModelPath() string {
	if p := os.Getenv("GOVOICE_TURN_MODEL_PATH"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return filepath.Join(vad.Root(), "third_party", "smart_turn_v3.2_cpu.onnx")
}

// ConfigureModel records the model path for this process and child workers.
func ConfigureModel(path string) {
	if path != "" {
		_ = os.Setenv("GOVOICE_TURN_MODEL_PATH", path)
	}
}

// VerifyModel checks the file against PinnedModelSHA256.
func VerifyModel(path string) ([32]byte, error) {
	_, h, err := readVerified(path)
	return h, err
}

func readVerified(path string) ([]byte, [32]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, [32]byte{}, fmt.Errorf("read turn model: %w", err)
	}
	h := sha256.Sum256(data)
	if got := hex.EncodeToString(h[:]); got != PinnedModelSHA256 {
		return nil, h, fmt.Errorf("%w: expected %s, got %s", ErrModelHashMismatch, PinnedModelSHA256, got)
	}
	return data, h, nil
}

// Runner is one ONNX Runtime session with preallocated tensors; not safe for
// concurrent use. It is what the worker process runs and what the in-process
// backend wraps in tests.
type Runner struct {
	sess *ort.AdvancedSession
	in   *ort.Tensor[float32]
	out  *ort.Tensor[float32]
}

// NewRunner loads the model from the exact bytes it hash-checked, so a file
// swapped after the check (TOCTOU) is never what ONNX Runtime runs.
func NewRunner(modelPath string, intraOp int) (*Runner, error) {
	data, _, err := readVerified(modelPath)
	if err != nil {
		return nil, err
	}
	if err := vad.InitORT(); err != nil {
		return nil, err
	}
	opts, err := ort.NewSessionOptions()
	if err != nil {
		return nil, err
	}
	defer opts.Destroy()
	opts.SetIntraOpNumThreads(max(intraOp, 1))
	opts.SetInterOpNumThreads(1)
	opts.SetExecutionMode(ort.ExecutionModeSequential)
	in, err := ort.NewEmptyTensor[float32](ort.NewShape(1, nMels, NumFrames))
	if err != nil {
		return nil, err
	}
	out, err := ort.NewEmptyTensor[float32](ort.NewShape(1, 1))
	if err != nil {
		in.Destroy()
		return nil, err
	}
	s, err := ort.NewAdvancedSessionWithONNXData(data, []string{"input_features"}, []string{"logits"},
		[]ort.Value{in}, []ort.Value{out}, opts)
	if err != nil {
		in.Destroy()
		out.Destroy()
		return nil, err
	}
	return &Runner{s, in, out}, nil
}

// Infer returns the probability that the turn is complete (the model's
// output is already a sigmoid; Pipecat thresholds it at 0.5).
func (r *Runner) Infer(pcm []int16) (float32, error) {
	copy(r.in.GetData(), Features(pcm))
	if err := r.sess.Run(); err != nil {
		return 0, err
	}
	p := r.out.GetData()[0]
	if math.IsNaN(float64(p)) || p < 0 || p > 1 {
		return 0, fmt.Errorf("smartturn: model output %v outside [0,1]", p)
	}
	return p, nil
}

func (r *Runner) Close() {
	r.sess.Destroy()
	r.in.Destroy()
	r.out.Destroy()
}
