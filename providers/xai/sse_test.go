package xai

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestSSEReader_NormalStream(t *testing.T) {
	input := "event: message\ndata: {\"delta\": \"Hello \"}\n\nevent: message\ndata: {\"delta\": \"world!\"}\n\ndata: [DONE]\n\n"
	reader := NewSSEReader(strings.NewReader(input), 1024, 4096, 65536)

	ev1, err := reader.ReadEvent()
	if err != nil {
		t.Fatalf("ev1 error: %v", err)
	}
	if ev1.Data != "{\"delta\": \"Hello \"}" {
		t.Fatalf("ev1 data mismatch: %q", ev1.Data)
	}

	ev2, err := reader.ReadEvent()
	if err != nil {
		t.Fatalf("ev2 error: %v", err)
	}
	if ev2.Data != "{\"delta\": \"world!\"}" {
		t.Fatalf("ev2 data mismatch: %q", ev2.Data)
	}

	ev3, err := reader.ReadEvent()
	if !errors.Is(err, io.EOF) {
		t.Fatalf("expected EOF at terminal [DONE], got err=%v, ev=%+v", err, ev3)
	}

	if !reader.SawTerminal() {
		t.Fatal("expected SawTerminal == true")
	}
}

func TestSSEReader_SplitLinesAndEvents(t *testing.T) {
	// Tests multiple data: lines within one event and comments
	input := ": ping comment\nevent: message\ndata: line 1\ndata: line 2\n\ndata: [DONE]\n\n"
	reader := NewSSEReader(strings.NewReader(input), 1024, 4096, 65536)

	ev, err := reader.ReadEvent()
	if err != nil {
		t.Fatalf("read event: %v", err)
	}
	expected := "line 1\nline 2"
	if ev.Data != expected {
		t.Fatalf("expected %q, got %q", expected, ev.Data)
	}

	_, err = reader.ReadEvent()
	if !errors.Is(err, io.EOF) {
		t.Fatalf("expected EOF, got %v", err)
	}
}

func TestSSEReader_MissingTerminalEvent(t *testing.T) {
	// Stream ends abruptly without [DONE]
	input := "event: message\ndata: {\"delta\": \"unfinished\"}\n\n"
	reader := NewSSEReader(strings.NewReader(input), 1024, 4096, 65536)

	ev, err := reader.ReadEvent()
	if err != nil {
		t.Fatalf("ev error: %v", err)
	}
	if ev.Data != "{\"delta\": \"unfinished\"}" {
		t.Fatalf("unexpected data: %q", ev.Data)
	}

	// Next read encounters EOF without [DONE]
	_, err = reader.ReadEvent()
	if !errors.Is(err, ErrMissingTerminalEvent) {
		t.Fatalf("expected ErrMissingTerminalEvent, got: %v", err)
	}
	if reader.SawTerminal() {
		t.Fatal("expected SawTerminal == false")
	}
}

func TestSSEReader_OversizedLine(t *testing.T) {
	longLine := "data: " + strings.Repeat("A", 200) + "\n\n"
	reader := NewSSEReader(strings.NewReader(longLine), 100, 4096, 65536)

	_, err := reader.ReadEvent()
	if !errors.Is(err, ErrOversizedLine) {
		t.Fatalf("expected ErrOversizedLine, got %v", err)
	}
}

func TestSSEReader_OversizedEvent(t *testing.T) {
	// Multiple lines that together exceed maxEventSize
	part := "data: " + strings.Repeat("B", 100) + "\n"
	eventStr := strings.Repeat(part, 5) + "\n" // 5 * 106 = 530 bytes > 300 maxEventSize
	reader := NewSSEReader(strings.NewReader(eventStr), 200, 300, 65536)

	_, err := reader.ReadEvent()
	if !errors.Is(err, ErrOversizedEvent) {
		t.Fatalf("expected ErrOversizedEvent, got %v", err)
	}
}

func TestSSEReader_OversizedTotalBody(t *testing.T) {
	streamStr := "data: 1234567890\n\ndata: 1234567890\n\ndata: 1234567890\n\n"
	reader := NewSSEReader(strings.NewReader(streamStr), 100, 200, 25)

	var readErr error
	for i := 0; i < 5; i++ {
		_, err := reader.ReadEvent()
		if err != nil {
			readErr = err
			break
		}
	}
	if !errors.Is(readErr, ErrOversizedBody) {
		t.Fatalf("expected ErrOversizedBody, got %v", readErr)
	}
}
