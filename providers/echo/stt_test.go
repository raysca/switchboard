package echo

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"govoice/core"
	"govoice/providers/google"
)

var _ google.STTStreamOpener = (*STTOpener)(nil)

func loud() []byte {
	b := make([]byte, 640)
	for i := 0; i+1 < len(b); i += 2 {
		b[i], b[i+1] = 0x00, 0x40 // 16384
	}
	return b
}

func open(t *testing.T, f STTFigures, audio bool) google.STTStream {
	t.Helper()
	s, err := NewSTTOpener(f, audio).OpenStream(context.Background(), google.DefaultSTTConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func recvAll(t *testing.T, s google.STTStream) (out []*google.STTResult, err error) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			rs, e := s.Recv()
			if e != nil {
				err = e
				return
			}
			out = append(out, rs...)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Recv did not finish")
	}
	return out, err
}

func TestSTTCountsVoicedFramesIntoWords(t *testing.T) {
	s := open(t, STTFigures{FramesPerWord: 4, VoicedThreshold: 500}, true)
	for i := 0; i < 8; i++ {
		_ = s.Send(loud())
	}
	for i := 0; i < 8; i++ {
		_ = s.Send(make([]byte, 640)) // silence is not speech
	}
	_ = s.CloseSend()
	rs, err := recvAll(t, s)
	if err != io.EOF || len(rs) != 1 || !rs[0].IsFinal || rs[0].Transcript != "hello hello" {
		t.Fatalf("results %+v err %v", rs, err)
	}
}

func TestSTTInstantCountsEveryFrame(t *testing.T) {
	s := open(t, STTFigures{FramesPerWord: 1}, true)
	for i := 0; i < 3; i++ {
		_ = s.Send(make([]byte, 640))
	}
	_ = s.CloseSend()
	rs, _ := recvAll(t, s)
	if len(rs) != 1 || rs[0].Transcript != "hello hello hello" {
		t.Fatalf("results %+v", rs)
	}
}

func TestSTTInterimsAreCumulativeWithMediaOffsets(t *testing.T) {
	s := open(t, STTFigures{Interims: true, FramesPerWord: 2}, true)
	for i := 0; i < 4; i++ {
		_ = s.Send(loud())
	}
	_ = s.CloseSend()
	rs, _ := recvAll(t, s)
	if len(rs) != 3 || rs[0].IsFinal || rs[0].Transcript != "hello" || rs[1].Transcript != "hello hello" || !rs[2].IsFinal {
		t.Fatalf("results %+v", rs)
	}
	if rs[0].ResultEndOffset != 40*time.Millisecond || rs[1].ResultEndOffset != 80*time.Millisecond {
		t.Fatalf("offsets %v %v, want 40ms and 80ms of media", rs[0].ResultEndOffset, rs[1].ResultEndOffset)
	}
}

func TestSTTNoInterimsOnlyAFinalAfterCloseSend(t *testing.T) {
	s := open(t, STTFigures{FinalDelay: 60 * time.Millisecond, FramesPerWord: 1}, true)
	_ = s.Send(loud())
	_ = s.CloseSend()
	start := time.Now()
	rs, err := recvAll(t, s)
	if len(rs) != 1 || !rs[0].IsFinal || err != io.EOF {
		t.Fatalf("results %+v err %v", rs, err)
	}
	if d := time.Since(start); d < 60*time.Millisecond {
		t.Fatalf("final after %v, want >= 60ms", d)
	}
}

func TestSTTTextModeNeverTranscribes(t *testing.T) {
	s := open(t, STTFigures{FramesPerWord: 1}, false)
	for i := 0; i < 5; i++ {
		_ = s.Send(loud())
	}
	_ = s.CloseSend()
	if rs, err := recvAll(t, s); len(rs) != 0 || err != io.EOF {
		t.Fatalf("text mode produced %+v err %v", rs, err)
	}
}

func TestSTTRecvReturnsOnCancelAndClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s, _ := NewSTTOpener(STTFigures{FramesPerWord: 1}, true).OpenStream(ctx, google.DefaultSTTConfig())
	errc := make(chan error, 1)
	go func() { _, err := s.Recv(); errc <- err }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Recv after cancel: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Recv blocked after cancel")
	}
	s2 := open(t, STTFigures{FramesPerWord: 1}, true)
	go func() { time.Sleep(20 * time.Millisecond); _ = s2.Close() }()
	if _, err := s2.Recv(); err == nil {
		t.Fatal("Recv after Close should fail")
	}
}

func TestSTTQueueStaysBounded(t *testing.T) {
	s := open(t, STTFigures{Interims: true, FramesPerWord: 1}, true).(*sttStream)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 10000; i++ {
			_ = s.Send(loud())
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Send blocked on a full queue")
	}
	if n := len(s.results); n > cap(s.results) {
		t.Fatalf("queue %d over capacity %d", n, cap(s.results))
	}
}

func TestRealSTTProcessorTranscribesThroughEcho(t *testing.T) {
	cfg := google.DefaultSTTConfig()
	cfg.Provider = "echo"
	cfg.StreamOpener = NewSTTOpener(STTFigures{FinalDelay: 50 * time.Millisecond, FramesPerWord: 1}, true)
	proc, err := google.NewSTTProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sink := &core.PassthroughProcessor{}
	pipe, _ := core.NewPipeline(proc, sink)
	sess, _ := core.NewSession(1, pipe, core.DefaultResourceBudget())
	if err := sess.Start(); err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	_ = sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	_ = sess.Post(&core.InputAudioFrame{Samples: loud(), Gen: 1})
	_ = sess.Post(&core.InputAudioFrame{Samples: loud(), Gen: 1})
	_ = sess.Post(&core.UserStoppedSpeakingFrame{Gen: 1})
	var final *core.TranscriptCandidateFrame
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end) && final == nil; time.Sleep(10 * time.Millisecond) {
		for _, f := range sink.GetFrames() {
			if c, ok := f.(*core.TranscriptCandidateFrame); ok && c.Final {
				final = c
			}
		}
	}
	if final == nil || final.Text != "hello hello" || final.Provider != "echo" {
		t.Fatalf("final %+v", final)
	}
}

func TestSTTWordsStopAtTheCap(t *testing.T) {
	s := open(t, STTFigures{FramesPerWord: 1}, true).(*sttStream)
	for i := 0; i < maxWords+100; i++ {
		_ = s.Send(loud())
	}
	if len(s.words) != maxWords {
		t.Fatalf("%d words, want the cap %d", len(s.words), maxWords)
	}
}

func TestSTTNeverAnswersAfterClose(t *testing.T) {
	s := open(t, STTFigures{FramesPerWord: 1}, true)
	_ = s.Send(loud())
	_ = s.CloseSend()
	_ = s.Close() // interrupted: a closed stream must not hand back the transcript it had
	for i := 0; i < 50; i++ {
		if rs, err := s.Recv(); len(rs) != 0 || err == nil {
			t.Fatalf("Recv after Close returned %+v, %v", rs, err)
		}
	}
}
