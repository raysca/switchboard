package adapter

import (
	"encoding/binary"
	"math"
	"time"
)

type Event int

const (
	EventNone Event = iota
	EventSpeechStart
	EventSpeechEnd   // silence after a real utterance: decode it
	EventSpeechAbort // silence after a click or blip: discard it
)

const minSpeech = 200 * time.Millisecond

// Endpointer decides utterance boundaries from RMS energy. ponytail: a fixed energy threshold works
// for clean and synthetic audio; a noisy microphone needs a real VAD (the repo's vad/ or sherpa-onnx).
type Endpointer struct {
	rate      int
	silence   time.Duration
	threshold float64
	inSpeech  bool
	speech    time.Duration // voiced time of the current utterance
	quiet     time.Duration // consecutive quiet time since the last voiced chunk
}

func NewEndpointer(rate int, silence time.Duration, threshold float64) *Endpointer {
	return &Endpointer{rate: rate, silence: silence, threshold: threshold}
}

func (e *Endpointer) Reset() { e.inSpeech, e.speech, e.quiet = false, 0, 0 }

// Feed consumes a chunk of PCM16LE mono of any length.
func (e *Endpointer) Feed(pcm []byte) Event {
	n := len(pcm) / 2
	if n == 0 {
		return EventNone
	}
	d := time.Duration(n) * time.Second / time.Duration(e.rate)
	voiced := rms(pcm, n) >= e.threshold
	if !e.inSpeech {
		if !voiced {
			return EventNone
		}
		e.inSpeech, e.speech, e.quiet = true, d, 0
		return EventSpeechStart
	}
	if voiced {
		e.speech += d
		e.quiet = 0
		return EventNone
	}
	e.quiet += d
	if e.quiet < e.silence {
		return EventNone
	}
	short := e.speech < minSpeech
	e.Reset()
	if short {
		return EventSpeechAbort
	}
	return EventSpeechEnd
}

func rms(pcm []byte, n int) float64 {
	var sum float64
	for i := 0; i < n; i++ {
		v := float64(int16(binary.LittleEndian.Uint16(pcm[2*i:]))) / 32768
		sum += v * v
	}
	return math.Sqrt(sum / float64(n))
}
