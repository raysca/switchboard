package core_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/coretest"
)

// 1. Test deterministic context eviction and provider-limit rejection
func TestPhase2_DeterministicContextEviction_AndLimitRejection(t *testing.T) {
	// Budget allows max 4 messages and 60 tokens
	cfg := core.ContextConfig{
		MaxMessages: 4,
		MaxTokens:   60,
	}
	ctxPool := core.NewConversationContext(cfg)

	// Set system instruction (Must never be evicted)
	ctxPool.SetSystemInstruction("You are a helpful voice assistant.")

	// Turn 1
	err := ctxPool.AddMessage(core.Message{
		Role:    core.RoleUser,
		Content: "Hello",
		TurnID:  "turn-1",
	})
	if err != nil {
		t.Fatalf("AddMessage turn-1 user failed: %v", err)
	}

	err = ctxPool.AddMessage(core.Message{
		Role:    core.RoleAssistant,
		Content: "Hi there!",
		TurnID:  "turn-1",
		Partial: false,
	})
	if err != nil {
		t.Fatalf("AddMessage turn-1 assistant failed: %v", err)
	}

	// Current messages: [System, User 1, Asst 1] (3 messages)
	if len(ctxPool.Messages()) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(ctxPool.Messages()))
	}

	// Turn 2
	err = ctxPool.AddMessage(core.Message{
		Role:    core.RoleUser,
		Content: "What is the weather?",
		TurnID:  "turn-2",
	})
	if err != nil {
		t.Fatalf("AddMessage turn-2 user failed: %v", err)
	}

	err = ctxPool.AddMessage(core.Message{
		Role:    core.RoleAssistant,
		Content: "It is sunny and 72 degrees.",
		TurnID:  "turn-2",
		Partial: false,
	})
	if err != nil {
		t.Fatalf("AddMessage turn-2 assistant failed: %v", err)
	}

	// Adding Turn 2 assistant would make 5 messages (> MaxMessages 4),
	// so oldest complete turn-pair (Turn 1: User & Assistant) must be evicted!
	msgs := ctxPool.Messages()
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages after eviction, got %d", len(msgs))
	}
	if ctxPool.EvictionCount() != 1 {
		t.Fatalf("expected 1 eviction, got %d", ctxPool.EvictionCount())
	}

	// Invariants: System instruction preserved, newest turn preserved
	if msgs[0].Role != core.RoleSystem || msgs[0].Content != "You are a helpful voice assistant." {
		t.Fatalf("system instruction was corrupted or evicted: %+v", msgs[0])
	}
	if msgs[1].TurnID != "turn-2" || msgs[2].TurnID != "turn-2" {
		t.Fatalf("expected turn-2 preserved, got: %+v, %+v", msgs[1], msgs[2])
	}

	// Turn 3: User message whose tokens push total tokens over MaxTokens (60)
	longUserContent := "Thank you very much for all your help! Could you also provide a detailed forecast for the week?"
	err = ctxPool.AddMessage(core.Message{
		Role:    core.RoleUser,
		Content: longUserContent,
		TurnID:  "turn-3",
	})
	if err != nil {
		t.Fatalf("AddMessage turn-3 user failed: %v", err)
	}

	// Now Turn 2 is evicted to keep within token limits
	if ctxPool.EvictionCount() != 2 {
		t.Fatalf("expected 2 evictions, got %d", ctxPool.EvictionCount())
	}

	msgs = ctxPool.Messages()
	// Preserved: System instruction and current User turn-3!
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages (system and user turn-3), got %d", len(msgs))
	}
	if msgs[0].Role != core.RoleSystem || msgs[1].TurnID != "turn-3" {
		t.Fatalf("invariants violated: %+v", msgs)
	}

	// Provider-limit rejection test:
	// A message whose tokens alone exceed MaxTokens (60)
	hugeContent := strings.Repeat("This is an excessively long user message that far exceeds token limits. ", 20)
	err = ctxPool.AddMessage(core.Message{
		Role:    core.RoleUser,
		Content: hugeContent,
		TurnID:  "turn-4",
	})
	if err == nil {
		t.Fatalf("expected ErrContextLimitExceeded, got nil")
	}
	// Context must remain intact and coherent after rejection
	msgsAfterReject := ctxPool.Messages()
	if len(msgsAfterReject) != 2 {
		t.Fatalf("expected context rollback to 2 messages, got %d", len(msgsAfterReject))
	}
}

// 2. Test duplicate and late final transcripts cannot create another user turn
func TestPhase2_DuplicateAndLateFinalTranscripts(t *testing.T) {
	ctxPool := core.NewConversationContext(core.ContextConfig{MaxMessages: 10, MaxTokens: 1000})
	userProc := core.NewUserContextProcessor(ctxPool, core.UserContextConfig{TurnPrefix: "turn"})
	passProc := &coretest.PassthroughProcessor{}

	pipe, err := core.NewPipeline(userProc, passProc)
	if err != nil {
		t.Fatalf("NewPipeline failed: %v", err)
	}

	sess, err := core.NewSession(101, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatalf("NewSession failed: %v", err)
	}
	if err := sess.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer sess.Close()

	// Post first final transcript
	err = sess.Post(&core.FinalTranscriptionFrame{
		Text: "Hello assistant",
		Turn: "turn-1",
		Gen:  1,
	})
	if err != nil {
		t.Fatalf("Post final transcript failed: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	if userProc.TurnsCreated.Load() != 1 {
		t.Fatalf("expected 1 turn created, got %d", userProc.TurnsCreated.Load())
	}
	if len(ctxPool.Messages()) != 1 {
		t.Fatalf("expected 1 message in context, got %d", len(ctxPool.Messages()))
	}

	// Post duplicate final transcript with same turn ID
	err = sess.Post(&core.FinalTranscriptionFrame{
		Text: "Hello assistant",
		Turn: "turn-1",
		Gen:  1,
	})
	if err != nil {
		t.Fatalf("Post duplicate transcript failed: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	// Post duplicate transcript with same text but empty turn ID
	err = sess.Post(&core.FinalTranscriptionFrame{
		Text: "Hello assistant",
		Turn: "",
		Gen:  1,
	})
	if err != nil {
		t.Fatalf("Post duplicate text failed: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	// Post late transcript for an older generation (advance generation first)
	sess.IncrementGeneration() // now Gen 2
	err = sess.Post(&core.FinalTranscriptionFrame{
		Text: "Late transcript from Gen 1",
		Turn: "turn-old",
		Gen:  1, // Stale!
	})
	if err != nil {
		t.Fatalf("Post late transcript failed: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	// Verify that duplicates and late transcripts were dropped
	if userProc.DuplicateDropCount.Load() != 2 {
		t.Fatalf("expected 2 duplicate drops, got %d", userProc.DuplicateDropCount.Load())
	}
	if sess.StaleDiscarded.Load() != 1 {
		t.Fatalf("expected 1 stale generation discard by session, got %d", sess.StaleDiscarded.Load())
	}
	if userProc.TurnsCreated.Load() != 1 {
		t.Fatalf("expected turns created to still be 1, got %d", userProc.TurnsCreated.Load())
	}
	if len(ctxPool.Messages()) != 1 {
		t.Fatalf("expected 1 message in context, got %d", len(ctxPool.Messages()))
	}
}

// 3. Test interruption before first audio records no assistant text
func TestPhase2_InterruptionBeforeFirstAudio_RecordsNoAssistantText(t *testing.T) {
	ctxPool := core.NewConversationContext(core.ContextConfig{MaxMessages: 10, MaxTokens: 1000})
	userProc := core.NewUserContextProcessor(ctxPool, core.UserContextConfig{TurnPrefix: "turn"})
	asstProc := core.NewAssistantContextProcessor(ctxPool, core.DefaultAssistantContextConfig())

	// LLM with delay so interruption happens before audio completion
	fakeLLM := coretest.NewLLMProcessor(coretest.LLMConfig{
		ResponseChunks: []string{"Hello there! ", "How can I help you today?"},
		ChunkDelay:     100 * time.Millisecond,
	})

	fakeTTS := coretest.NewTTSProcessor(coretest.TTSConfig{
		FramesPerUnit: 2,
		FrameDelay:    50 * time.Millisecond,
	})

	fakeTransport := coretest.NewTransportOutput(coretest.TransportConfig{
		WriteDelay: 50 * time.Millisecond,
	})

	pipe, err := core.NewPipeline(userProc, asstProc, fakeLLM, fakeTTS, fakeTransport)
	if err != nil {
		t.Fatalf("NewPipeline failed: %v", err)
	}

	sess, err := core.NewSession(102, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatalf("NewSession failed: %v", err)
	}
	if err := sess.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer sess.Close()

	// 1. User turn starts
	err = sess.Post(&core.FinalTranscriptionFrame{
		Text: "Hello",
		Turn: "turn-1",
		Gen:  1,
	})
	if err != nil {
		t.Fatalf("Post final transcript failed: %v", err)
	}

	// 2. Interruption arrives almost immediately before any audio write completes
	time.Sleep(20 * time.Millisecond)
	err = sess.Post(&core.UserStartedSpeakingFrame{
		Seq: 1,
		Gen: 1,
	})
	if err != nil {
		t.Fatalf("Post UserStartedSpeaking failed: %v", err)
	}

	time.Sleep(250 * time.Millisecond)

	// Verify context: Contains User turn, but ZERO assistant text
	msgs := ctxPool.Messages()
	if len(msgs) != 1 {
		t.Fatalf("expected exactly 1 message (user), got %d messages: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != core.RoleUser {
		t.Fatalf("expected user role, got: %s", msgs[0].Role)
	}
}

// 4. Test mid-unit interruption excludes the partly written unit
func TestPhase2_MidUnitInterruption_ExcludesPartlyWrittenUnit(t *testing.T) {
	ctxPool := core.NewConversationContext(core.ContextConfig{MaxMessages: 10, MaxTokens: 1000})
	userProc := core.NewUserContextProcessor(ctxPool, core.UserContextConfig{TurnPrefix: "turn"})
	asstProc := core.NewAssistantContextProcessor(ctxPool, core.DefaultAssistantContextConfig())

	pipe, err := core.NewPipeline(userProc, asstProc, &coretest.PassthroughProcessor{})
	if err != nil {
		t.Fatalf("NewPipeline failed: %v", err)
	}

	sess, err := core.NewSession(103, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatalf("NewSession failed: %v", err)
	}
	if err := sess.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer sess.Close()

	// Start user turn
	_ = sess.Post(&core.FinalTranscriptionFrame{Text: "Tell me a story", Turn: "turn-1", Gen: 1})
	time.Sleep(20 * time.Millisecond)

	// LLM emits Unit 1 ("Once upon a time.") and Unit 2 ("In a galaxy far away.")
	_ = sess.Post(&core.LLMTextDeltaFrame{
		Delta: "Once upon a time. In a galaxy far away.",
		Turn:  "turn-1",
		Gen:   1,
	})
	time.Sleep(30 * time.Millisecond)

	// Simulate Unit 1 final write completion arrives (delivered!)
	_ = sess.Post(&core.WriteCompletionFrame{
		Turn:    "turn-1",
		Gen:     1,
		UnitID:  1,
		IsFinal: true,
	})
	time.Sleep(20 * time.Millisecond)

	// Simulate Unit 2 frame 1 (NOT final) write completion arrives (mid-unit!)
	_ = sess.Post(&core.WriteCompletionFrame{
		Turn:    "turn-1",
		Gen:     1,
		UnitID:  2,
		IsFinal: false, // Frame 1 of unit 2, unit 2 is partly written!
	})
	time.Sleep(20 * time.Millisecond)

	// Barge-in interruption occurs before Unit 2 final frame is written!
	_ = sess.Post(&core.UserStartedSpeakingFrame{
		Seq: 1,
		Gen: 1,
	})
	time.Sleep(50 * time.Millisecond)

	// Invariant: Unit 2 was partly written, so it MUST BE EXCLUDED IN FULL.
	// Only Unit 1 was fully written before the barrier.
	msgs := ctxPool.Messages()
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages (User and Partial Assistant), got %d: %+v", len(msgs), msgs)
	}

	asstMsg := msgs[1]
	if asstMsg.Role != core.RoleAssistant {
		t.Fatalf("expected assistant role, got: %s", asstMsg.Role)
	}
	if !asstMsg.Partial {
		t.Fatalf("expected Partial=true for interrupted assistant turn")
	}
	if asstMsg.Content != "Once upon a time." {
		t.Fatalf("expected content 'Once upon a time.', got: %q", asstMsg.Content)
	}
}

// 5. Test interruption between units records only fully written units with Partial=true
func TestPhase2_InterruptionBetweenUnits_RecordsOnlyFullyWrittenUnitsWithPartialTrue(t *testing.T) {
	ctxPool := core.NewConversationContext(core.ContextConfig{MaxMessages: 10, MaxTokens: 1000})
	userProc := core.NewUserContextProcessor(ctxPool, core.UserContextConfig{TurnPrefix: "turn"})
	asstProc := core.NewAssistantContextProcessor(ctxPool, core.DefaultAssistantContextConfig())

	pipe, err := core.NewPipeline(userProc, asstProc, &coretest.PassthroughProcessor{})
	if err != nil {
		t.Fatalf("NewPipeline failed: %v", err)
	}

	sess, err := core.NewSession(104, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatalf("NewSession failed: %v", err)
	}
	if err := sess.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer sess.Close()

	_ = sess.Post(&core.FinalTranscriptionFrame{Text: "Explain physics", Turn: "turn-1", Gen: 1})
	time.Sleep(20 * time.Millisecond)

	// Emit 3 units: Unit 1, Unit 2, Unit 3
	_ = sess.Post(&core.LLMTextDeltaFrame{
		Delta: "Physics is the study of matter. Energy and force are key concepts. Relativity is also important.",
		Turn:  "turn-1",
		Gen:   1,
	})
	time.Sleep(30 * time.Millisecond)

	// Complete write for Unit 1
	_ = sess.Post(&core.WriteCompletionFrame{Turn: "turn-1", Gen: 1, UnitID: 1, IsFinal: true})
	// Complete write for Unit 2
	_ = sess.Post(&core.WriteCompletionFrame{Turn: "turn-1", Gen: 1, UnitID: 2, IsFinal: true})
	time.Sleep(20 * time.Millisecond)

	// Interruption occurs between Unit 2 and Unit 3 (Unit 3 was never written)
	_ = sess.Post(&core.UserStartedSpeakingFrame{Seq: 2, Gen: 1})
	time.Sleep(50 * time.Millisecond)

	msgs := ctxPool.Messages()
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	asstMsg := msgs[1]
	if !asstMsg.Partial {
		t.Fatalf("expected Partial=true")
	}
	expected := "Physics is the study of matter. Energy and force are key concepts."
	if asstMsg.Content != expected {
		t.Fatalf("expected %q, got %q", expected, asstMsg.Content)
	}
}

// 6. Test write completion racing the barrier has deterministic ordering
func TestPhase2_WriteCompletionRacingBarrier_DeterministicOrdering(t *testing.T) {
	ctxPool := core.NewConversationContext(core.ContextConfig{MaxMessages: 10, MaxTokens: 1000})
	userProc := core.NewUserContextProcessor(ctxPool, core.UserContextConfig{TurnPrefix: "turn"})
	asstProc := core.NewAssistantContextProcessor(ctxPool, core.DefaultAssistantContextConfig())

	pipe, err := core.NewPipeline(userProc, asstProc, &coretest.PassthroughProcessor{})
	if err != nil {
		t.Fatalf("NewPipeline failed: %v", err)
	}

	sess, err := core.NewSession(105, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatalf("NewSession failed: %v", err)
	}
	if err := sess.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer sess.Close()

	_ = sess.Post(&core.FinalTranscriptionFrame{Text: "Question", Turn: "turn-1", Gen: 1})
	time.Sleep(10 * time.Millisecond)

	_ = sess.Post(&core.LLMTextDeltaFrame{
		Delta: "Unit one content. Unit two content.",
		Turn:  "turn-1",
		Gen:   1,
	})
	time.Sleep(20 * time.Millisecond)

	// Interruption barrier triggers: advances generation to 2
	_ = sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})

	// Racing write completion from old generation (Gen 1) arrives AFTER barrier
	_ = sess.Post(&core.WriteCompletionFrame{
		Turn:    "turn-1",
		Gen:     1, // Old generation!
		UnitID:  1,
		IsFinal: true,
	})

	time.Sleep(50 * time.Millisecond)

	// Verify racing write completion with stale generation was discarded
	// and did NOT mutate context or turn state
	msgs := ctxPool.Messages()
	for _, m := range msgs {
		if m.Role == core.RoleAssistant {
			t.Fatalf("stale racing write completion mutated assistant context: %+v", m)
		}
	}
}

// 7. Test LLM/TTS output arriving after cancellation never reaches output or context
func TestPhase2_LLMTTSOutputAfterCancellation_NeverReachesOutputOrContext(t *testing.T) {
	ctxPool := core.NewConversationContext(core.ContextConfig{MaxMessages: 10, MaxTokens: 1000})
	userProc := core.NewUserContextProcessor(ctxPool, core.UserContextConfig{TurnPrefix: "turn"})
	asstProc := core.NewAssistantContextProcessor(ctxPool, core.DefaultAssistantContextConfig())

	// Configured with IgnoreCancel=true: simulated rogue LLM/TTS that keeps emitting after cancel
	fakeLLM := coretest.NewLLMProcessor(coretest.LLMConfig{
		ResponseChunks: []string{"chunk 1. ", "rogue chunk after cancel."},
		ChunkDelay:     80 * time.Millisecond,
		IgnoreCancel:   true,
	})

	fakeTTS := coretest.NewTTSProcessor(coretest.TTSConfig{
		FramesPerUnit: 2,
		FrameDelay:    50 * time.Millisecond,
		IgnoreCancel:  true,
	})

	fakeTransport := coretest.NewTransportOutput(coretest.TransportConfig{WriteDelay: 0})

	pipe, err := core.NewPipeline(userProc, asstProc, fakeLLM, fakeTTS, fakeTransport)
	if err != nil {
		t.Fatalf("NewPipeline failed: %v", err)
	}

	sess, err := core.NewSession(106, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatalf("NewSession failed: %v", err)
	}
	if err := sess.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer sess.Close()

	_ = sess.Post(&core.FinalTranscriptionFrame{Text: "Test", Turn: "turn-1", Gen: 1})
	time.Sleep(20 * time.Millisecond)

	// Barge-in: cancels LLM/TTS and increments generation to 2
	_ = sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})

	// Wait for rogue LLM/TTS background goroutines to emit their delayed frames
	time.Sleep(250 * time.Millisecond)

	// Verify that late completions were fenced by generation
	if sess.StaleDiscarded.Load() == 0 {
		t.Fatalf("expected StaleDiscarded counter > 0, got %d", sess.StaleDiscarded.Load())
	}

	// Verify transport received no frames from old generation accepted
	written := fakeTransport.GetWrittenFrames()
	for _, wf := range written {
		if wf.Gen == 1 {
			t.Fatalf("stale frame was written to transport: %+v", wf)
		}
	}
}

// 8. Test flush timeout fails/degrades explicitly without blocking the run loop
func TestPhase2_FlushTimeoutFailsOrDegradesExplicitlyWithoutBlocking(t *testing.T) {
	ctxPool := core.NewConversationContext(core.ContextConfig{MaxMessages: 10, MaxTokens: 1000})
	userProc := core.NewUserContextProcessor(ctxPool, core.UserContextConfig{TurnPrefix: "turn"})

	// Short flush timeout for test (50ms)
	asstCfg := core.DefaultAssistantContextConfig()
	asstCfg.FlushTimeout = 50 * time.Millisecond
	asstProc := core.NewAssistantContextProcessor(ctxPool, asstCfg)

	// Simulate transport hanging on flush acknowledgement (withhold FlushProcessedFrame)
	fakeTransport := coretest.NewTransportOutput(coretest.TransportConfig{
		SimulateFlushHang: true,
	})

	pipe, err := core.NewPipeline(userProc, asstProc, fakeTransport)
	if err != nil {
		t.Fatalf("NewPipeline failed: %v", err)
	}

	sess, err := core.NewSession(107, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatalf("NewSession failed: %v", err)
	}
	if err := sess.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer sess.Close()

	_ = sess.Post(&core.FinalTranscriptionFrame{Text: "Hello", Turn: "turn-1", Gen: 1})
	time.Sleep(10 * time.Millisecond)

	_ = sess.Post(&core.LLMTextDeltaFrame{Delta: "Response text here.", Turn: "turn-1", Gen: 1})
	time.Sleep(10 * time.Millisecond)

	// Interruption occurs, triggering flush frame
	_ = sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})

	// Wait for flush timeout to fire (50ms timeout)
	time.Sleep(100 * time.Millisecond)

	// Verify flush timeout counter incremented
	if asstProc.FlushTimeouts.Load() != 1 {
		t.Fatalf("expected FlushTimeouts == 1, got %d", asstProc.FlushTimeouts.Load())
	}

	// Crucial invariant: session run loop is NOT blocked!
	// Next turn must process normally
	err = sess.Post(&core.FinalTranscriptionFrame{
		Text: "Subsequent user speech works",
		Turn: "turn-2",
		Gen:  sess.CurrentGeneration(),
	})
	if err != nil {
		t.Fatalf("post after flush timeout failed: %v", err)
	}

	time.Sleep(30 * time.Millisecond)

	msgs := ctxPool.Messages()
	foundTurn2 := false
	for _, m := range msgs {
		if m.TurnID == "turn-2" {
			foundTurn2 = true
			break
		}
	}
	if !foundTurn2 {
		t.Fatalf("turn-2 was not processed: run loop was blocked by flush timeout")
	}
}

// 9. Test next turn succeeds in the same session with coherent context
func TestPhase2_NextTurnSucceedsInSameSessionWithCoherentContext(t *testing.T) {
	ctxPool := core.NewConversationContext(core.ContextConfig{MaxMessages: 10, MaxTokens: 1000})
	ctxPool.SetSystemInstruction("You are a helpful assistant.")

	userProc := core.NewUserContextProcessor(ctxPool, core.UserContextConfig{TurnPrefix: "turn"})
	asstProc := core.NewAssistantContextProcessor(ctxPool, core.DefaultAssistantContextConfig())

	fakeTransport := coretest.NewTransportOutput(coretest.TransportConfig{WriteDelay: 0})

	pipe, err := core.NewPipeline(userProc, asstProc, fakeTransport)
	if err != nil {
		t.Fatalf("NewPipeline failed: %v", err)
	}

	sess, err := core.NewSession(108, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatalf("NewSession failed: %v", err)
	}
	if err := sess.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer sess.Close()

	// --- Turn 1: Interrupted ---
	gen1 := sess.CurrentGeneration()
	_ = sess.Post(&core.FinalTranscriptionFrame{Text: "What is the weather?", Turn: "turn-1", Gen: gen1})
	time.Sleep(10 * time.Millisecond)

	// Assistant produces two units
	_ = sess.Post(&core.LLMTextDeltaFrame{
		Delta: "The weather is sunny in Seattle. It might rain tomorrow.",
		Turn:  "turn-1",
		Gen:   gen1,
	})
	time.Sleep(20 * time.Millisecond)

	// Unit 1 write completes
	_ = sess.Post(&core.WriteCompletionFrame{Turn: "turn-1", Gen: gen1, UnitID: 1, IsFinal: true})
	time.Sleep(10 * time.Millisecond)

	// User interrupts mid-turn before Unit 2 completes
	_ = sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: gen1})
	time.Sleep(30 * time.Millisecond)

	// --- Turn 2: Completes without interruption in the SAME session ---
	gen2 := sess.CurrentGeneration()
	_ = sess.Post(&core.FinalTranscriptionFrame{Text: "Nevermind, what time is it?", Turn: "turn-2", Gen: gen2})
	time.Sleep(10 * time.Millisecond)

	_ = sess.Post(&core.LLMTextDeltaFrame{
		Delta: "It is currently 3:30 PM.",
		Turn:  "turn-2",
		Gen:   gen2,
	})
	_ = sess.Post(&core.LLMTextFrame{
		Text: "",
		Turn: "turn-2",
		Gen:  gen2,
	})
	time.Sleep(20 * time.Millisecond)

	// Complete write for Turn 2's final unit. Unit IDs remain monotonic across
	// turns, including the interrupted turn's two emitted units.
	_ = sess.Post(&core.WriteCompletionFrame{Turn: "turn-2", Gen: gen2, UnitID: 3, IsFinal: true})
	time.Sleep(30 * time.Millisecond)

	// Verify coherent multi-turn context
	msgs := ctxPool.Messages()
	if len(msgs) != 5 {
		t.Fatalf("expected 5 messages in coherent history, got %d: %+v", len(msgs), msgs)
	}

	// 0: System prompt
	if msgs[0].Role != core.RoleSystem {
		t.Fatalf("msg 0 expected system prompt, got: %+v", msgs[0])
	}
	// 1: Turn 1 User
	if msgs[1].Role != core.RoleUser || msgs[1].TurnID != "turn-1" || msgs[1].Content != "What is the weather?" {
		t.Fatalf("msg 1 mismatch: %+v", msgs[1])
	}
	// 2: Turn 1 Assistant (Interrupted, Partial=true, only Unit 1)
	if msgs[2].Role != core.RoleAssistant || msgs[2].TurnID != "turn-1" || !msgs[2].Partial || msgs[2].Content != "The weather is sunny in Seattle." {
		t.Fatalf("msg 2 mismatch (expected partial): %+v", msgs[2])
	}
	// 3: Turn 2 User
	if msgs[3].Role != core.RoleUser || msgs[3].TurnID != "turn-2" || msgs[3].Content != "Nevermind, what time is it?" {
		t.Fatalf("msg 3 mismatch: %+v", msgs[3])
	}
	// 4: Turn 2 Assistant (Completed, Partial=false)
	if msgs[4].Role != core.RoleAssistant || msgs[4].TurnID != "turn-2" || msgs[4].Partial || msgs[4].Content != "It is currently 3:30 PM." {
		t.Fatalf("msg 4 mismatch (expected full): %+v", msgs[4])
	}

	if sess.State() != core.StateRunning {
		t.Fatalf("expected session still running, got: %v", sess.State())
	}
}

func TestAssistantContext_NoDuplicateSynthesisOnFinalLLMTextFrame(t *testing.T) {
	ctxPool := core.NewConversationContext(core.ContextConfig{MaxMessages: 10, MaxTokens: 1000})
	cfg := core.DefaultAssistantContextConfig()
	cfg.MinUnitLength = 5
	cfg.MaxUnitLength = 100
	asstProc := core.NewAssistantContextProcessor(ctxPool, cfg)

	var emittedUnits []string
	sink := &coretest.PassthroughProcessor{}

	pipe, err := core.NewPipeline(asstProc, sink)
	if err != nil {
		t.Fatalf("NewPipeline failed: %v", err)
	}
	sess, err := core.NewSession(101, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatalf("NewSession failed: %v", err)
	}
	if err := sess.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer sess.Close()

	// 1. Send UserMessageFrame to initialize turn
	_ = sess.Post(&core.UserMessageFrame{Turn: "turn-1", Gen: 1, Content: "Hello"})
	time.Sleep(20 * time.Millisecond)

	// 2. Stream two deltas that form a complete sentence and an incomplete fragment
	_ = sess.Post(&core.LLMTextDeltaFrame{Turn: "turn-1", Gen: 1, Delta: "Hello world. How are "})
	_ = sess.Post(&core.LLMTextDeltaFrame{Turn: "turn-1", Gen: 1, Delta: "you today?"})
	time.Sleep(30 * time.Millisecond)

	// 3. Send final LLMTextFrame containing the entire accumulated text
	fullText := "Hello world. How are you today?"
	_ = sess.Post(&core.LLMTextFrame{Turn: "turn-1", Gen: 1, Text: fullText})
	time.Sleep(30 * time.Millisecond)

	// Inspect emitted synthesis units
	for _, f := range sink.GetFrames() {
		if su, ok := f.(*core.SynthesisUnitFrame); ok {
			emittedUnits = append(emittedUnits, su.Text)
		}
	}

	// Should emit exactly two units: "Hello world." and "How are you today?"
	// NOT four units with "Hello world." repeated!
	if len(emittedUnits) != 2 {
		t.Fatalf("expected exactly 2 synthesis units, got %d: %+v", len(emittedUnits), emittedUnits)
	}
	if emittedUnits[0] != "Hello world." {
		t.Errorf("expected unit 0 'Hello world.', got %q", emittedUnits[0])
	}
	if emittedUnits[1] != "How are you today?" {
		t.Errorf("expected unit 1 'How are you today?', got %q", emittedUnits[1])
	}
}

func TestAssistantContext_NoPrematureCommit_BeforeLLMComplete(t *testing.T) {
	ctxPool := core.NewConversationContext(core.ContextConfig{MaxMessages: 10, MaxTokens: 1000})
	asstProc := core.NewAssistantContextProcessor(ctxPool, core.AssistantContextConfig{
		MinUnitLength: 5,
		MaxUnitLength: 100,
	})

	pipe, err := core.NewPipeline(asstProc)
	if err != nil {
		t.Fatalf("NewPipeline failed: %v", err)
	}
	sess, err := core.NewSession(109, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatalf("NewSession failed: %v", err)
	}
	if err := sess.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer sess.Close()

	// 1. Initialize turn
	_ = sess.Post(&core.UserMessageFrame{Turn: "turn-1", Gen: 1, Content: "Tell me a story"})
	time.Sleep(20 * time.Millisecond)

	// 2. Stream sentence 1
	_ = sess.Post(&core.LLMTextDeltaFrame{Turn: "turn-1", Gen: 1, Delta: "Once upon a time. "})
	time.Sleep(20 * time.Millisecond)

	// 3. Sentence 1 write completes and TTS signals stopped
	_ = sess.Post(&core.WriteCompletionFrame{Turn: "turn-1", Gen: 1, UnitID: 1, IsFinal: true})
	_ = sess.Post(&core.TTSStoppedFrame{Turn: "turn-1", Gen: 1})
	time.Sleep(20 * time.Millisecond)

	// Assistant turn MUST NOT be committed because LLM generation is not complete!
	msgs := ctxPool.Messages()
	for _, m := range msgs {
		if m.Role == core.RoleAssistant {
			t.Fatalf("premature commit detected: assistant message committed while LLM still generating: %q", m.Content)
		}
	}

	// 4. Stream sentence 2 and finish LLM response
	_ = sess.Post(&core.LLMTextDeltaFrame{Turn: "turn-1", Gen: 1, Delta: "There lived a brave knight."})
	_ = sess.Post(&core.LLMTextFrame{Turn: "turn-1", Gen: 1, Text: "Once upon a time. There lived a brave knight."})
	time.Sleep(20 * time.Millisecond)

	// 5. Sentence 2 write completes
	_ = sess.Post(&core.WriteCompletionFrame{Turn: "turn-1", Gen: 1, UnitID: 2, IsFinal: true})
	time.Sleep(20 * time.Millisecond)

	// Now assistant turn MUST be committed as a single complete response
	msgs = ctxPool.Messages()
	var asstMsgs []core.Message
	for _, m := range msgs {
		if m.Role == core.RoleAssistant {
			asstMsgs = append(asstMsgs, m)
		}
	}
	if len(asstMsgs) != 1 {
		t.Fatalf("expected exactly 1 committed assistant message, got %d", len(asstMsgs))
	}
	if asstMsgs[0].Partial {
		t.Errorf("expected complete (non-partial) turn, got partial")
	}
	expected := "Once upon a time. There lived a brave knight."
	if asstMsgs[0].Content != expected {
		t.Errorf("expected content %q, got %q", expected, asstMsgs[0].Content)
	}
}

func TestWriteCompletionErrorDoesNotCommitAssistantText(t *testing.T) {
	ctxPool := core.NewConversationContext(core.ContextConfig{MaxMessages: 10, MaxTokens: 1000})
	userProc := core.NewUserContextProcessor(ctxPool, core.UserContextConfig{TurnPrefix: "turn"})
	asstProc := core.NewAssistantContextProcessor(ctxPool, core.DefaultAssistantContextConfig())
	pipe, err := core.NewPipeline(userProc, asstProc, &coretest.PassthroughProcessor{})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := core.NewSession(1, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Start(); err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	_ = sess.Post(&core.FinalTranscriptionFrame{Text: "hello", Turn: "turn-1", Gen: 1})
	_ = sess.Post(&core.LLMTextFrame{Text: "This text was not delivered.", Turn: "turn-1", Gen: 1})
	time.Sleep(30 * time.Millisecond)
	_ = sess.Post(&core.WriteCompletionFrame{
		Turn:    "turn-1",
		Gen:     1,
		UnitID:  1,
		IsFinal: true,
		Err:     errors.New("write timeout"),
	})
	time.Sleep(30 * time.Millisecond)

	for _, m := range ctxPool.Messages() {
		if m.Role == core.RoleAssistant {
			t.Fatalf("failed audio write committed assistant text %q", m.Content)
		}
	}
}
