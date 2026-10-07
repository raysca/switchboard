// Package mixer is the outbound audio stage: a gain control (Processor) and a
// looping background-ambience source (Asset/Cursor) that the WebSocket paced
// writer mixes under the bot's speech. See docs/audio-filters.md.
package mixer

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"

	"govoice/core"
)

// Gain range accepted for audio.output_gain_db.
const (
	MinGainDB = -30.0
	MaxGainDB = 6.0
)

const gainShift = 12 // Q12: +6 dB (x1.995) times int16 stays inside int32

// GainQ returns the fixed-point multiplier for db (4096 = unity).
func GainQ(db float64) int32 { return int32(math.Round(math.Pow(10, db/20) * (1 << gainShift))) }

// Scale returns s*q/4096 rounded and clamped to int16.
func Scale(s int16, q int32) int16 {
	v := (int32(s)*q + 1<<(gainShift-1)) >> gainShift
	return clamp(v)
}

func clamp(v int32) int16 {
	if v > math.MaxInt16 {
		return math.MaxInt16
	}
	if v < math.MinInt16 {
		return math.MinInt16
	}
	return int16(v)
}

// ValidateGainDB checks the output gain range.
func ValidateGainDB(db float64) error {
	if math.IsNaN(db) || db < MinGainDB || db > MaxGainDB {
		return fmt.Errorf("output_gain_db %v out of range %v..%v", db, MinGainDB, MaxGainDB)
	}
	return nil
}

// Gain scales OutputAudioFrames (PCM16LE). Frames are copy-on-write: the
// incoming frame and its buffer are never mutated. Build it only when the gain
// is not 0 dB; at 0 dB the pipeline carries no extra stage.
type Gain struct{ q int32 }

func NewGain(db float64) (*Gain, error) {
	if err := ValidateGainDB(db); err != nil {
		return nil, err
	}
	return &Gain{q: GainQ(db)}, nil
}

func (g *Gain) ProcessFrame(_ context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	a, ok := f.(*core.OutputAudioFrame)
	if !ok || dir != core.Downstream || len(a.Samples) == 0 || len(a.Samples)%2 != 0 ||
		(a.Encoding != core.EncodingPCM16LE && a.Encoding != "") {
		return e.PushDirection(f, dir)
	}
	cp := *a
	cp.Samples = make([]byte, len(a.Samples))
	for i := 0; i < len(a.Samples); i += 2 {
		s := Scale(int16(binary.LittleEndian.Uint16(a.Samples[i:])), g.q)
		binary.LittleEndian.PutUint16(cp.Samples[i:], uint16(s))
	}
	return e.PushDirection(&cp, dir)
}
