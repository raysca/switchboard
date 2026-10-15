package adapter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// TTSBackend returns raw PCM16LE mono at sampleRate; the caller closes the reader.
type TTSBackend interface {
	Synthesize(ctx context.Context, text, voice string, sampleRate int) (io.ReadCloser, error)
}

// STTBackend transcribes one complete utterance of PCM16LE mono.
type STTBackend interface {
	Transcribe(ctx context.Context, pcm []byte, sampleRate int, language string) (string, error)
}

// BackendError is a non-200 answer from a model server.
type BackendError struct {
	Status int
	Msg    string
}

func (e *BackendError) Error() string { return fmt.Sprintf("backend status %d: %s", e.Status, e.Msg) }

// httpStatusFor maps a backend failure to what the xAI adapters expect: a backend 4xx (unknown
// voice) is a client error (400), a full queue is 503, anything else is retryable (502).
func httpStatusFor(err error) int {
	var be *BackendError
	switch {
	case errors.Is(err, ErrBusy):
		return http.StatusServiceUnavailable
	case errors.As(err, &be) && be.Status >= 400 && be.Status < 500:
		return http.StatusBadRequest
	default:
		return http.StatusBadGateway
	}
}
