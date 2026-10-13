// Package echo is a simulated provider: STT, LLM and TTS backends that cost nothing and need no
// network. They plug into the real Google processors (the same retries, breaker, usage, tracing,
// tool loop and unit splitting), so a call through echo exercises the real pipeline code. The
// figures imitate Google's measured timing (docs/benchmarks/first-audio-latency.md).
package echo

import "time"

// Mimic names whose timing the echo backends imitate.
type Mimic string

const (
	// MimicGoogle imitates Google's measured latencies.
	MimicGoogle Mimic = "google"
	// MimicInstant has no delays and one placeholder word per audio frame: the framework alone is measured.
	MimicInstant Mimic = "instant"
)

// Valid reports whether m is a known mimic.
func (m Mimic) Valid() bool { return m == MimicGoogle || m == MimicInstant }

// STTFigures shape the simulated recognition stream.
type STTFigures struct {
	Interims        bool          // emit cumulative interim results while the caller speaks
	FinalDelay      time.Duration // the final arrives this long after the caller stops
	FramesPerWord   int           // one placeholder word per this many voiced 20 ms frames (0 or 1: every frame)
	VoicedThreshold int           // peak PCM16 magnitude that counts as speech (0: every frame counts)
}

// LLMFigures shape the simulated model's streaming.
type LLMFigures struct {
	FirstToken time.Duration // before the first piece
	TokenDelay time.Duration // between pieces
	// WordChunks streams one word per piece, as a real model does. Off, a reply is a few phrase-sized
	// pieces (label, "You said: ", the caller's words): easy to assert on, and what the harnesses use.
	WordChunks bool
}

// TTSFigures shape the simulated synthesis.
type TTSFigures struct {
	FirstByte        time.Duration // before a unit's first audio
	FirstBytePerChar time.Duration // plus this per character (unary synthesis returns the whole unit at once)
	CharsPerSecond   int           // speech rate: audio is as long as the text takes to say; 0 = two frames
}

// STT returns the figures for the given speech model.
func (m Mimic) STT(model string) STTFigures {
	if m == MimicInstant {
		return STTFigures{FramesPerWord: 1}
	}
	f := STTFigures{FinalDelay: 1200 * time.Millisecond, FramesPerWord: 20, VoicedThreshold: 500} // chirp_2: no interims
	if model == "latest_long" {
		f.Interims, f.FinalDelay = true, 500*time.Millisecond
	}
	return f
}

// LLM returns the figures for the language model.
func (m Mimic) LLM() LLMFigures {
	if m == MimicInstant {
		return LLMFigures{}
	}
	return LLMFigures{FirstToken: 700 * time.Millisecond, TokenDelay: 30 * time.Millisecond, WordChunks: true}
}

// TTS returns the figures for how the server synthesizes the voice: streaming (Chirp 3 HD voices)
// delivers audio from the first bytes; unary returns the whole unit at once, so it pays before any
// audio. Unary: about 1 s plus 12 ms per character (the benchmark's 1.13 s for 11 characters is
// "mostly fixed overhead"), which keeps the longest unit (250 characters, 4 s) well inside the real
// processor's 10 s attempt timeout.
func (m Mimic) TTS(streaming bool) TTSFigures {
	if m == MimicInstant {
		return TTSFigures{}
	}
	f := TTSFigures{CharsPerSecond: 15}
	if streaming {
		f.FirstByte = 400 * time.Millisecond
	} else {
		f.FirstByte, f.FirstBytePerChar = 1000*time.Millisecond, 12*time.Millisecond
	}
	return f
}
