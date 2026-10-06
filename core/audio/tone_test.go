package audio

import (
	"testing"
	"time"

	"govoice/core"
)

func TestDTMFFrame_ValidationAndFrequencies(t *testing.T) {
	validDigits := []rune{'0', '1', '2', '3', '4', '5', '6', '7', '8', '9', '*', '#', 'A', 'B', 'C', 'D'}

	for _, d := range validDigits {
		frame := &DTMFFrame{
			Digit:    d,
			Duration: 100 * time.Millisecond,
			Turn:     "turn-1",
			Gen:      1,
		}
		if err := frame.Validate(); err != nil {
			t.Fatalf("expected valid digit %c, got %v", d, err)
		}
		freqs, err := frame.Frequencies()
		if err != nil {
			t.Fatalf("failed getting frequencies for %c: %v", d, err)
		}
		if freqs[0] <= 0 || freqs[1] <= 0 || freqs[0] >= freqs[1] {
			t.Fatalf("invalid frequency pair for %c: %v", d, freqs)
		}
		if frame.Type() != core.FrameDTMF {
			t.Fatalf("expected FrameDTMF, got %v", frame.Type())
		}
		if frame.Generation() != 1 {
			t.Fatalf("expected Gen 1, got %d", frame.Generation())
		}
	}

	// Invalid digits
	invalidFrame := &DTMFFrame{Digit: 'X', Duration: 100 * time.Millisecond}
	if err := invalidFrame.Validate(); err == nil {
		t.Fatal("expected error on invalid DTMF digit 'X'")
	}

	// Invalid duration
	zeroDurFrame := &DTMFFrame{Digit: '5', Duration: 0}
	if err := zeroDurFrame.Validate(); err == nil {
		t.Fatal("expected error on zero duration")
	}

	oversizedDurFrame := &DTMFFrame{Digit: '5', Duration: 10 * time.Second}
	if err := oversizedDurFrame.Validate(); err == nil {
		t.Fatal("expected error on oversized duration")
	}
}

func TestToneFrame_Validation(t *testing.T) {
	// 440 Hz concert pitch tone
	tone := &ToneFrame{
		Frequencies: []float64{440.0},
		Duration:    200 * time.Millisecond,
		Volume:      0.8,
		Turn:        "turn-1",
		Gen:         1,
	}

	if err := tone.Validate(); err != nil {
		t.Fatalf("expected valid tone, got %v", err)
	}
	if tone.Type() != core.FrameTone {
		t.Fatalf("expected FrameTone, got %v", tone.Type())
	}
	if tone.Generation() != 1 {
		t.Fatalf("expected Gen 1, got %d", tone.Generation())
	}

	// Invalid frequency
	badFreqTone := &ToneFrame{
		Frequencies: []float64{10.0}, // below 20 Hz
		Duration:    100 * time.Millisecond,
		Volume:      0.5,
	}
	if err := badFreqTone.Validate(); err == nil {
		t.Fatal("expected error on frequency below 20 Hz")
	}

	// Invalid volume
	badVolTone := &ToneFrame{
		Frequencies: []float64{1000.0},
		Duration:    100 * time.Millisecond,
		Volume:      1.5,
	}
	if err := badVolTone.Validate(); err == nil {
		t.Fatal("expected error on volume > 1.0")
	}

	// Empty frequencies
	emptyTone := &ToneFrame{
		Frequencies: nil,
		Duration:    100 * time.Millisecond,
		Volume:      0.5,
	}
	if err := emptyTone.Validate(); err == nil {
		t.Fatal("expected error on empty frequencies")
	}
}
