package audio

import (
	"encoding/binary"
	"errors"
	"testing"
)

func TestAudioMixer_SourceCountBound(t *testing.T) {
	mixer, err := NewAudioMixer(MixerConfig{MaxSources: 3})
	if err != nil {
		t.Fatalf("NewAudioMixer: %v", err)
	}
	defer mixer.Close()

	if err := mixer.AddSource("src-1", 1.0, 1); err != nil {
		t.Fatalf("AddSource 1: %v", err)
	}
	if err := mixer.AddSource("src-2", 0.5, 1); err != nil {
		t.Fatalf("AddSource 2: %v", err)
	}
	if err := mixer.AddSource("src-3", 0.8, 1); err != nil {
		t.Fatalf("AddSource 3: %v", err)
	}

	// 4th source should fail because MaxSources is 3
	if err := mixer.AddSource("src-4", 1.0, 1); err == nil {
		t.Fatal("expected ErrTooManyMixerSources on exceeding MaxSources bound")
	} else if !errors.Is(err, ErrTooManyMixerSources) {
		t.Fatalf("expected ErrTooManyMixerSources, got %v", err)
	}

	// Updating existing source should succeed even when at limit
	if err := mixer.AddSource("src-2", 0.7, 1); err != nil {
		t.Fatalf("updating existing source failed: %v", err)
	}
}

func TestAudioMixer_FormatMismatchAndClipping(t *testing.T) {
	mixer, _ := NewAudioMixer(MixerConfig{SampleRate: 24000, Channels: 1})
	defer mixer.Close()

	_ = mixer.AddSource("s1", 1.0, 1)
	_ = mixer.AddSource("s2", 1.0, 1)

	// Format mismatch: passed 16000 Hz instead of configured 24000 Hz
	inputs := map[string][]byte{
		"s1": {0, 10},
	}
	_, err := mixer.MixChunks(inputs, 16000, 1)
	if err == nil {
		t.Fatal("expected ErrMixerFormatMismatch on sample rate mismatch")
	} else if !errors.Is(err, ErrMixerFormatMismatch) {
		t.Fatalf("expected ErrMixerFormatMismatch, got %v", err)
	}

	// Test mixing two loud signals that saturate/clip at 32767
	// 25000 + 25000 = 50000 -> clipped to 32767
	s1 := make([]byte, 2)
	s2 := make([]byte, 2)
	binary.LittleEndian.PutUint16(s1, uint16(int16(25000)))
	binary.LittleEndian.PutUint16(s2, uint16(int16(25000)))

	mixed, err := mixer.MixChunks(map[string][]byte{"s1": s1, "s2": s2}, 24000, 1)
	if err != nil {
		t.Fatalf("MixChunks: %v", err)
	}
	if len(mixed) != 2 {
		t.Fatalf("expected 2 bytes, got %d", len(mixed))
	}
	res := int16(binary.LittleEndian.Uint16(mixed))
	if res != 32767 {
		t.Fatalf("expected clipped value 32767, got %d", res)
	}
}

func TestAudioMixer_CancellationAndInterruption(t *testing.T) {
	mixer, _ := NewAudioMixer(MixerConfig{SampleRate: 24000, Channels: 1})
	defer mixer.Close()

	_ = mixer.AddSource("stale-gen1", 1.0, 1)
	_ = mixer.AddSource("active-gen2", 1.0, 2)

	// Advance generation fence to 2; gen 1 should be dropped
	mixer.SetGeneration(2)

	s1 := make([]byte, 2)
	s2 := make([]byte, 2)
	binary.LittleEndian.PutUint16(s1, uint16(int16(1000)))
	binary.LittleEndian.PutUint16(s2, uint16(int16(2000)))

	mixed, err := mixer.MixChunks(map[string][]byte{
		"stale-gen1":  s1,
		"active-gen2": s2,
	}, 24000, 1)

	if err != nil {
		t.Fatalf("MixChunks: %v", err)
	}
	res := int16(binary.LittleEndian.Uint16(mixed))
	// Stale gen 1 source must have been dropped; only gen 2 (2000) should be present
	if res != 2000 {
		t.Fatalf("expected 2000 from active generation, got %d (stale source was not dropped)", res)
	}
}
