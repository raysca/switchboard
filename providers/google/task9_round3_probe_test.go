package google

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/coretest"
	coretranscript "govoice/core/transcript"
)

func TestRound3GoogleBargeInTranscriptActuallyCommits(t *testing.T) {
	cfg := DefaultSTTConfig()
	opener := &mockSTTOpener{}
	cfg.StreamOpener = opener
	p, _ := NewSTTProcessor(cfg)
	fc := coretranscript.DefaultConfig()
	fc.GracePeriod = 5 * time.Millisecond
	fc.FinalizationTimeout = 20 * time.Millisecond
	finalizer, _ := coretranscript.NewProcessor(fc)
	pool := core.NewConversationContext(core.ContextConfig{MaxMessages: 10, MaxTokens: 1000})
	user := core.NewUserContextProcessor(pool, core.UserContextConfig{RequireCommittedTranscript: true})
	assistant := core.NewAssistantContextProcessor(pool, core.DefaultAssistantContextConfig())
	sink := &coretest.PassthroughProcessor{}
	pipe, _ := core.NewPipeline(p, finalizer, user, assistant, sink)
	s, _ := core.NewSession(998, pipe, core.DefaultResourceBudget())
	s.Start()
	defer s.Close()
	s.Post(&core.UserMessageFrame{Turn: "bot", Gen: 1, Content: "prior"})
	s.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	until := time.Now().Add(time.Second)
	for s.CurrentGeneration() != 2 && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	if s.CurrentGeneration() != 2 {
		t.Fatal("no barge-in")
	}
	s.Post(&core.InputAudioFrame{Samples: make([]byte, 640), Gen: 2})
	var st *mockSTTStream
	for st == nil && time.Now().Before(until) {
		opener.mu.Lock()
		if len(opener.streams) > 0 {
			st = opener.streams[len(opener.streams)-1]
		}
		opener.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	if st == nil {
		t.Fatal("no stream")
	}
	st.PushResult(&STTResult{Transcript: "new user", IsFinal: true, ResultEndOffset: time.Second})
	s.Post(&core.UserStoppedSpeakingFrame{Seq: 2, Gen: 2})
	time.Sleep(100 * time.Millisecond)
	for _, m := range pool.Messages() {
		if m.Content == "new user" {
			return
		}
	}
	t.Fatalf("generation-2 STT candidate never committed after VAD stop; messages=%v err=%v", pool.Messages(), s.Err())
}

func TestRound3GoogleMultiInterimResponseTimeoutIsComplete(t *testing.T) {
	p, _ := NewSTTProcessor(DefaultSTTConfig())
	st := &activeStream{turnID: "A", gen: 1, id: 1}
	fcfg := coretranscript.DefaultConfig()
	f, _ := coretranscript.NewFinalizer(fcfg)
	t0 := time.Unix(100, 0)
	for i, r := range []*STTResult{{Transcript: "to be", Stability: .9, ResultEndOffset: time.Second}, {Transcript: "or not to be", Stability: .1, ResultEndOffset: 2 * time.Second}} {
		text, err := p.assembleResultLocked(st, r)
		if err != nil {
			t.Fatal(err)
		}
		f.Observe(coretranscript.Candidate{Turn: "A", Generation: 1, Text: text, At: t0.Add(time.Duration(i) * time.Millisecond), Stability: float64(r.Stability), HasStability: true})
	}
	f.Complete("A", 1, t0.Add(10*time.Millisecond))
	out, err := f.Advance(t0.Add(3 * time.Second))
	if err != nil || len(out) != 1 || out[0].Text != "to be or not to be" {
		t.Fatalf("consecutive interim portions lost at timeout: out=%v err=%v", out, err)
	}
}

func TestRound3GoogleAssemblyErrorCannotLaterCommitPartial(t *testing.T) {
	p, _ := NewSTTProcessor(DefaultSTTConfig())
	st := &activeStream{turnID: "A", gen: 1, id: 1}
	first, err := p.assembleResultLocked(st, &STTResult{Transcript: strings.Repeat("x", 17000), IsFinal: true, ResultEndOffset: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.assembleResultLocked(st, &STTResult{Transcript: strings.Repeat("y", 17000), IsFinal: true, ResultEndOffset: 2 * time.Second})
	if err != ErrTranscriptAssemblyCapacity {
		t.Fatalf("capacity err=%v", err)
	}
	fc := coretranscript.DefaultConfig()
	fc.GracePeriod = 0
	fp, _ := coretranscript.NewProcessor(fc)
	sink := &coretest.PassthroughProcessor{}
	pipe, _ := core.NewPipeline(fp, sink)
	s, _ := core.NewSession(996, pipe, core.DefaultResourceBudget())
	s.Start()
	defer s.Close()
	s.Post(&core.TranscriptCandidateFrame{Turn: "A", Gen: 1, Text: first, Final: true, Timestamp: time.Now()})
	s.Post(&core.ErrorFrame{Err: err})
	s.Post(&core.TurnEventFrame{Kind: core.TurnEventCompleted, Turn: "A", Gen: 1, At: time.Now()})
	time.Sleep(20 * time.Millisecond)
	for _, fr := range sink.GetFrames() {
		if c, ok := fr.(*core.FinalTranscriptionFrame); ok {
			t.Fatalf("assembly overflow still committed truncated text (%d bytes)", len(c.Text))
		}
	}
}

func TestRound3GoogleAutomaticRotationDoesNotDuplicateCurrentAudio(t *testing.T) {
	var nowNanos atomic.Int64
	nowNanos.Store(time.Unix(1, 0).UnixNano())
	cfg := DefaultSTTConfig()
	cfg.Now = func() time.Time { return time.Unix(0, nowNanos.Load()) }
	cfg.StreamRotationThreshold = time.Second
	opener := &mockSTTOpener{}
	cfg.StreamOpener = opener
	p, _ := NewSTTProcessor(cfg)
	pipe, _ := core.NewPipeline(p, &coretest.PassthroughProcessor{})
	s, _ := core.NewSession(995, pipe, core.DefaultResourceBudget())
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_ = s.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	_ = s.Post(&core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1})
	// The first stream opens on a session task; rotation needs it attached.
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		for deadline := time.Now().Add(2 * time.Second); !cond(); time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal(what)
			}
		}
	}
	waitFor("first stream never attached", func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.active != nil && p.active.stream != nil
	})
	nowNanos.Add(int64(2 * time.Second))
	_ = s.Post(&core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1})
	waitFor("rotation not triggered", func() bool {
		opener.mu.Lock()
		defer opener.mu.Unlock()
		return len(opener.streams) == 2
	})
	opener.mu.Lock()
	second := opener.streams[1]
	opener.mu.Unlock()
	second.mu.Lock()
	total := 0
	for _, b := range second.audioChunks {
		total += len(b)
	}
	second.mu.Unlock()
	if total != 1280 {
		t.Fatalf("new stream contains repeated current audio: got %d bytes want 1280", total)
	}
}
