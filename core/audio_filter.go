package core

import (
	"context"
	"time"
)

const (
	PCM16LE   AudioEncoding = EncodingPCM16LE
	Float32LE AudioEncoding = "Float32LE"
)

// AudioFormat is the complete raw PCM format of a stream.
type AudioFormat struct {
	Encoding   AudioEncoding
	SampleRate int
	Channels   int
}

// AudioFilter owns the mutable DSP state of one stream. Implementations must
// serialize concurrent calls and return caller-owned output buffers.
type AudioFilter interface {
	InputFormat() AudioFormat
	OutputFormat() AudioFormat
	Latency() time.Duration
	Process(context.Context, []byte) ([]byte, error)
	Flush(context.Context) ([]byte, error)
	Reset() error
	Close() error
}
