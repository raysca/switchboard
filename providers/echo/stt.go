package echo

import (
	"context"
	"io"
	"strings"
	"sync"
	"time"

	"govoice/providers/google"
)

const (
	frameDuration = 20 * time.Millisecond
	frameBytes    = 640 // 20 ms of 16 kHz PCM16 mono, the format the real Google STT stream takes
	queueCap      = 64  // interim results held for the reader; beyond it interims are dropped, finals never
	maxWords      = 512 // an utterance stops gaining words here, so a stuck-open stream cannot grow without bound
)

// STTOpener opens simulated Google recognition streams: the real google STT processor (stream per
// utterance, rotation, transcript assembly, tracing) runs against them. Echo cannot recognise
// speech: in audio mode it turns voiced audio into placeholder words; in text mode it never
// transcribes (typed turns bypass STT).
type STTOpener struct {
	f         STTFigures
	audioMode bool
}

func NewSTTOpener(f STTFigures, audioMode bool) *STTOpener {
	return &STTOpener{f: f, audioMode: audioMode}
}

func (o *STTOpener) OpenStream(ctx context.Context, _ google.STTConfig) (google.STTStream, error) {
	ctx, cancel := context.WithCancel(ctx)
	return &sttStream{f: o.f, audio: o.audioMode, done: ctx.Done(), cancel: cancel,
		results: make(chan *google.STTResult, queueCap), sent: make(chan struct{})}, nil
}

type sttStream struct {
	f     STTFigures
	audio bool
	// done closes when the stream's context ends or Close is called. The context itself is not
	// kept (AGENTS.md): cancellation is all the stream needs, and it reports context.Canceled.
	done   <-chan struct{}
	cancel context.CancelFunc

	results  chan *google.STTResult
	sent     chan struct{}
	sendOnce sync.Once

	mu        sync.Mutex
	media     int // 20 ms frames received
	voiced    int
	words     []string
	finalDone bool
}

func (s *sttStream) Send(audio []byte) error {
	if !s.audio {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.media += max(len(audio)/frameBytes, 1)
	if s.f.VoicedThreshold > 0 && peakMagnitude(audio) < s.f.VoicedThreshold {
		return nil
	}
	s.voiced++
	if s.voiced%max(s.f.FramesPerWord, 1) != 0 {
		return nil
	}
	if len(s.words) >= maxWords {
		return nil
	}
	s.words = append(s.words, "hello")
	if s.f.Interims {
		r := &google.STTResult{Transcript: strings.Join(s.words, " "), Stability: 0.9, ResultEndOffset: time.Duration(s.media) * frameDuration}
		select {
		case s.results <- r:
		default: // a slow reader never blocks the audio path
		}
	}
	return nil
}

func (s *sttStream) CloseSend() error {
	s.sendOnce.Do(func() { close(s.sent) })
	return nil
}

func (s *sttStream) Close() error {
	_ = s.CloseSend()
	s.cancel()
	return nil
}

func (s *sttStream) Recv() ([]*google.STTResult, error) {
	for {
		select {
		case r := <-s.results:
			return []*google.STTResult{r}, nil
		default:
		}
		select {
		case r := <-s.results:
			return []*google.STTResult{r}, nil
		case <-s.done:
			return nil, context.Canceled
		case <-s.sent:
			select { // interims queued before the audio ended come first
			case r := <-s.results:
				return []*google.STTResult{r}, nil
			default:
			}
			return s.final()
		}
	}
}

// final answers once, FinalDelay after the audio ended, with every word heard; then io.EOF.
func (s *sttStream) final() ([]*google.STTResult, error) {
	select { // a closed stream answers nothing, even if the audio had ended too
	case <-s.done:
		return nil, context.Canceled
	default:
	}
	s.mu.Lock()
	if s.finalDone || len(s.words) == 0 {
		s.finalDone = true
		s.mu.Unlock()
		return nil, io.EOF
	}
	s.finalDone = true
	text, offset := strings.Join(s.words, " "), time.Duration(s.media)*frameDuration
	s.mu.Unlock()
	if s.f.FinalDelay > 0 {
		t := time.NewTimer(s.f.FinalDelay)
		defer t.Stop()
		select {
		case <-s.done:
			return nil, context.Canceled
		case <-t.C:
		}
	}
	return []*google.STTResult{{Transcript: text, IsFinal: true, Stability: 1, ResultEndOffset: offset}}, nil
}

// peakMagnitude is the largest absolute PCM16LE sample in audio.
func peakMagnitude(audio []byte) int {
	peak := 0
	for i := 0; i+1 < len(audio); i += 2 {
		v := int(int16(uint16(audio[i]) | uint16(audio[i+1])<<8))
		if v < 0 {
			v = -v
		}
		peak = max(peak, v)
	}
	return peak
}
