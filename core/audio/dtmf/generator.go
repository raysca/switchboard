package dtmf

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"
)

// GeneratorConfig controls tone synthesis. Defaults: 80 ms tone, 80 ms gap
// (inside the 70-100 ms range telephony IVRs accept), -10 dBFS peak for the
// summed pair.
type GeneratorConfig struct {
	SampleRate int
	Tone, Gap  time.Duration
	// LevelDBFS is the peak level of the summed dual tone (<= 0).
	LevelDBFS float64
}

func DefaultGeneratorConfig(sampleRate int) GeneratorConfig {
	return GeneratorConfig{SampleRate: sampleRate, Tone: 80 * time.Millisecond, Gap: 80 * time.Millisecond, LevelDBFS: -10}
}

const rampTime = 4 * time.Millisecond // raised-cosine edges avoid clicks and splatter

func (c GeneratorConfig) validate() error {
	if c.SampleRate < 8000 || c.Tone < 40*time.Millisecond || c.Tone > 500*time.Millisecond ||
		c.Gap < 40*time.Millisecond || c.Gap > time.Second || c.LevelDBFS > 0 || c.LevelDBFS < -60 {
		return errors.New("dtmf: invalid generator config")
	}
	return nil
}

// Generate renders digits as PCM16LE mono: each digit is Tone of dual-tone then
// Gap of silence (the trailing gap is kept so back-to-back sends stay separated).
func Generate(digits string, cfg GeneratorConfig) ([]byte, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	rate := float64(cfg.SampleRate)
	toneN := int(rate * cfg.Tone.Seconds())
	gapN := int(rate * cfg.Gap.Seconds())
	rampN := int(rate * rampTime.Seconds())
	amp := math.Pow(10, cfg.LevelDBFS/20) * 32767 / 2 // two equal tones sum to the peak
	out := make([]byte, 0, len(digits)*(toneN+gapN)*2)
	for i := 0; i < len(digits); i++ {
		lo, hi, ok := digitFreqs(digits[i])
		if !ok {
			return nil, fmt.Errorf("dtmf: invalid digit %q", digits[i])
		}
		for n := 0; n < toneN; n++ {
			env := 1.0
			if n < rampN {
				env = 0.5 - 0.5*math.Cos(math.Pi*float64(n)/float64(rampN))
			} else if n >= toneN-rampN {
				env = 0.5 - 0.5*math.Cos(math.Pi*float64(toneN-1-n)/float64(rampN))
			}
			t := float64(n) / rate
			v := amp * env * (math.Sin(2*math.Pi*lo*t) + math.Sin(2*math.Pi*hi*t))
			out = binary.LittleEndian.AppendUint16(out, uint16(int16(math.Round(v))))
		}
		out = append(out, make([]byte, gapN*2)...)
	}
	return out, nil
}
