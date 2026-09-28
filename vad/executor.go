package vad

import (
	"errors"
	"fmt"
	"sync/atomic"
	"time"
)

var (
	ErrStale        = errors.New("vad job stale (deadline passed before execution)")
	ErrExecFull     = errors.New("vad executor queue full")
	ErrSessionLimit = errors.New("vad per-session outstanding limit")
	ErrWorker       = errors.New("vad worker failed")
)

// Job represents a single chunk inference unit.
type Job struct {
	Stream    Stream
	RequestID uint64
	SessionID uint64
	Seq       uint64
	Input     [InputSamples]float32
	State     [StateLen]float32
	Prob      float32

	Due       time.Time
	Deadline  time.Time
	Submitted time.Time
	Started   time.Time
	Finished  time.Time
	Err       error
	Done      func(*Job)
}

// Stream represents a session's interface to submit VAD jobs to the executor.
type Stream interface {
	Submit(j *Job) error
	Close()
}

// SharedExecutor is the common interface implemented by all VAD executors:
// InProcessShardedExecutor (development) and WorkerProcessExecutor (production).
type SharedExecutor interface {
	Register(sessionID uint64) Stream
	Stats() Stats
	Capacity() (available, total int)
	IsReady() bool
	Close() error
}

// executorBackend is the internal interface implemented by in-process and worker-process backends.
type executorBackend interface {
	register(sessionID uint64) Stream
	stats() Stats
	capacity() (available, total int)
	isReady() bool
	pids() []int
	queueDepth() int
	injectFault(workerIdx int) error
	hardKill(workerIdx int) error
	close() error
}

// Stats provides telemetry across all executor workers.
type Stats struct {
	Streams       int64
	Submitted     int64
	RejectedFull  int64
	RejectedLimit int64
	Stale         int64
	WorkerErrors  int64
	WorkerHangs   int64
	Restarts      int64
	QueueDepth    int64
	AvgBatch      float64
	BusyNanos     int64
}

// Config specifies the runtime executor configuration.
type Config struct {
	Mode            string        // "process" (prod default) | "sharded" (dev) | "global" | "stub"
	Workers         int           // number of worker shards or child processes
	Batch           int           // max batch size (1..8)
	QueueCap        int           // total queue capacity across workers
	IntraOp         int           // ORT intra-op threads
	PerSessionLimit int           // maximum outstanding jobs per session (strictly 1)
	ParentDeadline  time.Duration // parent monotonic deadline for worker response
	ParentGrace     time.Duration // grace period before SIGKILL
	WorkerBinary    string        // path to cmd/vadworker binary
	StubCost        time.Duration // stub cost for benchmark harness
}

func (c Config) String() string {
	return fmt.Sprintf("%s-w%d-b%d-i%d", c.Mode, c.Workers, c.Batch, c.IntraOp)
}

// DefaultConfig returns production default configuration (worker-process executor).
func DefaultConfig() Config {
	return Config{
		Mode:            "process",
		Workers:         2,
		Batch:           4,
		QueueCap:        1024,
		IntraOp:         1,
		PerSessionLimit: 1,
		ParentDeadline:  50 * time.Millisecond,
		ParentGrace:     50 * time.Millisecond,
	}
}

// Executor coordinates VAD inference across sessions and exposes telemetry and capacity signals.
type Executor struct {
	cfg     Config
	backend executorBackend
}

// New creates an Executor according to cfg.Mode.
// Default is worker-process executor ("process").
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
		cfg.QueueCap = 1024
	}
	if cfg.Workers < 1 {
		cfg.Workers = 1
	}
	if cfg.ParentDeadline <= 0 {
		cfg.ParentDeadline = 50 * time.Millisecond
	}
	if cfg.ParentGrace <= 0 {
		cfg.ParentGrace = 50 * time.Millisecond
	}

	switch cfg.Mode {
	case "process":
		return NewWorkerProcessExecutor(cfg)
	case "sharded", "global", "stub":
		return NewInProcessShardedExecutor(cfg)
	default:
		return nil, fmt.Errorf("unknown vad executor mode %q (must be 'process', 'sharded', 'global', or 'stub')", cfg.Mode)
	}
}

func (e *Executor) Register(sessionID uint64) Stream {
	return e.backend.register(sessionID)
}

func (e *Executor) Stats() Stats {
	return e.backend.stats()
}

func (e *Executor) Capacity() (available, total int) {
	return e.backend.capacity()
}

func (e *Executor) IsReady() bool {
	return e.backend.isReady()
}

func (e *Executor) Config() Config {
	return e.cfg
}

func (e *Executor) QueueDepth() int {
	return e.backend.queueDepth()
}

func (e *Executor) PIDs() []int {
	return e.backend.pids()
}

func (e *Executor) InjectFault(workerIdx int) error {
	return e.backend.injectFault(workerIdx)
}

func (e *Executor) HardKill(workerIdx int) error {
	return e.backend.hardKill(workerIdx)
}

func (e *Executor) Close() error {
	return e.backend.close()
}

// atomicStats tracks counters safely across goroutines.
type atomicStats struct {
	streams       atomic.Int64
	submitted     atomic.Int64
	rejectedFull  atomic.Int64
	rejectedLimit atomic.Int64
	stale         atomic.Int64
	workerErrors  atomic.Int64
	workerHangs   atomic.Int64
	restarts      atomic.Int64
	batches       atomic.Int64
	batchedJobs   atomic.Int64
	busyNanos     atomic.Int64
}

func (s *atomicStats) snapshot(queueDepth int64) Stats {
	st := Stats{
		Streams:       s.streams.Load(),
		Submitted:     s.submitted.Load(),
		RejectedFull:  s.rejectedFull.Load(),
		RejectedLimit: s.rejectedLimit.Load(),
		Stale:         s.stale.Load(),
		WorkerErrors:  s.workerErrors.Load(),
		WorkerHangs:   s.workerHangs.Load(),
		Restarts:      s.restarts.Load(),
		QueueDepth:    queueDepth,
		BusyNanos:     s.busyNanos.Load(),
	}
	if b := s.batches.Load(); b > 0 {
		st.AvgBatch = float64(s.batchedJobs.Load()) / float64(b)
	}
	return st
}
