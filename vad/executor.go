package vad

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"govoice/voice"
)

var (
	ErrStale        = errors.New("vad job stale (deadline passed before execution)")
	ErrExecFull     = errors.New("vad executor queue full")
	ErrSessionLimit = errors.New("vad per-session outstanding limit")
	ErrWorker       = errors.New("vad worker failed")
)

type Job struct {
	Stream *Stream
	Seq    uint64
	Input  [InputSamples]float32
	State  [StateLen]float32
	Prob   float32

	Due, Deadline, Submitted, Started, Finished time.Time
	Err                                         error
	Done                                        func(*Job) // called on a worker goroutine
}

type Config struct {
	Mode            string // global | sharded | process | stub
	Workers         int    // workers (global/stub), shards (sharded), child processes (process)
	Batch           int
	QueueCap        int // total across queues
	IntraOp         int
	PerSessionLimit int
	StubCost        time.Duration // stub: busy-wait per job instead of ORT (harness control)
}

func (c Config) String() string {
	return fmt.Sprintf("%s-w%d-b%d-i%d", c.Mode, c.Workers, c.Batch, c.IntraOp)
}

type Executor struct {
	cfg    Config
	queues []chan *Job
	wg     sync.WaitGroup

	QueueWait voice.Hist // submitted -> worker picked up
	Inference voice.Hist // worker start -> finish (whole batch wall time)

	streams, submitted, rejectedFull, rejectedLimit, stale, workerErrors, restarts atomic.Int64
	batches, batchedJobs, busyNanos                                                atomic.Int64

	mu   sync.Mutex
	pids []int
}

type Stats struct {
	Streams, Submitted, RejectedFull, RejectedLimit, Stale, WorkerErrors, Restarts, QueueDepth int64
	AvgBatch                                                                                   float64
	BusyNanos                                                                                  int64
}

func (e *Executor) Stats() Stats {
	s := Stats{Streams: e.streams.Load(), Submitted: e.submitted.Load(), RejectedFull: e.rejectedFull.Load(),
		RejectedLimit: e.rejectedLimit.Load(), Stale: e.stale.Load(), WorkerErrors: e.workerErrors.Load(),
		Restarts: e.restarts.Load(), QueueDepth: int64(e.QueueDepth()), BusyNanos: e.busyNanos.Load()}
	if b := e.batches.Load(); b > 0 {
		s.AvgBatch = float64(e.batchedJobs.Load()) / float64(b)
	}
	return s
}

func (e *Executor) Config() Config { return e.cfg }

func (e *Executor) QueueDepth() int {
	n := 0
	for _, q := range e.queues {
		n += len(q)
	}
	return n
}

func (e *Executor) PIDs() []int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]int(nil), e.pids...)
}

func New(cfg Config) (*Executor, error) {
	if cfg.Batch < 1 {
		cfg.Batch = 1
	}
	if cfg.IntraOp < 1 {
		cfg.IntraOp = 1
	}
	if cfg.PerSessionLimit < 1 {
		cfg.PerSessionLimit = 1
	}
	if cfg.QueueCap < 1 {
		cfg.QueueCap = 4096
	}
	e := &Executor{cfg: cfg}
	nq := 1
	if cfg.Mode == "sharded" {
		nq = cfg.Workers
	}
	for i := 0; i < nq; i++ {
		e.queues = append(e.queues, make(chan *Job, max(1, cfg.QueueCap/nq)))
	}
	for i := 0; i < cfg.Workers; i++ {
		q := e.queues[i%nq]
		var run func([]*Job) error
		var closer func()
		switch cfg.Mode {
		case "global", "sharded":
			r, err := NewRunner(cfg.Batch, cfg.IntraOp)
			if err != nil {
				return nil, err
			}
			run, closer = r.Run, r.Close
		case "stub":
			cost := cfg.StubCost
			run = func(jobs []*Job) error {
				for t := time.Now(); time.Since(t) < cost*time.Duration(len(jobs)); {
				}
				return nil
			}
			closer = func() {}
		case "process":
			c := &child{e: e, batch: cfg.Batch}
			if err := c.start(); err != nil {
				return nil, err
			}
			run, closer = c.run, c.stop
		default:
			return nil, fmt.Errorf("unknown vad mode %q", cfg.Mode)
		}
		e.wg.Add(1)
		go e.worker(q, run, closer)
	}
	return e, nil
}

func (e *Executor) Close() {
	for _, q := range e.queues {
		close(q)
	}
	e.wg.Wait()
}

func (e *Executor) worker(q chan *Job, run func([]*Job) error, closer func()) {
	defer e.wg.Done()
	defer closer()
	B := e.cfg.Batch
	batch := make([]*Job, 0, B)
	for j := range q {
		batch = append(batch[:0], j)
	drain:
		for len(batch) < B {
			select {
			case j2, ok := <-q:
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
			if now.After(j.Deadline) {
				j.Err = ErrStale
				e.stale.Add(1)
				e.finish(j)
				continue
			}
			live = append(live, j)
		}
		if len(live) == 0 {
			continue
		}
		err := run(live)
		fin := time.Now()
		e.busyNanos.Add(int64(fin.Sub(now)))
		e.batches.Add(1)
		e.batchedJobs.Add(int64(len(live)))
		if err != nil {
			e.workerErrors.Add(int64(len(live)))
		}
		for _, j := range live {
			j.Finished = fin
			if err != nil {
				j.Err = ErrWorker
			}
			e.QueueWait.Record(j.Started.Sub(j.Submitted))
			e.Inference.Record(fin.Sub(j.Started))
			e.finish(j)
		}
	}
}

func (e *Executor) finish(j *Job) {
	j.Stream.outstanding.Add(-1)
	j.Done(j)
}

// Stream is a session's registration with the shared runtime. The streaming
// state itself travels in the Job, so any worker (or process) can run any job.
type Stream struct {
	ID          uint64
	e           *Executor
	q           chan *Job
	outstanding atomic.Int32
}

func (e *Executor) Register(id uint64) *Stream {
	e.streams.Add(1)
	return &Stream{ID: id, e: e, q: e.queues[id%uint64(len(e.queues))]}
}

func (s *Stream) Close() { s.e.streams.Add(-1) }

// Submit is non-blocking: enforces per-session fairness and bounded queueing.
func (s *Stream) Submit(j *Job) error {
	e := s.e
	if int(s.outstanding.Add(1)) > e.cfg.PerSessionLimit {
		s.outstanding.Add(-1)
		e.rejectedLimit.Add(1)
		return ErrSessionLimit
	}
	j.Stream, j.Err = s, nil
	j.Submitted = time.Now()
	select {
	case s.q <- j:
		e.submitted.Add(1)
		return nil
	default:
		s.outstanding.Add(-1)
		e.rejectedFull.Add(1)
		return ErrExecFull
	}
}

// ---- Strategy C: local worker processes over stdin/stdout ----

type child struct {
	e     *Executor
	batch int
	cmd   *exec.Cmd
	wc    io.WriteCloser
	w     *bufio.Writer
	r     *bufio.Reader
	pid   int
}

func WorkerBinary() string { return filepath.Join(Root(), "bin", "vadworker") }

func (c *child) start() error {
	cmd := exec.Command(WorkerBinary(), fmt.Sprint(c.batch))
	cmd.Stderr = os.Stderr
	w, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	r, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	c.cmd, c.wc, c.w, c.r = cmd, w, bufio.NewWriterSize(w, 1<<16), bufio.NewReaderSize(r, 1<<16)
	c.e.mu.Lock()
	if c.pid != 0 {
		for i, p := range c.e.pids {
			if p == c.pid {
				c.e.pids = append(c.e.pids[:i], c.e.pids[i+1:]...)
				break
			}
		}
	}
	c.pid = cmd.Process.Pid
	c.e.pids = append(c.e.pids, c.pid)
	c.e.mu.Unlock()
	return nil
}

func f32bytes(p *float32, n int) []byte { return unsafe.Slice((*byte)(unsafe.Pointer(p)), n*4) }

func (c *child) run(jobs []*Job) error {
	err := c.roundtrip(jobs)
	if err != nil { // crashed worker: in-flight jobs fail, respawn
		c.stop()
		c.e.restarts.Add(1)
		if serr := c.start(); serr != nil {
			fmt.Fprintln(os.Stderr, "vadworker respawn failed:", serr)
		}
	}
	return err
}

func (c *child) roundtrip(jobs []*Job) error {
	n := uint32(len(jobs))
	if _, err := c.w.Write(unsafe.Slice((*byte)(unsafe.Pointer(&n)), 4)); err != nil {
		return err
	}
	for _, j := range jobs {
		if _, err := c.w.Write(f32bytes(&j.Input[0], InputSamples)); err != nil {
			return err
		}
		if _, err := c.w.Write(f32bytes(&j.State[0], StateLen)); err != nil {
			return err
		}
	}
	if err := c.w.Flush(); err != nil {
		return err
	}
	for _, j := range jobs {
		if _, err := io.ReadFull(c.r, f32bytes(&j.Prob, 1)); err != nil {
			return err
		}
		if _, err := io.ReadFull(c.r, f32bytes(&j.State[0], StateLen)); err != nil {
			return err
		}
	}
	return nil
}

func (c *child) stop() {
	c.wc.Close()
	c.cmd.Process.Kill()
	c.cmd.Wait()
}
