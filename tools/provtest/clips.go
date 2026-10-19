package provtest

import (
	"embed"
	"encoding/json"
	"fmt"
	"path"

	"govoice/tools/internal/wav"
)

//go:embed testdata/clips
var clipFS embed.FS

// Clip is a standard 16 kHz mono STT fixture with a known transcript.
type Clip struct {
	ID       string   `json:"id"`
	Text     string   `json:"text"`
	Language string   `json:"language"`
	File     string   `json:"file"`
	Segments []string `json:"segments,omitempty"` // sentences separated by a pause (resumed speech)
	MaxWER   float64  `json:"max_wer,omitempty"`  // per-clip WER limit (0 = Options.MaxWER)
	PCM      []byte   `json:"-"`                  // 16 kHz mono PCM16
}

const clipRate = 16000

// LoadClips reads testdata/clips/clips.json and decodes every clip.
func LoadClips() ([]Clip, error) {
	b, err := clipFS.ReadFile("testdata/clips/clips.json")
	if err != nil {
		return nil, err
	}
	var clips []Clip
	if err := json.Unmarshal(b, &clips); err != nil {
		return nil, err
	}
	for i := range clips {
		raw, err := clipFS.ReadFile(path.Join("testdata/clips", clips[i].File))
		if err != nil {
			return nil, err
		}
		pcm, rate, err := wav.ReadWAV(raw)
		if err != nil || rate != clipRate {
			return nil, fmt.Errorf("clip %s: rate %d: %v", clips[i].ID, rate, err)
		}
		clips[i].PCM = pcm
	}
	return clips, nil
}
