package provtest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"govoice/core"
	"govoice/core/transport/resample"
)

// Synthesize speaks text through t (a TTS target) and returns the audio at the pipeline rate.
func Synthesize(t Target, text string, timeout time.Duration) ([]byte, error) {
	p, err := t.Build(Variant{})
	if err != nil {
		return nil, fmt.Errorf("build: %s", Redact(err.Error()))
	}
	g, err := newRig(p)
	if err != nil {
		return nil, err
	}
	defer g.close()
	_ = g.sess.Post(&core.SynthesisUnitFrame{Turn: "t1", Gen: g.gen(), UnitID: 1, Text: text, IsFinal: true})
	waitFor(timeout, func() bool {
		s, _ := frames[*core.TTSStoppedFrame](g.sink)
		return len(s) > 0 || len(g.errs()) > 0
	})
	if e := g.errs(); len(e) > 0 {
		return nil, fmt.Errorf("%s", Redact(e[0].Error()))
	}
	audio, _ := frames[*core.OutputAudioFrame](g.sink)
	var pcm []byte
	for _, f := range audio {
		pcm = append(pcm, f.Samples...)
	}
	if len(pcm) == 0 {
		return nil, fmt.Errorf("no audio")
	}
	return pcm, nil
}

// GenClips synthesizes the standard STT fixtures with one TTS target (3 calls)
// into dir: three sentences plus a fourth built from two of them with a 1 s
// pause, 16 kHz mono IMA ADPCM WAV, and clips.json.
func GenClips(t Target, dir string, b *Budget) error {
	say := func(text string) ([]byte, error) {
		if err := b.Spend(1); err != nil {
			return nil, err
		}
		pcm, err := Synthesize(t, text, 30*time.Second)
		if err != nil {
			return nil, err
		}
		rs, err := resample.NewResampler(pipelineRate, clipRate)
		if err != nil {
			return nil, err
		}
		return rs.ResamplePCM16(pcm)
	}
	specs := []Clip{
		{ID: "two-plus-two", Text: "What is two plus two?", MaxWER: 0.2},                                        // 1 of 5 words
		{ID: "capital-of-france", Text: "What is the capital of France?", MaxWER: 0.2},                          // 1 of 6
		{ID: "twelve-words", Text: "The quick brown fox jumps over the lazy dog near the river.", MaxWER: 0.25}, // 3 of 12
	}
	pcm := map[string][]byte{}
	for i := range specs {
		p, err := say(specs[i].Text)
		if err != nil {
			return fmt.Errorf("%s: %w", specs[i].ID, err)
		}
		pcm[specs[i].ID] = p
	}
	pause := Silence(clipRate, 1.0)
	two := append(append(append([]byte{}, pcm["capital-of-france"]...), pause...), pcm["two-plus-two"]...)
	pcm["two-sentences-pause"] = two
	specs = append(specs, Clip{ID: "two-sentences-pause", Text: "What is the capital of France? What is two plus two?",
		Segments: []string{"What is the capital of France?", "What is two plus two?"}, MaxWER: 0.2}) // 2 of 11
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for i := range specs {
		specs[i].Language, specs[i].File = "en-US", specs[i].ID+".wav"
		if err := os.WriteFile(filepath.Join(dir, specs[i].File), EncodeIMAWAV(pcm[specs[i].ID], clipRate), 0o644); err != nil {
			return err
		}
	}
	out, _ := json.MarshalIndent(specs, "", "  ")
	return os.WriteFile(filepath.Join(dir, "clips.json"), append(out, '\n'), 0o644)
}
