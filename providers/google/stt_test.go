package google

import (
	"errors"
	"sync"
	"testing"
	"time"

	"govoice/core"
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

	pipe, _ := core.NewPipeline(proc)
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

	pipe, _ := core.NewPipeline(proc)
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

	time.Sleep(100 * time.Millisecond)

	rotations := proc.RotationsCount.Load()
	if rotations < 5 {
		t.Fatalf("expected at least 5 stream rotations, got %d", rotations)
	}

	mu.Lock()
	defer mu.Unlock()

	// Verify deduplication: despite Stream 1 and Stream 2 both returning "hello world",
	// exactly 1 FinalTranscriptionFrame was emitted and 0 duplicate turns!
	if len(finals) != 1 {
		t.Fatalf("expected exactly 1 deduplicated final transcript, got %d: %v", len(finals), finals)
	}
	if finals[0] != "hello world" {
		t.Fatalf("unexpected transcript: %s", finals[0])
	}
	if proc.DeduplicatedResults.Load() < 1 {
		t.Fatalf("expected at least 1 deduplicated result recorded in metrics, got %d", proc.DeduplicatedResults.Load())
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
