package audio

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"govoice/core"
)

var (
	ErrInvalidDTMFDigit = errors.New("audio: invalid DTMF digit (must be 0-9, *, #, or A-D)")
	ErrInvalidToneFreq  = errors.New("audio: invalid tone frequency (must be between 20 Hz and 20,000 Hz)")
	ErrInvalidDuration  = errors.New("audio: duration must be positive and bounded")
)

const (
	MaxToneDuration = 5 * time.Second
)

// Standard DTMF Dual Frequencies (Hz)
var dtmfFrequencies = map[rune][2]float64{
	'1': {697, 1209},
	'2': {697, 1336},
	'3': {697, 1477},
	'A': {697, 1633},
	'4': {770, 1209},
	'5': {770, 1336},
	'6': {770, 1477},
	'B': {770, 1633},
	'7': {852, 1209},
	'8': {852, 1336},
	'9': {852, 1477},
	'C': {852, 1633},
	'*': {941, 1209},
	'0': {941, 1336},
	'#': {941, 1477},
	'D': {941, 1633},
}

// DTMFFrame represents a transport-neutral DTMF digit event (e.g., telephone keypad input).
type DTMFFram struct {
	Digit    rune          `json:"digit"`
	Duration time.Duration `json:"duration"`
	Turn     string        `json:"turn,omitempty"`
	Gen      uint64        `json:"gen,omitempty"`
}

// DTMFFrame implements core.Frame and core.GenerationalFrame.
type DTMFFrame struct {
	Digit    rune          `json:"digit"`
	Duration time.Duration `json:"duration"`
	Turn     string        `json:"turn,omitempty"`
	Gen      uint64        `json:"gen,omitempty"`
}

func (f *DTMFFrame) Type() core.FrameType    { return core.FrameDTMF }
func (f *DTMFFrame) Generation() uint64      { return f.Gen }

// Validate verifies that the DTMF digit and duration conform to standards.
func (f *DTMFFrame) Validate() error {
	ch := strings.ToUpper(string(f.Digit))
	if len(ch) != 1 {
		return ErrInvalidDTMFDigit
	}
	r := rune(ch[0])
	if _, ok := dtmfFrequencies[r]; !ok {
		return fmt.Errorf("%w: %q", ErrInvalidDTMFDigit, f.Digit)
	}
	if f.Duration <= 0 || f.Duration > MaxToneDuration {
		return fmt.Errorf("%w: %v", ErrInvalidDuration, f.Duration)
	}
	return nil
}

// Frequencies returns the standard dual-tone frequencies in Hz for this DTMF digit.
func (f *DTMFFrame) Frequencies() ([2]float64, error) {
	ch := strings.ToUpper(string(f.Digit))
	if len(ch) != 1 {
		return [2]float64{}, ErrInvalidDTMFDigit
	}
	freqs, ok := dtmfFrequencies[rune(ch[0])]
	if !ok {
		return [2]float64{}, fmt.Errorf("%w: %q", ErrInvalidDTMFDigit, f.Digit)
	}
	return freqs, nil
}

// ToneFrame represents a transport-neutral arbitrary audio tone (single or dual frequency).
type ToneFrame struct {
	Frequencies []float64     `json:"frequencies"`
	Duration    time.Duration `json:"duration"`
	Volume      float64       `json:"volume"` // 0.0 to 1.0
	Turn        string        `json:"turn,omitempty"`
	Gen         uint64        `json:"gen,omitempty"`
}

func (f *ToneFrame) Type() core.FrameType    { return core.FrameTone }
func (f *ToneFrame) Generation() uint64      { return f.Gen }

// Validate verifies the frequencies, volume, and duration.
func (f *ToneFrame) Validate() error {
	if len(f.Frequencies) == 0 || len(f.Frequencies) > 4 {
		return errors.New("audio: tone must specify between 1 and 4 frequencies")
	}
	for _, freq := range f.Frequencies {
		if freq < 20 || freq > 20000 {
			return fmt.Errorf("%w: %v", ErrInvalidToneFreq, freq)
		}
	}
	if f.Duration <= 0 || f.Duration > MaxToneDuration {
		return fmt.Errorf("%w: %v", ErrInvalidDuration, f.Duration)
	}
	if f.Volume < 0.0 || f.Volume > 1.0 {
		return errors.New("audio: tone volume must be between 0.0 and 1.0")
	}
	return nil
}
