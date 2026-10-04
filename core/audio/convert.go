package audio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// Float32ToPCM16LE converts finite normalized samples with saturation.
func Float32ToPCM16LE(samples []float32) ([]byte, error) {
	out := make([]byte, len(samples)*2)
	for i, v := range samples {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return nil, fmt.Errorf("audio: nonfinite float sample at %d", i)
		}
		var s int16
		switch {
		case v <= -1:
			s = -32768
		case v >= 1:
			s = 32767
		default:
			scaled := math.Round(float64(v) * 32768)
			if scaled > 32767 {
				scaled = 32767
			}
			s = int16(scaled)
		}
		binary.LittleEndian.PutUint16(out[2*i:], uint16(s))
	}
	return out, nil
}

func PCM16LEToFloat32(pcm []byte) ([]float32, error) {
	if len(pcm)%2 != 0 {
		return nil, errors.New("audio: PCM16LE buffer has odd length")
	}
	out := make([]float32, len(pcm)/2)
	for i := range out {
		out[i] = float32(int16(binary.LittleEndian.Uint16(pcm[2*i:]))) / 32768
	}
	return out, nil
}

// ConvertChannels maps mono to stereo by duplication and stereo to mono by
// arithmetic averaging with integer truncation toward zero.
func ConvertChannels(pcm []byte, from, to int) ([]byte, error) {
	if (from != 1 && from != 2) || (to != 1 && to != 2) {
		return nil, fmt.Errorf("audio: unsupported channel map %d to %d", from, to)
	}
	if len(pcm)%(2*from) != 0 {
		return nil, errors.New("audio: incomplete PCM frame")
	}
	if from == to {
		return append([]byte(nil), pcm...), nil
	}
	frames := len(pcm) / (2 * from)
	out := make([]byte, frames*2*to)
	for i := 0; i < frames; i++ {
		if from == 1 {
			v := binary.LittleEndian.Uint16(pcm[2*i:])
			binary.LittleEndian.PutUint16(out[4*i:], v)
			binary.LittleEndian.PutUint16(out[4*i+2:], v)
		} else {
			l := int32(int16(binary.LittleEndian.Uint16(pcm[4*i:])))
			r := int32(int16(binary.LittleEndian.Uint16(pcm[4*i+2:])))
			binary.LittleEndian.PutUint16(out[2*i:], uint16(int16((l+r)/2)))
		}
	}
	return out, nil
}
