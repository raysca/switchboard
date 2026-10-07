package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	workPrefix = "govoice-loadtest-"
	pidFile    = "harness.pid"
)

// sweepStaleWork removes leftover work dirs in tmp from harnesses that were
// SIGKILLed: older than maxAge and whose recorded pid is not alive. A dir
// without a readable pid file is treated as dead once old enough.
func sweepStaleWork(tmp string, maxAge time.Duration) {
	dirs, _ := filepath.Glob(filepath.Join(tmp, workPrefix+"*"))
	for _, d := range dirs {
		fi, err := os.Lstat(d) // never follow a symlink out of tmp
		if err != nil || !fi.IsDir() || time.Since(fi.ModTime()) < maxAge {
			continue
		}
		if b, err := os.ReadFile(filepath.Join(d, pidFile)); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pidAlive(pid) {
				continue
			}
		}
		os.RemoveAll(d)
	}
}

func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
