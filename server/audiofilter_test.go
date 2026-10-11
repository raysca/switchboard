package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"govoice/core/audio/dtmf"
	"govoice/core/audio/filter"
	"govoice/core/audio/mixer"
	"govoice/vad"
)

func TestAudioFilterConfigValidation(t *testing.T) {
	ok := DefaultConfig(ProfileDevelopment)
	if err := validateAudio(ok); err != nil {
		t.Fatalf("default: %v", err)
	}
	for name, mut := range map[string]func(*Config){
		"unknown":  func(c *Config) { c.Audio.InputFilters = []string{"krisp"} },
		"dup":      func(c *Config) { c.Audio.InputFilters = []string{"denoise", "denoise"} },
		"negative": func(c *Config) { c.Audio.DenoiseStrength = -0.1 },
		"over":     func(c *Config) { c.Audio.DenoiseStrength = 1.01 },
	} {
		c := DefaultConfig(ProfileDevelopment)
		mut(c)
		if err := validateAudio(c); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	c := DefaultConfig(ProfileDevelopment)
	c.Audio.InputFilters, c.Audio.DenoiseStrength = []string{"denoise"}, 1
	if err := validateAudio(c); err != nil {
		t.Fatal(err)
	}
}

// Off by default: no processor is inserted. On: transport -> DTMF -> filter -> VAD.
func TestAudioFilterPipelineOrder(t *testing.T) {
	pipe, _ := dtmfTestPipeline(t, DefaultConfig(ProfileDevelopment))
	for _, p := range pipe.Processors() {
		if _, ok := p.(*filter.Processor); ok {
			t.Fatal("filter processor present with no filters configured")
		}
	}
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Audio.InputFilters = []string{"denoise"}
	cfg.DTMF.Detect = true
	pipe, _ = dtmfTestPipeline(t, cfg)
	procs := pipe.Processors()
	if _, ok := procs[0].(*dtmf.InputProcessor); !ok {
		t.Fatalf("processor 0 is %T, want DTMF detector", procs[0])
	}
	if _, ok := procs[1].(*filter.Processor); !ok {
		t.Fatalf("processor 1 is %T, want filter", procs[1])
	}
	if _, ok := procs[2].(*vad.Processor); !ok {
		t.Fatalf("processor 2 is %T, want VAD", procs[2])
	}
	// Without DTMF the filter is first.
	cfg.DTMF.Detect = false
	pipe, _ = dtmfTestPipeline(t, cfg)
	if _, ok := pipe.Processors()[0].(*filter.Processor); !ok {
		t.Fatal("filter should lead when DTMF is off")
	}
}

// Every call gets its own filter state.
func TestAudioFilterPerCallInstances(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Audio.InputFilters = []string{"denoise"}
	a, _ := buildInputFilter(cfg, nil)
	b, _ := buildInputFilter(cfg, nil)
	if a == nil || a == b {
		t.Fatal("filter processor must be new per call")
	}
}

func TestAudioFilterMetrics(t *testing.T) {
	var m Metrics
	m.filters.observe("denoise", 30_000)
	m.filters.observe("surprise-label", 2_000_000_000)
	var sb strings.Builder
	m.filters.write(&sb)
	out := sb.String()
	for _, want := range []string{
		`voice_audio_filter_frames_total{filter="denoise"} 1`,
		`voice_audio_filter_frames_total{filter="other"} 1`,
		`voice_audio_filter_latency_seconds_bucket{filter="denoise",le="5e-05"} 1`,
		`voice_audio_filter_latency_seconds_bucket{filter="other",le="+Inf"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "surprise") {
		t.Error("unbounded label leaked")
	}
}

func TestAudioOutputConfigValidationAndBuild(t *testing.T) {
	c := DefaultConfig(ProfileDevelopment)
	if g, err := buildOutputGain(c); g != nil || err != nil {
		t.Fatalf("0 dB must insert no stage: %v %v", g, err)
	}
	if u, err := loadBackground(c, 24000); u != nil || err != nil {
		t.Fatalf("no file must mean no underlay: %v", err)
	}
	for name, mut := range map[string]func(*Config){
		"gain-low":    func(c *Config) { c.Audio.OutputGainDB = -31 },
		"gain-high":   func(c *Config) { c.Audio.OutputGainDB = 7 },
		"bg-no-file":  func(c *Config) { c.Audio.Background.GainDB = ptrF(-10) },
		"bg-gain-pos": func(c *Config) { c.Audio.Background.File, c.Audio.Background.GainDB = "x.wav", ptrF(3) },
	} {
		c := DefaultConfig(ProfileDevelopment)
		mut(c)
		if err := validateAudio(c); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	c.Audio.Background.File, c.Audio.Background.GainDB = "x.wav", ptrF(0) // explicit 0 dB is full level, not "default"
	if err := validateAudio(c); err != nil {
		t.Fatalf("background gain_db 0 rejected: %v", err)
	}
	c.Audio.OutputGainDB = -6
	g, err := buildOutputGain(c)
	if err != nil || g == nil {
		t.Fatalf("gain stage: %v %v", g, err)
	}
}

// Gain is the stage right before the transport, only when configured.
func TestOutputGainPipelinePlacement(t *testing.T) {
	pipe, _ := dtmfTestPipeline(t, DefaultConfig(ProfileDevelopment))
	for _, p := range pipe.Processors() {
		if _, ok := p.(*mixer.Gain); ok {
			t.Fatal("gain stage present at 0 dB")
		}
	}
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Audio.OutputGainDB = -6
	pipe, _ = dtmfTestPipeline(t, cfg)
	procs := pipe.Processors()
	if _, ok := procs[len(procs)-2].(*mixer.Gain); !ok {
		t.Fatalf("processor before transport is %T, want gain", procs[len(procs)-2])
	}
}

func TestBackgroundLoadedOncePerProcessCursorPerSession(t *testing.T) {
	pcm := make([]byte, 2*2400)
	wav := append([]byte("RIFF\x00\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00\xc0\x5d\x00\x00\x80\xbb\x00\x00\x02\x00\x10\x00data"), byte(len(pcm)), byte(len(pcm)>>8), 0, 0)
	path := filepath.Join(t.TempDir(), "amb.wav")
	if err := os.WriteFile(path, append(wav, pcm...), 0o600); err != nil {
		t.Fatal(err)
	}
	c := DefaultConfig(ProfileDevelopment)
	c.Audio.Background.File = path
	mk, err := loadBackground(c, 24000)
	if err != nil || mk == nil {
		t.Fatalf("load: %v", err)
	}
	a, b := mk(), mk()
	if a == b || a.Rate() != 24000 {
		t.Fatal("each session needs its own cursor at the player rate")
	}
	c.Audio.Background.File = filepath.Join(t.TempDir(), "nope.wav")
	if _, err := loadBackground(c, 24000); err == nil {
		t.Fatal("missing file accepted")
	}
}

func ptrF(v float64) *float64 { return &v }

func TestAudioCuesValidationAndLoad(t *testing.T) {
	pcm := make([]byte, 2*2400)
	wav := append([]byte("RIFF\x00\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00\xc0\x5d\x00\x00\x80\xbb\x00\x00\x02\x00\x10\x00data"), byte(len(pcm)), byte(len(pcm)>>8), 0, 0)
	path := filepath.Join(t.TempDir(), "chime.wav")
	if err := os.WriteFile(path, append(wav, pcm...), 0o600); err != nil {
		t.Fatal(err)
	}
	c := DefaultConfig(ProfileDevelopment)
	if mk, err := loadCues(c, 24000); mk != nil || err != nil {
		t.Fatalf("no cues configured must mean none: %v %v", mk, err)
	}
	c.Audio.Cues = map[string]CueConfig{"chime": {File: path, Loop: ptrB(false)}}
	if err := validateAudio(c); err != nil {
		t.Fatalf("valid cue rejected: %v", err)
	}
	mk, err := loadCues(c, 24000)
	if err != nil || mk["chime"] == nil {
		t.Fatalf("load: %v %v", mk, err)
	}
	a, b := mk["chime"](), mk["chime"]()
	if a == b || a.Rate() != 24000 {
		t.Fatal("each call needs its own cursor at the player rate")
	}
	for name, cues := range map[string]map[string]CueConfig{
		"no-file":   {"chime": {}},
		"bad-name":  {"Bad Name!": {File: path}},
		"gain-high": {"chime": {File: path, GainDB: ptrF(3)}},
		"too-many":  tooManyCues(path),
	} {
		c := DefaultConfig(ProfileDevelopment)
		c.Audio.Cues = cues
		if err := validateAudio(c); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	c.Audio.Cues = map[string]CueConfig{"chime": {File: filepath.Join(t.TempDir(), "nope.wav")}}
	if _, err := loadCues(c, 24000); err == nil {
		t.Fatal("missing file accepted")
	}
}

func tooManyCues(path string) map[string]CueConfig {
	m := map[string]CueConfig{}
	for i := range maxCues + 1 {
		m["c"+string(rune('a'+i))] = CueConfig{File: path}
	}
	return m
}

func ptrB(v bool) *bool { return &v }
