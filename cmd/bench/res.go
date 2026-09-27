package main

import (
	"encoding/csv"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/metrics"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Res struct {
	RSSMB, FootprintMB, HeapMB float64
	HeapObjects                uint64
	Goroutines, Threads, FDs   int
	GCPauseP99ms, GCCPUFrac    float64
}

var footRe = regexp.MustCompile(`Footprint: ([\d.]+) (KB|MB|GB|B)`)

func footprintMB(pid int) float64 {
	out, _ := exec.Command("footprint", "-p", strconv.Itoa(pid)).Output()
	m := footRe.FindSubmatch(out)
	if m == nil {
		return -1
	}
	v, _ := strconv.ParseFloat(string(m[1]), 64)
	return v * map[string]float64{"B": 1.0 / (1 << 20), "KB": 1.0 / 1024, "MB": 1, "GB": 1024}[string(m[2])]
}

func psInt(args ...string) int {
	out, _ := exec.Command("ps", args...).Output()
	v, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return v
}

func threads(pid int) int {
	out, _ := exec.Command("ps", "-M", "-p", strconv.Itoa(pid)).Output()
	return strings.Count(string(out), "\n") - 1
}

var rmSamples = []metrics.Sample{
	{Name: "/memory/classes/heap/objects:bytes"},
	{Name: "/gc/heap/objects:objects"},
	{Name: "/gc/pauses:seconds"},
	{Name: "/cpu/classes/gc/total:cpu-seconds"},
	{Name: "/cpu/classes/total:cpu-seconds"},
}

// sample reads process resources. External tools (ps/footprint) only when full.
func sample(full bool) Res {
	metrics.Read(rmSamples)
	r := Res{
		HeapMB:      float64(rmSamples[0].Value.Uint64()) / (1 << 20),
		HeapObjects: rmSamples[1].Value.Uint64(),
		Goroutines:  runtime.NumGoroutine(),
	}
	if h := rmSamples[2].Value.Float64Histogram(); h != nil {
		var n, c uint64
		for _, v := range h.Counts {
			n += v
		}
		for i, v := range h.Counts {
			c += v
			if n > 0 && c*100 >= n*99 {
				r.GCPauseP99ms = h.Buckets[i+1] * 1000
				break
			}
		}
	}
	if tot := rmSamples[4].Value.Float64(); tot > 0 {
		r.GCCPUFrac = rmSamples[3].Value.Float64() / tot
	}
	if fds, err := os.ReadDir("/dev/fd"); err == nil {
		r.FDs = len(fds)
	}
	if full {
		pid := os.Getpid()
		r.RSSMB = float64(psInt("-o", "rss=", "-p", strconv.Itoa(pid))) / 1024
		r.Threads = threads(pid)
		r.FootprintMB = footprintMB(pid)
	}
	return r
}

func (r Res) cols() []string {
	return []string{f2(r.RSSMB), f2(r.FootprintMB), f2(r.HeapMB), fmt.Sprint(r.HeapObjects), fmt.Sprint(r.Goroutines),
		fmt.Sprint(r.Threads), fmt.Sprint(r.FDs), f3(r.GCPauseP99ms), f3(r.GCCPUFrac)}
}

var resHeader = []string{"rss_mb", "footprint_mb", "heap_mb", "heap_objects", "goroutines", "threads", "fds", "gc_pause_p99_ms", "gc_cpu_frac"}

// cpuSeconds returns this process's user+sys CPU time plus that of child pids.
func cpuSeconds(children []int) float64 {
	var ru syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	t := float64(ru.Utime.Nano()+ru.Stime.Nano()) / 1e9
	for _, pid := range children {
		out, _ := exec.Command("ps", "-o", "time=", "-p", strconv.Itoa(pid)).Output()
		t += parsePsTime(strings.TrimSpace(string(out)))
	}
	return t
}

func parsePsTime(s string) float64 { // [[hh:]mm:]ss.cc
	var t float64
	for _, p := range strings.Split(s, ":") {
		v, _ := strconv.ParseFloat(p, 64)
		t = t*60 + v
	}
	return t
}

type CSV struct {
	f *os.File
	w *csv.Writer
}

func newCSV(name string, header ...string) *CSV {
	os.MkdirAll(*outDir, 0o755)
	f, err := os.Create(filepath.Join(*outDir, name))
	if err != nil {
		panic(err)
	}
	c := &CSV{f, csv.NewWriter(f)}
	c.w.Write(header)
	return c
}

func (c *CSV) Row(v ...any) {
	s := make([]string, 0, len(v))
	for _, x := range v {
		switch y := x.(type) {
		case []string:
			s = append(s, y...)
		case float64:
			s = append(s, f3(y))
		case time.Duration:
			s = append(s, f3(ms(y)))
		default:
			s = append(s, fmt.Sprint(y))
		}
	}
	c.w.Write(s)
	c.w.Flush()
}

func (c *CSV) Close() { c.w.Flush(); c.f.Close() }

func ms(d time.Duration) float64 { return float64(d) / 1e6 }
func f2(v float64) string        { return strconv.FormatFloat(v, 'f', 2, 64) }
func f3(v float64) string        { return strconv.FormatFloat(v, 'f', 3, 64) }
func logf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, time.Now().Format("15:04:05 ")+format+"\n", a...)
}
