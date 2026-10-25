package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"govoice/server/agent"
	"govoice/server/eval"
	"govoice/server/storage"
)

// The worker's timings (spec "Jobs", plan S10). A server restart mid-job costs that job one attempt: its lease
// expires and the next claimer takes it as attempt 2.
const (
	evalLease     = 2 * time.Minute  // a claim's lease; a heartbeat extends it
	evalHeartbeat = 30 * time.Second // how often a running job extends its lease
	evalIdlePoll  = time.Second      // an empty queue is polled this often (plus jitter)
)

// evalStopWait is how long stop waits for the slots before it gives up and logs (a variable so a test can shorten it).
var evalStopWait = 15 * time.Second

// evalJudgeWait bounds one judge completion (a variable so a test can shorten it).
var evalJudgeWait = time.Minute

// Indexes of Metrics.evalJobs, and their label values (voice_eval_jobs_total{result}).
const (
	evalDone = iota
	evalRetry
	evalDead
	evalLeaseLost
)

var evalJobResults = [4]string{"done", "retry", "dead", "lease_lost"}

// evalBackoff is how long a failed job waits before its next attempt: 30 s, doubling, at most 10 minutes.
func evalBackoff(attempts int) time.Duration {
	const base, limit = 30 * time.Second, 10 * time.Minute
	if attempts < 1 {
		return base
	}
	if attempts > 5 { // 30s << 5 is already past the limit; also keeps the shift in range
		return limit
	}
	return min(base<<(attempts-1), limit)
}

// evalRunFunc runs one claimed job. A nil err means results are final. Otherwise category (a short class, never
// content) says why, and final means retrying cannot help.
type evalRunFunc func(ctx context.Context, job storage.EvalJob) (results []storage.EvalResult, category string, final bool, err error)

// evalWorker is a pool of slots that claim eval jobs from the shared queue and run them on this instance.
// Everything it knows is in the database, so any number of instances can run one.
type evalWorker struct {
	s     *Server
	store *storage.Store
	run   evalRunFunc

	lease, heartbeat, idle time.Duration
	backoff                func(attempts int) time.Duration

	host, id string
	ctx      context.Context // owner lifetime
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	waitOnce sync.Once
	done     chan struct{} // closed once every slot has returned
}

func newEvalWorker(s *Server, store *storage.Store) *evalWorker {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "host"
	}
	w := &evalWorker{s: s, store: store, run: s.runCaseJob, lease: evalLease, heartbeat: evalHeartbeat, idle: evalIdlePoll, backoff: evalBackoff,
		host: host, id: storage.NewID()}
	w.ctx, w.cancel = context.WithCancel(context.Background())
	w.done = make(chan struct{})
	return w
}

// startEvalWorker binds the internal eval listener and starts the pool of the management plane, sized
// evaluation.max_concurrent_calls. It runs at the end of Start; 0 turns the worker (and the listener) off on this instance.
func (s *Server) startEvalWorker() {
	m := s.mgmt.Load()
	n := s.cfg.Evaluation.MaxConcurrentCalls
	if m == nil || n <= 0 {
		return
	}
	w := newEvalWorker(s, m.store)
	if !m.eval.CompareAndSwap(nil, w) { // a second Start must not leak the first pool
		w.cancel()
		return
	}
	if err := s.startEvalListener(); err != nil {
		s.logger.Error("eval worker not started", slog.String("err", err.Error()))
		m.eval.CompareAndSwap(w, nil)
		return
	}
	w.start(n)
}

// stopEvalWorker ends the pool and waits for it (at most until ctx ends); safe with no management plane, no worker, or
// after a stop.
func (s *Server) stopEvalWorker(ctx context.Context) {
	if m := s.mgmt.Load(); m != nil {
		m.eval.Load().stop(ctx)
	}
}

func (w *evalWorker) start(slots int) {
	for i := range slots {
		owner := fmt.Sprintf("%s:%s/%d", w.host, w.id, i) // the whole UUID: a prefix of a UUIDv7 repeats within about a minute
		w.wg.Add(1)
		go w.slot(owner)
	}
}

// stop cancels the slots and waits for them, at most evalStopWait and never past ctx (Shutdown's budget). A job in
// flight writes nothing: its lease expires. It reports whether every slot has returned: when not, the store must not
// be closed under them. It can be called again and is nil-safe.
func (w *evalWorker) stop(ctx context.Context) bool {
	if w == nil {
		return true
	}
	w.cancel()
	w.waitOnce.Do(func() { go func() { w.wg.Wait(); close(w.done) }() })
	t := time.NewTimer(evalStopWait)
	defer t.Stop()
	select {
	case <-w.done:
		return true
	case <-t.C:
	case <-ctx.Done():
	}
	w.s.logger.Warn("eval worker still running at stop")
	return false
}

// slot claims and runs jobs until the worker stops. A panic anywhere in one pass (the claim, a write) is logged and the
// slot goes on after a pause: the pool never loses a slot.
func (w *evalWorker) slot(owner string) {
	defer w.wg.Done()
	for w.ctx.Err() == nil {
		if !w.pass(owner) {
			t := time.NewTimer(w.idle + time.Duration(rand.Int64N(int64(w.idle/4)+1)))
			select {
			case <-w.ctx.Done():
				t.Stop()
			case <-t.C:
			}
		}
	}
}

// pass claims one job and runs it; it reports whether it ran one (false: the queue is empty, the claim failed or
// something panicked, so the slot waits).
func (w *evalWorker) pass(owner string) (ran bool) {
	defer func() {
		if r := recover(); r != nil {
			w.s.logger.Error("panic in eval worker", "panic", w.s.redactor.Redact(fmt.Sprint(r)))
			ran = false
		}
	}()
	job, ok, ends, err := w.store.Eval().Claim(w.ctx, owner, time.Now(), w.lease)
	m := w.s.metrics
	m.evalJobs[evalDead].Add(uint64(ends.Dead)) // jobs whose lease expired after their last attempt
	m.evalRunsFinished.Add(uint64(ends.RunsFinished))
	if err != nil && w.ctx.Err() == nil {
		w.s.logger.Warn("eval claim failed", slog.String("err", w.s.redactor.Redact(err.Error())))
	}
	if err != nil || !ok {
		return false
	}
	w.process(job)
	return true
}

// process runs one claimed job under a heartbeat and writes its outcome, fenced by the claim. A stop writes nothing,
// and neither does a job whose lease is lost: another worker owns it now.
func (w *evalWorker) process(job storage.EvalJob) {
	jctx, jcancel := context.WithCancel(w.ctx)
	defer jcancel()
	var lost atomic.Bool
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		w.heartbeats(jctx, jcancel, job, &lost)
	}()
	results, category, final, err := w.safeRun(jctx, job)
	jcancel()
	<-hbDone
	if lost.Load() || w.ctx.Err() != nil {
		return
	}
	m, log, ev, now := w.s.metrics, w.s.logger, w.store.Eval(), time.Now()
	if err == nil {
		finished, werr := ev.Complete(w.ctx, job, now, results)
		switch {
		case errors.Is(werr, storage.ErrLeaseLost):
			m.evalJobs[evalLeaseLost].Add(1)
		case werr != nil:
			log.Warn("eval complete failed", slog.String("job", job.ID), slog.String("err", w.s.redactor.Redact(werr.Error())))
		default:
			m.evalJobs[evalDone].Add(1)
			if finished {
				m.evalRunsFinished.Add(1)
			}
		}
		return
	}
	log.Warn("eval job failed", slog.String("job", job.ID), slog.String("run", job.RunID), slog.Int("attempt", job.Attempts),
		slog.String("category", category), slog.String("err", w.s.redactor.Redact(err.Error())))
	dead, finished, werr := ev.Fail(w.ctx, job, now, category, w.backoff(job.Attempts), final)
	switch {
	case errors.Is(werr, storage.ErrLeaseLost):
		m.evalJobs[evalLeaseLost].Add(1)
	case werr != nil:
		log.Warn("eval fail write failed", slog.String("job", job.ID), slog.String("err", w.s.redactor.Redact(werr.Error())))
	case dead:
		m.evalJobs[evalDead].Add(1)
		if finished {
			m.evalRunsFinished.Add(1)
		}
	default:
		m.evalJobs[evalRetry].Add(1)
	}
}

// safeRun turns a panic in the job (a scorer, a provider, a call) into a retryable failure of category panic: the
// pool never dies of one job. The panic value can carry call text, so it is redacted before it is logged.
func (w *evalWorker) safeRun(ctx context.Context, job storage.EvalJob) (results []storage.EvalResult, category string, final bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			w.s.logger.Error("panic in eval job", slog.String("job", job.ID), "panic", w.s.redactor.Redact(fmt.Sprint(r)))
			results, category, final, err = nil, "panic", false, errors.New("eval job panicked")
		}
	}()
	return w.run(ctx, job)
}

// heartbeats extends the lease until ctx ends. On a lost lease it marks lost, counts it and cancels the job.
func (w *evalWorker) heartbeats(ctx context.Context, cancel context.CancelFunc, job storage.EvalJob, lost *atomic.Bool) {
	defer func() {
		if r := recover(); r != nil {
			w.s.logger.Error("panic in eval heartbeat", "panic", w.s.redactor.Redact(fmt.Sprint(r)))
		}
	}()
	t := time.NewTicker(w.heartbeat)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		err := w.store.Eval().Heartbeat(ctx, job, time.Now(), w.lease)
		switch {
		case errors.Is(err, storage.ErrLeaseLost):
			lost.Store(true) // before the cancel: process reads it once the job returns
			w.s.metrics.evalJobs[evalLeaseLost].Add(1)
			cancel()
			return
		case err != nil && ctx.Err() == nil:
			w.s.logger.Warn("eval heartbeat failed", slog.String("job", job.ID), slog.String("err", w.s.redactor.Redact(err.Error())))
		}
	}
}

// evalLiveRequired reports whether a run of this (rewritten) config would reach a paid model without
// GOVOICE_LIVE=1 on this process. The API refuses such a run when it is enqueued; the worker checks again so that
// a queued job can never spend credit on its own.
func (s *Server) evalLiveRequired(draft json.RawMessage) bool {
	return eval.NeedsLive(draft) && s.env("GOVOICE_LIVE") != "1"
}

// runCaseJob makes the call of one run_case job, scores it and returns the rows to store. The subject of each
// result is the bare job subject: storage derives the stored one. The run row names the version (or the draft), the
// providers mode and so the rewrite.
func (s *Server) runCaseJob(ctx context.Context, job storage.EvalJob) ([]storage.EvalResult, string, bool, error) {
	var cj eval.CaseJob
	if err := json.Unmarshal(job.Payload, &cj); err != nil {
		return nil, "payload", true, fmt.Errorf("job payload: %w", err)
	}
	st := s.store
	ag, err := st.Agents().Current(ctx, job.AgentID)
	switch {
	case errors.Is(err, sql.ErrNoRows), err == nil && (ag.Archived || !ag.Enabled):
		return nil, "agent_gone", true, errors.New("the agent is archived, disabled or deleted")
	case err != nil:
		return nil, "load", false, fmt.Errorf("load agent: %w", err)
	}
	run, err := st.Eval().GetRun(ctx, job.AgentID, job.RunID)
	if err != nil {
		return nil, "load", false, fmt.Errorf("load run: %w", err)
	}

	var (
		config  json.RawMessage
		tools   []agent.ToolSelection
		version int
	)
	if run.Draft {
		config = run.DraftConfigJSON
		if len(run.DraftToolsJSON) > 0 {
			if err := json.Unmarshal(run.DraftToolsJSON, &tools); err != nil {
				return nil, "payload", true, fmt.Errorf("draft tools: %w", err)
			}
		}
	} else {
		v, err := st.Agents().Version(ctx, run.AgentVersionID)
		if err != nil {
			return nil, "load", false, fmt.Errorf("load version: %w", err)
		}
		config, version = v.ConfigJSON, v.Version
		for _, t := range v.MCPTools {
			tools = append(tools, agent.ToolSelection{ServerID: t.ServerID, RemoteName: t.RemoteName, SchemaHash: t.SchemaHash, Required: t.Required})
		}
	}
	draft, err := eval.Rewrite(config, run.Providers)
	if err != nil {
		return nil, "rewrite", true, err
	}
	if s.evalLiveRequired(draft) {
		return nil, "live_required", true, errors.New("the run reaches a paid model and this process has no GOVOICE_LIVE=1")
	}
	def, err := agent.Decode(config)
	if err != nil {
		return nil, "rewrite", true, err
	}
	suite, err := st.Eval().Suite(ctx, job.AgentID)
	if err != nil {
		return nil, "load", false, err
	}
	settings, err := st.Eval().ScorerSettings(ctx, job.AgentID)
	if err != nil {
		return nil, "load", false, err
	}
	if err := st.Eval().MarkRunStarted(ctx, run.ID, time.Now()); err != nil {
		return nil, "load", false, err
	}

	ea := eval.Agent{ID: ag.ID, Name: ag.Name, VersionID: run.AgentVersionID, Version: version, Start: def.Start, Draft: draft, Tools: tools,
		StepNames: make(map[string]string, len(def.Steps)), Prompts: make(map[string]string, len(def.Steps))}
	for _, step := range def.Steps {
		ea.StepNames[step.ID], ea.Prompts[step.ID] = step.Name, step.Config.LLM.SystemPrompt
	}
	next := eval.ScriptCaller(cj.Case.Caller.Script)
	if p := cj.Case.Caller.Persona; p != nil {
		// The persona is the free offline model only (plan S12); with no endpoint the case cannot run at all.
		ask, _, closeAsk, err := s.completer(ctx, "offline", "")
		if err != nil {
			return nil, "persona_unavailable", true, err
		}
		defer closeAsk()
		next = eval.PersonaCaller(*p, ag.Name, ask)
	}

	obs, err := s.evalCall(ctx, ea, next)
	if err != nil {
		return nil, evalHarnessCategory(err), false, err
	}

	judges, closeJudges := s.judgeFor(ctx, suite, settings)
	defer closeJudges()
	thresholds := make(map[string]float64, len(settings))
	for _, set := range settings {
		if set.Threshold != nil {
			thresholds[set.Scorer] = *set.Threshold
		}
	}
	scored := eval.RunScorers(ctx, eval.BuildInput(ea, obs, &cj.Case), judges, thresholds)
	if err := ctx.Err(); err != nil { // cancelled while scoring: the verdicts may be cut short, so none is stored
		return nil, "timeout", false, err
	}
	key := run.AgentVersionID
	if run.Draft {
		key = "draft:" + run.ConfigHash
	}
	results := make([]storage.EvalResult, 0, len(scored))
	for _, r := range scored {
		results = append(results, storage.EvalResult{SubjectKind: "case_run", SubjectID: job.Subject, Scorer: r.Scorer, ScorerVersion: r.Version, VersionKey: key,
			AgentID: run.AgentID, AgentVersionID: run.AgentVersionID, ConfigHash: run.ConfigHash, RunID: run.ID, CaseID: cj.CaseID, CaseHash: cj.CaseHash,
			SessionID: obs.SessionID, Score: r.Score, Outcome: string(r.Outcome), Reason: r.Reason, JudgeModel: r.JudgeModel})
	}
	return results, "", false, nil
}

// evalCall makes the call with the CLI's defaults, counted as active while it runs.
func (s *Server) evalCall(ctx context.Context, a eval.Agent, next eval.NextFunc) (eval.Observed, error) {
	s.metrics.evalCallsActive.Add(1)
	defer s.metrics.evalCallsActive.Add(-1)
	return eval.RunCall(ctx, localTarget{s}, a, next, eval.Opts{TurnTimeout: 45 * time.Second, Quiet: 1500 * time.Millisecond})
}

// errJudgeNeedsLive is every ask of a paid judge on a process without GOVOICE_LIVE=1: no request is made.
var errJudgeNeedsLive = errors.New("a paid judge needs GOVOICE_LIVE=1 on this process")

// judgeFor resolves each model-graded scorer's judge: its own setting, else the suite default, else none (the
// scorer is then skipped). A judge that cannot be built, or is paid without GOVOICE_LIVE=1, answers every ask with
// that error, so the result is an error outcome, never a skip. Each ask has its own deadline. The returned func
// releases the built judges.
func (s *Server) judgeFor(ctx context.Context, suite storage.EvalSuite, settings []storage.EvalScorerSetting) (eval.JudgeFor, func()) {
	built := map[string]*eval.Judge{}
	var closers []func()
	judge := func(scorer string) *eval.Judge {
		provider, model := suite.JudgeProvider, suite.JudgeModel
		for _, set := range settings {
			if set.Scorer == scorer && set.JudgeProvider != "" {
				provider, model = set.JudgeProvider, set.JudgeModel
			}
		}
		if provider == "" {
			return nil
		}
		key := provider + "/" + model
		if j, ok := built[key]; ok {
			return j
		}
		j := &eval.Judge{Model: provider}
		if model != "" {
			j.Model = key
		}
		fail := func(err error) {
			j.Ask = func(context.Context, string, string) (string, error) { return "", err }
		}
		if provider != "offline" && provider != "echo" && s.env("GOVOICE_LIVE") != "1" {
			fail(errJudgeNeedsLive) // before the completer: nothing is built, no key is read
		} else if ask, resolved, closeAsk, err := s.completer(ctx, provider, model); err != nil {
			fail(err)
		} else {
			closers = append(closers, closeAsk)
			j.Model = provider + "/" + resolved // the model the catalog default resolved to
			j.Ask = func(ctx context.Context, system, user string) (string, error) {
				ctx, cancel := context.WithTimeout(ctx, evalJudgeWait)
				defer cancel()
				return ask(ctx, system, user)
			}
		}
		built[key] = j
		return j
	}
	return judge, func() {
		for _, c := range closers {
			c()
		}
	}
}

// evalHarnessCategory names why a call could not be made or read back. Only the category is stored.
func evalHarnessCategory(err error) string {
	msg := err.Error()
	switch {
	case errors.Is(err, errVoiceToken):
		return "token"
	case strings.Contains(msg, "no settled reply"):
		return "turn_timeout"
	case strings.Contains(msg, "did not end within"), strings.Contains(msg, "traces incomplete"), strings.Contains(msg, " traces: "):
		return "stored_rows"
	case strings.Contains(msg, "caller: "):
		return "caller"
	case strings.HasPrefix(msg, "call "):
		return "dial"
	}
	return "call"
}
