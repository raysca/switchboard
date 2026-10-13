package core

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// FakeSTTConfig scripts a FakeSTTProcessor. Each audio frame carries one word:
// Samples[0] indexes Words, so any replay of the same audio yields the same text.
type FakeSTTConfig struct {
	Provider string
	Words    []string
	// FinalEvery emits a provider final (cumulative text) after every N frames. 0 = only at UserStopped.
	FinalEvery int
	// FailAfter kills the stream after that many audio frames (mid-stream drop):
	// an async ErrorFrame, then every audio frame errors. 0 = never.
	FailAfter int
	// FailErr is the failure; defaults to a generic error.
	FailErr error
	// InvalidateOnFail also emits a TranscriptInvalidationFrame and an
	// ErrTranscriptInvalidated error first (the xAI send-failure shape).
	InvalidateOnFail bool
	// ZombieOnFail makes the dead stream emit one more candidate afterwards.
	ZombieOnFail bool
}

// FakeSTTProcessor is a scriptable streaming STT: cumulative interim candidates
// per audio frame, a final at UserStopped, candidates emitted from its own
// goroutine like a provider's receiver.
type FakeSTTProcessor struct {
	cfg FakeSTTConfig

	mu       sync.Mutex
	session  SessionController
	index    int
	turn     string
	gen      uint64
	words    []string
	frames   int // audio frames this stream
	received []byte
	broken   error
	work     chan func()
}

func NewFakeSTTProcessor(cfg FakeSTTConfig) *FakeSTTProcessor {
	if cfg.FailErr == nil {
		cfg.FailErr = fmt.Errorf("fake stt %s: stream dropped", cfg.Provider)
	}
	return &FakeSTTProcessor{cfg: cfg, work: make(chan func(), 256)}
}

func (p *FakeSTTProcessor) Attach(s SessionController, index int) error {
	p.mu.Lock()
	p.session, p.index = s, index
	p.mu.Unlock()
	return s.Go(func(ctx context.Context) {
		for {
			select {
			case <-ctx.Done():
				return
			case fn := <-p.work:
				fn()
			}
		}
	})
}

// Break makes every audio frame fail with err from now on (stream open failure).
func (p *FakeSTTProcessor) Break(err error) {
	p.mu.Lock()
	p.broken = err
	p.mu.Unlock()
}

// Received is every audio byte accepted, in order.
func (p *FakeSTTProcessor) Received() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]byte(nil), p.received...)
}

func (p *FakeSTTProcessor) emit(fr Frame, gen uint64) {
	select {
	case p.work <- func() { _ = p.session.Reenter(fr, Downstream, p.index+1, gen) }:
	default:
	}
}

func (p *FakeSTTProcessor) candidateLocked(final bool) *TranscriptCandidateFrame {
	return &TranscriptCandidateFrame{Text: strings.Join(p.words, " "), Turn: TurnID(p.turn), Gen: Generation(p.gen),
		Final: final, EndOfUtterance: final && p.cfg.FinalEvery == 0, Provider: p.cfg.Provider}
}

func (p *FakeSTTProcessor) ProcessFrame(_ context.Context, f Frame, dir Direction, e Emitter) error {
	p.mu.Lock()
	switch fr := f.(type) {
	case *UserStartedSpeakingFrame:
		p.turn, p.gen = fmt.Sprintf("turn-%d-%d", fr.Gen, fr.Seq), fr.Gen
	case *TurnEventFrame:
		if fr.Kind == TurnEventStarted {
			p.turn, p.gen = string(fr.Turn), uint64(fr.Gen)
		}
	case *UserStoppedSpeakingFrame:
		if p.turn != "" && len(p.words) > 0 && p.broken == nil {
			p.emit(p.candidateLocked(true), p.gen)
		}
		p.words, p.frames = nil, 0
		p.turn = ""
	case *InputAudioFrame:
		if p.broken != nil {
			err := p.broken
			p.mu.Unlock()
			return err
		}
		p.received = append(p.received, fr.Samples...)
		p.frames++
		if p.cfg.FailAfter > 0 && p.frames >= p.cfg.FailAfter {
			p.broken = p.cfg.FailErr
			gen := p.gen
			if p.cfg.InvalidateOnFail {
				p.emit(&TranscriptInvalidationFrame{Turn: TurnID(p.turn), Gen: Generation(gen), Err: ErrTranscriptInvalidated}, gen)
				p.emit(&ErrorFrame{Err: fmt.Errorf("%w: %v", ErrTranscriptInvalidated, p.cfg.FailErr), Turn: TurnID(p.turn), Gen: Generation(gen)}, gen)
			} else {
				p.emit(&ErrorFrame{Err: p.cfg.FailErr}, gen)
			}
			if p.cfg.ZombieOnFail {
				p.words = append(p.words, "ZOMBIE")
				p.emit(p.candidateLocked(true), gen)
			}
			break
		}
		if len(fr.Samples) > 0 && int(fr.Samples[0]) < len(p.cfg.Words) {
			// Audio before the turn binds (VAD reports onset late) still counts, as in providers.
			p.words = append(p.words, p.cfg.Words[fr.Samples[0]])
			if p.turn != "" {
				p.emit(p.candidateLocked(false), p.gen)
				if p.cfg.FinalEvery > 0 && p.frames%p.cfg.FinalEvery == 0 {
					p.emit(p.candidateLocked(true), p.gen)
				}
			}
		}
	}
	p.mu.Unlock()
	return e.PushDirection(f, dir)
}
