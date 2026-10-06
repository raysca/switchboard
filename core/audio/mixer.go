package audio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
)

var (
	ErrTooManyMixerSources = errors.New("audio: mixer maximum sources limit exceeded")
	ErrMixerFormatMismatch = errors.New("audio: mixer input format does not match configured output format")
	ErrMixerClosed         = errors.New("audio: mixer is closed")
)

const (
	DefaultMaxMixerSources = 8
)

// MixerConfig configures an AudioMixer instance.
type MixerConfig struct {
	SampleRate int // Default: 24000 Hz or 16000 Hz
	Channels   int // Default: 1 (mono)
	MaxSources int // Default: 8
}

// AudioMixer provides a deterministic, bounded, multi-source PCM16 audio mixing engine.
// It clips samples safely within int16 bounds [-32768, 32767] with soft or hard saturation.
type AudioMixer struct {
	cfg       MixerConfig
	mu        sync.Mutex
	sources   map[string]*mixerSource
	closed    bool
	activeGen uint64
}

type mixerSource struct {
	id     string
	volume float64 // 0.0 to 1.0
	gen    uint64
}

// NewAudioMixer creates a new bounded AudioMixer.
func NewAudioMixer(cfg MixerConfig) (*AudioMixer, error) {
	if cfg.SampleRate <= 0 {
		cfg.SampleRate = 24000
	}
	if cfg.Channels <= 0 {
		cfg.Channels = 1
	}
	if cfg.MaxSources <= 0 {
		cfg.MaxSources = DefaultMaxMixerSources
	}
	return &AudioMixer{
		cfg:     cfg,
		sources: make(map[string]*mixerSource),
	}, nil
}

// AddSource registers an audio source stream with a designated gain factor and turn generation.
func (m *AudioMixer) AddSource(sourceID string, volume float64, gen uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrMixerClosed
	}
	if len(m.sources) >= m.cfg.MaxSources {
		if _, exists := m.sources[sourceID]; !exists {
			return ErrTooManyMixerSources
		}
	}
	if volume < 0.0 {
		volume = 0.0
	} else if volume > 1.0 {
		volume = 1.0
	}
	m.sources[sourceID] = &mixerSource{
		id:     sourceID,
		volume: volume,
		gen:    gen,
	}
	return nil
}

// RemoveSource unregisters an audio source.
func (m *AudioMixer) RemoveSource(sourceID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sources, sourceID)
}

// SetGeneration advances the cancellation fence, dropping older-generation sources.
func (m *AudioMixer) SetGeneration(gen uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.activeGen = gen
	for id, src := range m.sources {
		if src.gen != 0 && src.gen < gen {
			delete(m.sources, id)
		}
	}
}

// MixChunks mixes multiple input PCM16LE sample slices synchronously into one output slice.
// If formats mismatch configured settings, ErrMixerFormatMismatch is returned.
func (m *AudioMixer) MixChunks(inputs map[string][]byte, sampleRate int, channels int) ([]byte, error) {
	if sampleRate != m.cfg.SampleRate || channels != m.cfg.Channels {
		return nil, fmt.Errorf("%w: expected %d Hz mono, got %d Hz %d channels", ErrMixerFormatMismatch, m.cfg.SampleRate, sampleRate, channels)
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, ErrMixerClosed
	}

	// Find maximum byte length among active and registered source chunks
	maxLen := 0
	for id, data := range inputs {
		if src, ok := m.sources[id]; ok {
			if src.gen != 0 && src.gen < m.activeGen {
				continue
			}
			if len(data) > maxLen {
				maxLen = len(data)
			}
		}
	}

	if maxLen == 0 {
		m.mu.Unlock()
		return nil, nil
	}

	numSamples := maxLen / 2
	mixed := make([]int32, numSamples)

	for id, data := range inputs {
		src, ok := m.sources[id]
		if !ok || (src.gen != 0 && src.gen < m.activeGen) {
			continue
		}
		vol := src.volume

		sampleCount := len(data) / 2
		for i := 0; i < sampleCount; i++ {
			raw := int16(binary.LittleEndian.Uint16(data[i*2 : i*2+2]))
			scaled := int32(float64(raw) * vol)
			mixed[i] += scaled
		}
	}
	m.mu.Unlock()

	// Saturate/clip into PCM16LE
	out := make([]byte, numSamples*2)
	for i := 0; i < numSamples; i++ {
		val := mixed[i]
		if val > 32767 {
			val = 32767
		} else if val < -32768 {
			val = -32768
		}
		binary.LittleEndian.PutUint16(out[i*2:i*2+2], uint16(int16(val)))
	}

	return out, nil
}

// Close closes the mixer and releases sources.
func (m *AudioMixer) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	m.sources = nil
	return nil
}
