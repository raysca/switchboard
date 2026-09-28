package vad

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// inProcessBackend implements executorBackend using in-process sharded ORT sessions.
type inProcessBackend struct {
	cfg          Config
	queues       []chan *Job
	wg           sync.WaitGroup
	statCounters atomicStats
	closed       atomic.Bool
}

type inProcessStream struct {
	id          uint64
	backend     *inProcessBackend
	q           chan *Job
	outstanding atomic.Int32
}

func (s *inProcessStream) Submit(j *Job) error {
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

func (s *inProcessStream) Close() {
	s.backend.statCounters.streams.Add(-1)
}

// NewInProcessShardedExecutor creates an in-process sharded Executor.
func NewInProcessShardedExecutor(cfg Config) (*Executor, error) {
	e := &Executor{cfg: cfg}
	b := &inProcessBackend{
		cfg: cfg,
	}
	e.backend = b

	nq := 1
	if cfg.Mode == "sharded" {
		nq = cfg.Workers
	}
	for i := 0; i < nq; i++ {
		b.queues = append(b.queues, make(chan *Job, max(1, cfg.QueueCap/nq)))
	}

	for i := 0; i < cfg.Workers; i++ {
		q := b.queues[i%nq]
		var run func([]*Job) error
		var closer func()

		switch cfg.Mode {
		case "global", "sharded":
			r, err := NewRunner(cfg.Batch, cfg.IntraOp)
			if err != nil {
				return nil, fmt.Errorf("in-process runner: %w", err)
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
		default:
			return nil, fmt.Errorf("unknown in-process mode %q", cfg.Mode)
		}

		b.wg.Add(1)
		go b.worker(q, run, closer)
	}

	return e, nil
}

func (b *inProcessBackend) register(sessionID uint64) Stream {
	b.statCounters.streams.Add(1)
	qIdx := sessionID % uint64(len(b.queues))
	return &inProcessStream{
		id:      sessionID,
		backend: b,
		q:       b.queues[qIdx],
	}
}

func (b *inProcessBackend) stats() Stats {
	return b.statCounters.snapshot(int64(b.queueDepth()))
}

func (b *inProcessBackend) capacity() (available, total int) {
	if b.closed.Load() {
		return 0, b.cfg.Workers
	}
	return b.cfg.Workers, b.cfg.Workers
}

func (b *inProcessBackend) isReady() bool {
	return !b.closed.Load()
}

func (b *inProcessBackend) queueDepth() int {
	var depth int
	for _, q := range b.queues {
		depth += len(q)
	}
	return depth
}

func (b *inProcessBackend) pids() []int {
	return nil // in-process; no child PIDs
}

func (b *inProcessBackend) injectFault(workerIdx int) error {
	return fmt.Errorf("fault injection not supported on in-process executor (would crash host process)")
}

func (b *inProcessBackend) hardKill(workerIdx int) error {
	return fmt.Errorf("hard kill not supported on in-process executor")
}

func (b *inProcessBackend) close() error {
	if b.closed.Swap(true) {
		return nil
	}
	for _, q := range b.queues {
		close(q)
	}
	b.wg.Wait()
	return nil
}

func (b *inProcessBackend) worker(q chan *Job, run func([]*Job) error, closer func()) {
	defer b.wg.Done()
	defer closer()

	B := b.cfg.Batch
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
			if !j.Deadline.IsZero() && now.After(j.Deadline) {
				j.Err = ErrStale
				b.statCounters.stale.Add(1)
				b.finish(j)
				continue
			}
			live = append(live, j)
		}

		if len(live) == 0 {
			continue
		}

		err := run(live)
		fin := time.Now()
		b.statCounters.busyNanos.Add(int64(fin.Sub(now)))
		b.statCounters.batches.Add(1)
		b.statCounters.batchedJobs.Add(int64(len(live)))
		if err != nil {
			b.statCounters.workerErrors.Add(int64(len(live)))
		}

		for _, j := range live {
			j.Finished = fin
			if err != nil {
				j.Err = ErrWorker
			}
			b.finish(j)
		}
	}
}

func (b *inProcessBackend) finish(j *Job) {
	if st, ok := j.Stream.(*inProcessStream); ok {
		st.outstanding.Add(-1)
	}
	if j.Done != nil {
		j.Done(j)
	}
}
