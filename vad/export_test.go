package vad

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	if _, err := os.Stat(WorkerBinary()); err != nil {
		cmd := exec.Command("go", "build", "-o", WorkerBinary(), "./cmd/vadworker")
		cmd.Dir = Root()
		if out, err := cmd.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "build vadworker: %v\n%s", err, out)
			os.Exit(1)
		}
	}
	code := m.Run()
	ShutdownORT()
	if code == 0 {
		if err := goleak.Find(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			code = 1
		}
	}
	os.Exit(code)
}

// HardKill SIGKILLs worker workerIdx of a process executor.
func (e *Executor) HardKill(workerIdx int) error {
	wp := e.backend.(*workerProcessBackend).workers[workerIdx]
	wp.mu.Lock()
	defer wp.mu.Unlock()
	if wp.cmd == nil || wp.cmd.Process == nil {
		return errors.New("worker not running")
	}
	err := wp.cmd.Process.Kill()
	wp.killAndReapLocked()
	return err
}
