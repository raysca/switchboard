package audio

/*
#cgo CFLAGS: -DFLOATING_POINT -DUSE_SMALLFT -DEXPORT= -I${SRCDIR}/native -I${SRCDIR}/native/libspeexdsp
#cgo LDFLAGS: -lm
#include <speex/speex_preprocess.h>
*/
import "C"

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"
	"unsafe"

	"govoice/core"
)

var ErrClosed = errors.New("audio: stream closed")

// SpeexDenoiser wraps the SpeexDSP spectral noise suppressor. One instance
// owns one stream; methods serialize access to its native state.
type SpeexDenoiser struct {
	mu      sync.Mutex
	fmt     core.AudioFormat
	state   *C.SpeexPreprocessState
	pending []int16
	frame   int
	primed  bool
	flushed bool
}

var _ core.AudioFilter = (*SpeexDenoiser)(nil)

func NewSpeexDenoiser(format core.AudioFormat) (*SpeexDenoiser, error) {
	if format.Encoding != core.PCM16LE || format.Channels != 1 || !supportedRate(format.SampleRate) {
		return nil, fmt.Errorf("audio: unsupported denoiser format %+v", format)
	}
	f := &SpeexDenoiser{fmt: format, frame: format.SampleRate / 100}
	if err := f.newState(); err != nil {
		return nil, err
	}
	return f, nil
}

func supportedRate(rate int) bool {
	return rate == 8000 || rate == 16000 || rate == 24000 || rate == 48000
}

func (f *SpeexDenoiser) newState() error {
	f.state = C.speex_preprocess_state_init(C.int(f.frame), C.int(f.fmt.SampleRate))
	if f.state == nil {
		return errors.New("audio: SpeexDSP allocation failed")
	}
	yes := C.int(1)
	level := C.int(-20)
	if C.speex_preprocess_ctl(f.state, C.SPEEX_PREPROCESS_SET_DENOISE, unsafe.Pointer(&yes)) != 0 ||
		C.speex_preprocess_ctl(f.state, C.SPEEX_PREPROCESS_SET_NOISE_SUPPRESS, unsafe.Pointer(&level)) != 0 {
		C.speex_preprocess_state_destroy(f.state)
		f.state = nil
		return errors.New("audio: SpeexDSP configuration failed")
	}
	return nil
}

func (f *SpeexDenoiser) InputFormat() core.AudioFormat  { return f.fmt }
func (f *SpeexDenoiser) OutputFormat() core.AudioFormat { return f.fmt }
func (f *SpeexDenoiser) Latency() time.Duration         { return 10 * time.Millisecond }

// PendingSamples reports the partial frame retained between Process calls.
// It is always less than one 10 ms denoiser frame.
func (f *SpeexDenoiser) PendingSamples() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pending)
}

func (f *SpeexDenoiser) Process(ctx context.Context, input []byte) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state == nil {
		return nil, ErrClosed
	}
	if f.flushed {
		return nil, ErrFlushed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(input)%2 != 0 {
		return nil, errors.New("audio: PCM16LE buffer has odd length")
	}
	output := make([]byte, 0, len(input))
	for i := 0; i < len(input); i += 2 {
		// Check before consuming each sample. Once a frame is complete, the
		// bounded native call is committed so cancellation can never strand a
		// full frame in pending state.
		if err := ctx.Err(); err != nil {
			return output, err
		}
		f.pending = append(f.pending, int16(binary.LittleEndian.Uint16(input[i:])))
		if len(f.pending) == f.frame {
			output = f.processFrame(output, f.pending)
			f.pending = f.pending[:0]
		}
	}
	return output, nil
}

func (f *SpeexDenoiser) Flush(ctx context.Context) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state == nil {
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.flushed {
		return nil, nil
	}
	n := len(f.pending)
	out := make([]byte, 0, (f.frame+n)*2)
	if n > 0 {
		for len(f.pending) < f.frame {
			f.pending = append(f.pending, 0)
		}
		out = f.processFrame(out, f.pending)
		f.pending = f.pending[:0]
	}
	if f.primed {
		before := len(out)
		out = f.processFrame(out, make([]int16, f.frame))
		keep := f.frame
		if n > 0 {
			keep = n
		}
		out = out[:before+keep*2]
	}
	f.flushed = true
	return out, nil
}

// processFrame runs one complete native frame. Speex preprocessing delays the
// signal by one frame: the first result is only priming and is intentionally
// withheld. Every later result belongs to the preceding input frame.
func (f *SpeexDenoiser) processFrame(output []byte, frame []int16) []byte {
	C.speex_preprocess_run(f.state, (*C.spx_int16_t)(unsafe.Pointer(&frame[0])))
	if !f.primed {
		f.primed = true
		return output
	}
	for _, sample := range frame {
		output = binary.LittleEndian.AppendUint16(output, uint16(sample))
	}
	return output
}

func (f *SpeexDenoiser) Reset() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state == nil {
		return ErrClosed
	}
	C.speex_preprocess_state_destroy(f.state)
	f.state = nil
	f.pending = nil
	f.primed = false
	f.flushed = false
	return f.newState()
}

func (f *SpeexDenoiser) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state != nil {
		C.speex_preprocess_state_destroy(f.state)
		f.state = nil
	}
	f.pending = nil
	f.primed = false
	f.flushed = true
	return nil
}
