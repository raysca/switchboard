package vad

import (
	"bytes"
	"errors"
	"fmt"
	"io"
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
	cfg          Config
	modelSHA     [32]byte
	workers      []*workerProcess
	queues       []chan *Job
	statCounters atomicStats
	closed       atomic.Bool
	closeMu      sync.RWMutex // Submit (R) vs closing the queues (W)
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
	s.backend.closeMu.RLock()
	defer s.backend.closeMu.RUnlock()
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
	mu         sync.Mutex
	healthy    bool
	crashes    []time.Time
	down       bool          // counted out of backend.availCount
	respawning bool          // a respawnLoop is running
	up         atomic.Bool   // healthy and serving: handOff only sends to an up worker
	quit       chan struct{} // closed by close: ends respawnLoop's wait
	closed     bool
	gapHook    func() // deliberate nil-in-production test seam, read under the wp.mu executeBatch already holds; runs between its health read and markDown
	wg         sync.WaitGroup
	inFlightMu sync.Mutex
	inFlight   map[uint64]*Job
}

func WorkerBinary() string {
	return filepath.Join(Root(), "bin", "vadworker")
}

// NewWorkerProcessExecutor creates a WorkerProcessExecutor with verified Silero artifacts.
func NewWorkerProcessExecutor(cfg Config) (*Executor, error) {
	modelSHA, err := VerifyModelArtifact(ModelPath())
	if err != nil {
		return nil, fmt.Errorf("vad worker executor: %w", err)
	}

	if cfg.WorkerBinary == "" {
		cfg.WorkerBinary = WorkerBinary()
	}

	e := &Executor{cfg: cfg}
	b := &workerProcessBackend{
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
			quit:     make(chan struct{}),
		}
		b.workers = append(b.workers, wp)

		p, err := wp.start()
		if err != nil {
			_ = b.close()
			return nil, fmt.Errorf("vad worker %d startup handshake failed: %w", i, err)
		}
		wp.mu.Lock()
		wp.installLocked(p)
		wp.mu.Unlock()
		b.availCount.Add(1)
		wp.up.Store(true)

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

func (b *workerProcessBackend) close() error {
	// Swap under the write lock: once it is released no Submit can still be
	// between its closed check and its send, so closing the queues is safe.
	b.closeMu.Lock()
	wasClosed := b.closed.Swap(true)
	b.closeMu.Unlock()
	if wasClosed {
		return nil
	}
	for _, wp := range b.workers {
		wp.close()
	}
	for _, q := range b.queues {
		if q != nil { // nil when a later worker failed its startup handshake
			close(q)
		}
	}
	for _, wp := range b.workers {
		wp.wg.Wait()
	}
	return nil
}

// workerProc is a started worker process that passed its handshake.
type workerProc struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
}

func (p workerProc) reap() {
	_ = p.stdin.Close()
	_ = p.stdout.Close()
	_ = p.cmd.Process.Kill()
	_ = p.cmd.Wait()
}

// start starts a worker process and checks its model without taking wp.mu
// (the handshake can take 3 s and the run loop must not wait on it). The
// caller installs the result with installLocked.
func (wp *workerProcess) start() (workerProc, error) {
	cmd := exec.Command(wp.backend.cfg.WorkerBinary, fmt.Sprint(wp.backend.cfg.Batch))
	cmd.Env = WorkerEnv()
	cmd.Stderr = wp.stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return workerProc{}, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return workerProc{}, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return workerProc{}, fmt.Errorf("start worker: %w", err)
	}
	p := workerProc{cmd: cmd, stdin: stdin, stdout: stdout}
	if err := wp.handshake(stdin, stdout); err != nil {
		p.reap()
		return workerProc{}, err
	}
	return p, nil
}

// installLocked makes p the worker's process. wp.mu held.
func (wp *workerProcess) installLocked(p workerProc) {
	wp.cmd, wp.stdin, wp.stdout, wp.healthy = p.cmd, p.stdin, p.stdout, true
}

// handshake sends the expected model hash and checks the worker's answer within 3 s.
func (wp *workerProcess) handshake(stdin io.Writer, stdout io.Reader) error {
	req := HandshakeReq{ExpectedModelSHA256: wp.backend.modelSHA, MaxBatch: uint32(wp.backend.cfg.Batch)}
	if err := WriteFrame(stdin, EncodeHandshakeReq(req)); err != nil {
		return fmt.Errorf("send handshake: %w", err)
	}
	type hsResult struct {
		resp HandshakeResp
		err  error
	}
	hsCh := make(chan hsResult, 1)
	go func() { // ends with the reply, or when reap closes stdout
		payload, err := ReadFrame(stdout, nil)
		if err != nil {
			hsCh <- hsResult{err: err}
			return
		}
		resp, err := DecodeHandshakeResp(payload)
		hsCh <- hsResult{resp: resp, err: err}
	}()
	t := time.NewTimer(3 * time.Second)
	defer t.Stop()
	select {
	case res := <-hsCh:
		switch {
		case res.err != nil:
			return fmt.Errorf("handshake read/decode: %w", res.err)
		case res.resp.Status != StatusOK:
			return fmt.Errorf("%w: worker returned status %d", ErrWorkerHandshake, res.resp.Status)
		case !bytes.Equal(res.resp.ActualModelSHA256[:], wp.backend.modelSHA[:]):
			return fmt.Errorf("%w: worker model SHA mismatch", ErrModelHashMismatch)
		}
		return nil
	case <-t.C:
		return errors.New("worker handshake timed out")
	}
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
	close(wp.quit) // backend.close calls close once per worker
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
	hook := wp.gapHook
	wp.mu.Unlock()

	if !healthy {
		if hook != nil {
			hook()
		}
		// Down (crashed, or killed between batches): restart it off this loop
		// and let a live worker serve these chunks instead of failing them.
		wp.markDown()
		wp.handOff(jobs, ErrWorker)
		return
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
		wp.handleWorkerFailure(jobs, ErrWorker, true)
		return
	}

	if err := WriteFrame(stdin, payload); err != nil {
		wp.handleWorkerFailure(jobs, fmt.Errorf("%w: %v", ErrWorker, err), true) // died before reading them
		return
	}

	resCh := make(chan readResult, 1)

	go func() {
		p, err := ReadFrame(stdout, nil)
		resCh <- readResult{payload: p, err: err}
	}()

	if res, ok := awaitReply(resCh, deadline, wp.backend.cfg.ParentGrace); ok {
		fin := time.Now()
		wp.backend.statCounters.busyNanos.Add(int64(fin.Sub(now)))
		wp.backend.statCounters.batches.Add(1)
		wp.backend.statCounters.batchedJobs.Add(int64(len(jobs)))

		if res.err != nil {
			wp.handleWorkerFailure(jobs, fmt.Errorf("%w: %v", ErrWorker, res.err), false)
			return
		}

		respItems, err := DecodeInferResp(res.payload)
		if err != nil {
			wp.handleWorkerFailure(jobs, fmt.Errorf("%w: decode response: %v", ErrWorker, err), false)
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
				wp.backend.finish(j)
			}
		}
		// By request id from reqItems, not j.RequestID: a finished job may already be
		// resubmitted and re-stamped by another worker (handOff).
		for i, j := range jobs {
			if _, ok := wp.inFlight[reqItems[i].RequestID]; ok {
				delete(wp.inFlight, reqItems[i].RequestID)
				j.Err = ErrWorker
				wp.backend.statCounters.workerErrors.Add(1)
				wp.backend.finish(j)
			}
		}
		wp.inFlightMu.Unlock()
	} else {
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

		wp.markDown()
	}
}

type readResult struct {
	payload []byte
	err     error
}

// awaitReply waits for the worker's reply until deadline, then for one more
// grace period. The deadline is wall-clock time in this process: when the
// parent itself is descheduled (host CPU contention, GC), its timer and a
// reply already in the pipe become ready together, and the loadtest at 384
// calls saw healthy workers killed with the reply arriving <1 ms after a
// timer that fired 100 ms late. The extra grace is measured after the parent
// runs again, so only a worker that stays silent is declared hung.
func awaitReply(resCh <-chan readResult, deadline time.Time, grace time.Duration) (readResult, bool) {
	t := time.NewTimer(time.Until(deadline))
	defer t.Stop()
	select {
	case res := <-resCh:
		return res, true
	case <-t.C:
	}
	t.Reset(grace)
	select {
	case res := <-resCh:
		return res, true
	case <-t.C:
		return readResult{}, false
	}
}

// handleWorkerFailure kills a worker whose pipe failed. The batch goes to a
// live worker only if retry (the worker never read it: the jobs' input and
// state are unchanged); a batch the worker read and died on may be a poison
// chunk, which must not kill every worker in turn, so it fails.
func (wp *workerProcess) handleWorkerFailure(jobs []*Job, err error, retry bool) {
	wp.mu.Lock()
	wp.killAndReapLocked()
	wp.mu.Unlock()

	wp.inFlightMu.Lock()
	for _, j := range jobs {
		delete(wp.inFlight, j.RequestID)
	}
	wp.inFlightMu.Unlock()

	wp.markDown()
	if retry {
		wp.handOff(jobs, err)
		return
	}
	for _, j := range jobs {
		j.Err = err
		wp.backend.statCounters.workerErrors.Add(1)
		wp.backend.finish(j)
	}
}

// markDown takes the worker out of capacity and of handOff targets and starts
// one respawnLoop, unless the backend is closing. Called on the run loop.
func (wp *workerProcess) markDown() {
	wp.mu.Lock()
	defer wp.mu.Unlock()
	if wp.healthy && !wp.closed {
		// A respawn finished since the caller saw the worker down (every other
		// caller just ran killAndReapLocked, which clears healthy): marking it
		// down would orphan the new process behind a second respawnLoop.
		return
	}
	wp.up.Store(false)
	if !wp.down {
		wp.down = true
		wp.backend.availCount.Add(-1)
	}
	if wp.closed || wp.respawning {
		return
	}
	wp.respawning = true
	wait := wp.noteCrashLocked()
	wp.wg.Add(1) // the run loop holds a wg count here, so this never races backend.close's Wait at zero
	go wp.respawnLoop(wait)
}

// respawnLoop restarts the worker off its run loop, backing off while the
// restart keeps failing, until it succeeds or the backend closes.
func (wp *workerProcess) respawnLoop(wait time.Duration) {
	defer wp.wg.Done()
	for {
		t := time.NewTimer(wait)
		select {
		case <-wp.quit:
			t.Stop()
			return
		case <-t.C:
		}
		if p, err := wp.start(); err == nil {
			// One critical section: a crash noticed after this sees a worker that
			// is up and healthy, marks it down and starts a new respawnLoop.
			wp.mu.Lock()
			if wp.closed {
				wp.mu.Unlock()
				p.reap()
				return
			}
			wp.installLocked(p)
			wp.respawning, wp.down = false, false
			wp.backend.availCount.Add(1)
			wp.up.Store(true)
			wp.mu.Unlock()
			return
		}
		wp.mu.Lock()
		wait = wp.noteCrashLocked()
		wp.mu.Unlock()
	}
}

// noteCrashLocked records a failure (one restart) and returns the wait before
// the next start: 20 ms doubling to 1 s, and 1 s while crash-looping. wp.mu held.
func (wp *workerProcess) noteCrashLocked() time.Duration {
	now := time.Now()
	cutoff := now.Add(-CrashLoopWindow)
	pruned := wp.crashes[:0]
	for _, t := range wp.crashes {
		if t.After(cutoff) {
			pruned = append(pruned, t)
		}
	}
	wp.crashes = append(pruned, now)
	wp.backend.statCounters.restarts.Add(1)
	n := len(wp.crashes)
	if n >= CrashLoopThreshold {
		return time.Second
	}
	return min(10*time.Millisecond*time.Duration(1<<min(n, 6)), time.Second)
}

// handOff gives the jobs of a down worker to a live one: a Silero job carries
// its detector state (Job.State) and a session has one outstanding job, so any
// worker can serve it in order. A job no live worker can take now fails with
// failErr, as before. Called on the run loop.
func (wp *workerProcess) handOff(jobs []*Job, failErr error) {
	b := wp.backend
	failed := jobs[:0]
	b.closeMu.RLock() // Submit's guard: no queue is closed while it is held
	for _, j := range jobs {
		if q := b.liveQueue(wp.id); q != nil && !b.closed.Load() {
			select {
			case q <- j:
				continue
			default:
			}
		}
		failed = append(failed, j)
	}
	b.closeMu.RUnlock()
	for _, j := range failed {
		j.Err = failErr
		b.statCounters.workerErrors.Add(1)
		b.finish(j)
	}
}

// liveQueue is the queue of the next worker after from that is up, or nil.
func (b *workerProcessBackend) liveQueue(from int) chan *Job {
	for i := 1; i < len(b.workers); i++ {
		if w := b.workers[(from+i)%len(b.workers)]; w.up.Load() {
			return w.q
		}
	}
	return nil
}

func (b *workerProcessBackend) finish(j *Job) {
	if st, ok := j.Stream.(*workerStream); ok {
		st.outstanding.Add(-1)
	}
	if j.Done != nil {
		j.Done(j)
	}
}
