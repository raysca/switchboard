package stack

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRandHexLength(t *testing.T) {
	if got := RandHex(32); len(got) != 64 || got == RandHex(32) {
		t.Fatalf("RandHex(32) = %q", got)
	}
}

// Call must never echo a response body (it can carry secrets) nor the key.
func TestCallErrorDoesNotEchoBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "SECRET-BODY "+r.Header.Get("Authorization"), 500)
	}))
	defer srv.Close()
	s := &Server{Admin: srv.URL, Key: "k-123"}
	_, err := s.Call("POST", "agents", map[string]string{"a": "b"})
	if err == nil || strings.Contains(err.Error(), "SECRET-BODY") || strings.Contains(err.Error(), "k-123") {
		t.Fatalf("err = %v", err)
	}
}

// A server that exits during start must not leave its children running.
func TestStartSweepsTheGroupOfAServerThatExited(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	script := filepath.Join(dir, "fake-server.sh")
	body := "#!/bin/sh\nsleep 60 &\necho $! > " + pidFile + "\nexit 1\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := Start(context.Background(), Spec{Root: dir, Bin: script, Dir: dir, Config: func(int, int) []byte { return nil }})
	if err == nil {
		t.Fatal("Start succeeded")
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if syscall.Kill(pid, 0) != nil {
			return
		}
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("the exited server's child %d is still running", pid)
}

// A server that accepts the probe and never answers is timed out per request.
func TestReadyProbeIsBoundedPerRequest(t *testing.T) {
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer hang.Close()
	start := time.Now()
	if r, err := readyClient.Get(hang.URL + "/ready"); err == nil {
		r.Body.Close()
		t.Fatal("expected a timeout")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("probe took %v", d)
	}
}

// Stop signals the group once: a second call (cleanup after a failed run) must not signal a pgid the kernel may
// have handed to an unrelated process.
func TestStopSignalsTheGroupOnce(t *testing.T) {
	var calls []syscall.Signal
	old := killGroup
	killGroup = func(_ int, sig syscall.Signal) error { calls = append(calls, sig); return nil }
	defer func() { killGroup = old }()
	done := make(chan struct{})
	close(done)
	s := &Server{cmd: &exec.Cmd{Process: &os.Process{Pid: 424242}}, done: done}
	s.Stop()
	first := len(calls)
	s.Stop()
	if first == 0 || len(calls) != first {
		t.Fatalf("signals: first Stop %d, after the second %d", first, len(calls))
	}
}
