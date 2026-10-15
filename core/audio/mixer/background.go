package mixer

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"

	"govoice/core/transport/resample"
)

// MaxFileBytes caps the ambience file size (operator config, read once).
const MaxFileBytes = 10 << 20

// Asset is decoded, resampled and gain-scaled ambience, immutable after load
// and shared read-only by every session. Each session plays it through its
// own Cursor.
type Asset struct {
	pcm  []int16
	rate int
}

// LoadAsset reads a PCM16 mono WAV, resamples it once to rate and bakes in
// gainDB (-60..0).
func LoadAsset(path string, rate int, gainDB float64) (*Asset, error) {
	if rate <= 0 {
		return nil, errors.New("mixer: rate must be positive")
	}
	if math.IsNaN(gainDB) || gainDB < -60 || gainDB > 0 {
		return nil, fmt.Errorf("mixer: background gain_db %v out of range -60..0", gainDB)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxFileBytes {
		return nil, fmt.Errorf("mixer: %s exceeds %d bytes", path, MaxFileBytes)
	}
	return newAsset(b, rate, gainDB)
}

func newAsset(wav []byte, rate int, gainDB float64) (*Asset, error) {
	pcm, srcRate, err := parseWAV(wav)
	if err != nil {
		return nil, err
	}
	if srcRate > rate {
		pcm = lowPass(pcm, float64(rate)*0.45/float64(srcRate))
	}
	if srcRate != rate {
		r, err := resample.NewResampler(srcRate, rate)
		if err != nil {
			return nil, err
		}
		if pcm, err = r.ResamplePCM16(pcm); err != nil {
			return nil, err
		}
	}
	n := len(pcm) / 2
	if n < rate/50 { // shorter than one 20 ms frame
		return nil, errors.New("mixer: ambience shorter than 20 ms")
	}
	q := GainQ(gainDB)
	a := &Asset{pcm: make([]int16, n), rate: rate}
	for i := range a.pcm {
		a.pcm[i] = Scale(int16(binary.LittleEndian.Uint16(pcm[2*i:])), q)
	}
	// Crossfade the last 5 ms into the first 5 ms and drop them, so the loop
	// point has no step (click). A one-shot just starts with that blend.
	if x := rate / 200; n >= 4*x {
		for i := 0; i < x; i++ {
			t := n - x + i
			a.pcm[i] = int16((int32(a.pcm[i])*int32(i) + int32(a.pcm[t])*int32(x-i)) / int32(x))
		}
		a.pcm = a.pcm[:n-x]
	}
	return a, nil
}

// lowPass is a 63-tap Hann-windowed sinc FIR (cutoff fc, in cycles/sample)
// run once at load so linear-resampling a 44.1/48 kHz file does not alias.
func lowPass(b []byte, fc float64) []byte {
	const half = 31
	var h [2*half + 1]float64
	var sum float64
	for k := range h {
		m := float64(k - half)
		v := 2 * fc
		if m != 0 {
			v = math.Sin(2*math.Pi*fc*m) / (math.Pi * m)
		}
		v *= 0.5 - 0.5*math.Cos(2*math.Pi*float64(k)/(2*half))
		h[k], sum = v, sum+v
	}
	n := len(b) / 2
	out := make([]byte, 2*n)
	for i := 0; i < n; i++ {
		var acc float64
		for k := range h {
			if j := i + k - half; j >= 0 && j < n {
				acc += h[k] * float64(int16(binary.LittleEndian.Uint16(b[2*j:])))
			}
		}
		binary.LittleEndian.PutUint16(out[2*i:], uint16(clamp(int32(math.Round(acc/sum)))))
	}
	return out
}

// parseWAV returns the PCM16 mono payload and its rate.
func parseWAV(b []byte) ([]byte, int, error) {
	if len(b) < 12 || string(b[:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return nil, 0, errors.New("mixer: not a RIFF/WAVE file")
	}
	rate, haveFmt := 0, false
	for p := 12; p+8 <= len(b); {
		id, size := string(b[p:p+4]), int(binary.LittleEndian.Uint32(b[p+4:]))
		body := p + 8
		end := min(body+size, len(b)) // tolerate a truncated or streamed data size
		switch id {
		case "fmt ":
			if end-body < 16 {
				return nil, 0, errors.New("mixer: short fmt chunk")
			}
			tag := binary.LittleEndian.Uint16(b[body:])
			ch := binary.LittleEndian.Uint16(b[body+2:])
			bits := binary.LittleEndian.Uint16(b[body+14:])
			if tag == 0xFFFE && end-body >= 26 { // WAVE_FORMAT_EXTENSIBLE: real tag leads the SubFormat GUID
				tag = binary.LittleEndian.Uint16(b[body+24:])
			}
			if tag != 1 || ch != 1 || bits != 16 {
				return nil, 0, fmt.Errorf("mixer: need PCM16 mono WAV (format %d, %d ch, %d bit); convert with e.g. sox in.wav -c 1 -b 16 out.wav", tag, ch, bits)
			}
			rate, haveFmt = int(binary.LittleEndian.Uint32(b[body+4:])), true
			if rate < 8000 || rate > 192000 { // bounds the resampled size (a 1 Hz header would ask for gigabytes)
				return nil, 0, fmt.Errorf("mixer: WAV rate %d outside 8000..192000", rate)
			}
		case "data":
			if !haveFmt || rate <= 0 {
				return nil, 0, errors.New("mixer: data before fmt chunk")
			}
			return b[body : end-(end-body)%2], rate, nil
		}
		p = body + size + size%2
	}
	return nil, 0, errors.New("mixer: no data chunk")
}

// Rate is the sample rate of the asset (and of every Cursor over it).
func (a *Asset) Rate() int { return a.rate }

// NewCursor starts a session's playback at the beginning.
func (a *Asset) NewCursor(loop bool) *Cursor { return &Cursor{a: a, loop: loop, q: unityQ} }

// NewCursorGain is NewCursor with the session's own level on top of whatever the asset has baked in, so
// one shared asset serves sessions that want it at different volumes.
func (a *Asset) NewCursorGain(loop bool, gainDB float64) *Cursor {
	return &Cursor{a: a, loop: loop, q: GainQ(gainDB)}
}

const unityQ = 1 << gainShift

// Cursor is one session's playback position. Not safe for concurrent use: the
// paced writer drives it from its single Run goroutine.
type Cursor struct {
	a    *Asset
	pos  int
	loop bool
	done bool
	q    int32 // fixed-point session gain, unityQ = none
}

func (c *Cursor) Rate() int    { return c.a.rate }
func (c *Cursor) Active() bool { return !c.done }

// Mix writes PCM16LE to dst: the ambience alone when src is nil, else
// src + ambience, saturating. src (if given) must be len(dst) bytes. It
// advances the position by len(dst)/2 samples and never allocates. A
// non-looping asset ends with silence and then reports Active() == false.
func (c *Cursor) Mix(dst, src []byte) {
	pcm := c.a.pcm
	for i := 0; i+1 < len(dst); i += 2 {
		var s int32
		if !c.done {
			s = int32(pcm[c.pos])
			if c.q != unityQ {
				s = int32(Scale(int16(s), c.q))
			}
			if c.pos++; c.pos == len(pcm) {
				c.pos, c.done = 0, !c.loop
			}
		}
		if src != nil {
			s += int32(int16(binary.LittleEndian.Uint16(src[i:])))
		}
		binary.LittleEndian.PutUint16(dst[i:], uint16(clamp(s)))
	}
}
