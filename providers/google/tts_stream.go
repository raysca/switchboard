package google

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"

	"govoice/core/netpool"

	texttospeechpb "cloud.google.com/go/texttospeech/apiv1/texttospeechpb"
)

// IsChirp3HD reports whether voice is a Chirp 3: HD voice ("en-US-Chirp3-HD-Aoede"), the only
// voice family StreamingSynthesize supports.
func IsChirp3HD(voice string) bool {
	return strings.Contains(strings.ToLower(voice), "chirp3-hd")
}

// StreamingTTSClient is a TTSClient that can also synthesize over one StreamingSynthesize stream.
type StreamingTTSClient interface {
	TTSClient
	// StreamSynthesize sends pieces, in order, on one stream and passes the PCM16LE audio to
	// onAudio as it arrives. It returns nil once the stream ended cleanly, otherwise the first
	// error: a provider error, or the one onAudio returned (unchanged).
	StreamSynthesize(ctx context.Context, pieces []string, voice string, sampleRate int, onAudio func(pcm []byte) error) error
}

var _ StreamingTTSClient = (*RealGoogleTTSClient)(nil)

// StreamSynthesize opens one StreamingSynthesize stream on the client's (warm) gRPC connection.
func (c *RealGoogleTTSClient) StreamSynthesize(ctx context.Context, pieces []string, voice string, sampleRate int, onAudio func([]byte) error) error {
	netpool.CountRequest("google", "tts")
	stream, err := c.client.StreamingSynthesize(ctx)
	if err != nil {
		return MapError("tts", err)
	}
	return runSynthStream(stream, voice, voiceLanguage(voice, c.languageCode), sampleRate, pieces, onAudio)
}

// synthStream is the part of the gRPC stream the wire code uses (tests fake it).
type synthStream interface {
	Send(*texttospeechpb.StreamingSynthesizeRequest) error
	Recv() (*texttospeechpb.StreamingSynthesizeResponse, error)
	CloseSend() error
}

// runSynthStream sends the voice config and the pieces, half-closes, and delivers audio until the
// response stream ends.
func runSynthStream(s synthStream, voice, language string, sampleRate int, pieces []string, onAudio func([]byte) error) error {
	sendErr := s.Send(&texttospeechpb.StreamingSynthesizeRequest{StreamingRequest: &texttospeechpb.StreamingSynthesizeRequest_StreamingConfig{
		StreamingConfig: &texttospeechpb.StreamingSynthesizeConfig{
			Voice: &texttospeechpb.VoiceSelectionParams{LanguageCode: language, Name: voice},
			StreamingAudioConfig: &texttospeechpb.StreamingAudioConfig{
				AudioEncoding:   texttospeechpb.AudioEncoding_PCM,
				SampleRateHertz: int32(sampleRate),
			},
		},
	}})
	for _, piece := range pieces {
		if sendErr != nil {
			break
		}
		sendErr = s.Send(&texttospeechpb.StreamingSynthesizeRequest{StreamingRequest: &texttospeechpb.StreamingSynthesizeRequest_Input{
			Input: &texttospeechpb.StreamingSynthesisInput{InputSource: &texttospeechpb.StreamingSynthesisInput_Text{Text: piece}},
		}})
	}
	switch {
	case sendErr == nil:
		if err := s.CloseSend(); err != nil {
			return MapError("tts", err)
		}
	case !errors.Is(sendErr, io.EOF):
		return MapError("tts", sendErr)
	}
	// io.EOF from Send means the stream already ended; Recv returns the real cause.
	first := true
	for {
		resp, err := s.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return MapError("tts", err)
		}
		audio := resp.GetAudioContent()
		if len(audio) == 0 {
			continue
		}
		if first {
			audio, first = dropWAVHeader(audio), false
		}
		if len(audio) == 0 {
			continue
		}
		if err := onAudio(audio); err != nil {
			return err
		}
	}
}

// dropWAVHeader removes a RIFF/WAVE header from the first audio chunk, if there is one.
// ponytail: looks for the "data" chunk id in the first 128 bytes only; a header split across
// chunks would need buffering. Streaming PCM is not expected to carry a header at all.
func dropWAVHeader(b []byte) []byte {
	if len(b) < 12 || !bytes.Equal(b[:4], []byte("RIFF")) {
		return b
	}
	if i := bytes.Index(b[:min(len(b), 128)], []byte("data")); i >= 0 && i+8 <= len(b) {
		return b[i+8:]
	}
	return nil
}
