package wav

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
)

// Clips are committed as IMA ADPCM WAV (4 bits/sample) to stay under the
// 150 KB fixture budget; ReadWAV returns 16-bit PCM for both encodings.

var imaIndex = [8]int{-1, -1, -1, -1, 2, 4, 6, 8}
var imaStep = [89]int{7, 8, 9, 10, 11, 12, 13, 14, 16, 17, 19, 21, 23, 25, 28, 31, 34, 37, 41, 45, 50, 55, 60, 66, 73, 80, 88, 97, 107, 118, 130, 143, 157, 173, 190, 209, 230, 253, 279, 307, 337, 371, 408, 449, 494, 544, 598, 658, 724, 796, 876, 963, 1060, 1166, 1282, 1411, 1552, 1707, 1878, 2066, 2272, 2499, 2749, 3024, 3327, 3660, 4026, 4428, 4871, 5358, 5894, 6484, 7132, 7845, 8630, 9493, 10442, 11487, 12635, 13899, 15289, 16818, 18500, 20350, 22385, 24623, 27086, 29794, 32767}

const (
	imaBlock   = 256
	imaPerBlk  = (imaBlock-4)*2 + 1
	wavFmtPCM  = 1
	wavFmtIMA  = 0x11
	imaHeadLen = 4
)

type ima struct{ pred, idx int }

func (s *ima) decode(code int) int {
	step := imaStep[s.idx]
	v := step >> 3
	if code&4 != 0 {
		v += step
	}
	if code&2 != 0 {
		v += step >> 1
	}
	if code&1 != 0 {
		v += step >> 2
	}
	if code&8 != 0 {
		s.pred -= v
	} else {
		s.pred += v
	}
	s.pred = max(-32768, min(32767, s.pred))
	s.idx = max(0, min(88, s.idx+imaIndex[code&7]))
	return s.pred
}

func (s *ima) encode(sample int) int {
	step := imaStep[s.idx]
	diff, code := sample-s.pred, 0
	if diff < 0 {
		code, diff = 8, -diff
	}
	if diff >= step {
		code |= 4
		diff -= step
	}
	if diff >= step>>1 {
		code |= 2
		diff -= step >> 1
	}
	if diff >= step>>2 {
		code |= 1
	}
	s.decode(code)
	return code
}

// EncodeIMAWAV encodes mono 16-bit PCM as an IMA ADPCM WAV file.
func EncodeIMAWAV(pcm []byte, rate int) []byte {
	n := len(pcm) / 2
	sample := func(i int) int {
		if i >= n {
			return 0
		}
		return int(int16(binary.LittleEndian.Uint16(pcm[2*i:])))
	}
	var data bytes.Buffer
	for pos := 0; pos < n; pos += imaPerBlk {
		st := ima{pred: sample(pos)}
		blk := make([]byte, imaBlock)
		binary.LittleEndian.PutUint16(blk, uint16(int16(st.pred)))
		blk[2] = byte(st.idx)
		for k := 0; k < imaPerBlk-1; k += 2 {
			lo := st.encode(sample(pos + 1 + k))
			hi := st.encode(sample(pos + 2 + k))
			blk[imaHeadLen+k/2] = byte(lo | hi<<4)
		}
		data.Write(blk)
	}
	var b bytes.Buffer
	w := func(v any) { _ = binary.Write(&b, binary.LittleEndian, v) }
	fmtLen := uint32(20)
	b.WriteString("RIFF")
	w(uint32(4 + 8 + fmtLen + 8 + 4 + 8 + uint32(data.Len())))
	b.WriteString("WAVEfmt ")
	w(fmtLen)
	w(uint16(wavFmtIMA))
	w(uint16(1))
	w(uint32(rate))
	w(uint32(rate * imaBlock / imaPerBlk))
	w(uint16(imaBlock))
	w(uint16(4))
	w(uint16(2))
	w(uint16(imaPerBlk))
	b.WriteString("fact")
	w(uint32(4))
	w(uint32(n))
	b.WriteString("data")
	w(uint32(data.Len()))
	b.Write(data.Bytes())
	return b.Bytes()
}

// EncodePCMWAV wraps 16-bit mono PCM in a plain WAV header.
func EncodePCMWAV(pcm []byte, rate int) []byte {
	var b bytes.Buffer
	w := func(v any) { _ = binary.Write(&b, binary.LittleEndian, v) }
	b.WriteString("RIFF")
	w(uint32(36 + len(pcm)))
	b.WriteString("WAVEfmt ")
	w(uint32(16))
	w(uint16(wavFmtPCM))
	w(uint16(1))
	w(uint32(rate))
	w(uint32(rate * 2))
	w(uint16(2))
	w(uint16(16))
	b.WriteString("data")
	w(uint32(len(pcm)))
	b.Write(pcm)
	return b.Bytes()
}

// ReadWAV decodes a mono PCM16 or IMA ADPCM WAV into 16-bit PCM.
func ReadWAV(b []byte) (pcm []byte, rate int, err error) {
	if len(b) < 12 || string(b[:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return nil, 0, errors.New("not a WAV file")
	}
	var tag, ch, bits, align int
	var data []byte
	facts := -1
	for p := 12; p+8 <= len(b); {
		id, n := string(b[p:p+4]), int(binary.LittleEndian.Uint32(b[p+4:]))
		body := b[p+8 : min(len(b), p+8+n)]
		switch id {
		case "fmt ":
			if len(body) < 16 {
				return nil, 0, errors.New("short fmt chunk")
			}
			tag, ch = int(binary.LittleEndian.Uint16(body)), int(binary.LittleEndian.Uint16(body[2:]))
			rate, align, bits = int(binary.LittleEndian.Uint32(body[4:])), int(binary.LittleEndian.Uint16(body[12:])), int(binary.LittleEndian.Uint16(body[14:]))
		case "fact":
			if len(body) >= 4 {
				facts = int(binary.LittleEndian.Uint32(body))
			}
		case "data":
			data = body
		}
		p += 8 + n + n&1
	}
	if ch != 1 || data == nil {
		return nil, 0, errors.New("want mono WAV with a data chunk")
	}
	switch {
	case tag == wavFmtPCM && bits == 16:
		return data[:len(data)&^1], rate, nil
	case tag == wavFmtIMA && align == imaBlock:
		out := make([]byte, 0, len(data)*4)
		for off := 0; off+imaBlock <= len(data); off += imaBlock {
			blk := data[off : off+imaBlock]
			st := ima{pred: int(int16(binary.LittleEndian.Uint16(blk))), idx: min(88, int(blk[2]))}
			out = binary.LittleEndian.AppendUint16(out, uint16(int16(st.pred)))
			for _, by := range blk[imaHeadLen:] {
				out = binary.LittleEndian.AppendUint16(out, uint16(int16(st.decode(int(by&15)))))
				out = binary.LittleEndian.AppendUint16(out, uint16(int16(st.decode(int(by>>4)))))
			}
		}
		if facts >= 0 && facts*2 <= len(out) {
			out = out[:facts*2]
		}
		return out, rate, nil
	}
	return nil, 0, errors.New("unsupported WAV encoding")
}

// RMS is the root-mean-square level of 16-bit PCM (0..32768).
func RMS(pcm []byte) float64 {
	n := len(pcm) / 2
	if n == 0 {
		return 0
	}
	var sum float64
	for i := 0; i < n; i++ {
		v := float64(int16(binary.LittleEndian.Uint16(pcm[2*i:])))
		sum += v * v
	}
	return math.Sqrt(sum / float64(n))
}

// Silence returns d seconds of 16-bit mono silence at rate.
func Silence(rate int, seconds float64) []byte { return make([]byte, int(float64(rate)*seconds)*2) }
