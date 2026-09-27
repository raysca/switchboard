package xai

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
)

const (
	DefaultMaxLineLength        = 64 * 1024       // 64KB per line
	DefaultMaxEventSize         = 256 * 1024      // 256KB per event
	DefaultMaxTotalResponseSize = 2 * 1024 * 1024 // 2MB total response body
)

// SSEEvent represents a single parsed Server-Sent Event.
type SSEEvent struct {
	Type string // Event type, e.g. "message", "response.text.delta"
	Data string // Event data
}

// SSEReader parses Server-Sent Events with bounded memory.
type SSEReader struct {
	r             *bufio.Reader
	maxLineLen    int
	maxEventSize  int
	maxTotalBytes int
	totalBytes    int
	sawTerminal   bool
}

// NewSSEReader creates an SSEReader with configured size bounds.
func NewSSEReader(r io.Reader, maxLineLen, maxEventSize, maxTotalBytes int) *SSEReader {
	if maxLineLen <= 0 {
		maxLineLen = DefaultMaxLineLength
	}
	if maxEventSize <= 0 {
		maxEventSize = DefaultMaxEventSize
	}
	if maxTotalBytes <= 0 {
		maxTotalBytes = DefaultMaxTotalResponseSize
	}

	return &SSEReader{
		r:             bufio.NewReaderSize(r, 8*1024),
		maxLineLen:    maxLineLen,
		maxEventSize:  maxEventSize,
		maxTotalBytes: maxTotalBytes,
	}
}

// readBoundedLine reads a single line delimited by \n or \r\n, bounding bytes read.
func (s *SSEReader) readBoundedLine() ([]byte, error) {
	var line []byte
	for {
		chunk, isPrefix, err := s.r.ReadLine()
		if err != nil {
			if len(line) > 0 && err == io.EOF {
				return line, nil
			}
			return nil, err
		}

		s.totalBytes += len(chunk)
		if s.totalBytes > s.maxTotalBytes {
			return nil, ErrOversizedBody
		}

		if len(line)+len(chunk) > s.maxLineLen {
			return nil, ErrOversizedLine
		}

		line = append(line, chunk...)
		if !isPrefix {
			break
		}
	}
	return line, nil
}

// ReadEvent reads the next SSEEvent from the stream.
// When [DONE] is encountered, returns (nil, io.EOF) and sets sawTerminal = true.
// If stream ends without [DONE], returns (nil, ErrMissingTerminalEvent).
func (s *SSEReader) ReadEvent() (*SSEEvent, error) {
	if s.sawTerminal {
		return nil, io.EOF
	}

	var eventType string
	var dataLines []string
	eventBytes := 0

	for {
		rawLine, err := s.readBoundedLine()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if len(dataLines) > 0 {
					// Emit trailing event if any
					event := &SSEEvent{
						Type: eventType,
						Data: strings.Join(dataLines, "\n"),
					}
					if event.Type == "" {
						event.Type = "message"
					}
					return event, nil
				}
				if !s.sawTerminal {
					return nil, ErrMissingTerminalEvent
				}
				return nil, io.EOF
			}
			return nil, err
		}

		line := bytes.TrimSpace(rawLine)
		if len(line) == 0 {
			// Empty line marks end of an event block
			if len(dataLines) == 0 {
				continue
			}
			data := strings.Join(dataLines, "\n")
			if strings.TrimSpace(data) == "[DONE]" {
				s.sawTerminal = true
				return nil, io.EOF
			}
			if eventType == "" {
				eventType = "message"
			}
			return &SSEEvent{
				Type: eventType,
				Data: data,
			}, nil
		}

		// Comment line
		if line[0] == ':' {
			continue
		}

		eventBytes += len(line)
		if eventBytes > s.maxEventSize {
			return nil, ErrOversizedEvent
		}

		strLine := string(line)
		if strings.HasPrefix(strLine, "event:") {
			eventType = strings.TrimSpace(strings.TrimPrefix(strLine, "event:"))
		} else if strings.HasPrefix(strLine, "data:") {
			dataVal := strings.TrimPrefix(strLine, "data:")
			// If there is leading space, strip just one leading space per SSE standard
			if len(dataVal) > 0 && dataVal[0] == ' ' {
				dataVal = dataVal[1:]
			}
			dataLines = append(dataLines, dataVal)
			if strings.TrimSpace(dataVal) == "[DONE]" {
				s.sawTerminal = true
				return nil, io.EOF
			}
		}
	}
}

// SawTerminal reports whether a terminal [DONE] event was encountered.
func (s *SSEReader) SawTerminal() bool {
	return s.sawTerminal
}
