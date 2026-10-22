package resample

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sync"
)

var (
	ErrInvalidSampleRate = errors.New("resample: unsupported sample rate conversion")
	ErrInvalidAlignment  = errors.New("resample: audio buffer size not aligned to 16-bit PCM")
)

// Resampler is a stateful streaming resampler for mono 16-bit signed PCM audio.
// It maintains internal phase and boundary state across successive chunks so that
// chunk boundaries do not introduce phase distortion, clicks, or timing drift.
type Resampler struct {
	mu         sync.Mutex
	inRate     int
	outRate    int
	ratio      float64 // inRate / outRate
	phase      float64 // fractional input index accumulator
	lastSample int16   // trailing sample from previous chunk for interpolation
	hasHistory bool
	rem        [2]int16 // 48->16 only: input samples not yet forming a full 3-sample group
	nRem       int
}

// NewResampler creates a new stateful Resampler for converting mono PCM16 audio
// from inRate to outRate. Supported rates include 16000, 24000, 48000 Hz.
func NewResampler(inRate, outRate int) (*Resampler, error) {
	if inRate <= 0 || outRate <= 0 {
		return nil, fmt.Errorf("%w: rates must be positive (in=%d, out=%d)", ErrInvalidSampleRate, inRate, outRate)
	}
	return &Resampler{
		inRate:  inRate,
		outRate: outRate,
		ratio:   float64(inRate) / float64(outRate),
	}, nil
}

// Reset clears the resampler's internal history and phase accumulator.
func (r *Resampler) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.phase = 0
	r.lastSample = 0
	r.hasHistory = false
	r.nRem = 0
}

// ResamplePCM16 processes a chunk of raw 16-bit little-endian mono PCM audio.
// It returns the resampled PCM bytes.
func (r *Resampler) ResamplePCM16(pcmIn []byte) ([]byte, error) {
	if len(pcmIn)%2 != 0 {
		return nil, ErrInvalidAlignment
	}
	if len(pcmIn) == 0 {
		return []byte{}, nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// If inRate equals outRate, pass-through copy
	if r.inRate == r.outRate {
		out := make([]byte, len(pcmIn))
		copy(out, pcmIn)
		return out, nil
	}

	// Downsampling: 48kHz -> 16kHz (ratio 3.0)
	if r.inRate == 48000 && r.outRate == 16000 {
		return r.downsample48To16(pcmIn), nil
	}

	numSamplesIn := len(pcmIn) / 2
	sample := func(i int) float64 { return float64(int16(binary.LittleEndian.Uint16(pcmIn[2*i:]))) }

	// General stateful linear interpolation resampler. The output count is found first (same break
	// rule as the fill loop below) so the result is the only allocation.
	n := 0
	for ph := r.phase; ; ph += r.ratio {
		if idx := int(math.Floor(ph)); idx >= 0 && idx >= numSamplesIn-1 {
			break // needs the next chunk's data to interpolate further
		}
		n++
	}
	out := make([]byte, 2*n)
	phase := r.phase
	for k := 0; k < n; k++ {
		idx := int(math.Floor(phase))
		frac := phase - float64(idx)

		var s0, s1 float64
		if idx < 0 {
			if r.hasHistory {
				s0 = float64(r.lastSample)
			} else {
				s0 = sample(0)
			}
			s1 = sample(0)
		} else {
			s0, s1 = sample(idx), sample(idx+1)
		}

		interpolated := s0 + frac*(s1-s0)
		// Clamp to int16 range
		if interpolated > 32767 {
			interpolated = 32767
		} else if interpolated < -32768 {
			interpolated = -32768
		}
		binary.LittleEndian.PutUint16(out[2*k:], uint16(int16(math.Round(interpolated))))

		phase += r.ratio
	}

	// Update phase and trailing sample for next chunk
	r.phase = phase - float64(numSamplesIn)
	r.lastSample = int16(binary.LittleEndian.Uint16(pcmIn[2*numSamplesIn-2:]))
	r.hasHistory = true
	return out, nil
}

// downsample48To16 applies a 3-tap boxcar anti-aliasing filter before 3:1
// decimation, reading little-endian PCM16 directly from pcmIn. Leftover samples
// (0-2) are carried in r.rem so output is independent of input chunking. The
// result is a fresh exact-size slice (callers may retain it); this is the only
// allocation. Caller holds r.mu.
func (r *Resampler) downsample48To16(pcmIn []byte) []byte {
	n := len(pcmIn) / 2
	groups := (r.nRem + n) / 3
	out := make([]byte, groups*2)
	at := func(j int) int32 { // j indexes the logical stream rem ++ pcmIn
		if j < r.nRem {
			return int32(r.rem[j])
		}
		k := (j - r.nRem) * 2
		return int32(int16(binary.LittleEndian.Uint16(pcmIn[k:])))
	}
	for i := 0; i < groups; i++ {
		avg := int16((at(3*i) + at(3*i+1) + at(3*i+2)) / 3)
		binary.LittleEndian.PutUint16(out[i*2:], uint16(avg))
	}
	left := r.nRem + n - groups*3
	var rem [2]int16
	for k := 0; k < left; k++ {
		rem[k] = int16(at(groups*3 + k))
	}
	r.rem, r.nRem = rem, left
	return out
}
