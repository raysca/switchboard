package vad

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// CrashLoopThreshold is the number of crashes within CrashLoopWindow that triggers crash-loop status.
	CrashLoopThreshold = 5
	CrashLoopWindow    = 10 * time.Second
	MaxStderrBytes     = 64 * 1024
)

// workerProcessBackend implements executorBackend by managing isolated child worker processes.
type workerProcessBackend struct {
	exec         *Executor
	cfg          Config
	modelSHA     [32]byte
	workers      []*workerProcess
	queues       []chan *Job
	statCounters atomicStats
	closed       atomic.Bool
	nextReqID    atomic.Uint64
	availCount   atomic.Int32
}

type workerStream struct {
	id          uint64
	backend     *workerProcessBackend
	q           chan *Job
	outstanding atomic.Int32
}

func (s *workerStream) Submit(j *Job) error {
	if s.backend.closed.Load() {
		return ErrWorker
	}
	if int(s.outstanding.Add(1)) > s.backend.cfg.PerSessionLimit {
		s.outstanding.Add(-1)
		s.backend.statCounters.rejectedLimit.Add(1)
		return ErrSessionLimit
	}
	j.Stream = s
	j.SessionID = s.id
	j.Submitted = time.Now()
	j.Err = nil

	select {
	case s.q <- j:
		s.backend.statCounters.submitted.Add(1)
		return nil
	default:
		s.outstanding.Add(-1)
		s.backend.statCounters.rejectedFull.Add(1)
		return ErrExecFull
	}
}

func (s *workerStream) Close() {
	s.backend.statCounters.streams.Add(-1)
}

// boundedBuffer is a thread-safe circular buffer for child stderr diagnostics.
type boundedBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func newBoundedBuffer(max int) *boundedBuffer {
	return &boundedBuffer{max: max, buf: make([]byte, 0, max)}
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if len(b.buf)+n > b.max {
		overflow := (len(b.buf) + n) - b.max
		if overflow >= len(b.buf) {
			b.buf = b.buf[:0]
			if n > b.max {
				p = p[n-b.max:]
			}
		} else {
			b.buf = b.buf[overflow:]
		}
	}
	b.buf = append(b.buf, p...)
	return n, nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

type workerProcess struct {
	id         int
	backend    *workerProcessBackend
	q          chan *Job
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	stdout     io.ReadCloser
	stderr     *boundedBuffer
	pid        int
	mu         sync.Mutex
	healthy    bool
	crashes    []time.Time
	closed     bool
	wg         sync.WaitGroup
	inFlightMu sync.Mutex
	inFlight   map[uint64]*Job
}

func WorkerBinary() string {
	return filepath.Join(Root(), "bin", "vadworker")
}

// EnsureWorkerBinary builds bin/vadworker if not present.
func EnsureWorkerBinary() (string, error) {
	binPath := WorkerBinary()
	if _, err := os.Stat(binPath); err == nil {
		return binPath, nil
	}
	if err := os.MkdirAll(filepath.Dir(binPath), 0755); err != nil {
		return "", err
	}
	cmd := exec.Command("go", "build", "-o", binPath, "./cmd/vadworker")
	cmd.Dir = Root()
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("compile vadworker: %w (%s)", err, string(out))
	}
	return binPath, nil
}

// NewWorkerProcessExecutor creates a WorkerProcessExecutor with verified Silero artifacts.
func NewWorkerProcessExecutor(cfg Config) (*Executor, error) {
	modelSHA, err := VerifyModelArtifact(ModelPath())
	if err != nil {
		return nil, fmt.Errorf("vad worker executor: %w", err)
	}

	binPath := cfg.WorkerBinary
	if binPath == "" {
		var err error
		binPath, err = EnsureWorkerBinary()
		if err != nil {
			return nil, fmt.Errorf("vad worker binary: %w", err)
		}
		cfg.WorkerBinary = binPath
	}

	e := &Executor{cfg: cfg}
	b := &workerProcessBackend{
		exec:     e,
		cfg:      cfg,
		modelSHA: modelSHA,
		queues:   make([]chan *Job, cfg.Workers),
	}
	e.backend = b

	for i := 0; i < cfg.Workers; i++ {
		q := make(chan *Job, max(1, cfg.QueueCap/cfg.Workers))
		b.queues[i] = q

		wp := &workerProcess{
			id:       i,
			backend:  b,
			q:        q,
			stderr:   newBoundedBuffer(MaxStderrBytes),
			inFlight: make(map[uint64]*Job),
		}
		b.workers = append(b.workers, wp)

		if err := wp.startAndHandshake(); err != nil {
			_ = b.close()
			return nil, fmt.Errorf("vad worker %d startup handshake failed: %w", i, err)
		}
		b.availCount.Add(1)

		wp.wg.Add(1)
		go wp.runLoop()
	}

	return e, nil
}

func (b *workerProcessBackend) register(sessionID uint64) Stream {
	b.statCounters.streams.Add(1)
	workerIdx := sessionID % uint64(len(b.queues))
	return &workerStream{
		id:      sessionID,
		backend: b,
		q:       b.queues[workerIdx],
	}
}

func (b *workerProcessBackend) stats() Stats {
	return b.statCounters.snapshot(int64(b.queueDepth()))
}

func (b *workerProcessBackend) capacity() (available, total int) {
	avail := int(b.availCount.Load())
	if avail < 0 {
		avail = 0
	}
	return avail, len(b.workers)
}

func (b *workerProcessBackend) isReady() bool {
	return b.availCount.Load() > 0 && !b.closed.Load()
}

func (b *workerProcessBackend) queueDepth() int {
	var depth int
	for _, q := range b.queues {
		depth += len(q)
	}
	return depth
}

func (b *workerProcessBackend) pids() []int {
	var pids []int
	for _, wp := range b.workers {
		wp.mu.Lock()
		if wp.pid != 0 && wp.healthy {
			pids = append(pids, wp.pid)
		}
		wp.mu.Unlock()
	}
	return pids
}

func (b *workerProcessBackend) injectFault(workerIdx int) error {
	if workerIdx < 0 || workerIdx >= len(b.workers) {
		return errors.New("invalid worker index")
	}
	wp := b.workers[workerIdx]
	wp.mu.Lock()
	defer wp.mu.Unlock()
	if wp.stdin == nil {
		return errors.New("worker not running")
	}
	frame := EncodeFaultInject()
	return WriteFrame(wp.stdin, frame)
}

func (b *workerProcessBackend) hardKill(workerIdx int) error {
	if workerIdx < 0 || workerIdx >= len(b.workers) {
		return errors.New("invalid worker index")
	}
	wp := b.workers[workerIdx]
	wp.mu.Lock()
	defer wp.mu.Unlock()
	if wp.cmd != nil && wp.cmd.Process != nil {
		err := wp.cmd.Process.Kill()
		wp.killAndReapLocked()
		return err
	}
	return errors.New("worker not running")
}

func (b *workerProcessBackend) close() error {
	if b.closed.Swap(true) {
		return nil
	}
	for _, wp := range b.workers {
		wp.close()
	}
	for _, q := range b.queues {
		close(q)
	}
	for _, wp := range b.workers {
		wp.wg.Wait()
	}
	return nil
}

func (wp *workerProcess) startAndHandshake() error {
	wp.mu.Lock()
	defer wp.mu.Unlock()

	cmd := exec.Command(wp.backend.cfg.WorkerBinary, fmt.Sprint(wp.backend.cfg.Batch))
	cmd.Stderr = wp.stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return fmt.Errorf("start worker: %w", err)
	}

	wp.cmd = cmd
	wp.stdin = stdin
	wp.stdout = stdout
	wp.pid = cmd.Process.Pid

	handshakeDeadline := time.Now().Add(3 * time.Second)
	req := HandshakeReq{
		ExpectedModelSHA256: wp.backend.modelSHA,
		MaxBatch:            uint32(wp.backend.cfg.Batch),
	}
	reqData := EncodeHandshakeReq(req)

	if err := WriteFrame(stdin, reqData); err != nil {
		wp.killAndReapLocked()
		return fmt.Errorf("send handshake: %w", err)
	}

	type hsResult struct {
		resp HandshakeResp
		err  error
	}
	hsCh := make(chan hsResult, 1)
	go func() {
		payload, err := ReadFrame(stdout, nil)
		if err != nil {
			hsCh <- hsResult{err: err}
			return
		}
		resp, err := DecodeHandshakeResp(payload)
		hsCh <- hsResult{resp: resp, err: err}
	}()

	select {
	case res := <-hsCh:
		if res.err != nil {
			wp.killAndReapLocked()
			return fmt.Errorf("handshake read/decode: %w", res.err)
		}
		if res.resp.Status != StatusOK {
			wp.killAndReapLocked()
			return fmt.Errorf("%w: worker returned status %d", ErrWorkerHandshake, res.resp.Status)
		}
		if !bytes.Equal(res.resp.ActualModelSHA256[:], wp.backend.modelSHA[:]) {
			wp.killAndReapLocked()
			return fmt.Errorf("%w: worker model SHA mismatch", ErrModelHashMismatch)
		}
	case <-time.After(time.Until(handshakeDeadline)):
		wp.killAndReapLocked()
		return errors.New("worker handshake timed out")
	}

	wp.healthy = true
	return nil
}

func (wp *workerProcess) killAndReapLocked() {
	if wp.stdin != nil {
		_ = wp.stdin.Close()
	}
	if wp.stdout != nil {
		_ = wp.stdout.Close()
	}
	if wp.cmd != nil && wp.cmd.Process != nil {
		_ = wp.cmd.Process.Kill()
		_ = wp.cmd.Wait()
	}
	wp.cmd = nil
	wp.healthy = false
}

func (wp *workerProcess) close() {
	wp.mu.Lock()
	wp.closed = true
	wp.killAndReapLocked()
	wp.mu.Unlock()
}

func (wp *workerProcess) runLoop() {
	defer wp.wg.Done()

	B := wp.backend.cfg.Batch
	batch := make([]*Job, 0, B)

	for j := range wp.q {
		batch = append(batch[:0], j)
	drain:
		for len(batch) < B {
			select {
			case j2, ok := <-wp.q:
				if !ok {
					break drain
				}
				batch = append(batch, j2)
			default:
				break drain
			}
		}

		now := time.Now()
		live := batch[:0]
		for _, j := range batch {
			j.Started = now
			if !j.Deadline.IsZero() && now.After(j.Deadline) {
				j.Err = ErrStale
				wp.backend.statCounters.stale.Add(1)
				wp.backend.finish(j)
				continue
			}
			live = append(live, j)
		}

		if len(live) == 0 {
			continue
		}

		wp.executeBatch(live)
	}

	wp.mu.Lock()
	wp.killAndReapLocked()
	wp.mu.Unlock()
}

func (wp *workerProcess) executeBatch(jobs []*Job) {
	wp.mu.Lock()
	healthy := wp.healthy && !wp.closed
	wp.mu.Unlock()

	if !healthy {
		if !wp.respawnWithBackoff() {
			for _, j := range jobs {
				j.Err = ErrWorker
				wp.backend.statCounters.workerErrors.Add(1)
				wp.backend.finish(j)
			}
			return
		}
	}

	reqItems := make([]InferItemReq, len(jobs))
	wp.inFlightMu.Lock()
	for i, j := range jobs {
		reqID := wp.backend.nextReqID.Add(1)
		j.RequestID = reqID
		wp.inFlight[reqID] = j

		reqItems[i] = InferItemReq{
			RequestID:         reqID,
			SessionID:         j.SessionID,
			Seq:               j.Seq,
			RelativeTimeoutMs: uint32(wp.backend.cfg.ParentDeadline / time.Millisecond),
			AudioFormat:       AudioFormatPCM16kFloat32,
			State:             j.State,
			Input:             j.Input,
		}
	}
	wp.inFlightMu.Unlock()

	now := time.Now()
	deadline := now.Add(wp.backend.cfg.ParentDeadline + wp.backend.cfg.ParentGrace)

	payload := EncodeInferReq(reqItems)
	wp.mu.Lock()
	stdin := wp.stdin
	stdout := wp.stdout
	cmd := wp.cmd
	wp.mu.Unlock()

	if stdin == nil || stdout == nil {
		wp.handleWorkerFailure(jobs, ErrWorker)
		return
	}

	if err := WriteFrame(stdin, payload); err != nil {
		wp.handleWorkerFailure(jobs, fmt.Errorf("%w: %v", ErrWorker, err))
		return
	}

	type readResult struct {
		payload []byte
		err     error
	}
	resCh := make(chan readResult, 1)

	go func() {
		p, err := ReadFrame(stdout, nil)
		resCh <- readResult{payload: p, err: err}
	}()

	select {
	case res := <-resCh:
		fin := time.Now()
		wp.backend.statCounters.busyNanos.Add(int64(fin.Sub(now)))
		wp.backend.statCounters.batches.Add(1)
		wp.backend.statCounters.batchedJobs.Add(int64(len(jobs)))

		if res.err != nil {
			wp.handleWorkerFailure(jobs, fmt.Errorf("%w: %v", ErrWorker, res.err))
			return
		}

		respItems, err := DecodeInferResp(res.payload)
		if err != nil {
			wp.handleWorkerFailure(jobs, fmt.Errorf("%w: decode response: %v", ErrWorker, err))
			return
		}

		wp.inFlightMu.Lock()
		for _, resp := range respItems {
			if j, ok := wp.inFlight[resp.RequestID]; ok {
				delete(wp.inFlight, resp.RequestID)
				j.Finished = fin
				if resp.Status == StatusOK {
					j.Prob = resp.Prob
					j.State = resp.State
					j.Err = nil
				} else {
					j.Err = ErrWorker
					wp.backend.statCounters.workerErrors.Add(1)
				}
				wp.backend.exec.QueueWait.Record(j.Started.Sub(j.Submitted))
				wp.backend.exec.Inference.Record(fin.Sub(j.Started))
				wp.backend.finish(j)
			}
		}
		for _, j := range jobs {
			if _, ok := wp.inFlight[j.RequestID]; ok {
				delete(wp.inFlight, j.RequestID)
				j.Err = ErrWorker
				wp.backend.statCounters.workerErrors.Add(1)
				wp.backend.finish(j)
			}
		}
		wp.inFlightMu.Unlock()

	case <-time.After(time.Until(deadline)):
		wp.backend.statCounters.workerHangs.Add(1)
		wp.backend.statCounters.workerErrors.Add(int64(len(jobs)))

		wp.mu.Lock()
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		wp.killAndReapLocked()
		wp.mu.Unlock()

		wp.inFlightMu.Lock()
		for _, j := range jobs {
			delete(wp.inFlight, j.RequestID)
			j.Err = ErrWorkerHung
			wp.backend.finish(j)
		}
		wp.inFlightMu.Unlock()

		_ = wp.respawnWithBackoff()
	}
}

func (wp *workerProcess) handleWorkerFailure(jobs []*Job, err error) {
	wp.backend.statCounters.workerErrors.Add(int64(len(jobs)))

	wp.mu.Lock()
	wp.killAndReapLocked()
	wp.mu.Unlock()

	wp.inFlightMu.Lock()
	for _, j := range jobs {
		delete(wp.inFlight, j.RequestID)
		j.Err = err
		wp.backend.finish(j)
	}
	wp.inFlightMu.Unlock()

	_ = wp.respawnWithBackoff()
}

func (wp *workerProcess) respawnWithBackoff() bool {
	wp.mu.Lock()
	if wp.closed {
		wp.mu.Unlock()
		return false
	}
	wp.backend.availCount.Add(-1)
	now := time.Now()
	wp.crashes = append(wp.crashes, now)

	cutoff := now.Add(-CrashLoopWindow)
	pruned := wp.crashes[:0]
	for _, t := range wp.crashes {
		if t.After(cutoff) {
			pruned = append(pruned, t)
		}
	}
	wp.crashes = pruned
	crashCount := len(wp.crashes)
	wp.backend.statCounters.restarts.Add(1)
	wp.mu.Unlock()

	if crashCount >= CrashLoopThreshold {
		time.Sleep(1 * time.Second)
	} else {
		backoff := 10 * time.Millisecond * time.Duration(1<<min(crashCount, 6))
		if backoff > 1000*time.Millisecond {
			backoff = 1000 * time.Millisecond
		}
		time.Sleep(backoff)
	}

	wp.mu.Lock()
	if wp.closed {
		wp.mu.Unlock()
		return false
	}
	wp.mu.Unlock()

	if err := wp.startAndHandshake(); err != nil {
		return false
	}

	wp.backend.availCount.Add(1)
	return true
}

func (b *workerProcessBackend) finish(j *Job) {
	if st, ok := j.Stream.(*workerStream); ok {
		st.outstanding.Add(-1)
	}
	if j.Done != nil {
		j.Done(j)
	}
}
