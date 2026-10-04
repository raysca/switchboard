package audio

/*
#include <speex/speex_resampler.h>
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"unsafe"
)

const (
	resamplerQuality       = C.SPEEX_RESAMPLER_QUALITY_DESKTOP
	maxResamplerInputBlock = 4096
	maxResamplerLatency    = 4096
	resamplerOutputSlack   = 64

	// MaxResamplerInputSamples bounds caller work and allocation per Process.
	MaxResamplerInputSamples = 48000
	// MaxResamplerOutputSamples is the largest allocation Process or a native
	// block may request (one second at the maximum supported 6:1 ratio, plus
	// bounded filter slack).
	MaxResamplerOutputSamples = MaxResamplerInputSamples*6 + resamplerOutputSlack
)

var (
	ErrFlushed                = errors.New("audio: stream flushed; reset before processing more input")
	ErrUnsupportedSampleRate  = errors.New("audio: unsupported sample rate")
	ErrResamplerInputTooLarge = errors.New("audio: resampler input exceeds per-call bound")
)

// Resampler is a bounded, mono PCM16 streaming adapter over SpeexDSP's
// band-limited resampler. One instance owns the native state for one stream.
type Resampler struct {
	mu sync.Mutex

	state   *C.SpeexResamplerState
	inRate  int
	outRate int
	input   uint64
	output  uint64
	latency int
	flushed bool
}

func NewResampler(inRate, outRate int) (*Resampler, error) {
	if !supportedRate(inRate) || !supportedRate(outRate) {
		return nil, fmt.Errorf("%w: %d to %d", ErrUnsupportedSampleRate, inRate, outRate)
	}
	r := &Resampler{inRate: inRate, outRate: outRate}
	if err := r.newState(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Resampler) newState() error {
	var code C.int
	r.state = C.speex_resampler_init(1, C.spx_uint32_t(r.inRate), C.spx_uint32_t(r.outRate), resamplerQuality, &code)
	if r.state == nil || code != C.RESAMPLER_ERR_SUCCESS {
		r.state = nil
		return resamplerError("initialize", code)
	}
	// Dropping the leading filter zeros makes a finite stream align with its
	// source. Flush supplies the matching tail and truncates to the exact
	// rational output length.
	if code = C.speex_resampler_skip_zeros(r.state); code != C.RESAMPLER_ERR_SUCCESS {
		C.speex_resampler_destroy(r.state)
		r.state = nil
		return resamplerError("skip leading zeros", code)
	}
	r.latency = int(C.speex_resampler_get_input_latency(r.state))
	if r.latency < 0 || r.latency > maxResamplerLatency {
		C.speex_resampler_destroy(r.state)
		r.state = nil
		return fmt.Errorf("audio: SpeexDSP input latency %d exceeds bound %d", r.latency, maxResamplerLatency)
	}
	return nil
}

func resamplerError(op string, code C.int) error {
	return fmt.Errorf("audio: SpeexDSP resampler %s: %s", op, C.GoString(C.speex_resampler_strerror(code)))
}

// Process consumes all samples or returns an error. Context cancellation is
// checked between bounded native calls so large caller chunks remain
// interruptible without retaining the caller's buffer.
func (r *Resampler) Process(ctx context.Context, input []int16) ([]int16, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == nil {
		return nil, ErrClosed
	}
	if r.flushed {
		return nil, ErrFlushed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	capacity, err := outputCapacity(len(input), r.inRate, r.outRate)
	if err != nil {
		return nil, err
	}

	result := make([]int16, 0, capacity)
	for offset := 0; offset < len(input); {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		end := offset + maxResamplerInputBlock
		if end > len(input) {
			end = len(input)
		}
		out, consumed, err := r.processBlock(input[offset:end])
		if err != nil {
			return result, err
		}
		if consumed == 0 {
			return result, errors.New("audio: SpeexDSP resampler made no input progress")
		}
		if len(out) > MaxResamplerOutputSamples-len(result) {
			return result, fmt.Errorf("audio: resampler output exceeds per-call bound %d", MaxResamplerOutputSamples)
		}
		result = append(result, out...)
		offset += consumed
		r.input += uint64(consumed)
		r.output += uint64(len(out))
	}
	return result, nil
}

func outputCapacity(input, inRate, outRate int) (int, error) {
	if input < 0 || input > MaxResamplerInputSamples {
		return 0, ErrResamplerInputTooLarge
	}
	product := uint64(input) * uint64(outRate)
	capacity := (product+uint64(inRate)-1)/uint64(inRate) + resamplerOutputSlack
	if capacity > MaxResamplerOutputSamples || capacity > uint64(^uint32(0)) {
		return 0, fmt.Errorf("audio: resampler output allocation %d exceeds bound %d", capacity, MaxResamplerOutputSamples)
	}
	return int(capacity), nil
}

func (r *Resampler) processBlock(input []int16) ([]int16, int, error) {
	capacity, err := outputCapacity(len(input), r.inRate, r.outRate)
	if err != nil {
		return nil, 0, err
	}
	output := make([]int16, capacity)
	inLen := C.spx_uint32_t(len(input))
	outLen := C.spx_uint32_t(len(output))
	code := C.speex_resampler_process_int(
		r.state,
		0,
		(*C.spx_int16_t)(unsafe.Pointer(&input[0])),
		&inLen,
		(*C.spx_int16_t)(unsafe.Pointer(&output[0])),
		&outLen,
	)
	if code != C.RESAMPLER_ERR_SUCCESS {
		return nil, int(inLen), resamplerError("process", code)
	}
	return output[:int(outLen)], int(inLen), nil
}

// Flush emits the finite stream's filter tail and fixes the result to
// floor(totalInput*outRate/inRate) samples. It is idempotent.
func (r *Resampler) Flush(ctx context.Context) ([]int16, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == nil {
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.flushed {
		return nil, nil
	}
	r.flushed = true
	target := r.input * uint64(r.outRate) / uint64(r.inRate)
	if r.output >= target {
		return nil, nil
	}
	remaining64 := target - r.output
	if remaining64 > MaxResamplerOutputSamples {
		r.flushed = false
		return nil, fmt.Errorf("audio: resampler flush output %d exceeds bound %d", remaining64, MaxResamplerOutputSamples)
	}
	remaining := int(remaining64)
	if r.latency == 0 {
		r.flushed = false
		return nil, errors.New("audio: SpeexDSP resampler has no bounded flush latency")
	}
	zeros := make([]int16, r.latency)
	result := make([]int16, 0, remaining)
	for len(result) < remaining {
		if err := ctx.Err(); err != nil {
			r.flushed = false
			return result, err
		}
		out, consumed, err := r.processBlock(zeros)
		if err != nil {
			r.flushed = false
			return result, err
		}
		if consumed == 0 || len(out) == 0 {
			r.flushed = false
			return result, errors.New("audio: SpeexDSP resampler made no flush progress")
		}
		if len(out) > remaining-len(result) {
			out = out[:remaining-len(result)]
		}
		result = append(result, out...)
	}
	r.output += uint64(len(result))
	return result, nil
}

func (r *Resampler) Reset() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == nil {
		return ErrClosed
	}
	if code := C.speex_resampler_reset_mem(r.state); code != C.RESAMPLER_ERR_SUCCESS {
		return resamplerError("reset", code)
	}
	if code := C.speex_resampler_skip_zeros(r.state); code != C.RESAMPLER_ERR_SUCCESS {
		return resamplerError("skip leading zeros after reset", code)
	}
	r.input = 0
	r.output = 0
	r.flushed = false
	return nil
}

func (r *Resampler) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state != nil {
		C.speex_resampler_destroy(r.state)
		r.state = nil
	}
	r.input = 0
	r.output = 0
	r.flushed = true
	return nil
}
