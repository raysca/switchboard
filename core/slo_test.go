package core_test

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"govoice/core"
)

// The caller speaks over the bot: how long until the flush reaches the transport output (where
// buffered audio is dropped), measured with a transport that takes 50 ms per write. AGENTS.md
// targets sub-200 ms barge-in; this pins the pipeline's share of it.
func TestInterruptionFlushP99(t *testing.T) {
	const n = 100
	const ceiling = 200 * time.Millisecond

	samples := make([]time.Duration, n)
	for i := 0; i < n; i++ {
		// A fresh context per sample keeps each flush inside the production
		// bounds. Interrupted turns are not complete pairs, so one shared
		// context cannot evict them once MaxMessages is reached.
		ctxPool := core.NewConversationContext(core.ContextConfig{MaxMessages: 20, MaxTokens: 4096})
		userProc := core.NewUserContextProcessor(ctxPool, core.UserContextConfig{TurnPrefix: "turn"})
		asstProc := core.NewAssistantContextProcessor(ctxPool, core.DefaultAssistantContextConfig())
		fakeTransport := core.NewFakeTransportOutput(core.FakeTransportConfig{
			WriteDelay: 50 * time.Millisecond,
		})

		pipe, err := core.NewPipeline(userProc, asstProc, fakeTransport)
		if err != nil {
			t.Fatalf("sample %d NewPipeline: %v", i, err)
		}
		sess, err := core.NewSession(uint64(i+1), pipe, core.DefaultResourceBudget())
		if err != nil {
			t.Fatalf("sample %d NewSession: %v", i, err)
		}
		if err := sess.Start(); err != nil {
			sess.Close()
			t.Fatalf("sample %d Start: %v", i, err)
		}
		fail := func(format string, args ...any) {
			sess.Close()
			t.Fatalf(format, args...)
		}

		beforeTurns := userProc.TurnsCreated.Load()
		gen := sess.CurrentGeneration()
		if err := sess.Post(&core.FinalTranscriptionFrame{
			Text: fmt.Sprintf("hello %d", i),
			Turn: fmt.Sprintf("turn-%d", i),
			Gen:  gen,
		}); err != nil {
			fail("sample %d post transcript: %v", i, err)
		}
		if !sloWaitUntil(time.Second, func() bool {
			return userProc.TurnsCreated.Load() > beforeTurns
		}) {
			fail("sample %d: assistant turn did not start", i)
		}

		beforeFlushes := fakeTransport.FlushCount.Load()
		start := time.Now()
		if err := sess.Post(&core.UserStartedSpeakingFrame{
			Seq: uint64(i + 1),
			Gen: sess.CurrentGeneration(),
		}); err != nil {
			fail("sample %d post UserStartedSpeaking: %v", i, err)
		}
		if !sloWaitUntil(ceiling, func() bool {
			return fakeTransport.FlushCount.Load() > beforeFlushes
		}) {
			fail("sample %d: the flush did not reach the transport within %s", i, ceiling)
		}
		samples[i] = time.Since(start)
		if err := sess.Close(); err != nil {
			t.Fatalf("sample %d Close: %v", i, err)
		}
	}

	slices.Sort(samples)
	p99 := samples[98]
	t.Logf("interruption flush p99=%s", p99)
	if p99 > ceiling {
		t.Fatalf("interruption flush p99 %s exceeds 200ms", p99)
	}
}

// Closing a session must return promptly (calls end and capacity frees up).
func TestSessionTeardownP99(t *testing.T) {
	const n = 100
	const ceiling = 2 * time.Second

	samples := make([]time.Duration, n)
	for i := 0; i < n; i++ {
		pipe, err := core.NewPipeline(&core.PassthroughProcessor{})
		if err != nil {
			t.Fatalf("sample %d NewPipeline: %v", i, err)
		}
		sess, err := core.NewSession(uint64(i+1), pipe, core.DefaultResourceBudget())
		if err != nil {
			t.Fatalf("sample %d NewSession: %v", i, err)
		}
		if err := sess.Start(); err != nil {
			t.Fatalf("sample %d Start: %v", i, err)
		}
		start := time.Now()
		if err := sess.Close(); err != nil {
			t.Fatalf("sample %d Close: %v", i, err)
		}
		samples[i] = time.Since(start)
	}

	slices.Sort(samples)
	p99 := samples[98]
	t.Logf("session teardown p99=%s", p99)
	if p99 > ceiling {
		t.Fatalf("session teardown p99 %s exceeds 2s", p99)
	}
}

func sloWaitUntil(limit time.Duration, ready func() bool) bool {
	deadline := time.Now().Add(limit)
	for {
		if ready() {
			return true
		}
		if time.Now().After(deadline) {
			return ready()
		}
		time.Sleep(time.Millisecond)
	}
}
