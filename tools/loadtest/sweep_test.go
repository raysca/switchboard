package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestSweepStaleWork(t *testing.T) {
	tmp := t.TempDir()
	dead := exec.Command("true")
	_ = dead.Run() // reaped: pid is no longer alive
	mk := func(name string, pid int, age time.Duration) string {
		d := filepath.Join(tmp, workPrefix+name)
		os.Mkdir(d, 0o755)
		os.WriteFile(filepath.Join(d, pidFile), []byte(strconv.Itoa(pid)), 0o644)
		old := time.Now().Add(-age)
		os.Chtimes(d, old, old)
		return d
	}
	deadOld := mk("dead", dead.Process.Pid, 2*time.Hour)
	liveOld := mk("live", os.Getpid(), 2*time.Hour)
	deadYoung := mk("young", dead.Process.Pid, time.Minute)
	other := filepath.Join(tmp, "unrelated")
	os.Mkdir(other, 0o755)
	os.Chtimes(other, time.Now().Add(-2*time.Hour), time.Now().Add(-2*time.Hour))

	// A prefixed symlink to an old dead dir elsewhere must not be followed.
	target := filepath.Join(t.TempDir(), "elsewhere")
	os.Mkdir(target, 0o755)
	os.WriteFile(filepath.Join(target, pidFile), []byte(strconv.Itoa(dead.Process.Pid)), 0o644)
	os.Chtimes(target, time.Now().Add(-2*time.Hour), time.Now().Add(-2*time.Hour))
	link := filepath.Join(tmp, workPrefix+"link")
	os.Symlink(target, link)

	sweepStaleWork(tmp, time.Hour)

	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }
	if exists(deadOld) {
		t.Error("dead old dir kept")
	}
	if !exists(liveOld) || !exists(deadYoung) || !exists(other) || !exists(filepath.Join(target, pidFile)) {
		t.Error("live, young or unrelated dir removed")
	}
}
