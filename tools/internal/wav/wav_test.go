package wav

import (
	"bytes"
	"math"
	"testing"
)

func TestADPCMRoundTrip(t *testing.T) {
	pcm := make([]byte, 0, 2*16000)
	for i := 0; i < 16000; i++ { // 1 s of 300 Hz + 1.1 kHz
		v := 6000*math.Sin(2*3.14159265*300*float64(i)/16000) + 2500*math.Sin(2*3.14159265*1100*float64(i)/16000)
		pcm = append(pcm, byte(int16(v)), byte(int16(v)>>8))
	}
	w := EncodeIMAWAV(pcm, 16000)
	if len(w) > len(pcm)/3 {
		t.Fatalf("not compressed: %d -> %d", len(pcm), len(w))
	}
	got, rate, err := ReadWAV(w)
	if err != nil || rate != 16000 || len(got) != len(pcm) {
		t.Fatalf("decode: rate=%d len=%d/%d err=%v", rate, len(got), len(pcm), err)
	}
	var sig, noise float64
	for i := 0; i < len(pcm); i += 2 {
		a, b := float64(int16(uint16(pcm[i])|uint16(pcm[i+1])<<8)), float64(int16(uint16(got[i])|uint16(got[i+1])<<8))
		sig += a * a
		noise += (a - b) * (a - b)
	}
	if snr := 10 * math.Log10(sig/noise); snr < 20 {
		t.Fatalf("SNR %.1f dB", snr)
	}
	if p2, _, err := ReadWAV(EncodePCMWAV(pcm, 16000)); err != nil || !bytes.Equal(p2, pcm) {
		t.Fatal("PCM WAV round trip")
	}
}

func TestReadWAVRobust(t *testing.T) {
	pcm := []byte{1, 0, 2, 0, 3, 0}
	good := EncodePCMWAV(pcm, 16000)
	for name, b := range map[string][]byte{
		"empty": nil, "short": good[:10], "no data": good[:36], "not riff": append([]byte("RIFX"), good[4:]...),
	} {
		if _, _, err := ReadWAV(b); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	// A truncated data chunk and an odd-sized chunk (padded) before data still parse.
	if got, _, err := ReadWAV(good[:len(good)-1]); err != nil || len(got) != 4 {
		t.Errorf("truncated: %v %d", err, len(got))
	}
	odd := append(append([]byte{}, good[:36]...), []byte("LIST\x03\x00\x00\x00abc\x00")...)
	odd = append(odd, good[36:]...)
	if got, rate, err := ReadWAV(odd); err != nil || rate != 16000 || !bytes.Equal(got, pcm) {
		t.Errorf("odd chunk: %v %d %v", err, rate, got)
	}
}
