package retention

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"govoice/server/storage"
)

// Store defines repository operations required by the retention job.
type Store interface {
	PurgeContent(ctx context.Context, cutoff time.Time, limit int) (storage.ContentPurgeResult, error)
	PurgeMetadata(ctx context.Context, cutoff time.Time, limit int) (stats storage.PurgeMetadataStats, err error)
	Preview(ctx context.Context, contentCutoff, metadataCutoff time.Time) (storage.RetentionPreview, error)
}

// RunResult summarizes the outcome of a retention execution.
type RunResult struct {
	SessionsDeleted  int64
	TurnsDeleted     int64
	SpansDeleted     int64
	EventsDeleted    int64
	ToolCallsDeleted int64
	TurnsPurged      int64
}

// RunRecord is one finished run, successful or not, as the status endpoint and the metrics report it.
type RunRecord struct {
	StartedAt time.Time
	Duration  time.Duration
	Result    RunResult
	Err       string // empty when the run completed
}

// Option configures a Job.
type Option func(*Job)

// WithInterval sets the recurring job run interval.
func WithInterval(d time.Duration) Option {
	return func(j *Job) {
		if d > 0 {
			j.interval = d
		}
	}
}

// WithObserver is called with every finished run (the metrics hook). It must not block.
func WithObserver(f func(RunRecord)) Option {
	return func(j *Job) { j.observe = f }
}

// WithLogger sets the structured logger for retention operations.
func WithLogger(logger *slog.Logger) Option {
	return func(j *Job) {
		if logger != nil {
			j.logger = logger
		}
	}
}

// Job performs background retention purges in bounded batches.
type Job struct {
	store    Store
	policy   Policy
	clock    func() time.Time
	interval time.Duration
	logger   *slog.Logger
	observe  func(RunRecord)
	last     atomic.Pointer[RunRecord]

	running  atomic.Bool
	cancel   context.CancelFunc // ends an in-flight run on Stop
	stopChan chan struct{}
	doneChan chan struct{}
	stopOnce sync.Once
}

// NewJob constructs a retention cleanup job.
func NewJob(store Store, policy Policy, opts ...Option) *Job {
	j := &Job{
		store:    store,
		policy:   policy,
		clock:    func() time.Time { return time.Now().UTC() },
		interval: 1 * time.Hour,
		logger:   slog.Default(),
		stopChan: make(chan struct{}),
		doneChan: make(chan struct{}),
	}
	for _, opt := range opts {
		opt(j)
	}
	return j
}

// RunOnce performs one pass of content and metadata purges across all bounded batches and records it (Last).
func (j *Job) RunOnce(ctx context.Context) (RunResult, error) {
	started := j.clock()
	began := time.Now()
	res, err := j.run(ctx)
	rec := RunRecord{StartedAt: started, Duration: time.Since(began), Result: res}
	if err != nil {
		rec.Err = err.Error()
	}
	j.last.Store(&rec)
	if j.observe != nil {
		j.observe(rec)
	}
	return res, err
}

// Last is the most recent finished run, if there has been one since the job was built.
func (j *Job) Last() (RunRecord, bool) {
	if rec := j.last.Load(); rec != nil {
		return *rec, true
	}
	return RunRecord{}, false
}

// Cutoffs are the times before which a run would purge content and metadata now; zero means that pass is off.
func (j *Job) Cutoffs() (content, metadata time.Time) {
	now := j.clock()
	if j.policy.ContentRetention > 0 {
		content = now.Add(-j.policy.ContentRetention)
	}
	if j.policy.MetadataRetention > 0 {
		metadata = now.Add(-j.policy.MetadataRetention)
	}
	return content, metadata
}

// Preview counts what a run started now would remove, without removing anything.
func (j *Job) Preview(ctx context.Context) (storage.RetentionPreview, error) {
	content, metadata := j.Cutoffs()
	return j.store.Preview(ctx, content, metadata)
}

func (j *Job) run(ctx context.Context) (RunResult, error) {
	if err := j.policy.Validate(); err != nil {
		return RunResult{}, err
	}

	batchSize := j.policy.BatchSize
	if batchSize <= 0 {
		batchSize = 500
	}

	var res RunResult
	now := j.clock()

	// 1. Content purge pass
	if j.policy.ContentRetention > 0 {
		contentCutoff := now.Add(-j.policy.ContentRetention)

		// A purged session no longer matches the selection, so each batch makes progress.
		for {
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
			batch, err := j.store.PurgeContent(ctx, contentCutoff, batchSize)
			if err != nil {
				return res, err
			}
			res.TurnsPurged += batch.TurnsPurged
			if batch.Examined < batchSize {
				break
			}
		}
	}

	// 2. Metadata purge pass
	if j.policy.MetadataRetention > 0 {
		metadataCutoff := now.Add(-j.policy.MetadataRetention)

		for {
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
			stats, err := j.store.PurgeMetadata(ctx, metadataCutoff, batchSize)
			if err != nil {
				return res, err
			}
			res.SessionsDeleted += stats.SessionsDeleted
			res.TurnsDeleted += stats.TurnsDeleted
			res.SpansDeleted += stats.SpansDeleted
			res.EventsDeleted += stats.EventsDeleted
			res.ToolCallsDeleted += stats.ToolCallsDeleted

			if stats.SessionsDeleted == 0 && stats.SpansDeleted == 0 && stats.EventsDeleted == 0 {
				break
			}
		}
	}

	return res, nil
}

// Start launches a periodic background goroutine running RunOnce.
func (j *Job) Start(ctx context.Context) {
	if j.running.Load() {
		return
	}
	ctx, j.cancel = context.WithCancel(ctx) // set before running is visible to Stop
	if !j.running.CompareAndSwap(false, true) {
		return
	}

	go func() {
		defer close(j.doneChan)
		ticker := time.NewTicker(j.interval)
		defer ticker.Stop()

		for {
			select {
			case <-j.stopChan:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				runCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
				_, err := j.RunOnce(runCtx)
				cancel()
				if err != nil && j.logger != nil {
					j.logger.Error("retention run failed", slog.String("err", err.Error()))
				}
			}
		}
	}()
}

// Stop stops the background retention job.
func (j *Job) Stop() {
	j.stopOnce.Do(func() {
		if j.running.Load() {
			j.cancel()
			close(j.stopChan)
			<-j.doneChan
			j.running.Store(false)
		}
	})
}
