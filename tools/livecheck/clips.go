package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"govoice/providers/google"
	"govoice/providers/nvidia"
	"govoice/providers/nvidia/rivapb"
	"govoice/server/catalog"
)

const (
	rate       = 16000
	chunkMS    = 20
	chunkBytes = rate * chunkMS / 1000 * 2
)

var silenceChunk = make([]byte, chunkBytes)

func pcmDur(b []byte) time.Duration     { return time.Duration(len(b)/2) * time.Second / rate }
func pcmSilence(d time.Duration) []byte { return make([]byte, int(d*rate/time.Second)*2) }

// clipSpec is a standard clip: a name and the exact words spoken in it (for WER).
type clipSpec struct{ name, text string }

var clipSpecs = []clipSpec{
	{"question", "What is two plus two?"},
	{"mars", "Tell me about the planet Mars."},
	{"backchannel", "Uh-huh, yeah."},
	{"pauseA", "I would like to know"},
	{"pauseB", "what the capital of France is."},
}

// fixtureQuestion is the committed recording of the "question" clip.
const fixtureQuestion = "e2e/fixtures/voice-debug.wav"

// clips holds the standard PCM16 16 kHz mono clips by name, silence-trimmed.
type clips map[string][]byte

func (c clips) text(name string) string {
	for _, s := range clipSpecs {
		if s.name == name {
			return s.text
		}
	}
	return ""
}

// loadWAV returns the PCM16 mono 16 kHz data of a WAV with silence trimmed (100 ms margin).
func loadWAV(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	i := bytes.Index(b, []byte("data"))
	if len(b) < 44 || string(b[:4]) != "RIFF" || i < 0 {
		return nil, fmt.Errorf("%s: not a WAV", path)
	}
	if r := binary.LittleEndian.Uint32(b[24:28]); r != rate {
		return nil, fmt.Errorf("%s: sample rate %d, want %d", path, r, rate)
	}
	return trimPCM(b[i+8:], path)
}

func trimPCM(pcm []byte, name string) ([]byte, error) {
	loud := func(f int) bool {
		var s float64
		for j := f * chunkBytes; j < (f+1)*chunkBytes && j+1 < len(pcm); j += 2 {
			v := float64(int16(binary.LittleEndian.Uint16(pcm[j:])))
			s += v * v
		}
		return math.Sqrt(s/(chunkBytes/2)) > 50
	}
	first, last := -1, -1
	for f := 0; f < len(pcm)/chunkBytes; f++ {
		if loud(f) {
			if first < 0 {
				first = f
			}
			last = f
		}
	}
	if first < 0 {
		return nil, fmt.Errorf("%s: no speech", name)
	}
	return pcm[max(first-5, 0)*chunkBytes : min(last+6, len(pcm)/chunkBytes)*chunkBytes], nil
}

// j1Index reads dir/clips.json (the shared provtest clip set) tolerantly and returns
// normalized spoken text -> file path. Accepted shapes: [{name,file,text}], {"clips":[...]},
// {name: "text"} and {name: {file,text}}. Matching by text means clip names need not agree.
func j1Index(dir string) map[string]string {
	b, err := os.ReadFile(filepath.Join(dir, "clips.json"))
	if err != nil {
		return nil
	}
	var top any
	if json.Unmarshal(b, &top) != nil {
		return nil
	}
	out := map[string]string{}
	add := func(name string, v any) {
		text, file := "", ""
		switch x := v.(type) {
		case string:
			text = x
		case map[string]any:
			text, _ = x["text"].(string)
			for _, k := range []string{"file", "path", "wav", "filename"} {
				if s, _ := x[k].(string); s != "" {
					file = s
					break
				}
			}
			if n, _ := x["name"].(string); n != "" && name == "" {
				name = n
			}
			if n, _ := x["id"].(string); n != "" && name == "" {
				name = n
			}
		}
		if file == "" && name != "" {
			file = name + ".wav"
		}
		if text != "" && file != "" {
			out[normText(text)] = filepath.Join(dir, file)
		}
	}
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case []any:
			for _, e := range x {
				add("", e)
			}
		case map[string]any:
			if l, ok := x["clips"]; ok {
				walk(l)
				return
			}
			for k, e := range x {
				add(k, e)
			}
		}
	}
	walk(top)
	return out
}

func normText(s string) string { return strings.Join(words(s), " ") }

var digitWords = strings.Fields("zero one two three four five six seven eight nine ten")

// contractions an STT may emit for words the clips spell out (ponytail: the common ones only).
var contractions = map[string]string{"what's": "what is", "it's": "it is", "that's": "that is", "i'm": "i am",
	"i'd": "i would", "i'll": "i will", "don't": "do not", "can't": "can not", "cannot": "can not", "you're": "you are"}

// words lowercases and splits s; digits 0-10 and "+" are spelled out so "2 + 2" equals "two plus two",
// and common contractions are expanded so "What's" equals "What is".
func words(s string) []string {
	s = strings.NewReplacer("+", " plus ", "’", "'").Replace(strings.ToLower(s))
	var out []string
	for _, w := range strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\'' }) {
		w = strings.Trim(w, "'")
		if n, err := strconv.Atoi(w); err == nil && n >= 0 && n < len(digitWords) {
			w = digitWords[n]
		}
		if e, ok := contractions[w]; ok {
			out = append(out, strings.Fields(e)...)
		} else if w != "" {
			out = append(out, w)
		}
	}
	return out
}

// wer is the word error rate of hyp against ref (1.0 when ref is empty and hyp is not).
func wer(ref, hyp string) float64 {
	r, h := words(ref), words(hyp)
	if len(r) == 0 {
		if len(h) == 0 {
			return 0
		}
		return 1
	}
	prev := make([]int, len(h)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(r); i++ {
		cur := make([]int, len(h)+1)
		cur[0] = i
		for j := 1; j <= len(h); j++ {
			cost := 1
			if r[i-1] == h[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return float64(prev[len(h)]) / float64(len(r))
}

// loadClips fills the standard clips from, in order: dir/<name>.pcm (our cache), a clip in
// dir named by dir/clips.json (the J1 set, matched by spoken text), the committed fixture.
// missing lists the clips that need synthesis.
func loadClips(dir, root string) (c clips, missing []string) {
	c = clips{}
	idx := j1Index(dir)
	for _, s := range clipSpecs {
		if b, err := os.ReadFile(filepath.Join(dir, s.name+".pcm")); err == nil {
			c[s.name] = b
			continue
		}
		if f := idx[normText(s.text)]; f != "" {
			if b, err := loadWAV(f); err == nil {
				c[s.name] = b
				continue
			}
		}
		if s.name == "question" {
			if b, err := loadWAV(filepath.Join(root, fixtureQuestion)); err == nil {
				c[s.name] = b
				continue
			}
		}
		missing = append(missing, s.name)
	}
	return c, missing
}

// fakeClips derives every clip from the question recording: the offline self-test needs
// speech-shaped audio, not the right words.
func fakeClips(q []byte) clips {
	half := len(q) / 2 / 2 * 2
	return clips{"question": q, "mars": q, "backchannel": q[:min(len(q), 40*chunkBytes)], "pauseA": q[:half], "pauseB": q[half:]}
}

// pauseClip is clip A, a silent gap, clip B.
func pauseClip(c clips, gap time.Duration) []byte {
	return append(append(append([]byte(nil), c["pauseA"]...), pcmSilence(gap)...), c["pauseB"]...)
}

// synthesizer turns text into 16 kHz PCM16 with a real TTS (clips are made once and cached).
type synthesizer interface {
	Synth(ctx context.Context, text string) ([]byte, error)
	Close()
	Name() string
}

// newSynth prefers Google TTS (what the shared clip set uses), else NVIDIA Riva TTS.
func newSynth(ctx context.Context, env func(string) string) (synthesizer, error) {
	if sa, err := catalog.NewResolver(providers, env).Secret("google", "tts"); err == nil {
		cfg := google.DefaultTTSConfig()
		cfg.CredentialsJSON = sa
		if cli, err := google.NewRealGoogleTTSClient(ctx, cfg); err == nil {
			return &googleSynth{cli}, nil
		}
	}
	lk := Profile{Defaults: map[string]string{"NVIDIA_TTS_FUNCTION_ID": "877104f7-e885-42b9-8de8-f6e4c6303969"}}.lookup(env)
	if key := lk("NVIDIA_API_KEY"); key != "" {
		conn, err := nvidia.Dial(nvidia.ConnConfig{APIKey: key, FunctionID: lk("NVIDIA_TTS_FUNCTION_ID")})
		if err != nil {
			return nil, err
		}
		return &nvidiaSynth{conn}, nil
	}
	return nil, errors.New("clips are missing and no Google (GOOGLE_APPLICATION_CREDENTIALS) or NVIDIA (NVIDIA_API_KEY) credentials can synthesize them")
}

type googleSynth struct{ c *google.RealGoogleTTSClient }

func (g *googleSynth) Name() string { return "google" }
func (g *googleSynth) Close()       { g.c.Close() }
func (g *googleSynth) Synth(ctx context.Context, text string) ([]byte, error) {
	return g.c.SynthesizeSpeech(ctx, text, "en-US-Neural2-D", rate)
}

type nvidiaSynth struct{ c *nvidia.Conn }

func (n *nvidiaSynth) Name() string { return "nvidia" }
func (n *nvidiaSynth) Close()       { n.c.Close() }
func (n *nvidiaSynth) Synth(ctx context.Context, text string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	st, err := rivapb.NewRivaSpeechSynthesisClient(n.c.Channel()).SynthesizeOnline(n.c.Context(cctx))
	if err == nil {
		err = st.Send(&rivapb.SynthesizeSpeechRequest{Text: text, LanguageCode: "en-US", Encoding: rivapb.AudioEncoding_LINEAR_PCM,
			SampleRateHz: rate, VoiceName: "Magpie-Multilingual.EN-US.Aria"})
	}
	if err != nil {
		return nil, err
	}
	_ = st.CloseSend()
	var pcm []byte
	for {
		r, err := st.Recv()
		if err == io.EOF {
			return pcm, nil
		}
		if err != nil {
			return nil, err
		}
		pcm = append(pcm, r.GetAudio()...)
	}
}

// synthesize makes the missing clips, caching raw PCM in dir.
// It returns the synthesis requests made (one per attempted clip), counted toward the actual spend.
func synthesize(ctx context.Context, c clips, missing []string, dir string, env func(string) string) (int, error) {
	if len(missing) == 0 {
		return 0, nil
	}
	sy, err := newSynth(ctx, env)
	if err != nil {
		return 0, err
	}
	defer sy.Close()
	n := 0
	for _, name := range missing {
		n++
		pcm, err := sy.Synth(ctx, c.text(name))
		if err != nil {
			return n, fmt.Errorf("synthesize %s with %s: %w", name, sy.Name(), err)
		}
		if c[name], err = trimPCM(pcm, name); err != nil {
			return n, err
		}
		if err := os.WriteFile(filepath.Join(dir, name+".pcm"), c[name], 0o600); err != nil {
			return n, err
		}
		fmt.Fprintf(stdout, "synthesized clip %s with %s TTS (%.1fs)\n", name, sy.Name(), pcmDur(c[name]).Seconds())
	}
	return n, nil
}
