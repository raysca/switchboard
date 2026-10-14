//go:build xaispeech

package xai

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/coretest"
	coretranscript "govoice/core/transcript"
	coreturn "govoice/core/turn"
)

// Agent-level repro of the live S2/S4 findings: the real xAI STT adapter, turn processor
// (manager + min_words 3), transcript finalizer, user and assistant context (barge-in on the
// turn event) driven in real time by 20 ms audio frames. The fake stream follows xAI's cadence
// as captured live: transcript.created on open, per-utterance interims about every 0.5 s,
// speech_final only on audio.done (its own endpointing is slower than our VAD stop), then an
// empty transcript.done. Audio carries a word index per frame so the fake can tell what it heard.

var agentWords = []string{"", "I", "would", "like", "to", "know", "what", "the", "capital", "of", "France", "is."}

type cadenceStream struct {
	id     int
	out    chan *STTMessage
	quit   chan struct{}
	once   sync.Once
	mu     sync.Mutex
	done   bool
	words  []string
	last   byte
	frames int
	heard  []string // every word received, for the diagnostics
	late   int      // audio frames after audio.done
	closed bool
}

func (s *cadenceStream) SendAudio(_ context.Context, b []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		s.late++
		return nil
	}
	if b[0] == 0 {
		s.last = 0
		return nil
	}
	if b[1] != s.last {
		s.words = append(s.words, agentWords[b[1]])
		s.heard = append(s.heard, agentWords[b[1]])
		s.last = b[1]
	}
	if s.frames++; s.frames%25 == 0 {
		s.out <- &STTMessage{Type: "transcript.partial", Text: strings.Join(s.words, " ")}
	}
	return nil
}

func (s *cadenceStream) CloseSend(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.done = true
	text := strings.Join(s.words, " ")
	time.AfterFunc(300*time.Millisecond, func() { // live: the final lands 0.2-0.4 s after audio.done
		if text != "" {
			s.out <- &STTMessage{Type: "transcript.partial", Text: text, IsFinal: true, SpeechFinal: true}
		}
		s.out <- &STTMessage{Type: "transcript.done"}
	})
	return nil
}

func (s *cadenceStream) Recv(ctx context.Context) (*STTMessage, error) {
	select {
	case m := <-s.out:
		return m, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.quit:
		return nil, io.EOF
	}
}

func (s *cadenceStream) Close() error {
	s.once.Do(func() { close(s.quit) })
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return nil
}

type cadenceOpener struct {
	mu      sync.Mutex
	streams []*cadenceStream
}

func (o *cadenceOpener) OpenStream(context.Context, STTConfig) (STTStream, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	s := &cadenceStream{id: len(o.streams) + 1, out: make(chan *STTMessage, 512), quit: make(chan struct{})}
	s.out <- &STTMessage{Type: "transcript.created"}
	o.streams = append(o.streams, s)
	return s, nil
}

func (o *cadenceOpener) report() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	var b strings.Builder
	for _, s := range o.streams {
		s.mu.Lock()
		fmt.Fprintf(&b, "stream %d heard %q (speech frames %d, late frames %d, closed %v)\n", s.id, s.heard, s.frames, s.late, s.closed)
		s.mu.Unlock()
	}
	return b.String()
}

type flowHarness struct {
	t      *testing.T
	sess   *core.Session
	op     *cadenceOpener
	asst   *core.AssistantContextProcessor
	sink   *coretest.PassthroughProcessor
	vadOn  bool
	speech int // consecutive speech frames
	quiet  int // consecutive silence frames
	seq    uint64
}

func newAgentHarness(t *testing.T) *flowHarness {
	t.Helper()
	h := &flowHarness{t: t, op: &cadenceOpener{}, sink: &coretest.PassthroughProcessor{}}
	cfg := DefaultSTTConfig()
	cfg.StreamOpener = h.op
	stt, _ := NewSTTProcessor(cfg)
	mc := coreturn.DefaultConfig()
	mc.StartStrategies = []coreturn.UserTurnStartStrategy{coreturn.MinWordsStart(3, true)}
	tp, err := coreturn.NewProcessor(mc, coreturn.ProcessorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	fin, _ := coretranscript.NewProcessor(coretranscript.DefaultConfig())
	fin.ManageTurns()
	ctxPool := core.NewConversationContext(core.ContextConfig{MaxMessages: 20, MaxTokens: 4000})
	user := core.NewUserContextProcessor(ctxPool, core.UserContextConfig{TurnPrefix: "turn", RequireCommittedTranscript: true})
	acfg := core.DefaultAssistantContextConfig()
	acfg.BargeInOnTurnEvent = true
	h.asst = core.NewAssistantContextProcessor(ctxPool, acfg)
	pipe, err := core.NewPipeline(stt, tp, fin, user, h.asst, h.sink)
	if err != nil {
		t.Fatal(err)
	}
	h.sess, _ = core.NewSession(77, pipe, core.DefaultResourceBudget())
	if err := h.sess.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.sess.Close() })
	return h
}

// frame posts 20 ms of audio (word index w, 0 = silence) and plays a VAD with a 60 ms start
// and an 800 ms stop (silero defaults: 2 and 25 chunks of 32 ms).
func (h *flowHarness) frame(w byte) {
	_ = h.sess.Post(&core.InputAudioFrame{Samples: []byte{min(w, 1), w, 0, 0}, SampleRate: 16000})
	if w != 0 {
		h.speech, h.quiet = h.speech+1, 0
		if !h.vadOn && h.speech == 3 {
			h.vadOn, h.seq = true, h.seq+1
			_ = h.sess.Post(&core.UserStartedSpeakingFrame{Gen: h.sess.CurrentGeneration(), Seq: h.seq, Timestamp: time.Now()})
		}
	} else {
		h.speech, h.quiet = 0, h.quiet+1
		if h.vadOn && h.quiet == 40 {
			h.vadOn = false
			_ = h.sess.Post(&core.UserStoppedSpeakingFrame{Gen: h.sess.CurrentGeneration(), Seq: h.seq, Timestamp: time.Now()})
		}
	}
	time.Sleep(20 * time.Millisecond)
}

func (h *flowHarness) say(words ...byte) {
	for _, w := range words {
		for range 10 { // 200 ms per word
			h.frame(w)
		}
	}
}

func (h *flowHarness) silence(d time.Duration) {
	for range int(d / (20 * time.Millisecond)) {
		h.frame(0)
	}
}

// heard joins every user message the context committed.
func (h *flowHarness) heard() (msgs []string) {
	for _, f := range h.sink.GetFrames() {
		if m, ok := f.(*core.UserMessageFrame); ok {
			msgs = append(msgs, m.Content)
		}
	}
	return msgs
}

// S4: "I would like to know" + 1 s pause + "what the capital of France is.". The VAD stops in
// the pause and the first half completes as a turn (fast STT vs 700 ms timeout: by config);
// the second half must still be transcribed and committed, not lost.
func TestXAIAgent_MidUtterancePauseKeepsSecondHalf(t *testing.T) {
	h := newAgentHarness(t)
	h.silence(400 * time.Millisecond)
	h.say(1, 2, 3, 4, 5)
	h.silence(time.Second)
	h.say(6, 7, 8, 9, 10, 11)
	h.silence(3 * time.Second)
	all := strings.Join(h.heard(), " ")
	if !strings.Contains(all, "I would like to know") || !strings.Contains(all, "what the capital of France is.") {
		t.Fatalf("committed user text %q is missing a half\n%s", h.heard(), h.op.report())
	}
	h.checkStreams(60)
}

// checkStreams: the stream that carried the last utterance got every one of its speech frames
// (the onset before the VAD start included: the stream opened on the silence after the previous
// stop is adopted, not parked), nothing was sent after audio.done, and no stream but the
// current one is left open.
func (h *flowHarness) checkStreams(lastFrames int) {
	h.t.Helper()
	h.op.mu.Lock()
	streams := h.op.streams
	h.op.mu.Unlock()
	var carried bool
	for i, s := range streams {
		s.mu.Lock()
		if len(s.heard) > 0 && s.heard[0] == "what" && s.frames == lastFrames {
			carried = true
		}
		bad := s.late > 0 || (!s.closed && i < len(streams)-1)
		s.mu.Unlock()
		if bad {
			h.t.Errorf("stream %d: audio after audio.done or left open", s.id)
		}
	}
	if !carried {
		h.t.Errorf("no stream heard the whole last utterance from its onset")
	}
	if h.t.Failed() {
		h.t.Log(h.op.report())
	}
}

// S2: the bot is replying when the user speaks a full sentence: the turn manager must barge in
// (3 words in an interim) while the user is still speaking, and the whole sentence is heard.
func TestXAIAgent_BargeInWhileBotSpeaks(t *testing.T) {
	h := newAgentHarness(t)
	h.silence(200 * time.Millisecond)
	_ = h.sess.Post(&core.UserMessageFrame{Turn: "bot-1", Gen: h.sess.CurrentGeneration(), Content: "tell me about mars", Timestamp: time.Now()})
	_ = h.sess.Reenter(&core.SpeakingStateFrame{State: core.SpeakingStarted, Turn: "bot-1", Gen: h.sess.CurrentGeneration()}, core.Upstream, h.sess.TailIndex(), 0)
	h.silence(time.Second) // the bot talks; the client streams silence
	h.say(6, 7, 8, 9, 10, 11)
	barged := h.asst.InterruptionCount.Load()
	h.silence(3 * time.Second)
	if barged == 0 {
		t.Fatalf("no barge-in while the user was speaking (interruptions after: %d)\n%s", h.asst.InterruptionCount.Load(), h.op.report())
	}
	if all := strings.Join(h.heard(), " "); !strings.Contains(all, "what the capital of France is.") {
		t.Fatalf("committed user text %q, want the whole sentence\n%s", h.heard(), h.op.report())
	}
	h.checkStreams(60)
}
