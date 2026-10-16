package smartturn

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"govoice/vad"
)

// Wire protocol (little endian), parent <-> cmd/turnworker over stdin/stdout:
//
//	handshake: parent sends the 32-byte expected model SHA-256; the worker
//	           answers u32 status (0 ok) + its 32-byte actual SHA-256.
//	request:   u32 sample count n (1..WindowLen), then n int16 samples.
//	response:  u32 status (0 ok), u32 float32 bits of P(complete).
//
// stdout carries only the protocol; stderr is bounded diagnostics.

const (
	statusOK       = 0
	statusErr      = 1
	handshakeTime  = 5 * time.Second
	crashThreshold = 5
	crashWindow    = 10 * time.Second
)

// WorkerBinary is the default turnworker location.
func WorkerBinary() string { return filepath.Join(vad.Root(), "bin", "turnworker") }

// PoolConfig tunes the supervised worker pool.
type PoolConfig struct {
	Binary   string        // default WorkerBinary()
	Workers  int           // child processes; each runs one inference at a time (default 1)
	IntraOp  int           // ORT threads per worker (default 2)
	HardStop time.Duration // a request older than this kills its worker (default 2s)
}

// Pool is the process-isolated Backend: children are supervised, a crash or
// hang kills and respawns the child with backoff (crash-loop throttled), and
// a request with no idle worker fails fast with ErrBusy.
type Pool struct {
	cfg    PoolConfig
	sha    [32]byte
	ws     []*worker
	idle   chan *worker
	up     atomic.Int32
	closed atomic.Bool
	wg     sync.WaitGroup
}

type worker struct {
	p       *Pool
	mu      sync.Mutex
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	stderr  tailBuffer
	crashes []time.Time
}

// tailBuffer keeps the last 4 KiB a child wrote to stderr, for start errors.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - 4096; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

var _ Backend = (*Pool)(nil)

// NewPool verifies the model, then starts and handshakes every worker.
func NewPool(cfg PoolConfig) (*Pool, error) {
	if cfg.Binary == "" {
		cfg.Binary = WorkerBinary()
	}
	cfg.Workers = max(cfg.Workers, 1)
	if cfg.IntraOp < 1 {
		cfg.IntraOp = 2 // 1 thread: ~45 ms/inference, 2: ~29 ms (M4 Pro)
	}
	if cfg.HardStop <= 0 {
		cfg.HardStop = 2 * time.Second
	}
	sha, err := VerifyModel(ModelPath())
	if err != nil {
		return nil, err
	}
	p := &Pool{cfg: cfg, sha: sha, idle: make(chan *worker, cfg.Workers)}
	for i := 0; i < cfg.Workers; i++ {
		w := &worker{p: p}
		p.ws = append(p.ws, w)
		if err := w.start(); err != nil {
			_ = p.Close()
			return nil, fmt.Errorf("smartturn worker %d: %w", i, err)
		}
		p.up.Add(1)
		p.idle <- w
	}
	return p, nil
}

func (p *Pool) Ready() bool { return p.up.Load() > 0 && !p.closed.Load() }

// Capacity reports live and configured workers.
func (p *Pool) Capacity() (up, total int) { return int(p.up.Load()), len(p.ws) }

func (p *Pool) Close() error {
	if p.closed.Swap(true) {
		return nil
	}
	for _, w := range p.ws {
		w.mu.Lock()
		w.killLocked()
		w.mu.Unlock()
	}
	p.wg.Wait()
	return nil
}

func (w *worker) start() error {
	cmd := exec.Command(w.p.cfg.Binary, fmt.Sprint(w.p.cfg.IntraOp))
	cmd.Env = vad.WorkerEnv() // secrets stay in the parent
	cmd.Stderr = &w.stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	w.mu.Lock()
	w.cmd, w.stdin, w.stdout = cmd, stdin, stdout
	if w.p.closed.Load() { // Close ran while we were starting: it cannot see this child
		w.killLocked()
		w.mu.Unlock()
		return ErrUnavailable
	}
	w.mu.Unlock()
	t := time.AfterFunc(handshakeTime, func() { _ = cmd.Process.Kill() })
	defer t.Stop()
	var resp [36]byte
	if _, err = stdin.Write(w.p.sha[:]); err == nil {
		_, err = io.ReadFull(stdout, resp[:])
	}
	if err == nil && (binary.LittleEndian.Uint32(resp[:4]) != statusOK || [32]byte(resp[4:]) != w.p.sha) {
		err = ErrModelHashMismatch
	}
	if err != nil {
		w.mu.Lock()
		w.killLocked()
		w.mu.Unlock()
		return fmt.Errorf("handshake: %w (stderr: %q)", err, w.stderr.String())
	}
	return nil
}

func (w *worker) killLocked() {
	if w.cmd == nil {
		return
	}
	_ = w.stdin.Close()
	_ = w.cmd.Process.Kill()
	_ = w.stdout.Close()
	_ = w.cmd.Wait()
	w.cmd = nil
}

// Infer runs one request on an idle worker. If ctx ends first the result is
// abandoned but the worker stays checked out until the child answers (or
// HardStop kills it), so a late reply can never be read by the next request.
func (p *Pool) Infer(ctx context.Context, pcm []int16) (float32, error) {
	if !p.Ready() {
		return 0, ErrUnavailable
	}
	if len(pcm) == 0 || len(pcm) > WindowLen {
		return 0, fmt.Errorf("smartturn: %d samples out of range", len(pcm))
	}
	var w *worker
	select {
	case w = <-p.idle:
	default:
		return 0, ErrBusy
	}
	type result struct {
		p   float32
		err error
	}
	ch := make(chan result, 1)
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		v, err := w.roundTrip(pcm)
		if err != nil {
			w.crash()
		} else {
			p.idle <- w
		}
		ch <- result{v, err}
	}()
	select {
	case r := <-ch:
		return r.p, r.err
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (w *worker) roundTrip(pcm []int16) (float32, error) {
	w.mu.Lock()
	stdin, stdout, cmd := w.stdin, w.stdout, w.cmd
	w.mu.Unlock()
	if cmd == nil {
		return 0, ErrUnavailable
	}
	hard := time.AfterFunc(w.p.cfg.HardStop, func() { _ = cmd.Process.Kill() }) // unblocks the read
	defer hard.Stop()
	req := make([]byte, 4+2*len(pcm))
	binary.LittleEndian.PutUint32(req, uint32(len(pcm)))
	for i, s := range pcm {
		binary.LittleEndian.PutUint16(req[4+2*i:], uint16(s))
	}
	if _, err := stdin.Write(req); err != nil {
		return 0, err
	}
	var resp [8]byte
	if _, err := io.ReadFull(stdout, resp[:]); err != nil {
		return 0, err
	}
	if binary.LittleEndian.Uint32(resp[:4]) != statusOK {
		return 0, errors.New("smartturn: worker reported an error")
	}
	v := math.Float32frombits(binary.LittleEndian.Uint32(resp[4:]))
	if v != v || v < 0 || v > 1 {
		return 0, errors.New("smartturn: bad worker output")
	}
	return v, nil
}

// crash kills the worker, counts it, and respawns it in the background; the
// worker rejoins idle only after a successful handshake.
func (w *worker) crash() {
	w.mu.Lock()
	w.killLocked()
	w.mu.Unlock()
	if w.p.closed.Load() {
		return
	}
	stats.crashes.Add(1)
	w.p.up.Add(-1)
	w.p.wg.Add(1)
	go func() {
		defer w.p.wg.Done()
		for attempt := 1; !w.p.closed.Load(); attempt++ {
			now := time.Now()
			kept := w.crashes[:0]
			for _, t := range append(w.crashes, now) {
				if now.Sub(t) < crashWindow {
					kept = append(kept, t)
				}
			}
			w.crashes = kept
			delay := 10 * time.Millisecond << min(len(kept), 6)
			if len(kept) >= crashThreshold {
				delay = time.Second // crash loop: stay down, retry slowly
			}
			time.Sleep(delay)
			if w.p.closed.Load() {
				return
			}
			err := w.start()
			if err == nil {
				w.p.up.Add(1)
				w.p.idle <- w
				return
			}
			// Bounded: attempts are paced by the backoff above (1/s once crash-looping).
			slog.Warn("smartturn: worker respawn failed", "attempt", attempt, "backoff", delay, "error", err)
		}
	}()
}

// ---- in-process backend (tests, dev) ----

// InProcess runs the model inside the server process: no crash isolation.
type InProcess struct {
	mu sync.Mutex
	r  *Runner
}

var _ Backend = (*InProcess)(nil)

func NewInProcess(modelPath string) (*InProcess, error) {
	r, err := NewRunner(modelPath, 1) // hash-checked
	if err != nil {
		return nil, err
	}
	return &InProcess{r: r}, nil
}

func (b *InProcess) Infer(ctx context.Context, pcm []int16) (float32, error) {
	if !b.mu.TryLock() {
		return 0, ErrBusy
	}
	defer b.mu.Unlock()
	if b.r == nil {
		return 0, ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return b.r.Infer(pcm)
}

func (b *InProcess) Ready() bool { return b.r != nil }

func (b *InProcess) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.r != nil {
		b.r.Close()
		b.r = nil
	}
	return nil
}
