package adapter

import (
	"encoding/binary"
	"math"
	"testing"
	"time"
)

const testRate = 16000

// pcmTone is a 440 Hz sine of amplitude amp (0..1 of full scale) lasting d.
func pcmTone(d time.Duration, amp float64) []byte {
	n := int(d * testRate / time.Second)
	b := make([]byte, 2*n)
	for i := 0; i < n; i++ {
		v := int16(amp * 32767 * math.Sin(2*math.Pi*440*float64(i)/testRate))
		binary.LittleEndian.PutUint16(b[2*i:], uint16(v))
	}
	return b
}

func pcmSilence(d time.Duration) []byte { return make([]byte, 2*int(d*testRate/time.Second)) }

// feed pushes pcm in 20 ms frames and returns the events that were not EventNone.
func feed(e *Endpointer, pcm []byte) []Event {
	var out []Event
	step := 2 * testRate / 50
	for i := 0; i < len(pcm); i += step {
		end := min(i+step, len(pcm))
		if ev := e.Feed(pcm[i:end]); ev != EventNone {
			out = append(out, ev)
		}
	}
	return out
}

func TestEndpointerSpeechThenSilence(t *testing.T) {
	e := NewEndpointer(testRate, 600*time.Millisecond, 0.01)
	pcm := append(append(pcmSilence(300*time.Millisecond), pcmTone(time.Second, 0.3)...), pcmSilence(time.Second)...)
	got := feed(e, pcm)
	if len(got) != 2 || got[0] != EventSpeechStart || got[1] != EventSpeechEnd {
		t.Fatalf("events %v", got)
	}
}

func TestEndpointerShortBlipAborts(t *testing.T) {
	e := NewEndpointer(testRate, 600*time.Millisecond, 0.01)
	got := feed(e, append(pcmTone(100*time.Millisecond, 0.3), pcmSilence(time.Second)...))
	if len(got) != 2 || got[0] != EventSpeechStart || got[1] != EventSpeechAbort {
		t.Fatalf("events %v", got)
	}
}

func TestEndpointerPauseInsideSpeechDoesNotEnd(t *testing.T) {
	e := NewEndpointer(testRate, 600*time.Millisecond, 0.01)
	pcm := append(append(pcmTone(500*time.Millisecond, 0.3), pcmSilence(300*time.Millisecond)...), pcmTone(500*time.Millisecond, 0.3)...)
	got := feed(e, pcm)
	if len(got) != 1 || got[0] != EventSpeechStart {
		t.Fatalf("a 300 ms pause ended the utterance: %v", got)
	}
}

func TestEndpointerSilenceAloneNeverStarts(t *testing.T) {
	e := NewEndpointer(testRate, 600*time.Millisecond, 0.01)
	if got := feed(e, pcmSilence(5*time.Second)); len(got) != 0 {
		t.Fatalf("events on silence: %v", got)
	}
}

func TestEndpointerResetAllowsNewUtterance(t *testing.T) {
	e := NewEndpointer(testRate, 600*time.Millisecond, 0.01)
	feed(e, pcmTone(time.Second, 0.3))
	e.Reset()
	if got := feed(e, pcmTone(time.Second, 0.3)); len(got) != 1 || got[0] != EventSpeechStart {
		t.Fatalf("after Reset: %v", got)
	}
}
