package vad

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestArtifactVerification(t *testing.T) {
	// Verify valid model
	modelPath := ModelPath()
	h, err := VerifyModelArtifact(modelPath)
	if err != nil {
		t.Fatalf("verify valid model artifact failed: %v", err)
	}
	if len(h) != 32 {
		t.Fatalf("expected 32-byte sha256, got %d", len(h))
	}

	// Verify missing model
	_, err = VerifyModelArtifact(filepath.Join(Root(), "third_party", "non_existent.onnx"))
	if err == nil {
		t.Fatal("expected error for non-existent model artifact")
	}

	// Verify corrupted model
	tmpDir := t.TempDir()
	fakeModel := filepath.Join(tmpDir, "corrupt.onnx")
	if err := os.WriteFile(fakeModel, []byte("corrupted model bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err = VerifyModelArtifact(fakeModel)
	if err == nil {
		t.Fatal("expected error for corrupted model artifact")
	}
}

func TestIPC_FramingAndValidation(t *testing.T) {
	// Test normal frame roundtrip
	var buf bytes.Buffer
	testPayload := []byte("hello vad1 ipc protocol")
	if err := WriteFrame(&buf, testPayload); err != nil {
		t.Fatalf("write frame: %v", err)
	}

	readBack, err := ReadFrame(&buf, nil)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if !bytes.Equal(readBack, testPayload) {
		t.Fatalf("expected %q, got %q", string(testPayload), string(readBack))
	}

	// Test oversized frame rejected before allocation
	var oversized bytes.Buffer
	var lenBuf [4]byte
	// Length = 128KB > 64KB max
	lenBuf[0] = 0x00
	lenBuf[1] = 0x00
	lenBuf[2] = 0x02
	lenBuf[3] = 0x00
	oversized.Write(lenBuf[:])

	_, err = ReadFrame(&oversized, nil)
	if err == nil {
		t.Fatal("expected error reading oversized frame")
	}

	// Test zero-length frame rejected
	var zeroBuf bytes.Buffer
	lenBuf = [4]byte{0, 0, 0, 0}
	zeroBuf.Write(lenBuf[:])
	_, err = ReadFrame(&zeroBuf, nil)
	if err == nil {
		t.Fatal("expected error reading zero-length frame")
	}
}

func TestIPC_HandshakeValidation(t *testing.T) {
	// Encode valid handshake req
	var sha [32]byte
	copy(sha[:], []byte("12345678901234567890123456789012"))
	req := HandshakeReq{
		ExpectedModelSHA256: sha,
		MaxBatch:            4,
	}
	reqData := EncodeHandshakeReq(req)
	decodedReq, err := DecodeHandshakeReq(reqData)
	if err != nil {
		t.Fatalf("decode handshake req: %v", err)
	}
	if decodedReq.MaxBatch != 4 || decodedReq.ExpectedModelSHA256 != sha {
		t.Fatalf("decoded handshake mismatch: %+v", decodedReq)
	}

	// Invalid magic
	badMagic := make([]byte, len(reqData))
	copy(badMagic, reqData)
	badMagic[0] = 0xFF
	_, err = DecodeHandshakeReq(badMagic)
	if err != ErrInvalidMagic {
		t.Fatalf("expected ErrInvalidMagic, got %v", err)
	}

	// Invalid version
	badVer := make([]byte, len(reqData))
	copy(badVer, reqData)
	badVer[4] = 0xFF
	_, err = DecodeHandshakeReq(badVer)
	if err != ErrInvalidVersion {
		t.Fatalf("expected ErrInvalidVersion, got %v", err)
	}
}

func TestWorkerProcess_HardKillAndRespawn(t *testing.T) {
	cfg := Config{
		Mode:            "process",
		Workers:         2,
		Batch:           4,
		QueueCap:        256,
		IntraOp:         1,
		PerSessionLimit: 1,
		ParentDeadline:  200 * time.Millisecond,
		ParentGrace:     200 * time.Millisecond,
	}

	exec, err := New(cfg)
	if err != nil {
		t.Fatalf("new worker process executor: %v", err)
	}
	defer exec.Close()

	if !exec.IsReady() {
		t.Fatal("executor should be ready")
	}
	avail, total := exec.Capacity()
	if avail != 2 || total != 2 {
		t.Fatalf("expected capacity (2,2), got (%d,%d)", avail, total)
	}

	// Submit jobs to Worker 0
	st := exec.Register(0) // session 0 -> worker 0
	defer st.Close()

	var wg sync.WaitGroup
	var completedJobs int
	var mu sync.Mutex

	for i := 0; i < 5; i++ {
		wg.Add(1)
		j := &Job{
			Seq: uint64(i + 1),
			Done: func(job *Job) {
				defer wg.Done()
				if job.Err == nil {
					mu.Lock()
					completedJobs++
					mu.Unlock()
				}
			},
		}
		if err := st.Submit(j); err != nil {
			t.Fatalf("submit job %d: %v", i, err)
		}
		wg.Wait()
	}

	if completedJobs != 5 {
		t.Fatalf("expected 5 completed jobs, got %d", completedJobs)
	}

	// Hard kill Worker 0
	if err := exec.HardKill(0); err != nil {
		t.Fatalf("hard kill worker 0: %v", err)
	}

	// Give a moment for kill and respawn
	time.Sleep(100 * time.Millisecond)

	// Submit subsequent job on Worker 0, should succeed after respawn
	wg.Add(1)
	var respawnJobDone bool
	jRespawn := &Job{
		Seq: 100,
		Done: func(job *Job) {
			defer wg.Done()
			if job.Err == nil {
				respawnJobDone = true
			}
		},
	}
	if err := st.Submit(jRespawn); err != nil {
		t.Fatalf("submit post-kill job: %v", err)
	}
	wg.Wait()

	if !respawnJobDone {
		t.Fatal("post-respawn job failed or did not complete")
	}

	stats := exec.Stats()
	if stats.Restarts < 1 {
		t.Fatalf("expected restarts >= 1, got %d", stats.Restarts)
	}
}

func TestWorkerProcess_SessionIsolation(t *testing.T) {
	cfg := Config{
		Mode:            "process",
		Workers:         2,
		Batch:           4,
		QueueCap:        256,
		IntraOp:         1,
		PerSessionLimit: 1,
		ParentDeadline:  200 * time.Millisecond,
		ParentGrace:     200 * time.Millisecond,
	}

	exec, err := New(cfg)
	if err != nil {
		t.Fatalf("new worker process executor: %v", err)
	}
	defer exec.Close()

	// Session 0 mapped to Worker 0 (0 % 2 == 0)
	// Session 1 mapped to Worker 1 (1 % 2 == 1)
	st0 := exec.Register(0)
	st1 := exec.Register(1)
	defer st0.Close()
	defer st1.Close()

	var s1SuccessCount int
	var s1ErrCount int
	var mu sync.Mutex

	// Worker 1 runs continuous jobs
	stopS1 := make(chan struct{})
	go func() {
		seq := uint64(1)
		for {
			select {
			case <-stopS1:
				return
			default:
				var jobWg sync.WaitGroup
				jobWg.Add(1)
				j := &Job{
					Seq: seq,
					Done: func(job *Job) {
						defer jobWg.Done()
						mu.Lock()
						if job.Err == nil {
							s1SuccessCount++
						} else {
							s1ErrCount++
						}
						mu.Unlock()
					},
				}
				_ = st1.Submit(j)
				jobWg.Wait()
				seq++
				time.Sleep(10 * time.Millisecond)
			}
		}
	}()

	// While Session 1 is running on Worker 1, kill Worker 0 repeatedly
	for k := 0; k < 2; k++ {
		time.Sleep(50 * time.Millisecond)
		_ = exec.HardKill(0)
		time.Sleep(100 * time.Millisecond)
	}

	close(stopS1)

	mu.Lock()
	s1Success := s1SuccessCount
	s1Errs := s1ErrCount
	mu.Unlock()

	if s1Errs > 0 {
		t.Fatalf("session 1 on worker 1 experienced %d errors while worker 0 was killed", s1Errs)
	}
	if s1Success < 5 {
		t.Fatalf("session 1 on worker 1 should have succeeded >= 5 times, got %d", s1Success)
	}
}

func TestWorkerProcess_ParentMonotonicDeadline_HungWorker(t *testing.T) {
	// Configure short parent deadline
	cfg := Config{
		Mode:            "process",
		Workers:         1,
		Batch:           2,
		QueueCap:        64,
		IntraOp:         1,
		PerSessionLimit: 1,
		ParentDeadline:  30 * time.Millisecond,
		ParentGrace:     20 * time.Millisecond,
	}

	exec, err := New(cfg)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	defer exec.Close()

	st := exec.Register(0)
	defer st.Close()

	// Normal job first
	var wg sync.WaitGroup
	wg.Add(1)
	var firstOk bool
	j1 := &Job{
		Seq: 1,
		Done: func(job *Job) {
			defer wg.Done()
			if job.Err == nil {
				firstOk = true
			}
		},
	}
	if err := st.Submit(j1); err != nil {
		t.Fatalf("submit j1: %v", err)
	}
	wg.Wait()
	if !firstOk {
		t.Fatal("first job failed")
	}

	// Verify stats show 0 hangs
	stats := exec.Stats()
	if stats.WorkerHangs != 0 {
		t.Fatalf("expected 0 worker hangs, got %d", stats.WorkerHangs)
	}
}

func TestDetectorStateCheckpointRecovery(t *testing.T) {
	cfg := DefaultProcessorConfig()
	proc := NewProcessor(nil, cfg)

	// Set initial known state
	var initial [StateLen]float32
	for i := 0; i < StateLen; i++ {
		initial[i] = float32(i + 1)
	}
	proc.state = initial
	proc.checkpointState = initial

	// Simulate failed inference job
	failedJob := &Job{
		Err: ErrWorker,
	}
	for i := 0; i < StateLen; i++ {
		failedJob.State[i] = 999.0
	}

	// Re-entry with error
	_ = proc.onTraceResult(&vadResultFrame{j: failedJob}, nil)

	// Verify state restored from checkpoint, not corrupted by failedJob
	for i := 0; i < StateLen; i++ {
		if proc.state[i] != initial[i] {
			t.Fatalf("state[%d] = %v, expected checkpoint value %v", i, proc.state[i], initial[i])
		}
	}

	// Simulate successful inference job
	var nextState [StateLen]float32
	for i := 0; i < StateLen; i++ {
		nextState[i] = float32((i + 1) * 2)
	}
	succJob := &Job{
		State: nextState,
		Prob:  0.8,
	}
	_ = proc.onTraceResult(&vadResultFrame{j: succJob}, nil)

	// Verify checkpoint updated
	for i := 0; i < StateLen; i++ {
		if proc.state[i] != nextState[i] || proc.checkpointState[i] != nextState[i] {
			t.Fatalf("state not updated on success: state=%v, checkpoint=%v", proc.state[i], proc.checkpointState[i])
		}
	}
}

// crashableWorker is a vadworker launcher that exits at once while flag exists, so a killed worker's
// restarts keep failing (a crash loop) until the test removes the flag.
func crashableWorker(t *testing.T, flag string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "vadworker.sh")
	script := "#!/bin/sh\n[ -e '" + flag + "' ] && exit 1\nexec '" + WorkerBinary() + "' \"$@\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// A worker that crash-loops must not leave the sessions pinned to it VAD-blind: from the first chunk after the
// crash they are served by the live worker, while the dead one keeps retrying off its run loop and comes back
// once it can start again.
func TestWorkerProcess_CrashLoopingWorkerHandsItsSessionsToALiveOne(t *testing.T) {
	flag := filepath.Join(t.TempDir(), "crash")
	exec, err := New(Config{Mode: "process", Workers: 2, Batch: 4, QueueCap: 256, IntraOp: 1, PerSessionLimit: 1,
		ParentDeadline: 200 * time.Millisecond, ParentGrace: 200 * time.Millisecond, WorkerBinary: crashableWorker(t, flag)})
	if err != nil {
		t.Fatalf("new worker process executor: %v", err)
	}
	defer exec.Close()
	st := exec.Register(0) // session 0 -> worker 0
	defer st.Close()
	run := func(seq uint64) error {
		done := make(chan error, 1)
		if err := st.Submit(&Job{Seq: seq, Done: func(j *Job) { done <- j.Err }}); err != nil {
			return err
		}
		select {
		case err := <-done:
			return err
		case <-time.After(2 * time.Second):
			return errors.New("the chunk never finished")
		}
	}
	waitUntil := func(what string, cond func() bool) {
		t.Helper()
		for end := time.Now().Add(5 * time.Second); !cond(); time.Sleep(5 * time.Millisecond) {
			if time.Now().After(end) {
				t.Fatalf("timed out waiting for %s", what)
			}
		}
	}
	if err := run(1); err != nil {
		t.Fatalf("warm-up: %v", err)
	}
	if err := os.WriteFile(flag, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := exec.HardKill(0); err != nil {
		t.Fatal(err)
	}
	for seq := uint64(2); seq <= 40; seq++ {
		if err := run(seq); err != nil {
			t.Fatalf("chunk %d after worker 0 crashed: %v", seq, err)
		}
	}
	waitUntil("worker 0 to crash-loop", func() bool { return exec.Stats().Restarts >= 3 })
	if avail, total := exec.Capacity(); avail != 1 || total != 2 {
		t.Fatalf("capacity (%d,%d) while worker 0 crash-loops, want (1,2)", avail, total)
	}
	if err := run(100); err != nil {
		t.Fatalf("a chunk during the crash loop: %v", err)
	}
	if err := os.Remove(flag); err != nil {
		t.Fatal(err)
	}
	waitUntil("worker 0 to come back", func() bool { a, _ := exec.Capacity(); return a == 2 })
	if err := run(101); err != nil {
		t.Fatalf("a chunk after recovery: %v", err)
	}
}

// poisonSeq is the chunk the fake worker below dies on.
const poisonSeq = 666

// TestFakePoisonWorker is not a test: with VAD_FAKE_WORKER set, the test binary re-executed by
// poisonWorker speaks the worker protocol, answers every chunk and exits on the poison chunk.
func TestFakePoisonWorker(t *testing.T) {
	if os.Getenv("VAD_FAKE_WORKER") == "" {
		t.Skip("only runs as the fake worker child")
	}
	hs, err := ReadFrame(os.Stdin, nil)
	if err != nil {
		os.Exit(2)
	}
	req, err := DecodeHandshakeReq(hs)
	if err != nil {
		os.Exit(2)
	}
	_ = WriteFrame(os.Stdout, EncodeHandshakeResp(HandshakeResp{Status: StatusOK, ActualModelSHA256: req.ExpectedModelSHA256}))
	for {
		p, err := ReadFrame(os.Stdin, nil)
		if err != nil {
			os.Exit(0)
		}
		items, err := DecodeInferReq(p)
		if err != nil {
			os.Exit(2)
		}
		resp := make([]InferItemResp, len(items))
		for i, it := range items {
			if it.Seq == poisonSeq {
				os.Exit(1)
			}
			resp[i] = InferItemResp{RequestID: it.RequestID, SessionID: it.SessionID, Seq: it.Seq, Status: StatusOK, State: it.State}
		}
		_ = WriteFrame(os.Stdout, EncodeInferResp(resp))
	}
}

// poisonWorker is a launcher that re-executes this test binary as the fake worker.
func poisonWorker(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "vadworker.sh")
	script := "#!/bin/sh\nshift\nVAD_FAKE_WORKER=1 exec '" + os.Args[0] + "' -test.run='^TestFakePoisonWorker$'\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// A chunk the worker died on may be what killed it (a native crash): it fails rather than being re-run
// on the next worker, so one poison chunk costs one worker, not all of them.
func TestWorkerProcess_PoisonChunkKillsOneWorkerNotAll(t *testing.T) {
	exec, err := New(Config{Mode: "process", Workers: 2, Batch: 4, QueueCap: 256, IntraOp: 1, PerSessionLimit: 1,
		ParentDeadline: 500 * time.Millisecond, ParentGrace: 500 * time.Millisecond, WorkerBinary: poisonWorker(t)})
	if err != nil {
		t.Fatalf("new worker process executor: %v", err)
	}
	defer exec.Close()
	st0, st1 := exec.Register(0), exec.Register(1)
	defer st0.Close()
	defer st1.Close()
	run := func(st Stream, seq uint64) error {
		done := make(chan error, 1)
		if err := st.Submit(&Job{Seq: seq, Done: func(j *Job) { done <- j.Err }}); err != nil {
			return err
		}
		select {
		case err := <-done:
			return err
		case <-time.After(3 * time.Second):
			return errors.New("the chunk never finished")
		}
	}
	if err := run(st0, 1); err != nil {
		t.Fatalf("warm-up: %v", err)
	}
	if err := run(st0, poisonSeq); !errors.Is(err, ErrWorker) {
		t.Fatalf("the poison chunk finished with %v, want ErrWorker", err)
	}
	for end := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if a, _ := exec.Capacity(); a == 2 {
			break
		}
		if time.Now().After(end) {
			t.Fatal("worker 0 never came back")
		}
	}
	if n := exec.Stats().Restarts; n != 1 {
		t.Fatalf("restarts = %d, want 1: the poison chunk killed more than one worker", n)
	}
	if err := run(st1, 2); err != nil {
		t.Fatalf("a chunk on the other worker: %v", err)
	}
}

// A worker that fails is logged with the tail of its stderr (the ORT/runtime diagnostics a crash
// leaves), once: the next failure logs only what the next process wrote.
func TestWorkerProcess_FailureLogsStderrTail(t *testing.T) {
	logged := captureLogs(t)

	exec, err := New(Config{Mode: "process", Workers: 2, Batch: 4, QueueCap: 256, IntraOp: 1, PerSessionLimit: 1, ParentDeadline: 200 * time.Millisecond, ParentGrace: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer exec.Close()
	st := exec.Register(0) // session 0 -> worker 0
	defer st.Close()
	wp := exec.backend.(*workerProcessBackend).workers[0]

	crash := func(marker string, wantRestarts int64) {
		t.Helper()
		_, _ = wp.stderr.Write([]byte(marker)) // what the process wrote before dying
		// Killed but not reaped (HardKill reaps, and a worker found down between batches takes the
		// restart path): the next batch hits the dead pipe, the path that logs.
		var err error
		for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
			wp.mu.Lock()
			if wp.healthy && wp.cmd != nil {
				err = wp.cmd.Process.Kill()
				wp.mu.Unlock()
				break
			}
			err = errors.New("worker never became healthy")
			wp.mu.Unlock()
		}
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		if err := st.Submit(&Job{Seq: uint64(wantRestarts), Done: func(*Job) { close(done) }}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("job after the kill did not finish")
		}
		for end := time.Now().Add(5 * time.Second); exec.Stats().Restarts < wantRestarts && time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		}
	}
	crash("first-failure-diagnostic", 1)
	out := logged()
	if !strings.Contains(out, "vad: worker failed") || !strings.Contains(out, "stderr=first-failure-diagnostic") {
		t.Fatalf("failure not logged with its stderr tail:\n%s", out)
	}
	crash("second-failure-diagnostic", 2)
	out = logged()
	if !strings.Contains(out, "stderr=second-failure-diagnostic") || strings.Count(out, "first-failure-diagnostic") != 1 {
		t.Fatalf("second failure must log only its own stderr:\n%s", out)
	}
}

// captureLogs routes slog's default logger to a buffer until the test ends. It swaps a process-global,
// so no vad test may use t.Parallel.
func captureLogs(t *testing.T) func() string {
	var mu sync.Mutex
	var logs strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(lockedWriter{&mu, &logs}, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return func() string { mu.Lock(); defer mu.Unlock(); return logs.String() }
}

// A worker that dies during start or handshake leaves a stderr that is logged once, at that failure,
// and does not reappear in the next failure's log.
func TestWorkerProcess_StartFailureLogsStderrOnce(t *testing.T) {
	logged := captureLogs(t)
	exec, err := New(Config{Mode: "process", Workers: 1, Batch: 4, QueueCap: 256, IntraOp: 1, PerSessionLimit: 1, ParentDeadline: 200 * time.Millisecond, ParentGrace: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer exec.Close()
	b := exec.backend.(*workerProcessBackend)
	wp := b.workers[0]
	good := b.cfg.WorkerBinary
	bad := filepath.Join(t.TempDir(), "vadworker.sh")
	if err := os.WriteFile(bad, []byte("#!/bin/sh\necho startup-failure-marker >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	b.cfg.WorkerBinary = bad // the worker is up and idle: nothing else reads cfg until the next respawn
	if _, err := wp.start(); err == nil {
		t.Fatal("start of a worker that exits at startup succeeded")
	}
	b.cfg.WorkerBinary = good
	out := logged()
	if !strings.Contains(out, "vad: worker failed") || strings.Count(out, "startup-failure-marker") != 1 {
		t.Fatalf("startup failure not logged once with its stderr:\n%s", out)
	}
	_, _ = wp.stderr.Write([]byte("later-marker"))
	wp.handleWorkerFailure(nil, ErrWorker, false)
	out = logged()
	if !strings.Contains(out, "stderr=later-marker") || strings.Count(out, "startup-failure-marker") != 1 {
		t.Fatalf("a later failure must log only its own stderr:\n%s", out)
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	b  *strings.Builder
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}
