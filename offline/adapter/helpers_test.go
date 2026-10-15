package adapter

import (
	"bytes"
	"context"
	"io"
	"sync/atomic"
	"testing"
)

func mustConfig(t *testing.T) Config {
	t.Helper()
	c, err := LoadConfig(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// fakeTTS returns pcm for any text; it records the voice and text, and reports cancellation of the request.
type fakeTTS struct {
	pcm       []byte
	err       error
	voice     atomic.Value // string
	gotText   atomic.Value // string
	cancelled chan struct{}
	block     chan struct{} // when non-nil, Synthesize blocks until ctx is cancelled
}

func (f *fakeTTS) Synthesize(ctx context.Context, text, voice string, _ int) (io.ReadCloser, error) {
	f.voice.Store(voice)
	f.gotText.Store(text)
	if f.block != nil {
		<-ctx.Done()
		if f.cancelled != nil {
			close(f.cancelled)
		}
		return nil, ctx.Err()
	}
	if f.err != nil {
		return nil, f.err
	}
	return io.NopCloser(bytes.NewReader(f.pcm)), nil
}

// fakeSTT calls fn for every transcription.
type fakeSTT struct {
	fn    func(ctx context.Context, pcm []byte) (string, error)
	calls atomic.Int32
}

func (f *fakeSTT) Transcribe(ctx context.Context, pcm []byte, _ int, _ string) (string, error) {
	f.calls.Add(1)
	return f.fn(ctx, pcm)
}

func okReady(context.Context) error { return nil }
