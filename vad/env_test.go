package vad

import (
	"bytes"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

func TestInitORTDisablesTelemetry(t *testing.T) {
	if err := InitORT(); err != nil {
		t.Skip(err)
	}
	if os.Getenv("ORT_DISABLE_TELEMETRY") == "" {
		t.Fatal("ORT_DISABLE_TELEMETRY not set after InitORT")
	}
}

// ORT treats an empty ORT_DISABLE_TELEMETRY as enabled (1DS writes mat-debug-*.log to
// TMPDIR and a device db under HOME); the worker must still come up with it off.
func TestWorkerTelemetryOffWithEmptyVar(t *testing.T) {
	if _, err := os.Stat(WorkerBinary()); err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	cmd := exec.Command(WorkerBinary(), "1") // stdin is /dev/null: exits after ORT init
	cmd.Env = append(WorkerEnv(), "TMPDIR="+dir, "HOME="+dir, "ORT_DISABLE_TELEMETRY=")
	_ = cmd.Run()
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatalf("telemetry wrote files: %v", ents)
	}
}

// The executor's real spawn path: secrets stay in the parent, locator vars reach the child.
func TestWorkerExecutorEnvDropsSecrets(t *testing.T) {
	if _, err := VerifyModelArtifact(ModelPath()); err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	script := dir + "/envdump"
	if err := os.WriteFile(script, []byte("#!/bin/sh\nenv > "+dir+"/env\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOVOICE_TEST_SECRET_DSN", "postgres://u:p@h/db")
	t.Setenv("GOVOICE_ROOT", Root())
	if _, err := NewWorkerProcessExecutor(Config{Workers: 1, Batch: 1, QueueCap: 1, WorkerBinary: script}); err == nil {
		t.Fatal("executor started on a fake worker")
	}
	b, _ := os.ReadFile(dir + "/env")
	env := strings.Split(string(b), "\n")
	if slices.ContainsFunc(env, func(e string) bool { return strings.Contains(e, "SECRET") }) {
		t.Fatalf("secret leaked to worker env: %v", env)
	}
	if !slices.Contains(env, "GOVOICE_ROOT="+Root()) {
		t.Fatalf("allowlisted var missing: %v", env)
	}
}

// A vadworker whose stdin closes after the handshake must exit 0 with a silent stderr
// (regression: ORT exit-time abort "recursive_mutex lock failed").
func TestWorkerExitsCleanOnStdinClose(t *testing.T) {
	sha, err := VerifyModelArtifact(ModelPath())
	if err != nil {
		t.Skip(err)
	}
	cmd := exec.Command(WorkerBinary(), "1")
	cmd.Env = WorkerEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := WriteFrame(in, EncodeHandshakeReq(HandshakeReq{ExpectedModelSHA256: sha, MaxBatch: 1})); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFrame(out, nil); err != nil {
		t.Fatalf("handshake: %v\n%s", err, stderr.String())
	}
	in.Close()
	if err := cmd.Wait(); err != nil || stderr.Len() != 0 {
		t.Fatalf("exit err=%v stderr=%q", err, stderr.String())
	}
}
