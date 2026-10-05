package google

import (
	"errors"
	"sync"
	"testing"
	"time"

	"govoice/core"
	coretranscript "govoice/core/transcript"
)

func TestSTT_RequestByteLimit(t *testing.T) {
	// Pinned max_request_bytes: 25600
	cfg := DefaultSTTConfig()
	cfg.MaxRequestBytes = 25600
	opener := &mockSTTOpener{}
	cfg.StreamOpener = opener

	proc, err := NewSTTProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}

	finalCfg := coretranscript.DefaultConfig()
	finalCfg.GracePeriod = 0
	finalizer, _ := coretranscript.NewProcessor(finalCfg)
	pipe, _ := core.NewPipeline(proc, finalizer)
	session, _ := core.NewSession(1, pipe, core.DefaultResourceBudget())
	_ = proc.Attach(session, 0)
	_ = session.Start()
	defer session.Close()

	// 1. Oversized chunk: 25601 bytes must be rejected
	oversizedAudio := &core.InputAudioFrame{
		Samples:    make([]byte, 25601),
		Encoding:   core.EncodingPCM16LE,
		SampleRate: 16000,
		Channels:   1,
	}
	_ = session.Post(oversizedAudio)
	time.Sleep(50 * time.Millisecond)

	if session.Err() == nil || !errors.Is(session.Err(), ErrRequestTooLarge) {
		t.Fatalf("expected ErrRequestTooLarge, got: %v", session.Err())
	}
}

func TestSTT_AcceleratedFiveRotationsWithoutDuplicates(t *testing.T) {
	// Accelerated STT test crossing at least five rotations without duplicate or missing turns
	opener := &mockSTTOpener{}
	cfg := DefaultSTTConfig()
	// Set very short rotation threshold so we cross > 5 rotations rapidly
	cfg.StreamRotationThreshold = 40 * time.Millisecond
	cfg.StreamOverlap = 50 * time.Millisecond // 1600 bytes overlap
	cfg.StreamOpener = opener

	streamIndex := 0
	opener.onOpenStream = func(c STTConfig) *mockSTTStream {
		streamIndex++
		curr := streamIndex
		st := newMockSTTStream()
		// On stream 1 and stream 2, simulate the provider returning the SAME final transcript
		// across the rotation boundary to test deduplication!
		if curr <= 2 {
			st.PushResult(&STTResult{Transcript: "hello world", IsFinal: true})
		}
		return st
	}

	proc, err := NewSTTProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}

	finalCfg := coretranscript.DefaultConfig()
	finalCfg.GracePeriod = 0
	finalizer, _ := coretranscript.NewProcessor(finalCfg)
	pipe, _ := core.NewPipeline(proc, finalizer)
	session, _ := core.NewSession(1, pipe, core.DefaultResourceBudget())
	_ = proc.Attach(session, 0)

	var finals []string
	var mu sync.Mutex
	session.OnOutput = func(f core.Frame) {
		mu.Lock()
		defer mu.Unlock()
		if finalFrame, ok := f.(*core.FinalTranscriptionFrame); ok {
			finals = append(finals, finalFrame.Text)
		}
	}

	_ = session.Start()
	defer session.Close()

	// Send UserStartedSpeakingFrame
	_ = session.Post(&core.UserStartedSpeakingFrame{
		Seq: 1,
		Gen: 1,
	})

	// Stream 20ms audio chunks (640 bytes at 16kHz mono) for 350ms (at least 6-7 rotations)
	chunk := make([]byte, 640)
	for i := 0; i < 20; i++ {
		_ = session.Post(&core.InputAudioFrame{
			Samples:    chunk,
			Encoding:   core.EncodingPCM16LE,
			SampleRate: 16000,
			Channels:   1,
			Gen:        1,
		})
		time.Sleep(20 * time.Millisecond)
	}
	_ = session.Post(&core.UserStoppedSpeakingFrame{Seq: 2, Gen: 1, Timestamp: time.Now()})

	time.Sleep(100 * time.Millisecond)

	rotations := proc.RotationsCount.Load()
	if rotations < 5 {
		t.Fatalf("expected at least 5 stream rotations, got %d", rotations)
	}

	mu.Lock()
	defer mu.Unlock()

	// Common policy, rather than a provider-local wall clock, commits once.
	if len(finals) != 1 {
		t.Fatalf("expected exactly 1 deduplicated final transcript, got %d: %v", len(finals), finals)
	}
	if finals[0] != "hello world" {
		t.Fatalf("unexpected transcript: %s", finals[0])
	}
	if proc.DeduplicatedResults.Load() != 0 {
		t.Fatalf("provider-local text dedupe must be disabled, got %d", proc.DeduplicatedResults.Load())
	}

	// Verify overlap: streams opened after stream 1 received overlap audio
	opener.mu.Lock()
	defer opener.mu.Unlock()
	for i := 1; i < len(opener.streams); i++ {
		st := opener.streams[i]
		st.mu.Lock()
		chunksCount := len(st.audioChunks)
		st.mu.Unlock()
		if chunksCount == 0 {
			t.Fatalf("stream %d did not receive any bridged audio chunks", i+1)
		}
	}
}

func TestSTT_CancellationAndLateOutputFencing(t *testing.T) {
	st := newMockSTTStream()
	opener := &mockSTTOpener{
		onOpenStream: func(cfg STTConfig) *mockSTTStream {
			return st
		},
	}

	cfg := DefaultSTTConfig()
	cfg.StreamOpener = opener
	proc, _ := NewSTTProcessor(cfg)
	pipe, _ := core.NewPipeline(proc)
	session, _ := core.NewSession(1, pipe, core.DefaultResourceBudget())
	_ = proc.Attach(session, 0)

	var transcripts []string
	var mu sync.Mutex
	session.OnOutput = func(f core.Frame) {
		mu.Lock()
		defer mu.Unlock()
		if ft, ok := f.(*core.FinalTranscriptionFrame); ok {
			transcripts = append(transcripts, ft.Text)
		}
	}

	_ = session.Start()
	defer session.Close()

	_ = session.Post(&core.UserStartedSpeakingFrame{Gen: 1})

	// Advance session generation (e.g. user barge-in or session cancelled)
	session.IncrementGeneration() // Gen = 2
	_ = session.Post(&core.InterruptionFrame{Gen: 1})

	// Now old stream returns a result for old Gen 1
	st.PushResult(&STTResult{Transcript: "stale speech", IsFinal: true})

	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	// Verify the stale transcript was fenced and not delivered to session output
	if len(transcripts) != 0 {
		t.Fatalf("expected 0 transcripts delivered for stale generation, got: %v", transcripts)
	}
}

func TestGoogleSTTEmitsRawCandidatesWithProviderSemantics(t *testing.T) {
	st := newMockSTTStream()
	st.PushResult(&STTResult{Transcript: "unstable", Stability: 0.4})
	st.PushResult(&STTResult{Transcript: "final", IsFinal: true})
	cfg := DefaultSTTConfig()
	cfg.StreamOpener = &mockSTTOpener{onOpenStream: func(STTConfig) *mockSTTStream { return st }}
	cfg.Now = func() time.Time { return time.Unix(30, 0) }
	proc, _ := NewSTTProcessor(cfg)
	sink := &core.PassthroughProcessor{}
	pipe, _ := core.NewPipeline(proc, sink)
	session, _ := core.NewSession(30, pipe, core.DefaultResourceBudget())
	_ = session.Start()
	defer session.Close()
	_ = session.Post(&core.UserStartedSpeakingFrame{Seq: 7, Gen: 1})
	_ = session.Post(&core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1})
	time.Sleep(50 * time.Millisecond)
	var candidates []*core.TranscriptCandidateFrame
	for _, frame := range sink.GetFrames() {
		if c, ok := frame.(*core.TranscriptCandidateFrame); ok {
			candidates = append(candidates, c)
		}
		if _, ok := frame.(*core.FinalTranscriptionFrame); ok {
			t.Fatalf("provider emitted a committed-frame type directly: %T", frame)
		}
	}
	if len(candidates) != 2 || candidates[0].Final || !candidates[0].HasStability || candidates[0].Stability < 0.39 || candidates[0].Stability > 0.41 || !candidates[1].Final {
		t.Fatalf("Google IsFinal/Stability mapping mismatch: %#v", candidates)
	}
}

func TestGoogleSTTAssemblesSequentialFinalsAndInterimFromWholeResponse(t *testing.T) {
	st := newMockSTTStream()
	cfg := DefaultSTTConfig()
	cfg.StreamOpener = &mockSTTOpener{onOpenStream: func(STTConfig) *mockSTTStream { return st }}
	proc, _ := NewSTTProcessor(cfg)
	sink := &core.PassthroughProcessor{}
	pipe, _ := core.NewPipeline(proc, sink)
	session, _ := core.NewSession(32, pipe, core.DefaultResourceBudget())
	_ = session.Start()
	defer session.Close()
	_ = session.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	_ = session.Post(&core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1})
	time.Sleep(10 * time.Millisecond)
	st.PushResults(
		&STTResult{Transcript: "hello", IsFinal: true, ResultEndOffset: time.Second},
		&STTResult{Transcript: "world", IsFinal: true, ResultEndOffset: 2 * time.Second},
		&STTResult{Transcript: "again", Stability: .9, ResultEndOffset: 2500 * time.Millisecond},
	)
	time.Sleep(30 * time.Millisecond)
	var got []string
	for _, frame := range sink.GetFrames() {
		if candidate, ok := frame.(*core.TranscriptCandidateFrame); ok {
			got = append(got, candidate.Text)
		}
	}
	want := []string{"hello", "hello world", "hello world again"}
	if len(got) != len(want) {
		t.Fatalf("batch results lost: got=%v want=%v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("result %d: got %q want %q", i, got[i], want[i])
		}
	}
}

func TestGoogleSTTReconcilesRotationOverlapByOffset(t *testing.T) {
	proc, _ := NewSTTProcessor(DefaultSTTConfig())
	old := &activeStream{turnID: "A", gen: 1}
	rotated := &activeStream{turnID: "A", gen: 1, offsetBase: 1500 * time.Millisecond}
	proc.mu.Lock()
	first := proc.assembleLocked(old, &STTResult{Transcript: "hello world", IsFinal: true, ResultEndOffset: 2 * time.Second})
	overlap := proc.assembleLocked(rotated, &STTResult{Transcript: "world", IsFinal: true, ResultEndOffset: 500 * time.Millisecond})
	proc.mu.Unlock()
	if first != "hello world" || overlap != "hello world" {
		t.Fatalf("overlap duplicated/lost text: first=%q overlap=%q", first, overlap)
	}
}

func TestGoogleSTTStreamOwnershipIsImmutableAcrossTurns(t *testing.T) {
	opener := &mockSTTOpener{}
	cfg := DefaultSTTConfig()
	cfg.StreamOpener = opener
	proc, _ := NewSTTProcessor(cfg)
	sink := &core.PassthroughProcessor{}
	pipe, _ := core.NewPipeline(proc, sink)
	session, _ := core.NewSession(33, pipe, core.DefaultResourceBudget())
	_ = session.Start()
	defer session.Close()
	openTurn := func(seq, gen uint64) *mockSTTStream {
		_ = session.Post(&core.UserStartedSpeakingFrame{Seq: seq, Gen: gen})
		_ = session.Post(&core.InputAudioFrame{Samples: make([]byte, 640), Gen: gen})
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			opener.mu.Lock()
			n := len(opener.streams)
			var st *mockSTTStream
			if n > 0 {
				st = opener.streams[n-1]
			}
			opener.mu.Unlock()
			if n >= int(seq) {
				return st
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatalf("stream %d did not open", seq)
		return nil
	}
	a := openTurn(1, 1)
	b := openTurn(2, 1)
	c := openTurn(3, 2)
	a.PushResult(&STTResult{Transcript: "late A", IsFinal: true, ResultEndOffset: time.Second})
	b.PushResult(&STTResult{Transcript: "turn B", IsFinal: true, ResultEndOffset: time.Second})
	c.PushResult(&STTResult{Transcript: "turn C", IsFinal: true, ResultEndOffset: time.Second})
	time.Sleep(30 * time.Millisecond)
	seen := map[string]string{}
	for _, frame := range sink.GetFrames() {
		if c, ok := frame.(*core.TranscriptCandidateFrame); ok {
			seen[c.Text] = string(c.Turn)
		}
	}
	if seen["late A"] != "turn-1-1" || seen["turn B"] != "turn-1-2" || seen["turn C"] != "turn-2-3" {
		t.Fatalf("stream result relabeled: %#v", seen)
	}
}
