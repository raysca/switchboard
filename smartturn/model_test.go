package smartturn

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"govoice/vad"
)

var testWorker string

func TestMain(m *testing.M) {
	dir, _ := os.MkdirTemp("", "turnworker")
	testWorker = filepath.Join(dir, "turnworker")
	cmd := exec.Command("go", "build", "-o", testWorker, "govoice/cmd/turnworker")
	cmd.Dir = vad.Root()
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build turnworker: %v\n%s", err, out)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	vad.ShutdownORT()
	os.RemoveAll(dir)
	os.Exit(code)
}

func noise(n int, amp float64) []int16 {
	r := rand.New(rand.NewPCG(1, 2))
	pcm := make([]int16, n)
	for i := range pcm {
		pcm[i] = int16(r.NormFloat64() * amp)
	}
	return pcm
}

// Real model, no network: output is a probability, deterministic, and matches
// onnxruntime-python on the golden clip.
func TestRealModelSmoke(t *testing.T) {
	b, err := NewInProcess(ModelPath())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	ctx := context.Background()
	for name, pcm := range map[string][]int16{"silence": make([]int16, 3*SampleRate), "noise": noise(3*SampleRate, 30)} {
		p1, err := b.Infer(ctx, pcm)
		if err != nil {
			t.Fatal(err)
		}
		p2, _ := b.Infer(ctx, pcm)
		if p1 < 0 || p1 > 1 || p1 != p2 {
			t.Fatalf("%s: %v %v", name, p1, p2)
		}
		t.Logf("%s: p(complete)=%.4f", name, p1)
	}
	g := loadGolden(t)
	p, err := b.Infer(ctx, goldenPCM())
	if err != nil || math.Abs(float64(p)-g.Prob) > 2e-3 {
		t.Fatalf("golden clip: got %v want %v (%v)", p, g.Prob, err)
	}
	// Features are bit-exact (features_test.go), but this quantized model's
	// output moves with ORT graph fusions and version: python ORT 1.24 alone
	// spans 0.799..0.829 on odd_dc_quiet across optimization levels, and Go's
	// ORT 1.29 gives 0.860. So compare loosely and require the same verdict.
	for _, s := range g.Signals {
		p, err := b.Infer(ctx, signal(s.Spec))
		if err != nil || math.Abs(float64(p)-s.Prob) > 0.075 || (p > 0.5) != (s.Prob > 0.5) {
			t.Fatalf("%s: got %v want %v (%v)", s.Name, p, s.Prob, err)
		}
		t.Logf("%s: go %.4f python %.4f", s.Name, p, s.Prob)
	}
}

func TestModelHashPinned(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "m.onnx")
	_ = os.WriteFile(bad, []byte("nope"), 0o600)
	if _, err := VerifyModel(bad); !errors.Is(err, ErrModelHashMismatch) {
		t.Fatalf("tampered model accepted: %v", err)
	}
	if _, err := VerifyModel(ModelPath()); err != nil {
		t.Fatal(err)
	}
}

func newPool(t *testing.T, workers int) *Pool {
	t.Helper()
	p, err := NewPool(PoolConfig{Binary: testWorker, Workers: workers})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func TestPoolMatchesInProcess(t *testing.T) {
	p := newPool(t, 1)
	got, err := p.Infer(context.Background(), goldenPCM())
	if err != nil || math.Abs(float64(got)-loadGolden(t).Prob) > 2e-3 {
		t.Fatalf("%v %v", got, err)
	}
}

func TestPoolBusyFailsFast(t *testing.T) {
	p := newPool(t, 1)
	w := <-p.idle // occupy the only worker
	if _, err := p.Infer(context.Background(), noise(1600, 10)); !errors.Is(err, ErrBusy) {
		t.Fatalf("want ErrBusy, got %v", err)
	}
	p.idle <- w
}

// Kill the child: the request fails, the crash is counted, the pool reports
// unavailable while down, then the worker respawns and serves again.
func TestPoolCrashRecovery(t *testing.T) {
	p := newPool(t, 1)
	pcm := noise(SampleRate, 20)
	if _, err := p.Infer(context.Background(), pcm); err != nil {
		t.Fatal(err)
	}
	before := Crashes()
	w := p.ws[0]
	w.mu.Lock()
	_ = w.cmd.Process.Kill()
	w.mu.Unlock()
	if _, err := p.Infer(context.Background(), pcm); err == nil {
		t.Fatal("inference on a dead worker succeeded")
	}
	if Crashes() != before+1 {
		t.Fatalf("crashes %d -> %d", before, Crashes())
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := p.Infer(context.Background(), pcm); err == nil {
			break
		} else if !errors.Is(err, ErrUnavailable) && !errors.Is(err, ErrBusy) {
			t.Fatalf("unexpected %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("worker did not come back")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestPoolBadBinaryFailsStartup(t *testing.T) {
	if _, err := NewPool(PoolConfig{Binary: "/nonexistent/turnworker"}); err == nil {
		t.Fatal("startup with missing binary succeeded")
	}
}

// Acceptance: p95 inference (features + ONNX, via the worker process) < 50 ms.
func TestInferenceLatencyP95(t *testing.T) {
	if testing.Short() {
		t.Skip("benchmark")
	}
	p := newPool(t, 1)
	pcm := goldenPCM()
	const n = 200
	lat := make([]float64, 0, n)
	for i := 0; i < n+10; i++ {
		t0 := time.Now()
		if _, err := p.Infer(context.Background(), pcm); err != nil {
			t.Fatal(err)
		}
		if i >= 10 { // warm-up
			lat = append(lat, float64(time.Since(t0).Microseconds())/1000)
		}
	}
	sort.Float64s(lat)
	p50, p95, p99 := lat[n/2], lat[n*95/100], lat[n*99/100]
	t.Logf("worker inference ms: p50=%.1f p95=%.1f p99=%.1f max=%.1f", p50, p95, p99, lat[n-1])
	if p95 >= 50 {
		t.Fatalf("p95 %.1f ms >= 50 ms", p95)
	}
}

// Parent death (SIGKILL, panic) closes the child's stdin: the worker must
// exit on EOF by itself, since macOS has no Pdeathsig.
func TestWorkerExitsWhenParentGoes(t *testing.T) {
	cmd := exec.Command(testWorker, "1")
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var sha [32]byte
	_, _ = hex.Decode(sha[:], []byte(PinnedModelSHA256))
	_, _ = stdin.Write(sha[:])
	var hs [36]byte
	if _, err := io.ReadFull(stdout, hs[:]); err != nil || hs[0] != 0 {
		t.Fatalf("handshake %v %v", hs[:4], err)
	}
	_ = stdin.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("worker exit: %v", err)
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("worker outlived its parent's stdin")
	}
}

// The child gets an allowlisted environment (no server secrets); its stderr
// is surfaced in the startup error.
func TestWorkerEnvSanitizedAndStderrReported(t *testing.T) {
	script := filepath.Join(t.TempDir(), "envdump")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nenv >&2\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOVOICE_CREDENTIAL_KEYS", "s3cret-key")
	t.Setenv("GOVOICE_ROOT", vad.Root())
	_, err := NewPool(PoolConfig{Binary: script})
	if err == nil {
		t.Fatal("pool started on a failing worker")
	}
	if msg := err.Error(); strings.Contains(msg, "s3cret-key") || !strings.Contains(msg, "GOVOICE_ROOT=") {
		t.Fatalf("child env/stderr: %v", msg)
	}
}

// A turnworker whose stdin closes after the handshake must exit 0 with a silent stderr.
func TestTurnWorkerExitsCleanOnStdinClose(t *testing.T) {
	sha, _ := hex.DecodeString(PinnedModelSHA256)
	cmd := exec.Command(testWorker)
	cmd.Env = vad.WorkerEnv()
	var stderr strings.Builder
	cmd.Stderr = &stderr
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	in.Write(sha)
	if _, err := io.ReadFull(out, make([]byte, 36)); err != nil {
		t.Fatalf("handshake: %v\n%s", err, stderr.String())
	}
	in.Close()
	if err := cmd.Wait(); err != nil || stderr.Len() != 0 {
		t.Fatalf("exit err=%v stderr=%q", err, stderr.String())
	}
}
