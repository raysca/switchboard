package dtmf

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"govoice/core"
)

// DebugDetected is emitted for every confirmed inbound key press. It carries no
// attributes: digits can be PINs or card numbers and stay out of telemetry.
const DebugDetected = "dtmf.detected"

func emitDebug(s core.SessionController, name string, gen uint64) {
	if s == nil {
		return
	}
	if em, ok := s.(interface{ EmitDebug(core.DebugEvent) }); ok {
		em.EmitDebug(core.NewDebugEvent(name, time.Now(), core.SessionID(strconv.FormatUint(s.ID(), 10)), "", core.Generation(gen), nil))
	}
}

// InputProcessor detects DTMF tones in inbound audio and emits an
// InputDTMFFrame after the audio that contained them. Place it first in the
// pipeline, before VAD. The audio is forwarded untouched.
type InputProcessor struct {
	det     *Detector
	rate    int
	session core.SessionController
}

func NewInputProcessor(cfg DetectorConfig) (*InputProcessor, error) {
	d, err := NewDetector(cfg)
	if err != nil {
		return nil, err
	}
	return &InputProcessor{det: d, rate: cfg.SampleRate}, nil
}

func (p *InputProcessor) Attach(s core.SessionController, _ int) error { p.session = s; return nil }

func (p *InputProcessor) ProcessFrame(_ context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	a, ok := f.(*core.InputAudioFrame)
	if !ok || dir != core.Downstream || a.SampleRate != p.rate || a.Channels > 1 {
		return e.PushDirection(f, dir)
	}
	if err := e.PushDirection(f, dir); err != nil {
		return err
	}
	for _, ev := range p.det.Write(a.Samples) {
		var gen uint64
		if p.session != nil {
			gen = p.session.CurrentGeneration()
		}
		emitDebug(p.session, DebugDetected, gen)
		if err := e.Push(&core.InputDTMFFrame{Digit: string(ev.Digit), Timestamp: time.Now(), Gen: gen}); err != nil {
			return err
		}
	}
	return nil
}

// AggregatorConfig mirrors Pipecat's DTMFAggregator.
type AggregatorConfig struct {
	Timeout    time.Duration // idle time after the last digit before flushing (default 2s)
	Terminator byte          // digit that flushes immediately (default '#')
	Prefix     string        // prepended to the digits in the user message (default "DTMF: ")
}

func DefaultAggregatorConfig() AggregatorConfig {
	return AggregatorConfig{Timeout: 2 * time.Second, Terminator: '#', Prefix: "DTMF: "}
}

// Aggregator turns runs of InputDTMFFrames into one committed user message
// ("DTMF: 1234#") for the LLM. Place it directly before the user context
// processor. The first digit of a run interrupts the bot, as in Pipecat. A run
// still pending when the session ends is dropped (the context dies with it).
type Aggregator struct {
	cfg     AggregatorConfig
	mu      sync.Mutex
	buf     []byte
	timer   *time.Timer
	seq     uint64
	session core.SessionController
	index   int
}

func NewAggregator(cfg AggregatorConfig) *Aggregator {
	d := DefaultAggregatorConfig()
	if cfg.Timeout <= 0 {
		cfg.Timeout = d.Timeout
	}
	if cfg.Terminator == 0 {
		cfg.Terminator = d.Terminator
	}
	if cfg.Prefix == "" {
		cfg.Prefix = d.Prefix
	}
	return &Aggregator{cfg: cfg}
}

func (a *Aggregator) Attach(s core.SessionController, index int) error {
	a.session, a.index = s, index
	return nil
}

func (a *Aggregator) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.timer != nil {
		a.timer.Stop()
	}
	a.buf = nil
	return nil
}

func (a *Aggregator) ProcessFrame(_ context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	in, ok := f.(*core.InputDTMFFrame)
	if !ok || dir != core.Downstream || len(in.Digit) != 1 || !Valid(in.Digit[0]) {
		return e.PushDirection(f, dir)
	}
	if err := e.PushDirection(f, dir); err != nil {
		return err
	}
	a.mu.Lock()
	first := len(a.buf) == 0
	a.buf = append(a.buf, in.Digit[0])
	flushNow := in.Digit[0] == a.cfg.Terminator || len(a.buf) >= maxAggregate
	if !flushNow {
		if a.timer != nil {
			a.timer.Stop()
		}
		a.timer = time.AfterFunc(a.cfg.Timeout, a.onTimeout)
	}
	a.mu.Unlock()
	if first && a.session != nil {
		if err := e.Push(&core.InterruptionFrame{Gen: a.session.CurrentGeneration()}); err != nil {
			return err
		}
	}
	if flushNow {
		if msg := a.take(); msg != nil {
			return e.Push(msg)
		}
	}
	return nil
}

func (a *Aggregator) onTimeout() {
	if msg := a.take(); msg != nil && a.session != nil {
		_ = a.session.Reenter(msg, core.Downstream, a.index+1, msg.Gen) // closed session: nothing to do
	}
}

// take drains the buffer into a committed transcript, or nil if it is empty.
func (a *Aggregator) take() *core.FinalTranscriptionFrame {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.buf) == 0 {
		return nil
	}
	if a.timer != nil {
		a.timer.Stop()
	}
	text := a.cfg.Prefix + string(a.buf)
	a.buf = nil
	a.seq++
	var gen uint64
	if a.session != nil {
		gen = a.session.CurrentGeneration()
	}
	return &core.FinalTranscriptionFrame{Text: text, Turn: fmt.Sprintf("dtmf-%d", a.seq), Gen: gen, Timestamp: time.Now(), Committed: true}
}

// OutputConfig configures outbound tone playback.
type OutputConfig struct {
	Tones GeneratorConfig // SampleRate must equal the pipeline's TTS output rate
	// Lead is how far ahead of real time frames are released into the paced
	// output (default 60ms, under the pacer's 100ms queue bound).
	Lead time.Duration
}

const (
	outChunk   = 20 * time.Millisecond
	maxPending = 8
	// maxAggregate flushes a run early so a stuck key cannot grow it without bound.
	maxAggregate = 64
	// MaxDigitsPerSend bounds one send; 32 digits is about 5 s of tones.
	MaxDigitsPerSend = 32
)

var ErrBusy = errors.New("dtmf: too many digit sends pending")

type toneReq struct {
	digits string
	gen    uint64
	done   chan struct{} // closed once played, cut short by barge-in, or dropped
}

// OutputProcessor plays OutputDTMFFrames as tone audio. Place it just before the
// transport. Tones are released at real-time pace as NonSpeech OutputAudioFrames
// tagged with the request's generation, so a barge-in (generation advance)
// stops the stream and the transport flush drops what is already queued.
type OutputProcessor struct {
	cfg     OutputConfig
	q       chan toneReq
	session core.SessionController
	index   int
}

func NewOutputProcessor(cfg OutputConfig) (*OutputProcessor, error) {
	if err := cfg.Tones.validate(); err != nil {
		return nil, err
	}
	if cfg.Lead <= 0 {
		cfg.Lead = 60 * time.Millisecond
	}
	return &OutputProcessor{cfg: cfg, q: make(chan toneReq, maxPending)}, nil
}

func (p *OutputProcessor) Attach(s core.SessionController, index int) error {
	p.session, p.index = s, index
	return s.Go(p.run)
}

// ValidateDigits checks a digit string against [0-9*#A-D]{1,MaxDigitsPerSend}.
func ValidateDigits(digits string) error {
	if digits == "" || len(digits) > MaxDigitsPerSend {
		return fmt.Errorf("digits must be 1 to %d characters", MaxDigitsPerSend)
	}
	for i := 0; i < len(digits); i++ {
		if !Valid(digits[i]) {
			return errors.New("digits may only contain 0-9, *, # and A-D")
		}
	}
	return nil
}

// Send queues digits for playback under generation gen (0 = current). It
// returns once queued, not once played.
func (p *OutputProcessor) Send(gen uint64, digits string) error {
	_, err := p.send(gen, digits)
	return err
}

// send queues like Send and returns a channel closed when playback ends.
func (p *OutputProcessor) send(gen uint64, digits string) (<-chan struct{}, error) {
	if err := ValidateDigits(digits); err != nil {
		return nil, err
	}
	if p.session == nil {
		return nil, errors.New("dtmf: output not attached to a session")
	}
	if gen == 0 {
		gen = p.session.CurrentGeneration()
	}
	r := toneReq{digits, gen, make(chan struct{})}
	select {
	case p.q <- r:
		return r.done, nil
	default:
		return nil, ErrBusy
	}
}

func (p *OutputProcessor) ProcessFrame(_ context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	o, ok := f.(*core.OutputDTMFFrame)
	if !ok || dir != core.Downstream {
		return e.PushDirection(f, dir)
	}
	// Consumed here. A bad or surplus request is dropped: it is a content
	// problem, not a reason to fail the call.
	_ = p.Send(o.Gen, o.Digits)
	return nil
}

func (p *OutputProcessor) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case r := <-p.q:
			ok := p.play(ctx, r)
			close(r.done)
			if !ok {
				return
			}
		}
	}
}

// play streams one request; false means the session is gone.
func (p *OutputProcessor) play(ctx context.Context, r toneReq) bool {
	pcm, err := Generate(r.digits, p.cfg.Tones)
	if err != nil {
		return true
	}
	rate := p.cfg.Tones.SampleRate
	step := int(float64(rate)*outChunk.Seconds()) * 2
	start := time.Now()
	for i, off := 0, 0; off < len(pcm); i, off = i+1, off+step {
		if wait := time.Until(start.Add(time.Duration(i)*outChunk - p.cfg.Lead)); wait > 0 {
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return false
			case <-t.C:
			}
		}
		if r.gen < p.session.CurrentGeneration() {
			return true // barge-in: stop generating; queued frames are flushed downstream
		}
		chunk := pcm[off:min(off+step, len(pcm))]
		fr := &core.OutputAudioFrame{
			Samples: chunk, Encoding: core.EncodingPCM16LE, SampleRate: rate, Channels: 1,
			Duration: time.Duration(len(chunk)/2) * time.Second / time.Duration(rate), Timestamp: time.Now(),
			Gen: r.gen, NonSpeech: true,
		}
		if err := p.session.Reenter(fr, core.Downstream, p.index+1, r.gen); err != nil {
			return !errors.Is(err, core.ErrSessionClosed)
		}
	}
	return true
}
