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
)

var ErrFlushed = errors.New("audio: stream flushed; reset before processing more input")

// Resampler is a bounded, mono PCM16 streaming adapter over SpeexDSP's
// band-limited resampler. One instance owns the native state for one stream.
type Resampler struct {
	mu sync.Mutex

	state   *C.SpeexResamplerState
	inRate  int
	outRate int
	input   uint64
	output  uint64
	flushed bool
}

func NewResampler(inRate, outRate int) (*Resampler, error) {
	if inRate <= 0 || outRate <= 0 {
		return nil, fmt.Errorf("audio: sample rates must be positive: %d to %d", inRate, outRate)
	}
	if uint64(inRate) > uint64(^uint32(0)) || uint64(outRate) > uint64(^uint32(0)) {
		return nil, fmt.Errorf("audio: sample rate exceeds SpeexDSP range: %d to %d", inRate, outRate)
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

	result := make([]int16, 0, outputCapacity(len(input), r.inRate, r.outRate))
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
		result = append(result, out...)
		offset += consumed
		r.input += uint64(consumed)
		r.output += uint64(len(out))
	}
	return result, nil
}

func outputCapacity(input, inRate, outRate int) int {
	return int((uint64(input)*uint64(outRate)+uint64(inRate)-1)/uint64(inRate)) + 64
}

func (r *Resampler) processBlock(input []int16) ([]int16, int, error) {
	capacity := outputCapacity(len(input), r.inRate, r.outRate)
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
	remaining := int(target - r.output)
	zeros := make([]int16, int(C.speex_resampler_get_input_latency(r.state))+maxResamplerInputBlock)
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
