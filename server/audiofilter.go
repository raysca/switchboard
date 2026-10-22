package server

import (
	"fmt"
	"io"
	"regexp"
	"slices"
	"sync/atomic"
	"time"

	"govoice/core"
	"govoice/core/audio/filter"
	"govoice/core/audio/mixer"
	"govoice/server/agent"
	"govoice/transport/ws"
)

// Names accepted by audio.input_filters. Add RNNoise/Krisp/AIC here once they
// exist as filter.Filter implementations (see docs/audio-filters.md).
var knownInputFilters = []string{"denoise"}

// ValidateAudio checks audio.input_filters and audio.denoise_strength.
func validateAudio(cfg *Config) error {
	seen := map[string]bool{}
	for _, n := range cfg.Audio.InputFilters {
		if !slices.Contains(knownInputFilters, n) {
			return fmt.Errorf("audio.input_filters: unknown filter %q (known: %v)", n, knownInputFilters)
		}
		if seen[n] {
			return fmt.Errorf("audio.input_filters: %q listed twice", n)
		}
		seen[n] = true
	}
	if s := cfg.Audio.DenoiseStrength; s < 0 || s > 1 || s != s {
		return fmt.Errorf("audio.denoise_strength %v out of range 0..1", s)
	}
	if err := mixer.ValidateGainDB(cfg.Audio.OutputGainDB); err != nil {
		return fmt.Errorf("audio.%w", err)
	}
	if err := validateSounds("backgrounds", cfg.Audio.Backgrounds); err != nil {
		return err
	}
	if d := cfg.Audio.BackgroundDefault; d != "" {
		if _, ok := cfg.Audio.Backgrounds[d]; !ok {
			return fmt.Errorf("audio.background_default %q is not in audio.backgrounds", d)
		}
	}
	if err := validateSounds("cues", cfg.Audio.Cues); err != nil {
		return err
	}
	return nil
}

const maxCues = 8

var cueName = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

// validateSounds checks a named-sound map (audio.cues or audio.backgrounds).
func validateSounds(kind string, sounds map[string]CueConfig) error {
	if len(sounds) > maxCues {
		return fmt.Errorf("audio.%s: at most %d sounds", kind, maxCues)
	}
	for name, c := range sounds {
		switch {
		case !cueName.MatchString(name):
			return fmt.Errorf("audio.%s: name %q must match %s", kind, name, cueName)
		case c.File == "":
			return fmt.Errorf("audio.%s.%s.file is required", kind, name)
		case c.GainDB != nil && (*c.GainDB < -60 || *c.GainDB > 0 || *c.GainDB != *c.GainDB):
			return fmt.Errorf("audio.%s.%s.gain_db %v out of range -60..0", kind, name, *c.GainDB)
		}
	}
	return nil
}

// loadCues reads every cue file once for the process (resampled to the
// transport's fixed player rate) and returns a per-call cursor factory by name,
// or nil when none are configured.
func loadCues(cfg *Config, rate int) (map[string]func() ws.Underlay, error) {
	if len(cfg.Audio.Cues) == 0 {
		return nil, nil
	}
	out := make(map[string]func() ws.Underlay, len(cfg.Audio.Cues))
	for name, c := range cfg.Audio.Cues {
		gain := -20.0
		if c.GainDB != nil {
			gain = *c.GainDB
		}
		a, err := mixer.LoadAsset(c.File, rate, gain)
		if err != nil {
			return nil, fmt.Errorf("audio.cues.%s.file: %w", name, err)
		}
		loop := c.Loop == nil || *c.Loop
		out[name] = func() ws.Underlay { return a.NewCursor(loop) }
	}
	return out, nil
}

// buildOutputGain returns the pre-transport gain stage, or nil at 0 dB. A flow's output_gain_db
// (even an explicit 0) replaces the server's.
func buildOutputGain(cfg *Config, agentAudio *agent.AudioSettings) (core.Processor, error) {
	if cfg == nil {
		return nil, nil
	}
	db := cfg.Audio.OutputGainDB
	if agentAudio != nil && agentAudio.OutputGainDB != nil {
		db = *agentAudio.OutputGainDB
	}
	if db == 0 {
		return nil, nil
	}
	return mixer.NewGain(db)
}

// background is a catalogue sound loaded once for the process (at unity, resampled to the transport's
// fixed player rate); each call plays its own cursor at the level it asks for.
type background struct {
	asset  *mixer.Asset
	gainDB float64
	loop   bool
}

// underlay starts a call's playback; a nil gain or loop takes the catalogue entry's.
func (b background) underlay(gainDB *float64, loop *bool) ws.Underlay {
	g, l := b.gainDB, b.loop
	if gainDB != nil {
		g = *gainDB
	}
	if loop != nil {
		l = *loop
	}
	return b.asset.NewCursorGain(l, g)
}

// loadBackgrounds reads every catalogue sound once.
func loadBackgrounds(cfg *Config, rate int) (map[string]background, error) {
	out := make(map[string]background, len(cfg.Audio.Backgrounds))
	for name, c := range cfg.Audio.Backgrounds {
		a, err := mixer.LoadAsset(c.File, rate, 0)
		if err != nil {
			return nil, fmt.Errorf("audio.backgrounds.%s.file: %w", name, err)
		}
		b := background{asset: a, gainDB: -30, loop: c.Loop == nil || *c.Loop}
		if c.GainDB != nil {
			b.gainDB = *c.GainDB
		}
		out[name] = b
	}
	return out, nil
}

// buildInputFilter returns the per-call filter processor, or nil when no
// filters are on (the pipeline is then unchanged). The server's input_filters apply to every call; a
// flow can also turn denoise on for its own calls. Each call gets its own filter instances: they carry
// per-session adaptive state.
func buildInputFilter(cfg *Config, agentAudio *agent.AudioSettings, obs filter.Observer) (core.Processor, error) {
	if cfg == nil {
		return nil, nil
	}
	strength, denoise := cfg.Audio.DenoiseStrength, slices.Contains(cfg.Audio.InputFilters, "denoise")
	if agentAudio != nil && agentAudio.Denoise != nil {
		strength, denoise = filter.DefaultDnSt, true
		if s := agentAudio.Denoise.Strength; s != nil {
			strength = *s
		}
	}
	var fs []filter.Filter
	if denoise {
		if strength == 0 {
			strength = filter.DefaultDnSt
		}
		d, err := filter.NewDenoiser(strength)
		if err != nil {
			return nil, err
		}
		fs = append(fs, d)
	}
	if len(fs) == 0 {
		return nil, nil
	}
	c := filter.NewChain(fs...)
	c.SetObserver(obs)
	return filter.NewProcessor(c, filter.DenoiseRate), nil
}

// filterMetrics counts chunks through the filter chain and times them. Label
// values are the fixed set in filterLabels, so cardinality is bounded.
type filterMetrics struct {
	labels [len(filterLabels)]filterStat
}

var filterLabels = [...]string{"denoise", "other"}

var filterBuckets = [...]float64{0.00005, 0.0001, 0.00025, 0.0005, 0.001, 0.005}

type filterStat struct {
	frames  atomic.Uint64
	buckets [len(filterBuckets) + 1]atomic.Uint64 // non-cumulative; last is +Inf
	sumNs   atomic.Uint64
}

func (m *filterMetrics) observe(name string, d time.Duration) {
	i := slices.Index(filterLabels[:], name)
	if i < 0 {
		i = len(filterLabels) - 1
	}
	s := &m.labels[i]
	s.frames.Add(1)
	s.sumNs.Add(uint64(d))
	b := 0
	for b < len(filterBuckets) && d.Seconds() > filterBuckets[b] {
		b++
	}
	s.buckets[b].Add(1)
}

func (m *filterMetrics) write(w io.Writer) {
	fmt.Fprint(w, "# HELP voice_audio_filter_frames_total Audio chunks processed by an input filter.\n# TYPE voice_audio_filter_frames_total counter\n")
	for i, l := range filterLabels {
		fmt.Fprintf(w, "voice_audio_filter_frames_total{filter=%q} %d\n", l, m.labels[i].frames.Load())
	}
	fmt.Fprint(w, "# HELP voice_audio_filter_latency_seconds Time spent in an input filter per chunk.\n# TYPE voice_audio_filter_latency_seconds histogram\n")
	for i, l := range filterLabels {
		s := &m.labels[i]
		var cum uint64
		for b, le := range filterBuckets {
			cum += s.buckets[b].Load()
			fmt.Fprintf(w, "voice_audio_filter_latency_seconds_bucket{filter=%q,le=\"%g\"} %d\n", l, le, cum)
		}
		cum += s.buckets[len(filterBuckets)].Load()
		fmt.Fprintf(w, "voice_audio_filter_latency_seconds_bucket{filter=%q,le=\"+Inf\"} %d\n", l, cum)
		fmt.Fprintf(w, "voice_audio_filter_latency_seconds_sum{filter=%q} %g\n", l, float64(s.sumNs.Load())/1e9)
		fmt.Fprintf(w, "voice_audio_filter_latency_seconds_count{filter=%q} %d\n", l, cum)
	}
}
