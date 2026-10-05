//go:build xaispeech

package xai

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"govoice/core"
)

type mockTTSClient struct {
	mu      sync.Mutex
	pcmData []byte
	err     error
	calls   int
}

func (m *mockTTSClient) Synthesize(ctx context.Context, text string, cfg TTSConfig) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.err != nil {
		return nil, m.err
	}
	return m.pcmData, nil
}

func TestXAITTS_Success(t *testing.T) {
	// 1920 bytes = exactly two 960-byte (20ms @ 24kHz) chunks
	rawPCM := make([]byte, 1920)
	for i := range rawPCM {
		rawPCM[i] = byte(i % 256)
	}

	mockClient := &mockTTSClient{pcmData: rawPCM}
	cfg := DefaultTTSConfig()
	cfg.Client = mockClient

	proc, err := NewTTSProcessor(cfg)
	if err != nil {
		t.Fatalf("new tts processor: %v", err)
	}

	sink := &core.PassthroughProcessor{}
	pipe, err := core.NewPipeline(proc, sink)
	if err != nil {
		t.Fatalf("new pipeline: %v", err)
	}

	session, err := core.NewSession(1, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	if err := session.Start(); err != nil {
		t.Fatalf("start session: %v", err)
	}
	defer session.Close()

	unit := &core.SynthesisUnitFrame{
		Text:    "Hello world",
		Turn:    "turn-1",
		Gen:     1,
		UnitID:  101,
		IsFinal: true,
	}

	if err := session.Post(unit); err != nil {
		t.Fatalf("post unit: %v", err)
	}

	deadline := time.After(2 * time.Second)
	var audioFrames []*core.OutputAudioFrame

	for len(audioFrames) < 2 {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for 2 audio frames, got %d", len(audioFrames))
		default:
			audioFrames = nil
			for _, f := range sink.GetFrames() {
				if af, ok := f.(*core.OutputAudioFrame); ok {
					audioFrames = append(audioFrames, af)
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	if len(audioFrames) != 2 {
		t.Fatalf("expected 2 audio frames, got %d", len(audioFrames))
	}

	if audioFrames[0].IsFinal {
		t.Error("frame 0 should not be final")
	}
	if !audioFrames[1].IsFinal {
		t.Error("frame 1 should be final")
	}
	if audioFrames[0].TurnFinal || !audioFrames[1].TurnFinal {
		t.Errorf("whole-turn final marker = [%v %v], want [false true]", audioFrames[0].TurnFinal, audioFrames[1].TurnFinal)
	}
	if len(audioFrames[0].Samples) != 960 {
		t.Errorf("expected 960 bytes in frame 0, got %d", len(audioFrames[0].Samples))
	}
	if len(audioFrames[1].Samples) != 960 {
		t.Errorf("expected 960 bytes in frame 1, got %d", len(audioFrames[1].Samples))
	}
}

func TestXAITTS_MultiUnitAudibleThenEmptyFinal(t *testing.T) {
	mockClient := &mockTTSClient{pcmData: make([]byte, 960)}
	cfg := DefaultTTSConfig()
	cfg.Client = mockClient
	proc, err := NewTTSProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sink := &core.PassthroughProcessor{}
	pipe, _ := core.NewPipeline(proc, sink)
	session, _ := core.NewSession(11, pipe, core.DefaultResourceBudget())
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err := session.Post(&core.SynthesisUnitFrame{Text: "audible", Turn: "turn", Gen: 1, UnitID: 1}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for proc.AudioFrames.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	mockClient.mu.Lock()
	mockClient.pcmData = nil
	mockClient.mu.Unlock()
	if err := session.Post(&core.SynthesisUnitFrame{Text: "", Turn: "turn", Gen: 1, UnitID: 2, IsFinal: true}); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		for _, frame := range sink.GetFrames() {
			if audio, ok := frame.(*core.OutputAudioFrame); ok && audio.UnitID == 2 {
				if len(audio.Samples) != 0 || !audio.IsFinal || !audio.TurnFinal {
					t.Fatalf("empty final=%#v", audio)
				}
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("xAI did not emit empty whole-turn final output")
}

func TestXAITTS_StaleGenerationDrop(t *testing.T) {
	mockClient := &mockTTSClient{pcmData: make([]byte, 960)}
	cfg := DefaultTTSConfig()
	cfg.Client = mockClient

	proc, err := NewTTSProcessor(cfg)
	if err != nil {
		t.Fatalf("new tts processor: %v", err)
	}

	sink := &core.PassthroughProcessor{}
	pipe, err := core.NewPipeline(proc, sink)
	if err != nil {
		t.Fatalf("new pipeline: %v", err)
	}

	session, err := core.NewSession(1, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	if err := session.Start(); err != nil {
		t.Fatalf("start session: %v", err)
	}
	defer session.Close()

	// Advance session generation to 2
	session.IncrementGeneration()

	// Post unit from generation 1 (stale)
	_ = session.Post(&core.SynthesisUnitFrame{
		Text:   "Old generation",
		Turn:   "turn-1",
		Gen:    1,
		UnitID: 100,
	})

	time.Sleep(100 * time.Millisecond)

	mockClient.mu.Lock()
	calls := mockClient.calls
	mockClient.mu.Unlock()

	if calls != 0 {
		t.Errorf("expected 0 synthesis calls for stale generation, got %d", calls)
	}
}

func TestXAITTS_ErrorHandling(t *testing.T) {
	mockClient := &mockTTSClient{err: errors.New("simulated synthesis error")}
	cfg := DefaultTTSConfig()
	cfg.Client = mockClient
	cfg.RetryPolicy.MaxAttempts = 1

	proc, err := NewTTSProcessor(cfg)
	if err != nil {
		t.Fatalf("new tts processor: %v", err)
	}

	sink := &core.PassthroughProcessor{}
	pipe, err := core.NewPipeline(proc, sink)
	if err != nil {
		t.Fatalf("new pipeline: %v", err)
	}

	session, err := core.NewSession(1, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	if err := session.Start(); err != nil {
		t.Fatalf("start session: %v", err)
	}
	defer session.Close()

	_ = session.Post(&core.SynthesisUnitFrame{
		Text:   "Failing text",
		Turn:   "turn-1",
		Gen:    1,
		UnitID: 102,
	})

	deadline := time.After(2 * time.Second)
	var errSeen bool

	for !errSeen {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for error frame")
		default:
			for _, f := range sink.GetFrames() {
				if ef, ok := f.(*core.ErrorFrame); ok && ef.Err != nil {
					errSeen = true
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	if !errSeen {
		t.Error("expected error frame emitted downstream")
	}
}

func TestXAITTS_MultiSentenceQueue_FIFO(t *testing.T) {
	rawPCM := make([]byte, 960) // 1 frame
	mockClient := &mockTTSClient{pcmData: rawPCM}
	cfg := DefaultTTSConfig()
	cfg.Client = mockClient

	proc, err := NewTTSProcessor(cfg)
	if err != nil {
		t.Fatalf("new tts processor: %v", err)
	}

	sink := &core.PassthroughProcessor{}
	pipe, err := core.NewPipeline(proc, sink)
	if err != nil {
		t.Fatalf("new pipeline: %v", err)
	}

	session, err := core.NewSession(2, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	if err := session.Start(); err != nil {
		t.Fatalf("start session: %v", err)
	}
	defer session.Close()

	// Post sentence 1 and sentence 2 rapidly in succession
	_ = session.Post(&core.SynthesisUnitFrame{
		Text:    "First sentence.",
		Turn:    "turn-1",
		Gen:     1,
		UnitID:  1,
		IsFinal: false,
	})
	_ = session.Post(&core.SynthesisUnitFrame{
		Text:    "Second sentence.",
		Turn:    "turn-1",
		Gen:     1,
		UnitID:  2,
		IsFinal: true,
	})

	deadline := time.After(2 * time.Second)
	var unitIDsSeen []uint64

	for len(unitIDsSeen) < 2 {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for both units to complete: saw unitIDs %v", unitIDsSeen)
		default:
			unitIDsSeen = nil
			for _, f := range sink.GetFrames() {
				if af, ok := f.(*core.OutputAudioFrame); ok {
					if len(unitIDsSeen) == 0 || unitIDsSeen[len(unitIDsSeen)-1] != af.UnitID {
						unitIDsSeen = append(unitIDsSeen, af.UnitID)
					}
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	if len(unitIDsSeen) != 2 || unitIDsSeen[0] != 1 || unitIDsSeen[1] != 2 {
		t.Fatalf("expected units [1, 2] in FIFO order, got: %v", unitIDsSeen)
	}

	mockClient.mu.Lock()
	calls := mockClient.calls
	mockClient.mu.Unlock()
	if calls != 2 {
		t.Fatalf("expected both sentences synthesized (calls=2), got %d", calls)
	}
}
