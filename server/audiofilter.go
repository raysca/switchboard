package server

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"sync/atomic"
	"time"

	"govoice/core"
	"govoice/core/audio/filter"
	"govoice/core/audio/mixer"
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
	if b := cfg.Audio.Background; b.File == "" && (b.GainDB != nil || b.Loop != nil) {
		return errors.New("audio.background.gain_db/loop set without audio.background.file")
	} else if g := b.GainDB; g != nil && (*g < -60 || *g > 0 || *g != *g) {
		return fmt.Errorf("audio.background.gain_db %v out of range -60..0", *g)
	}
	if err := validateCues(cfg.Audio.Cues); err != nil {
		return err
	}
	return nil
}

const maxCues = 8

var cueName = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

func validateCues(cues map[string]CueConfig) error {
	if len(cues) > maxCues {
		return fmt.Errorf("audio.cues: at most %d cues", maxCues)
	}
	for name, c := range cues {
		switch {
		case !cueName.MatchString(name):
			return fmt.Errorf("audio.cues: name %q must match %s", name, cueName)
		case c.File == "":
			return fmt.Errorf("audio.cues.%s.file is required", name)
		case c.GainDB != nil && (*c.GainDB < -60 || *c.GainDB > 0 || *c.GainDB != *c.GainDB):
			return fmt.Errorf("audio.cues.%s.gain_db %v out of range -60..0", name, *c.GainDB)
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

// buildOutputGain returns the pre-transport gain stage, or nil at 0 dB.
func buildOutputGain(cfg *Config) (core.Processor, error) {
	if cfg == nil || cfg.Audio.OutputGainDB == 0 {
		return nil, nil
	}
	return mixer.NewGain(cfg.Audio.OutputGainDB)
}

// loadBackground reads the ambience file once for the process (resampled to the
// transport's fixed player rate) and returns the per-session underlay factory,
// or nil when no file is configured.
func loadBackground(cfg *Config, rate int) (func() ws.Underlay, error) {
	b := cfg.Audio.Background
	if b.File == "" {
		return nil, nil
	}
	g := -30.0
	if b.GainDB != nil {
		g = *b.GainDB
	}
	a, err := mixer.LoadAsset(b.File, rate, g)
	if err != nil {
		return nil, fmt.Errorf("audio.background.file: %w", err)
	}
	loop := b.Loop == nil || *b.Loop
	return func() ws.Underlay { return a.NewCursor(loop) }, nil
}

// buildInputFilter returns the per-call filter processor, or nil when no
// filters are configured (the pipeline is then unchanged). Each call gets its
// own filter instances: they carry per-session adaptive state.
func buildInputFilter(cfg *Config, obs filter.Observer) (core.Processor, error) {
	if cfg == nil || len(cfg.Audio.InputFilters) == 0 {
		return nil, nil
	}
	var fs []filter.Filter
	for _, n := range cfg.Audio.InputFilters {
		switch n {
		case "denoise":
			s := cfg.Audio.DenoiseStrength
			if s == 0 {
				s = filter.DefaultDnSt
			}
			d, err := filter.NewDenoiser(s)
			if err != nil {
				return nil, err
			}
			fs = append(fs, d)
		default:
			return nil, fmt.Errorf("unknown input filter %q", n)
		}
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
