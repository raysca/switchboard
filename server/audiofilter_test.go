package server

import (
	"fmt"
	"govoice/server/agent"
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
	a, _ := buildInputFilter(cfg, nil, nil)
	b, _ := buildInputFilter(cfg, nil, nil)
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
	if g, err := buildOutputGain(c, nil); g != nil || err != nil {
		t.Fatalf("0 dB must insert no stage: %v %v", g, err)
	}
	if m, err := loadBackgrounds(c, 24000); len(m) != 0 || err != nil {
		t.Fatalf("no sounds must mean no catalogue: %v %v", m, err)
	}
	for name, mut := range map[string]func(*Config){
		"gain-low":        func(c *Config) { c.Audio.OutputGainDB = -31 },
		"gain-high":       func(c *Config) { c.Audio.OutputGainDB = 7 },
		"bg-no-file":      func(c *Config) { c.Audio.Backgrounds = map[string]CueConfig{"amb": {GainDB: ptrF(-10)}} },
		"bg-gain-pos":     func(c *Config) { c.Audio.Backgrounds = map[string]CueConfig{"amb": {File: "x.wav", GainDB: ptrF(3)}} },
		"bg-bad-name":     func(c *Config) { c.Audio.Backgrounds = map[string]CueConfig{"../x": {File: "x.wav"}} },
		"bg-default-none": func(c *Config) { c.Audio.BackgroundDefault = "nope" },
		"bg-too-many": func(c *Config) {
			c.Audio.Backgrounds = map[string]CueConfig{}
			for i := 0; i <= maxCues; i++ {
				c.Audio.Backgrounds[fmt.Sprintf("s%d", i)] = CueConfig{File: "x.wav"}
			}
		},
	} {
		c := DefaultConfig(ProfileDevelopment)
		mut(c)
		if err := validateAudio(c); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	c.Audio.Backgrounds = map[string]CueConfig{"amb": {File: "x.wav", GainDB: ptrF(0)}} // explicit 0 dB is full level, not "default"
	c.Audio.BackgroundDefault = "amb"
	if err := validateAudio(c); err != nil {
		t.Fatalf("background gain_db 0 rejected: %v", err)
	}
	c.Audio.OutputGainDB = -6
	g, err := buildOutputGain(c, nil)
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

func TestBackgroundsLoadedOncePerProcessCursorPerSession(t *testing.T) {
	pcm := make([]byte, 2*2400)
	wav := append([]byte("RIFF\x00\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00\xc0\x5d\x00\x00\x80\xbb\x00\x00\x02\x00\x10\x00data"), byte(len(pcm)), byte(len(pcm)>>8), 0, 0)
	path := filepath.Join(t.TempDir(), "amb.wav")
	if err := os.WriteFile(path, append(wav, pcm...), 0o600); err != nil {
		t.Fatal(err)
	}
	c := DefaultConfig(ProfileDevelopment)
	c.Audio.Backgrounds = map[string]CueConfig{"amb": {File: path}, "quiet": {File: path, GainDB: ptrF(-50), Loop: ptrBool(false)}}
	m, err := loadBackgrounds(c, 24000)
	if err != nil || len(m) != 2 {
		t.Fatalf("load: %v %v", m, err)
	}
	a, b := m["amb"].underlay(nil, nil), m["amb"].underlay(nil, nil)
	if a == b || a.Rate() != 24000 {
		t.Fatal("each session needs its own cursor at the player rate")
	}
	if m["amb"].gainDB != -30 || !m["amb"].loop || m["quiet"].gainDB != -50 || m["quiet"].loop {
		t.Fatalf("catalogue defaults: %+v %+v", m["amb"], m["quiet"])
	}
	c.Audio.Backgrounds = map[string]CueConfig{"gone": {File: filepath.Join(t.TempDir(), "nope.wav")}}
	if _, err := loadBackgrounds(c, 24000); err == nil || !strings.Contains(err.Error(), "audio.backgrounds.gone.file") {
		t.Fatalf("missing file: %v", err)
	}
}

func ptrBool(v bool) *bool { return &v }

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

func TestAnAgentsAudioSettingsReplaceTheServersPerCall(t *testing.T) {
	zero, minusSix := 0.0, -6.0
	strong := 0.8
	c := DefaultConfig(ProfileDevelopment)
	// Server defaults: nothing on.
	if f, err := buildInputFilter(c, nil, nil); f != nil || err != nil {
		t.Fatalf("server off, flow silent: %v %v", f, err)
	}
	if f, err := buildInputFilter(c, &agent.AudioSettings{Denoise: &agent.DenoiseSettings{}}, nil); f == nil || err != nil {
		t.Fatalf("flow denoise on, server off: %v %v", f, err)
	}
	if f, err := buildInputFilter(c, &agent.AudioSettings{Denoise: &agent.DenoiseSettings{Strength: &strong}}, nil); f == nil || err != nil {
		t.Fatalf("flow denoise with a strength: %v %v", f, err)
	}
	if f, err := buildInputFilter(c, &agent.AudioSettings{OutputGainDB: &minusSix}, nil); f != nil || err != nil {
		t.Fatalf("a flow without denoise leaves the server's choice: %v %v", f, err)
	}
	if g, err := buildOutputGain(c, &agent.AudioSettings{OutputGainDB: &minusSix}); g == nil || err != nil {
		t.Fatalf("flow gain, server 0: %v %v", g, err)
	}
	// Server on: the flow's silence keeps it, an explicit 0 dB removes the gain stage.
	c.Audio.InputFilters, c.Audio.OutputGainDB = []string{"denoise"}, -3
	if f, _ := buildInputFilter(c, &agent.AudioSettings{OutputGainDB: &minusSix}, nil); f == nil {
		t.Fatal("server denoise lost when the flow only sets gain")
	}
	if g, _ := buildOutputGain(c, nil); g == nil {
		t.Fatal("server gain lost for a flow with no audio settings")
	}
	if g, err := buildOutputGain(c, &agent.AudioSettings{OutputGainDB: &zero}); g != nil || err != nil {
		t.Fatalf("an explicit 0 dB must override the server's -3: %v %v", g, err)
	}
}
