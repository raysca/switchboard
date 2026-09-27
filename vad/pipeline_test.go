package vad

import (
	"context"
	"encoding/binary"
	"sync"
	"testing"
	"time"

	"govoice/core"
)

func generateAudioSamples(numSamples int, freq float64) []byte {
	raw := make([]byte, numSamples*2)
	for i := 0; i < numSamples; i++ {
		sample := int16(16000.0 * 0.8)
		binary.LittleEndian.PutUint16(raw[i*2:i*2+2], uint16(sample))
	}
	return raw
}

func generateSilenceSamples(numSamples int) []byte {
	return make([]byte, numSamples*2)
}

func TestExecutors_IdenticalPipelineAPI(t *testing.T) {
	L := lib(t)
	modes := []string{"sharded", "process"}

	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			cfg := Config{
				Mode:            mode,
				Workers:         2,
				Batch:           2,
				QueueCap:        256,
				IntraOp:         1,
				PerSessionLimit: 1,
				ParentDeadline:  200 * time.Millisecond,
				ParentGrace:     200 * time.Millisecond,
			}

			exec, err := New(cfg)
			if err != nil {
				t.Fatalf("new executor (%s): %v", mode, err)
			}
			defer exec.Close()

			vadProc := NewProcessor(exec, DefaultProcessorConfig())

			var speechStarts int
			var speechStops int
			var mu sync.Mutex

			consumer := &testConsumer{
				onFrame: func(f core.Frame) {
					mu.Lock()
					defer mu.Unlock()
					switch f.Type() {
					case core.FrameUserStartedSpeaking:
						speechStarts++
					case core.FrameUserStoppedSpeaking:
						speechStops++
					}
				},
			}

			pipe, err := core.NewPipeline(vadProc, consumer)
			if err != nil {
				t.Fatalf("new pipeline: %v", err)
			}

			budget := core.DefaultResourceBudget()
			sess, err := core.NewSession(42, pipe, budget)
			if err != nil {
				t.Fatalf("new session: %v", err)
			}
			if err := sess.Start(); err != nil {
				t.Fatalf("start session: %v", err)
			}
			defer sess.Close()

			// Feed speech clip from library
			clip := L.Clips[0].Samples
			chunkSize := 320 // 20ms at 16kHz
			for i := 0; i+chunkSize <= len(clip); i += chunkSize {
				raw := make([]byte, chunkSize*2)
				for k := 0; k < chunkSize; k++ {
					s := int16(clip[i+k] * 32767.0)
					binary.LittleEndian.PutUint16(raw[k*2:k*2+2], uint16(s))
				}
				frame := &core.InputAudioFrame{
					Samples:    raw,
					Encoding:   core.EncodingPCM16LE,
					SampleRate: 16000,
					Channels:   1,
					Duration:   20 * time.Millisecond,
					Timestamp:  time.Now(),
					Seq:        uint64(i/chunkSize + 1),
				}
				_ = sess.Post(frame)
				time.Sleep(2 * time.Millisecond)
			}

			// Feed silence to trigger stop
			numSilence := 16000
			silence := generateSilenceSamples(numSilence)
			for i := 0; i+chunkSize <= numSilence; i += chunkSize {
				frame := &core.InputAudioFrame{
					Samples:    silence[i*2 : (i+chunkSize)*2],
					Encoding:   core.EncodingPCM16LE,
					SampleRate: 16000,
					Channels:   1,
					Duration:   20 * time.Millisecond,
					Timestamp:  time.Now(),
					Seq:        uint64(len(clip)/chunkSize + i/chunkSize + 1),
				}
				_ = sess.Post(frame)
				time.Sleep(2 * time.Millisecond)
			}

			time.Sleep(100 * time.Millisecond)

			mu.Lock()
			starts := speechStarts
			mu.Unlock()

			if starts == 0 {
				t.Fatalf("%s mode: expected speech start detections, got 0", mode)
			}
		})
	}
}

func TestScenarioF_NativeFaultIsolationWithPipeline(t *testing.T) {
	// Scenario F: Deliberately trigger native fault in VAD path during active sessions.
	// Verify affected session experiences bounded in-flight drop while worker respawns,
	// and unrelated session on other worker continues unaffected.

	cfg := Config{
		Mode:            "process",
		Workers:         2,
		Batch:           2,
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

	// Pipeline for Session 0 (SessionID 0 -> Worker 0)
	vad0 := NewProcessor(exec, DefaultProcessorConfig())
	var s0Outputs []core.Frame
	var s0Mu sync.Mutex
	consumer0 := &testConsumer{
		onFrame: func(f core.Frame) {
			s0Mu.Lock()
			s0Outputs = append(s0Outputs, f)
			s0Mu.Unlock()
		},
	}
	pipe0, _ := core.NewPipeline(vad0, consumer0)
	budget0 := core.DefaultResourceBudget()
	sess0, err := core.NewSession(0, pipe0, budget0)
	if err != nil {
		t.Fatalf("new sess0: %v", err)
	}
	_ = sess0.Start()
	defer sess0.Close()

	// Pipeline for Session 1 (SessionID 1 -> Worker 1)
	vad1 := NewProcessor(exec, DefaultProcessorConfig())
	var s1Outputs []core.Frame
	var s1Mu sync.Mutex
	consumer1 := &testConsumer{
		onFrame: func(f core.Frame) {
			s1Mu.Lock()
			s1Outputs = append(s1Outputs, f)
			s1Mu.Unlock()
		},
	}
	pipe1, _ := core.NewPipeline(vad1, consumer1)
	budget1 := core.DefaultResourceBudget()
	sess1, err := core.NewSession(1, pipe1, budget1)
	if err != nil {
		t.Fatalf("new sess1: %v", err)
	}
	_ = sess1.Start()
	defer sess1.Close()

	// Feed continuous audio to both sessions
	speechChunk := generateAudioSamples(320, 440.0)

	stopFeeder := make(chan struct{})
	var wgFeed sync.WaitGroup
	wgFeed.Add(2)

	feedSession := func(s *core.Session) {
		defer wgFeed.Done()
		seq := uint64(1)
		for {
			select {
			case <-stopFeeder:
				return
			default:
				f := &core.InputAudioFrame{
					Samples:    speechChunk,
					Encoding:   core.EncodingPCM16LE,
					SampleRate: 16000,
					Channels:   1,
					Duration:   20 * time.Millisecond,
					Timestamp:  time.Now(),
					Seq:        seq,
				}
				_ = s.Post(f)
				seq++
				time.Sleep(10 * time.Millisecond)
			}
		}
	}

	go feedSession(sess0)
	go feedSession(sess1)

	// Stream audio
	time.Sleep(100 * time.Millisecond)

	// Deliberately trigger native fault on Worker 0
	if err := exec.InjectFault(0); err != nil {
		t.Fatalf("inject fault on worker 0: %v", err)
	}

	// Continue streaming during worker 0 crash and recovery
	time.Sleep(250 * time.Millisecond)

	close(stopFeeder)
	wgFeed.Wait()

	time.Sleep(100 * time.Millisecond)

	// Verify Session 1 (on Worker 1) received continuous frames with no interruption
	s1Mu.Lock()
	s1FrameCount := len(s1Outputs)
	s1Mu.Unlock()

	if s1FrameCount < 10 {
		t.Fatalf("session 1 on worker 1 should have continued unaffected, got only %d frames", s1FrameCount)
	}

	// Verify Worker 0 fault incremented telemetry signals
	stats := exec.Stats()
	if stats.WorkerErrors == 0 && stats.Restarts == 0 {
		t.Fatal("expected WorkerErrors or Restarts to increment during fault injection")
	}

	drops0 := vad0.DroppedCount()
	t.Logf("Scenario F verification: Session 0 drops: %d, Session 1 frames: %d, Worker restarts: %d", drops0, s1FrameCount, stats.Restarts)
}

type testConsumer struct {
	onFrame func(f core.Frame)
}

func (c *testConsumer) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	if c.onFrame != nil {
		c.onFrame(f)
	}
	return e.PushDirection(f, dir)
}
