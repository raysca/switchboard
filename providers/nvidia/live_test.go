//go:build integration

package nvidia

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"math/cmplx"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/transport/resample"
	"govoice/providers/nvidia/rivapb"
)

// Live NVCF checks. Env: NVIDIA_API_KEY, NVIDIA_TTS_FUNCTION_ID,
// NVIDIA_STT_FUNCTION_ID (falls back to NVIDIA_FUNCTION_ID), optional
// NVIDIA_LIVE_OUT (dir for WAVs), NVIDIA_LIVE_WAV (extra 16 kHz mono clip).
// The key is never printed.

func liveEnv(t *testing.T, fnVar string) (key, fn string) {
	t.Helper()
	key, fn = os.Getenv("NVIDIA_API_KEY"), os.Getenv(fnVar)
	if fn == "" {
		fn = os.Getenv("NVIDIA_FUNCTION_ID")
	}
	if key == "" || fn == "" {
		t.Skipf("NVIDIA_API_KEY and %s not set", fnVar)
	}
	return
}

// spectrum reports the share of energy per band and the largest sample step.
func spectrum(pcm []byte, rate int) string {
	n := len(pcm) / 2
	x := make([]float64, n)
	maxStep := 0.0
	for i := range x {
		x[i] = float64(int16(binary.LittleEndian.Uint16(pcm[2*i:])))
		if i > 0 {
			maxStep = math.Max(maxStep, math.Abs(x[i]-x[i-1]))
		}
	}
	const N = 1024
	bands := []float64{0, 300, 1000, 3000, 5000, 8000, 12000}
	energy := make([]float64, len(bands))
	buf := make([]complex128, N)
	for s := 0; s+N <= n; s += N {
		for i := range buf {
			buf[i] = complex(x[s+i]*(0.5-0.5*math.Cos(2*math.Pi*float64(i)/N)), 0)
		}
		fft(buf)
		for k := 1; k < N/2; k++ {
			f := float64(k) * float64(rate) / N
			for b := len(bands) - 1; b >= 0; b-- {
				if f >= bands[b] {
					energy[b] += real(buf[k] * cmplx.Conj(buf[k]))
					break
				}
			}
		}
	}
	tot := 0.0
	for _, e := range energy {
		tot += e
	}
	var sb strings.Builder
	for i, e := range energy {
		fmt.Fprintf(&sb, "%.0f+Hz:%.1f%% ", bands[i], 100*e/math.Max(tot, 1))
	}
	fmt.Fprintf(&sb, "maxStep=%.0f/32768", maxStep)
	return sb.String()
}

func fft(a []complex128) {
	n := len(a)
	for i, j := 1, 0; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j ^= bit
		if i < j {
			a[i], a[j] = a[j], a[i]
		}
	}
	for l := 2; l <= n; l <<= 1 {
		w := cmplx.Exp(complex(0, -2*math.Pi/float64(l)))
		for i := 0; i < n; i += l {
			wn := complex(1, 0)
			for j := 0; j < l/2; j++ {
				u, v := a[i+j], a[i+j+l/2]*wn
				a[i+j], a[i+j+l/2] = u+v, u-v
				wn *= w
			}
		}
	}
}

func saveWAV(t *testing.T, name string, pcm []byte, rate int) {
	dir := os.Getenv("NVIDIA_LIVE_OUT")
	if dir == "" {
		return
	}
	var b bytes.Buffer
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(36+len(pcm)))
	b.WriteString("WAVEfmt ")
	_ = binary.Write(&b, binary.LittleEndian, uint32(16))
	_ = binary.Write(&b, binary.LittleEndian, uint16(1))
	_ = binary.Write(&b, binary.LittleEndian, uint16(1))
	_ = binary.Write(&b, binary.LittleEndian, uint32(rate))
	_ = binary.Write(&b, binary.LittleEndian, uint32(rate*2))
	_ = binary.Write(&b, binary.LittleEndian, uint16(2))
	_ = binary.Write(&b, binary.LittleEndian, uint16(16))
	b.WriteString("data")
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(pcm)))
	b.Write(pcm)
	if err := os.WriteFile(filepath.Join(dir, name), b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Raw probe: sample rates, header check, TTFA, voice validity.
func TestLive_TTSProbe(t *testing.T) {
	key, fn := liveEnv(t, "NVIDIA_TTS_FUNCTION_ID")
	cfg := DefaultTTSConfig()
	cfg.APIKey, cfg.FunctionID = key, fn
	p, err := NewTTSProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	try := func(voice string, rate int) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		start := time.Now()
		st, err := p.client.SynthesizeOnline(p.conn.Context(ctx))
		if err == nil {
			err = st.Send(&rivapb.SynthesizeSpeechRequest{Text: "Hello there, this is a short test.", LanguageCode: "en-US",
				Encoding: rivapb.AudioEncoding_LINEAR_PCM, SampleRateHz: int32(rate), VoiceName: voice})
		}
		if err != nil {
			t.Logf("voice=%q rate=%d: open/send error: %v", voice, rate, SanitizeString(err.Error()))
			return
		}
		_ = st.CloseSend()
		var pcm []byte
		var first time.Duration
		msgs := 0
		for {
			r, err := st.Recv()
			if err != nil {
				if err.Error() != "EOF" {
					t.Logf("voice=%q rate=%d: recv error after %d bytes: %v", voice, rate, len(pcm), SanitizeString(err.Error()))
				}
				break
			}
			if first == 0 && len(r.GetAudio()) > 0 {
				first = time.Since(start)
			}
			msgs++
			pcm = append(pcm, r.GetAudio()...)
		}
		riff := len(pcm) >= 4 && string(pcm[:4]) == "RIFF"
		t.Logf("voice=%q rate=%d: %d bytes, %d msgs, %.2fs audio, TTFA=%v RIFFheader=%v", voice, rate, len(pcm), msgs,
			float64(len(pcm))/2/float64(rate), first.Round(time.Millisecond), riff)
		if len(pcm) > 0 {
			t.Logf("  spectrum: %s", spectrum(pcm, rate))
			saveWAV(t, fmt.Sprintf("probe_%d.wav", rate), pcm, rate)
		}
	}
	for _, r := range []int{24000, 22050, 44100} {
		try(cfg.Voice, r)
	}
	try("No.Such.Voice", 24000)
	try("", 24000)
}

// Three sentences through the processor at the pipeline rate.
func TestLive_TTSProcessor(t *testing.T) {
	key, fn := liveEnv(t, "NVIDIA_TTS_FUNCTION_ID")
	cfg := DefaultTTSConfig()
	cfg.APIKey, cfg.FunctionID = key, fn
	h := startTTS(t, cfg)
	var pcm []byte
	for i, s := range []string{"The quick brown fox jumps over the lazy dog.", "Please turn left at the next intersection.", "Thank you for calling, and have a wonderful day."} {
		t0 := time.Now()
		before := len(h.audio())
		h.say(t, s, i == 2)
		var first time.Duration
		deadline := time.Now().Add(20 * time.Second)
		for h.stopped() <= i && time.Now().Before(deadline) {
			if first == 0 && len(h.audio()) > before {
				first = time.Since(t0)
			}
			time.Sleep(2 * time.Millisecond)
		}
		t.Logf("sentence %d: TTFA=%v total=%v", i, first.Round(time.Millisecond), time.Since(t0).Round(time.Millisecond))
		if e := h.errs(); len(e) > 0 {
			t.Fatalf("errors: %v", e[0].Err)
		}
	}
	for _, f := range h.audio() {
		if f.SampleRate != 24000 || f.Channels != 1 {
			t.Fatalf("frame %d Hz %d ch", f.SampleRate, f.Channels)
		}
		pcm = append(pcm, f.Samples...)
	}
	if len(pcm) == 0 {
		t.Fatal("no audio")
	}
	t.Logf("total %.2fs of audio @24k; %s", float64(len(pcm))/2/24000, spectrum(pcm, 24000))
	saveWAV(t, "tts_24k.wav", pcm, 24000)
	rs, _ := resample.NewResampler(24000, 16000)
	p16, _ := rs.ResamplePCM16(pcm)
	saveWAV(t, "tts_16k.wav", p16, 16000)
}

func wavData(b []byte) []byte {
	i := bytes.Index(b, []byte("data"))
	return b[i+8:]
}

// Streams a 16 kHz clip through the STT processor in real time.
func runSTT(t *testing.T, label string, pcm []byte, model string) {
	key, fn := liveEnv(t, "NVIDIA_STT_FUNCTION_ID")
	cfg := DefaultSTTConfig()
	cfg.APIKey, cfg.FunctionID, cfg.Model = key, fn, model
	h := start(t, cfg)
	t0 := time.Now()
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	var firstInterim, firstFinal time.Duration
	seen := 0
	poll := func() {
		c := h.candidates()
		for ; seen < len(c); seen++ {
			x := c[seen]
			el := time.Since(t0).Round(time.Millisecond)
			if firstInterim == 0 {
				firstInterim = el
			}
			if x.Final && firstFinal == 0 {
				firstFinal = el
			}
			t.Logf("[%s] +%v final=%v eou=%v %q", label, el, x.Final, x.EndOfUtterance, x.Text)
		}
	}
	for off := 0; off+640 <= len(pcm); off += 640 {
		_ = h.sess.Post(&core.InputAudioFrame{Samples: pcm[off : off+640], SampleRate: 16000, Channels: 1, Gen: 1})
		time.Sleep(20 * time.Millisecond)
		poll()
	}
	tail := make([]byte, 640)
	for i := 0; i < 25; i++ { // 0.5 s trailing silence like VAD hangover
		_ = h.sess.Post(&core.InputAudioFrame{Samples: tail, SampleRate: 16000, Channels: 1, Gen: 1})
		time.Sleep(20 * time.Millisecond)
	}
	stopAt := time.Now()
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{})
	var last *core.TranscriptCandidateFrame
	for until := time.Now().Add(6 * time.Second); time.Now().Before(until); time.Sleep(10 * time.Millisecond) {
		poll()
		c := h.candidates()
		if len(c) > 0 && c[len(c)-1].EndOfUtterance {
			last = c[len(c)-1]
			break
		}
	}
	if last == nil {
		t.Errorf("[%s] no end-of-utterance final within 6s; errors=%v", label, h.errs())
		return
	}
	t.Logf("[%s] EOU final %v after stop: %q (first candidate %v, first final %v)", label, time.Since(stopAt).Round(time.Millisecond), last.Text, firstInterim, firstFinal)
}

func TestLive_STT(t *testing.T) {
	data, err := os.ReadFile(os.Getenv("NVIDIA_LIVE_OUT") + "/tts_16k.wav")
	if err != nil {
		t.Skip("run TestLive_TTSProcessor first (NVIDIA_LIVE_OUT)")
	}
	runSTT(t, "tts", wavData(data), os.Getenv("NVIDIA_STT_MODEL"))
	if f := os.Getenv("NVIDIA_LIVE_WAV"); f != "" {
		d, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		runSTT(t, "voice-debug", wavData(d), os.Getenv("NVIDIA_STT_MODEL"))
	}
}
