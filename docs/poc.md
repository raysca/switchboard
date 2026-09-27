# Go Voice Pipeline POC Spec

Sep 26, 2026

## 1. Purpose & End Game

Pipecat demonstrates a strong programming model for real-time conversational voice AI: typed frames flowing through a directed pipeline of frame processors, with processors able to inspect, consume, transform, and emit frames for VAD, STT, LLM, TTS, interruption handling, aggregation, transport, and control flow.

The goal of this project is **not to replace that programming model**.

The goal is to preserve the useful Pipecat-style `Frame -> FrameProcessor -> Pipeline` architecture while redesigning the runtime around Go's strengths: cheap concurrency, low startup overhead, efficient memory use, explicit supervision, and shared bounded execution resources.

The architectural problem being tested is Pipecat's production deployment model of one OS process per call session. That model provides useful isolation properties, but has significant costs:

- **Slow call startup** - process creation, interpreter startup, imports, model/runtime initialization, and per-process setup add latency before a call is ready.
- **Poor memory scaling** - N calls means N interpreter/process footprints and duplicated runtime state.
- **Inefficient sharing of expensive local resources** - CPU-bound/native components such as VAD are naturally duplicated or isolated per process rather than efficiently scheduled across sessions.
- **Capacity is implicit rather than controlled** - overload is discovered when processes become slow or hosts run out of resources rather than being enforced by explicit per-session and host-level budgets.

### End game of this POC

Determine whether a Go runtime can provide:

1. A Pipecat-like frame processor and pipeline programming model.
2. Very fast creation of isolated call sessions inside one long-lived process.
3. Strong enough session-level containment that one buggy, overloaded, stalled, or panicking call cannot materially affect unrelated calls.
4. Efficient shared execution of Silero VAD across many sessions while maintaining a real-time latency SLO.
5. A measurable and enforceable concurrent-call capacity per host.
6. A clear answer on whether VAD should run:
   - in-process,
   - in a sharded in-process execution plane, or
   - in a small fixed pool of local worker processes.

This is a **hypothesis-testing exercise**, not a framework implementation project.

The deliverable is evidence: measurements, graphs, failure demonstrations, discovered capacity limits, and a written architectural verdict. Code produced for the POC is disposable if the hypotheses fail.

---

## 2. Architectural Principles

### 2.1 Preserve the FrameProcessor programming model

The framework abstraction remains:

```text
Frame -> FrameProcessor -> FrameProcessor -> ... -> Pipeline output
```

Processors receive frames, inspect or modify them, consume them if appropriate, and emit zero, one, or many frames downstream or upstream.

The Go rewrite should intentionally preserve this model because it provides:

- composability,
- loose coupling between components,
- inspectable control flow,
- clean insertion/removal of processors,
- a natural model for audio, text, control, lifecycle, and interruption events.

The rewrite should **not** become a giant session state machine where VAD, STT, LLM, and TTS call each other directly.

### 2.2 Do not mechanically port Python execution semantics

The framework abstraction is the pipeline. Goroutines and channels are implementation details.

Avoid a literal translation such as:

```text
Python coroutine -> Go goroutine
asyncio.Queue    -> Go channel
one component    -> one permanent goroutine
```

unless that concurrency boundary is actually useful.

Cheap/synchronous processors should be able to run directly in the pipeline runtime. Asynchronous boundaries should exist where the workload requires them.

### 2.3 One call remains the fundamental isolation unit

A call does not get its own OS process, but it does get its own:

- session ID,
- pipeline instance,
- cancellation domain,
- supervision domain,
- processor state,
- bounded queues,
- resource budget,
- outstanding-task ownership,
- metrics namespace.

A session is therefore an **isolated logical runtime**, not merely a collection of unrelated goroutines.

### 2.4 Shared resources must be bounded and session-aware

Expensive shared resources such as VAD execution must:

- know which session submitted work,
- enforce bounded queueing,
- enforce per-session fairness,
- support cancellation,
- reject or shed stale work under overload,
- expose queue and execution latency,
- prevent one session from monopolising the server.

### 2.5 Process-level fault isolation is not fully reproducible in-process

The POC must distinguish:

**Session-level application failures that should be containable:**

- panic in managed Go code,
- processor error,
- runaway frame generation,
- full queues,
- slow consumers,
- hung provider calls,
- excess VAD submissions,
- failure to shut down,
- leaked managed goroutines/tasks.

**Process-wide failures that cannot be isolated purely with Go supervision:**

- native/cgo segfault,
- fatal Go runtime failure,
- process OOM,
- native memory corruption,
- unrecoverable runtime faults.

This is why local process-isolated VAD workers remain a candidate architecture rather than merely a fallback.

---

## 3. Proposed Runtime Architecture

```text
                           HOST
┌──────────────────────────────────────────────────────────────┐
│                                                              │
│                      Go Voice Runtime                        │
│                                                              │
│  ┌────────────────────────────────────────────────────────┐  │
│  │ Admission / Host Resource Manager                     │  │
│  │ - max sessions                                        │  │
│  │ - CPU / memory guardrails                             │  │
│  │ - VAD queue / latency guardrails                      │  │
│  └────────────────────────────────────────────────────────┘  │
│                                                              │
│  Session A              Session B              Session C      │
│  ┌──────────────┐       ┌──────────────┐       ┌──────────┐  │
│  │ Supervisor   │       │ Supervisor   │       │ ...      │  │
│  │ Budget       │       │ Budget       │       │          │  │
│  │ Pipeline     │       │ Pipeline     │       │          │  │
│  │  FP -> FP    │       │  FP -> FP    │       │          │  │
│  └──────┬───────┘       └──────┬───────┘       └──────────┘  │
│         │                      │                              │
│         └──────────────┬───────┘                              │
│                        ▼                                      │
│                 Shared VAD Runtime                            │
│          fairness / deadlines / bounded work                  │
│             global pool or sharded workers                    │
│                                                              │
└──────────────────────────────────────────────────────────────┘
```

### 3.1 Session runtime

A session owns one logical call pipeline.

Conceptually:

```go
type Session struct {
    ID       SessionID
    Pipeline *Pipeline

    ctx    context.Context
    cancel context.CancelFunc

    Supervisor *Supervisor
    Budget     *ResourceBudget
}
```

A session must be independently startable, cancellable, observable, and destroyable.

The session should not need a dedicated OS thread or process.

### 3.2 Resource budget

Each session receives explicit limits.

Candidate limits:

```go
type ResourceBudget struct {
    MaxQueuedFrames  int
    MaxAsyncTasks    int
    MaxPendingVAD    int
    MaxBufferedAudio time.Duration
}
```

The exact API is not important for the POC. The property is.

A session that exceeds its resource envelope should fail or shed work locally rather than placing unbounded pressure on the host.

### 3.3 Managed concurrency

Processors must not be able to create invisible session-owned work that escapes supervision.

Instead of arbitrary:

```go
go doWork()
```

session-owned asynchronous work should conceptually use:

```go
session.Go(func(ctx context.Context) error {
    return doWork(ctx)
})
```

The session supervisor must:

- recover ordinary panics inside managed goroutines,
- associate errors with the owning session,
- cancel sibling session work where appropriate,
- wait for managed work during teardown,
- expose leaked/non-terminating work,
- ensure one session failure does not tear down the host.

### 3.4 Bounded queues

Every asynchronous queue must be bounded.

When a queue fills, the system needs an explicit policy:

- block,
- drop,
- coalesce,
- reject,
- fail the session.

The policy may depend on frame type.

For example, stale audio frames can potentially be dropped or coalesced while lifecycle/control frames must not silently disappear.

---

## 4. Frame / Processor / Pipeline Model

### 4.1 Minimal Frame model

The POC only needs the frame types required to test the architecture.

Example:

```go
type Frame interface {
    Type() FrameType
}
```

Initial useful types may include:

- `AudioFrame`
- `SpeechStartedFrame`
- `SpeechStoppedFrame`
- `TextFrame`
- `ControlFrame`
- `ErrorFrame`

A full Pipecat-compatible frame taxonomy is out of scope.

### 4.2 Processor model

The POC should test a **frame-oriented processor API** rather than assuming every processor is a permanent goroutine joined by channels.

Conceptually:

```go
type Processor interface {
    ProcessFrame(
        ctx context.Context,
        frame Frame,
        direction Direction,
        emit Emitter,
    ) error
}
```

with:

```go
type Emitter interface {
    Push(frame Frame) error
    PushDirection(frame Frame, direction Direction) error
}
```

This preserves the Pipecat-style `process_frame()` / `push_frame()` semantics while allowing the Go runtime to choose efficient execution strategies.

### 4.3 Pipeline runtime

The pipeline owns processor ordering and frame routing.

```text
AudioFrame
    ↓
VADProcessor
    ↓
FakeSTT
    ↓
FakeLLM
    ↓
FakeTTS
```

The runtime should not require a goroutine or channel for every processor.

Async boundaries can be introduced where required.

### 4.4 Direction

The POC does not need full interruption semantics, but the API should leave room for bidirectional frames from the start.

```go
type Direction uint8

const (
    Downstream Direction = iota
    Upstream
)
```

This avoids designing an API that later makes interruption/control propagation awkward.

---

## 5. VAD Architecture

VAD is intentionally treated differently from ordinary processors because it is:

- high-frequency,
- CPU-bound,
- latency-sensitive,
- stateful,
- local/native,
- potentially implemented through cgo/ONNX Runtime.

From the framework user's perspective, VAD remains an ordinary `FrameProcessor`.

Internally it delegates inference to a shared execution subsystem.

```text
Session Pipeline
      │
  AudioFrame
      │
      ▼
SileroVADProcessor
      │
      │ submit(sessionID, state, audio)
      ▼
┌─────────────────────────────┐
│ Shared VAD Runtime          │
│                             │
│ bounded queueing            │
│ fairness                    │
│ deadlines                   │
│ worker scheduling           │
│ ONNX Runtime / Silero       │
└──────────────┬──────────────┘
               │
               ▼
          VAD result
               │
               ▼
SileroVADProcessor
               │
        SpeechStartedFrame
```

### 5.1 Per-session VAD state

Streaming VAD state belongs logically to the call.

The model/runtime should be shareable where possible, while recurrent/context state remains associated with the originating session.

Conceptually:

```go
type VADStreamState struct {
    RecurrentState []float32
    Context        []float32
    DetectorState  DetectorState
}
```

### 5.2 VAD execution strategies to compare

The POC should compare at least:

#### Strategy A - global bounded worker pool

```text
all sessions -> bounded queue -> N VAD workers
```

Simple baseline.

#### Strategy B - sharded/fair in-process VAD runtime

```text
sessionID -> shard(sessionID) -> bounded shard queue -> worker
```

Potential benefits:

- less contention,
- better fairness,
- better locality,
- more predictable queue behaviour,
- smaller failure domains for worker stalls.

#### Strategy C - local process-isolated VAD workers

```text
Go runtime
    │ local IPC
    ▼
small fixed VAD worker-process pool
```

This retains cheap Go call sessions while restoring process isolation around native/CPU VAD execution.

This strategy is optional if time is tight, but the architecture should leave room for it.

### 5.3 Fairness

One call must not be able to monopolise VAD capacity.

The scheduler should enforce a small per-session outstanding-work limit, potentially:

```text
max one outstanding VAD inference per session
```

or another small configurable number.

Global FIFO alone is not sufficient if one session can flood it.

### 5.4 Deadlines and stale work

VAD work is real-time work.

Each job should carry timing metadata such as:

```go
type VADJob struct {
    SessionID SessionID
    Sequence  uint64
    Deadline  time.Time
    Audio     []float32
    State     VADStreamState
}
```

If the executor becomes overloaded, stale audio should not accumulate indefinitely.

The runtime should measure and optionally test:

- dropping,
- coalescing,
- rejecting,
- terminating the offending session.

### 5.5 ONNX Runtime scheduling

The POC should avoid accidentally benchmarking two competing schedulers.

Start with tightly controlled ONNX Runtime threading and let the Go VAD runtime own concurrency.

Test alternative ONNX settings only as an explicit benchmark dimension.

---

## 6. Core Hypotheses

Each hypothesis must produce a clear pass/fail or capacity result backed by measurements.

### H1 - Call startup latency

A new warm Go call session can be created and made ready for its first audio frame substantially faster than a Python process-per-call baseline.

Startup latency must remain nearly flat as the host already contains increasing numbers of concurrent sessions.

Measure:

- cold server -> first session ready,
- warm session startup,
- startup at 1 / 10 / 100 / 500 / 1000+ existing sessions,
- burst creation of many sessions simultaneously.

The important property is not only "Go is faster", but:

> Session creation remains cheap as the process fills.

### H2 - Memory/resource scaling

The Go runtime has a substantially lower marginal resource cost per session than process-per-call Python.

Measure:

- process RSS,
- heap allocation,
- goroutine count,
- OS thread count,
- file descriptors where relevant,
- marginal memory/session,
- resource return after teardown.

Long-running churn must not cause unbounded growth.

### H3 - Session containment

An unhealthy session cannot materially affect healthy sessions.

The POC should test more than panic recovery.

Failure variants should include:

- panic in main processor execution,
- panic inside managed spawned work,
- frame flood,
- queue overflow,
- slow/stalled processor,
- non-returning remote-style fake processor,
- excess VAD submission,
- cancellation during outstanding work,
- intentionally misbehaving teardown.

For every variant verify:

1. target session is failed or cancelled,
2. its managed resources are reclaimed,
3. unaffected sessions continue,
4. unaffected session latency/error rate does not materially change,
5. no unbounded host queue or memory growth occurs.

### H4 - VAD real-time viability under concurrency

VAD is the primary CPU scheduling risk.

Measure VAD latency across increasing active-stream counts.

Latency must be decomposed into:

```text
audio due/arrival
   ↓
VAD queue wait
   ↓
inference execution
   ↓
VAD state/detection processing
   ↓
speech-start emitted
```

Record:

- queue wait p50/p95/p99,
- inference p50/p95/p99,
- total detection p50/p95/p99,
- queue depth,
- dropped/stale jobs,
- CPU utilization,
- per-session p99,
- fairness across sessions.

The POC must deliberately continue beyond acceptable performance to discover the latency knee / saturation point.

### H5 - Host concurrency ceiling

The runtime must produce a defensible capacity number for the tested hardware.

Capacity is not a single "maximum sessions" value.

Report:

1. **connected-session ceiling**,
2. **active-VAD-stream ceiling**,
3. **real-time SLO ceiling**,
4. **recommended production capacity with headroom**.

The real-time ceiling is the important number.

### H6 - Admission control can prevent overload collapse

Once the VAD/host SLO is understood, the runtime should be able to reject additional sessions before the server enters uncontrolled latency collapse.

Admission criteria may include:

- active session count,
- CPU saturation,
- VAD queue latency,
- VAD p99,
- memory pressure.

The POC does not need sophisticated autoscaling, but it should demonstrate that capacity can be made explicit rather than accidental.

---

## 7. In Scope

Build only the minimum architecture required to test H1-H6.

### Core runtime

- Minimal `Frame` model.
- Frame-oriented `Processor` interface.
- `Pipeline` with processor ordering and frame routing.
- Upstream/downstream direction support in the API.
- No requirement for one goroutine/channel per processor.

### Session runtime

- Start/stop/list sessions.
- One context/cancellation domain per session.
- Session supervisor for managed asynchronous work.
- Session resource budget.
- Bounded session queues.
- Metrics tagged by session where useful.

### Fake processors

- Fake STT.
- Fake LLM.
- Fake TTS.

Each should support configurable behaviour such as:

- normal latency,
- slow response,
- indefinite stall until cancellation,
- error,
- panic,
- frame flood.

These exist to exercise runtime containment, not provider functionality.

### Real Silero VAD

- ONNX Runtime integration.
- Streaming state per session.
- Shared execution runtime.
- Configurable execution strategy.
- Configurable worker/shard count.
- Controlled ONNX threading.

### Synthetic/recorded audio load generator

Per session:

- continuous real-time audio clock,
- silence and speech segments,
- known speech-onset timestamps,
- configurable conversational duty cycle,
- optional worst-case continuous activity.

Use speech samples or realistic recorded segments inserted at known timestamps rather than relying only on synthetic tones/noise.

### HTTP control surface

Example:

```text
POST   /sessions
DELETE /sessions/:id
GET    /sessions
POST   /sessions/:id/fault
GET    /metrics
```

Fault injection should support multiple failure modes, not only panic.

### Python baseline

Use separate baseline modes where appropriate.

#### Startup/memory baseline

A process-per-call Python baseline with production-relevant imports/runtime setup.

#### VAD baseline

Equivalent Silero/ONNX VAD processing with matching model, audio, and controlled runtime settings.

Do not treat an arbitrary minimal Python process as interchangeable with the production startup baseline.

---

## 8. Out of Scope

Deliberately defer:

- real STT provider integration,
- real LLM provider integration,
- real TTS provider integration,
- full Pipecat frame taxonomy,
- full interruption/barge-in flush behaviour,
- WebRTC transport,
- production WebSocket transport,
- config-driven pipeline definitions,
- processor restart policies/backoff trees,
- full observability stack,
- persistence,
- auth,
- multi-tenant business isolation,
- distributed VAD service,
- autoscaling/orchestration,
- framework API polish,
- frame pooling and premature micro-optimisation.

The POC should keep future interruption semantics possible but does not need to implement them.

---

## 9. Metrics

### 9.1 Session startup

Define at least:

```text
request received
    ->
session allocated
    ->
pipeline created
    ->
registered with shared services
    ->
ready for first audio frame
```

Record p50/p95/p99.

### 9.2 VAD latency

Record separate timestamps for:

```text
audio frame becomes due / arrives
VAD job submitted
worker begins inference
worker completes inference
speech event emitted
```

This lets the benchmark distinguish:

- scheduler/queue saturation,
- ONNX slowdown,
- algorithmic VAD delay.

### 9.3 Runtime resources

Continuously sample:

- RSS,
- Go heap size,
- heap object count,
- GC pause time,
- GC CPU fraction where practical,
- goroutine count,
- OS thread count,
- CPU utilization,
- cgo/native metrics where available,
- VAD queue depth,
- active sessions,
- active VAD streams.

### 9.4 Session containment

For healthy sessions record before/after fault injection:

- frame latency,
- VAD latency,
- error rate,
- dropped frames,
- queue depth.

---

## 10. Test Plan

### T1 - Warm call-start latency sweep

Start sessions one at a time while holding existing sessions open.

Example plateaus:

```text
1
10
50
100
200
500
1000
beyond if cheap
```

Record session creation time and first-frame-ready time.

Repeat for Python process-per-call baseline.

### T2 - Burst startup

Create bursts such as:

```text
10 simultaneous calls
50 simultaneous calls
100 simultaneous calls
250 simultaneous calls
```

Measure p50/p95/p99 call-ready latency and impact on already-running calls.

This tests realistic traffic spikes rather than only serial creation.

### T3 - Memory/resource scaling

Ramp sessions through increasing plateaus.

Measure:

- total RSS,
- marginal RSS/session,
- heap,
- goroutines,
- threads,
- file descriptors.

Compare with Python process-per-call baseline.

### T4 - Lifecycle churn / soak

Hold a target concurrency, for example 200 sessions, while continuously replacing sessions.

Run until either:

- a meaningful lifecycle count is reached, e.g. tens of thousands of session create/destroy cycles,
- or the selected soak duration completes.

Track:

- RSS,
- heap,
- goroutines,
- threads,
- VAD state/resource counts,
- cleanup latency.

Pass requires no unbounded upward trend.

### T5 - Session panic containment

With many healthy sessions running, inject:

- processor panic,
- panic in managed child goroutine.

Verify:

- failed session terminates,
- healthy sessions continue,
- no host crash,
- managed work returns to baseline.

### T6 - Session resource-abuse containment

Inject sessions that deliberately:

- flood frames,
- stop consuming,
- fill queues,
- submit excessive VAD work,
- hold async tasks open,
- ignore normal progress while remaining cancellable.

Verify the resource budget contains them.

This is a critical test: the architecture is not isolated merely because it can recover panics.

### T7 - VAD concurrency sweep

Ramp **active VAD streams** through:

```text
10
50
100
200
500
...
```

Continue beyond the expected production limit until clear degradation occurs.

For every plateau record:

- queue wait p50/p95/p99,
- inference p50/p95/p99,
- total VAD latency p50/p95/p99,
- per-session p99 distribution,
- CPU,
- queue depth,
- drops/stale work.

Repeat for multiple execution configurations.

### T8 - VAD scheduler comparison

Compare:

```text
A. global bounded worker pool
B. sharded/fair in-process runtime
C. local worker-process runtime, if implemented
```

For A/B also sweep worker/shard count.

The goal is not merely maximum throughput.

The goal is the highest number of active streams while preserving:

- latency SLO,
- fairness,
- bounded queues,
- session isolation.

### T9 - Connected sessions vs speech duty cycle

Capacity must be tested across realistic activity levels.

Example scenarios:

```text
1000 connected sessions @ 10% speech duty
1000 connected sessions @ 25%
500 connected sessions  @ 50%
300 connected sessions  @ 100% continuous VAD load
```

Exact values may be tuned.

This separates:

- cost of maintaining a call,
- cost of actively running VAD.

### T10 - Combined pipeline load

Run VAD concurrency tests while fake STT/LLM/TTS processors also perform realistic asynchronous work.

Confirm that shared runtime pressure does not cause unacceptable VAD tail latency.

### T11 - Admission control

Ramp until the measured VAD/host SLO approaches its limit.

Enable admission control.

Verify:

- additional sessions are rejected/redirected before latency collapse,
- existing sessions remain inside SLO,
- host resource usage remains bounded.

---

## 11. Evidence & Success Criteria

All pass bars should be agreed before the final benchmark run.

| Hypothesis | Evidence | Example pass criterion |
| --- | --- | --- |
| H1 Startup | Session-ready latency vs existing session count, Go vs Python | Warm Go startup remains within agreed low-ms bound and shows no material degradation as session count grows |
| H2 Resource scaling | RSS/heap/goroutine graphs and lifecycle-churn graphs | Marginal per-session cost is substantially smaller than process-per-call Python and no unbounded resource drift occurs |
| H3 Containment | Before/after healthy-session metrics for each injected failure | Target session is contained while unrelated sessions show no material latency/error impact |
| H4 VAD | Queue/inference/end-to-end p50/p95/p99 vs active streams | p99 stays below agreed real-time SLO up to a clearly reported active-stream ceiling |
| H5 Capacity | Connected-session and active-VAD-stream capacity curves | A reproducible real-time capacity ceiling and recommended production capacity are identified |
| H6 Admission | SLO metrics with/without admission control near saturation | Host rejects new work before entering sustained overload and protects existing sessions |

Do not require memory growth to be sub-linear. Linear growth is acceptable if the marginal session cost is small and bounded.

---

## 12. Capacity Definitions

The final report should distinguish:

### Technical maximum

Highest load at which the process still functions.

This number is mostly diagnostic.

### Real-time maximum

Highest load at which the agreed VAD/session SLO is still met.

This is the primary capacity result.

### Recommended production capacity

A lower number that preserves operational headroom.

For example:

```text
real-time ceiling:          N active VAD streams
recommended production:    ~70-80% of N
```

The exact headroom policy is outside the POC; the measured ceiling is not.

---

## 13. Candidate Outcomes

The POC should explicitly allow more than a binary result.

### Outcome A - fully in-process

```text
Go sessions
+
in-process shared/sharded VAD runtime
```

Selected if latency, fairness, isolation, and native-runtime risk are acceptable.

### Outcome B - hybrid

```text
Go sessions
+
small local VAD worker-process pool
```

Selected if session scaling works well but native/CPU VAD benefits materially from stronger process isolation.

This should be considered a successful architecture, not a failed Go rewrite.

### Outcome C - architectural revision required

If session isolation or VAD scheduling cannot meet the required SLO even after reasonable runtime changes, stop before building the full framework.

---

## 14. Deliverables

1. **Go POC source**
   - frame model,
   - processor model,
   - pipeline runtime,
   - session runtime,
   - supervisor,
   - resource budgets,
   - admission-control prototype,
   - VAD execution runtime,
   - HTTP control surface.

2. **Python baseline**
   - process-per-call startup/memory baseline,
   - comparable VAD baseline.

3. **Audio load generator**
   - known onset timestamps,
   - configurable duty cycle,
   - realistic speech samples,
   - arbitrary connected and active-stream counts.

4. **Fault-injection harness**
   - panic,
   - child panic,
   - frame flood,
   - stall,
   - queue overflow,
   - VAD abuse,
   - cancellation/teardown edge cases.

5. **Raw benchmark data**
   - session startup,
   - RSS/heap,
   - goroutine/thread counts,
   - VAD queue/inference/total latency,
   - error/drop counters,
   - fairness metrics.

6. **Graphs**
   - startup latency vs existing session count,
   - burst startup latency,
   - memory vs sessions,
   - resource use under churn,
   - VAD p50/p95/p99 vs active streams,
   - VAD queue wait vs active streams,
   - per-session VAD fairness,
   - capacity vs speech duty cycle,
   - scheduler-strategy comparison.

7. **Written verdict**
   - pass/fail per hypothesis,
   - best VAD execution strategy,
   - connected-session ceiling,
   - active-VAD real-time ceiling,
   - recommended production capacity,
   - isolation limitations,
   - recommendation for Outcome A, B, or C.

---

## 15. Suggested Milestones

### M0 - VAD micro-runtime

Before building much framework code:

- integrate Silero + ONNX Runtime,
- prove streaming state handling,
- benchmark inference,
- build minimal bounded worker execution,
- test many virtual streams,
- identify obvious scheduler/ORT issues.

This is the highest-uncertainty component and should be de-risked first.

### M1 - Frame / Processor / Pipeline skeleton

Build:

- minimal frame types,
- frame-oriented processor interface,
- emitter/router,
- direction support,
- fake processors.

No VAD integration required yet.

### M2 - Session runtime

Build:

- session start/stop,
- supervisor,
- managed async work,
- bounded queues,
- resource budget,
- lifecycle metrics.

Measure first warm startup numbers immediately.

### M3 - VAD FrameProcessor integration

Make Silero appear to the pipeline as a normal processor while delegating work to the shared VAD runtime.

### M4 - Isolation/fault tests

Implement and run:

- panic containment,
- child-goroutine panic,
- frame flood,
- queue overflow,
- stalled processor,
- teardown/cancellation edge cases.

### M5 - Startup and resource benchmarks

Run:

- serial startup sweep,
- burst startup,
- memory scaling,
- initial Python baseline.

### M6 - VAD capacity benchmarks

Run:

- active-stream sweep,
- worker/shard configurations,
- fairness metrics,
- connected-session vs duty-cycle matrix.

### M7 - Long-running churn and combined load

Run:

- lifecycle churn,
- fake STT/LLM/TTS combined load,
- resource-return verification.

### M8 - Admission-control experiment

Use the measured capacity knee to protect healthy sessions from overload.

### M9 - Optional VAD process-isolation comparison

If in-process native isolation or scheduling remains questionable, implement a minimal local VAD worker-process transport and repeat the critical VAD tests.

### M10 - Verdict

Produce comparative graphs and a written architectural recommendation.

---

## 16. Final Decision Questions

The POC should end by answering these questions explicitly:

1. **Can the Pipecat-style FrameProcessor model be preserved without reproducing Python's per-component/per-process execution costs?**
2. **How fast can a warm Go call session become ready?**
3. **Does call startup remain fast while hundreds or thousands of other sessions already exist?**
4. **What is the marginal memory/resource cost per call?**
5. **Can one unhealthy session be reliably contained without materially affecting healthy sessions?**
6. **How many connected sessions can one host maintain?**
7. **How many simultaneous active VAD streams can one host process while meeting the real-time p99 SLO?**
8. **At what point and for what reason does VAD latency degrade?**
9. **Does a global, sharded, or process-isolated VAD execution model provide the best result?**
10. **Can admission control prevent one host from entering overload collapse?**
11. **What concurrency level should production actually advertise per host?**
12. **Is the shared-process Go architecture sound enough to justify building the full framework?**

The POC succeeds if it produces trustworthy answers to those questions, including a negative answer if the architecture does not hold up.
