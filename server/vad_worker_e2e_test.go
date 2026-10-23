//go:build xaispeech

package server

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"

	"govoice/vad"
)

// crashableVADWorker is a vadworker launcher that exits at once while flag exists, so a killed worker's
// restarts keep failing (a crash loop) until the flag is removed.
func crashableVADWorker(t *testing.T, flag string) string {
	t.Helper()
	real := vad.WorkerBinary()
	if _, err := os.Stat(real); err != nil {
		t.Fatalf("no vadworker binary (run `make build`): %v", err)
	}
	path := filepath.Join(t.TempDir(), "vadworker.sh")
	script := "#!/bin/sh\n[ -e '" + flag + "' ] && exit 1\nexec '" + real + "' \"$@\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// vadWorkerPIDs lists this test process's live vadworker children.
func vadWorkerPIDs(t *testing.T) []int {
	t.Helper()
	out, err := exec.Command("pgrep", "-P", strconv.Itoa(os.Getpid()), "vadworker").Output()
	if err != nil {
		t.Fatalf("pgrep vadworker children: %v", err)
	}
	var pids []int
	for _, f := range strings.Fields(string(out)) {
		if pid, err := strconv.Atoi(f); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}

// pinRecorder records the session ids the calls register with, to prove the two calls sit on different workers.
type pinRecorder struct {
	*vad.Executor
	mu  sync.Mutex
	ids []uint64
}

func (p *pinRecorder) Register(id uint64) vad.Stream {
	p.mu.Lock()
	p.ids = append(p.ids, id)
	p.mu.Unlock()
	return p.Executor.Register(id)
}

// Two calls on the real process VAD executor (one per worker). One worker process is SIGKILLed mid-call and
// cannot restart (crash loop): both callers' speech is still detected, the dead worker stays out of capacity
// and the restarts metric counts its attempts.
func TestCallBehaviorVADKeepsHearingCallersWhileAWorkerCrashLoops(t *testing.T) {
	r := newAgentRigE2E(t, newProbe())
	flag := filepath.Join(t.TempDir(), "crash")
	cfg := vad.DefaultConfig()
	cfg.Workers = 2
	cfg.WorkerBinary = crashableVADWorker(t, flag)
	vad.ConfigureArtifacts("", "")
	vexec, err := vad.New(cfg)
	if err != nil {
		t.Fatalf("process VAD executor (run `make ort` and `make build`): %v", err)
	}
	t.Cleanup(func() { _ = vexec.Close() })
	rec := &pinRecorder{Executor: vexec}
	r.s.SetVADExecutor(rec)
	agentID := behaviorAgent(t, r, "Listener", "listenvoice", ``)
	calls := []*e2eCall{r.dial(agentID), r.dial(agentID)} // consecutive session ids: one per worker
	speech := callerSpeech(t)
	heard := func(c *e2eCall) int { return countOf(c.wireLog(), "msg:user-started-speaking") }
	for i, c := range calls {
		c.sendAudio(speech)
		eventually(t, fmt.Sprintf("call %d's speech before the crash", i), func() bool { return heard(c) >= 1 })
	}
	rec.mu.Lock()
	ids := append([]uint64(nil), rec.ids...)
	rec.mu.Unlock()
	if len(ids) != 2 || ids[0]%2 == ids[1]%2 {
		t.Fatalf("the calls registered VAD sessions %v, want two ids pinned to different workers", ids)
	}
	pids := vadWorkerPIDs(t)
	if len(pids) != 2 {
		t.Fatalf("vadworker children %v, want 2", pids)
	}
	if err := os.WriteFile(flag, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pids[0], syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	for i, c := range calls {
		before := heard(c)
		c.sendAudio(speech)
		eventually(t, fmt.Sprintf("call %d's speech after a worker crashed", i), func() bool { return heard(c) > before })
	}
	if avail, total := vexec.Capacity(); avail != 1 || total != 2 {
		t.Fatalf("capacity (%d,%d) while a worker crash-loops, want (1,2)", avail, total)
	}
	r.s.sampleVADMetrics()
	if n := r.s.metrics.VADWorkerRestartsTotal.Load(); n < 1 {
		t.Fatalf("voice_vad_worker_restarts_total = %d", n)
	}
	for i, c := range calls {
		if c.ended() {
			t.Fatalf("call %d ended", i)
		}
	}
}
