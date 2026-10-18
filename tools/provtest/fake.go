package provtest

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"govoice/core"
)

// In-process fake providers: the dry mode of the suite and the way its own
// logic is tested without a network. A Fault makes one fake deliberately
// non-conformant, to prove the matching check fails.

type Fault string

const (
	FaultNone        Fault = ""
	FaultNoFinal     Fault = "tts-never-final"       // TTS never marks IsFinal
	FaultNoInterrupt Fault = "ignore-interruption"   // TTS/LLM keep producing after barge-in
	FaultDropSecond  Fault = "stt-drops-2nd-segment" // STT loses text after the first pause
	FaultNoUsage     Fault = "llm-no-usage"          // LLM emits no UsageMetricsFrame
	FaultLeak        Fault = "leak"                  // Close leaves a goroutine behind
	FaultWrongRate   Fault = "tts-wrong-rate"        // TTS frames claim 16 kHz
	FaultSilent      Fault = "tts-silent"            // TTS audio is all zeros
	FaultRIFF        Fault = "tts-riff"              // TTS audio starts with a WAV header
	FaultNoSTTFinal  Fault = "stt-never-final"       // STT emits interims only
	FaultNotCumul    Fault = "stt-non-cumulative"    // STT finals do not extend each other
	FaultBadToolArgs Fault = "llm-bad-tool-args"     // LLM tool call arguments are malformed JSON
	FaultAuthServer  Fault = "auth-as-server-error"  // a bad credential is reported as a server error
	FaultEchoSecret  Fault = "echo-secret"           // the bad-credential error echoes the credential
)

// fakeAuthError stands in for a provider's 401: a client error carrying nothing
// secret (unless a fault makes it a server error or echo the credential).
type fakeAuthError struct {
	server bool
	echo   string
}

func (e fakeAuthError) Error() string {
	return "fake: 401 unauthorized (invalid credential)" + e.echo
}
func (e fakeAuthError) ClientError() bool { return !e.server }

const fakeBadSecret = "bad-credential-fake"

func (b *fakeBase) authErr() fakeAuthError {
	e := fakeAuthError{server: b.fault == FaultAuthServer}
	if b.fault == FaultEchoSecret {
		e.echo = " key=" + fakeBadSecret
	}
	return e
}

// FakeClassify maps fakeAuthError to the client class and anything else to a server error.
func FakeClassify(err error) string {
	var ce interface{ ClientError() bool }
	if errors.As(err, &ce) && ce.ClientError() {
		return classClient
	}
	return "server_error"
}

type fakeBase struct {
	sess  core.SessionController
	index int
	mu    sync.Mutex
	stops []context.CancelFunc
	fault Fault
	auth  bool // invalid credential
}

func (b *fakeBase) Attach(s core.SessionController, index int) error {
	b.sess, b.index = s, index
	return nil
}

func (b *fakeBase) Close() error {
	b.cancelAll()
	if b.fault == FaultLeak {
		for i := 0; i < 10; i++ {
			go func() { time.Sleep(time.Hour) }()
		}
	}
	return nil
}

func (b *fakeBase) cancelAll() {
	b.mu.Lock()
	stops := b.stops
	b.stops = nil
	b.mu.Unlock()
	for _, c := range stops {
		c()
	}
}

// task runs fn as a session task whose context an interruption cancels.
func (b *fakeBase) task(fn func(ctx context.Context)) {
	b.mu.Lock()
	ctx, cancel := context.WithCancel(b.sess.Context())
	b.stops = append(b.stops, cancel)
	b.mu.Unlock()
	if err := b.sess.Go(func(context.Context) { defer cancel(); fn(ctx) }); err != nil {
		cancel()
	}
}

func (b *fakeBase) emit(f core.Frame, gen uint64) {
	_ = b.sess.Reenter(f, core.Downstream, b.index+1, gen)
}

func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// ---- TTS -------------------------------------------------------------------

// FakeTTS speaks 60 ms of a tone per character in 20 ms frames.
type FakeTTS struct{ fakeBase }

func NewFakeTTS(f Fault, badCredential bool) *FakeTTS {
	return &FakeTTS{fakeBase{fault: f, auth: badCredential}}
}

func (t *FakeTTS) ProcessFrame(_ context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	switch fr := f.(type) {
	case *core.SynthesisUnitFrame:
		if t.auth {
			t.emit(&core.ErrorFrame{Err: t.authErr(), UnitID: fr.UnitID}, fr.Gen)
			return nil
		}
		t.task(func(ctx context.Context) { t.speak(ctx, fr) })
		return nil
	case *core.InterruptionFrame:
		if t.fault != FaultNoInterrupt {
			t.cancelAll()
		}
	}
	return e.PushDirection(f, dir)
}

func (t *FakeTTS) speak(ctx context.Context, u *core.SynthesisUnitFrame) {
	chars := utf8.RuneCountInString(u.Text)
	frames := chars * 3 // 60 ms per char / 20 ms
	if !sleep(ctx, 15*time.Millisecond) {
		return
	}
	phase := 0.0
	for i := 0; i < frames; i++ {
		b := make([]byte, frameBytes)
		for k := 0; k < frameBytes/2 && t.fault != FaultSilent; k++ {
			binary.LittleEndian.PutUint16(b[2*k:], uint16(int16(6000*math.Sin(phase))))
			phase += 2 * math.Pi * 220 / pipelineRate
		}
		if i == 0 && t.fault == FaultRIFF {
			copy(b, EncodePCMWAV(nil, pipelineRate))
		}
		rate := pipelineRate
		if t.fault == FaultWrongRate {
			rate = 16000
		}
		last := i == frames-1
		final := last && t.fault != FaultNoFinal
		t.emit(&core.OutputAudioFrame{Samples: b, Encoding: core.EncodingPCM16LE, SampleRate: rate, Channels: 1,
			Duration: 20 * time.Millisecond, Turn: u.Turn, Gen: u.Gen, UnitID: u.UnitID, IsFinal: final, TurnFinal: final && u.IsFinal}, u.Gen)
		if !sleep(ctx, 4*time.Millisecond) {
			return
		}
	}
	t.emit(&core.TTSStoppedFrame{Turn: u.Turn, Gen: u.Gen, UnitID: u.UnitID}, u.Gen)
	t.emit(&core.UsageMetricsFrame{Provider: "fake", Service: "tts", Gen: u.Gen, Characters: core.Int64Ptr(int64(chars))}, u.Gen)
}

// ---- STT -------------------------------------------------------------------

// FakeSTT recognises a clip by matching the audio it receives against the
// fixtures, emits cumulative interims every 200 ms and one final+EOU at the stop.
type FakeSTT struct {
	fakeBase
	clips []Clip

	turn   core.TurnID
	gen    uint64
	buf    []byte
	frames int
}

func NewFakeSTT(clips []Clip, f Fault, badCredential bool) *FakeSTT {
	return &FakeSTT{fakeBase: fakeBase{fault: f, auth: badCredential}, clips: clips}
}

// match finds the clip the received audio is the start of. At the final it is
// the longest clip fully received (a clip may be the start of a longer one);
// for an interim, the shortest clip the audio could still be.
func (s *FakeSTT) match(final bool) *Clip {
	var hit *Clip
	for i := range s.clips {
		c := &s.clips[i]
		n := min(len(s.buf), len(c.PCM))
		if n < 8192 || !bytes.Equal(s.buf[:n], c.PCM[:n]) || final && len(s.buf) < len(c.PCM) {
			continue
		}
		if hit == nil || final == (len(c.PCM) > len(hit.PCM)) {
			hit = c
		}
	}
	return hit
}

func (s *FakeSTT) text(c *Clip) string {
	if s.fault == FaultDropSecond && len(c.Segments) > 1 {
		return c.Segments[0]
	}
	return c.Text
}

func (s *FakeSTT) cand(text string, final bool) *core.TranscriptCandidateFrame {
	return &core.TranscriptCandidateFrame{Text: text, Timestamp: time.Now(), Turn: s.turn, Gen: core.Generation(s.gen), Final: final, EndOfUtterance: final, Provider: "fake"}
}

func (s *FakeSTT) ProcessFrame(_ context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	switch fr := f.(type) {
	case *core.UserStartedSpeakingFrame:
		s.turn, s.gen, s.buf, s.frames = core.TurnID(""), fr.Gen, nil, 0
	case *core.TurnEventFrame:
		if fr.Kind == core.TurnEventStarted {
			if s.turn != fr.Turn { // a rebind (same turn, newer generation) keeps the audio
				s.buf, s.frames = nil, 0
			}
			s.turn, s.gen = fr.Turn, uint64(fr.Gen)
		}
	case *core.InputAudioFrame:
		if s.auth {
			return s.authErr()
		}
		s.buf = append(s.buf, fr.Samples...)
		s.frames++
		if s.frames%10 == 0 {
			if c := s.match(false); c != nil {
				w := strings.Fields(s.text(c))
				n := min(len(w), max(1, len(w)*len(s.buf)/max(len(c.PCM), 1)))
				if err := e.Push(s.cand(strings.Join(w[:n], " "), false)); err != nil {
					return err
				}
			}
		}
	case *core.UserStoppedSpeakingFrame:
		if c := s.match(true); c != nil && s.turn != "" && s.fault != FaultNoSTTFinal {
			if s.fault == FaultNotCumul {
				early := s.cand("something else entirely", true)
				early.EndOfUtterance = false
				if err := e.Push(early); err != nil {
					return err
				}
			}
			if err := e.Push(s.cand(s.text(c), true)); err != nil {
				return err
			}
			s.buf = nil
		}
	}
	return e.PushDirection(f, dir)
}

// ---- LLM -------------------------------------------------------------------

type FakeLLM struct {
	fakeBase
	tools *ToolKit
	model bool // invalid model
}

func NewFakeLLM(f Fault, badCredential, badModel bool, tools *ToolKit) *FakeLLM {
	return &FakeLLM{fakeBase: fakeBase{fault: f, auth: badCredential}, tools: tools, model: badModel}
}

func (l *FakeLLM) ProcessFrame(_ context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	switch fr := f.(type) {
	case *core.UserMessageFrame:
		if l.auth || l.model {
			return l.authErr() // a processor error fails the session, as the real ones do
		}
		l.task(func(ctx context.Context) { l.reply(ctx, fr) })
		return nil
	case *core.InterruptionFrame:
		if l.fault != FaultNoInterrupt {
			l.cancelAll()
		}
	}
	return e.PushDirection(f, dir)
}

func (l *FakeLLM) reply(ctx context.Context, m *core.UserMessageFrame) {
	var text string
	switch {
	case strings.Contains(m.Content, "weather"):
		if l.tools != nil {
			args := `{"city":"Paris"}`
			if l.fault == FaultBadToolArgs {
				args = `{"city":"Paris"` // truncated: still mentions Paris
			}
			_, _ = l.tools.Exec.Execute(ctx, core.ToolCall{ID: "c1", Name: "get_weather", Arguments: []byte(args)})
		}
		text = "It is 21 degrees and sunny in Paris."
	case strings.Contains(m.Content, "story"):
		text = strings.Repeat("The keeper climbed the stairs. ", 100)
	default:
		text = "Two plus two is four."
	}
	n := int64(0)
	for _, w := range strings.SplitAfter(text, " ") {
		if !sleep(ctx, 5*time.Millisecond) {
			return
		}
		n++
		l.emit(&core.LLMTextDeltaFrame{Delta: w, Turn: m.Turn, Gen: m.Gen}, m.Gen)
	}
	l.emit(&core.LLMTextFrame{Text: text, Turn: m.Turn, Gen: m.Gen}, m.Gen)
	if l.fault != FaultNoUsage {
		l.emit(&core.UsageMetricsFrame{Provider: "fake", Service: "llm", Gen: m.Gen, PromptTokens: core.Int64Ptr(12), CompletionTokens: core.Int64Ptr(n)}, m.Gen)
	}
}

// ---- dry targets -----------------------------------------------------------

// FakeTarget is a dry-mode target for stage whose processors misbehave as fault says.
func FakeTarget(stage Stage, id string, clips []Clip, fault Fault) Target {
	t := Target{Provider: id, Stage: stage, Classify: FakeClassify, BadSecret: fakeBadSecret}
	t.Build = func(v Variant) (core.Processor, error) {
		switch stage {
		case TTS:
			return NewFakeTTS(fault, v.BadCredential), nil
		case STT:
			return NewFakeSTT(clips, fault, v.BadCredential), nil
		}
		return NewFakeLLM(fault, v.BadCredential, v.BadModel, v.Tools), nil
	}
	return t
}
