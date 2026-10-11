package google

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	texttospeechpb "cloud.google.com/go/texttospeech/apiv1/texttospeechpb"
)

type recvStep func() (*texttospeechpb.StreamingSynthesizeResponse, error)

// fakeSynthStream scripts a StreamingSynthesize stream.
type fakeSynthStream struct {
	mu         sync.Mutex
	sent       []*texttospeechpb.StreamingSynthesizeRequest
	failSendAt int // index of the Send that returns sendErr; -1: none
	sendErr    error
	steps      []recvStep
	closedSend bool
}

func newFakeSynthStream(steps ...recvStep) *fakeSynthStream {
	return &fakeSynthStream{failSendAt: -1, steps: steps}
}

func (f *fakeSynthStream) Send(r *texttospeechpb.StreamingSynthesizeRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := len(f.sent)
	f.sent = append(f.sent, r)
	if n == f.failSendAt {
		return f.sendErr
	}
	return nil
}

func (f *fakeSynthStream) Recv() (*texttospeechpb.StreamingSynthesizeResponse, error) {
	f.mu.Lock()
	if len(f.steps) == 0 {
		f.mu.Unlock()
		return nil, io.EOF
	}
	s := f.steps[0]
	f.steps = f.steps[1:]
	f.mu.Unlock()
	return s()
}

func (f *fakeSynthStream) CloseSend() error {
	f.mu.Lock()
	f.closedSend = true
	f.mu.Unlock()
	return nil
}

func audioStep(b []byte) recvStep {
	return func() (*texttospeechpb.StreamingSynthesizeResponse, error) {
		return &texttospeechpb.StreamingSynthesizeResponse{AudioContent: b}, nil
	}
}

func errStep(err error) recvStep {
	return func() (*texttospeechpb.StreamingSynthesizeResponse, error) { return nil, err }
}

func TestIsChirp3HD(t *testing.T) {
	for voice, want := range map[string]bool{
		"en-US-Chirp3-HD-Aoede": true,
		"en-gb-chirp3-hd-kore":  true,
		"en-US-Journey-F":       false,
		"en-US-Neural2-C":       false,
		"en-US-Chirp-HD-D":      false, // an older Chirp family: not supported by streaming
		"":                      false,
	} {
		if got := IsChirp3HD(voice); got != want {
			t.Errorf("IsChirp3HD(%q) = %t, want %t", voice, got, want)
		}
	}
}

func TestRunSynthStream_ConfigThenPiecesInOrderThenCloseSend(t *testing.T) {
	s := newFakeSynthStream(audioStep([]byte{1, 2}))
	var got []byte
	err := runSynthStream(s, "en-US-Chirp3-HD-Aoede", "en-US", 24000, []string{"Hello there.", "How are you?"}, func(b []byte) error {
		got = append(got, b...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.sent) != 3 {
		t.Fatalf("sent %d messages, want config + 2 pieces", len(s.sent))
	}
	cfg := s.sent[0].GetStreamingConfig()
	if cfg == nil || cfg.GetVoice().GetName() != "en-US-Chirp3-HD-Aoede" || cfg.GetVoice().GetLanguageCode() != "en-US" {
		t.Fatalf("first message = %v, want the voice config", s.sent[0])
	}
	if ac := cfg.GetStreamingAudioConfig(); ac.GetAudioEncoding() != texttospeechpb.AudioEncoding_PCM || ac.GetSampleRateHertz() != 24000 {
		t.Fatalf("audio config = %v, want PCM at 24000", ac)
	}
	for i, want := range []string{"Hello there.", "How are you?"} {
		if text := s.sent[i+1].GetInput().GetText(); text != want {
			t.Errorf("piece %d = %q, want %q", i, text, want)
		}
	}
	if !s.closedSend {
		t.Error("CloseSend was not called")
	}
	if !bytes.Equal(got, []byte{1, 2}) {
		t.Errorf("audio = %v", got)
	}
}

func TestRunSynthStream_DeliversAudioBeforeTheStreamEnds(t *testing.T) {
	gate := make(chan struct{})
	s := newFakeSynthStream(audioStep([]byte{1, 2, 3, 4}), func() (*texttospeechpb.StreamingSynthesizeResponse, error) {
		<-gate
		return nil, io.EOF
	})
	got := make(chan []byte, 4)
	done := make(chan error, 1)
	go func() {
		done <- runSynthStream(s, "v", "en-US", 24000, []string{"Hi."}, func(b []byte) error {
			got <- append([]byte(nil), b...)
			return nil
		})
	}()
	select {
	case b := <-got:
		if !bytes.Equal(b, []byte{1, 2, 3, 4}) {
			t.Fatalf("first chunk = %v", b)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("audio was not delivered before the stream ended")
	}
	select {
	case <-done:
		t.Fatal("returned before the stream ended")
	default:
	}
	close(gate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestDropWAVHeader(t *testing.T) {
	hdr := []byte("RIFF\x00\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00\xc0\x5d\x00\x00\x80\xbb\x00\x00\x02\x00\x10\x00data\xff\xff\xff\xff")
	if got := dropWAVHeader(append(append([]byte(nil), hdr...), 9, 8)); !bytes.Equal(got, []byte{9, 8}) {
		t.Errorf("header + pcm = %v, want [9 8]", got)
	}
	if got := dropWAVHeader(hdr); len(got) != 0 {
		t.Errorf("header only = %v, want nothing to play", got)
	}
	if got := dropWAVHeader([]byte{1, 2, 3, 4}); !bytes.Equal(got, []byte{1, 2, 3, 4}) {
		t.Errorf("plain pcm changed: %v", got)
	}
	if got := dropWAVHeader([]byte("RIFF\x00\x00\x00\x00WAVE")); len(got) != 0 {
		t.Errorf("RIFF without a data chunk = %v, want nothing", got)
	}
}

func TestRunSynthStream_DropsALeadingWAVHeaderOnlyOnTheFirstChunk(t *testing.T) {
	hdr := []byte("RIFF\x00\x00\x00\x00WAVEdata\xff\xff\xff\xff")
	s := newFakeSynthStream(audioStep(append(append([]byte(nil), hdr...), 1, 2)), audioStep([]byte("RIFFxxxxxxxxxxxx")))
	var got []byte
	if err := runSynthStream(s, "v", "en-US", 24000, []string{"Hi."}, func(b []byte) error { got = append(got, b...); return nil }); err != nil {
		t.Fatal(err)
	}
	if want := append([]byte{1, 2}, []byte("RIFFxxxxxxxxxxxx")...); !bytes.Equal(got, want) {
		t.Errorf("audio = %q, want %q (a later chunk that happens to start with RIFF is audio)", got, want)
	}
}

func TestRunSynthStream_MapsStreamErrors(t *testing.T) {
	s := newFakeSynthStream(errStep(status.Error(codes.Unavailable, "backend down")))
	err := runSynthStream(s, "v", "en-US", 24000, []string{"Hi."}, func([]byte) error { return nil })
	var pe *ProviderError
	if !errors.As(err, &pe) || !pe.Retryable {
		t.Fatalf("err = %v, want a retryable ProviderError", err)
	}
}

func TestRunSynthStream_SendEOFReportsTheRecvError(t *testing.T) {
	s := newFakeSynthStream(errStep(status.Error(codes.Unauthenticated, "bad key")))
	s.failSendAt, s.sendErr = 1, io.EOF // the stream already ended: gRPC reports the cause through Recv
	err := runSynthStream(s, "v", "en-US", 24000, []string{"Hi."}, func([]byte) error { return nil })
	var pe *ProviderError
	if !errors.As(err, &pe) || pe.Retryable {
		t.Fatalf("err = %v, want the non-retryable cause from Recv", err)
	}
}

func TestRunSynthStream_OnAudioErrorStopsReading(t *testing.T) {
	boom := errors.New("boom")
	s := newFakeSynthStream(audioStep([]byte{1}), audioStep([]byte{2}))
	err := runSynthStream(s, "v", "en-US", 24000, []string{"Hi."}, func([]byte) error { return boom })
	if err != boom {
		t.Fatalf("err = %v, want the onAudio error unchanged", err)
	}
	if len(s.steps) != 1 {
		t.Errorf("%d steps left, want 1: reading must stop at the first onAudio error", len(s.steps))
	}
}
