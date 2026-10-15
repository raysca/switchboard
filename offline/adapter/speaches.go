package adapter

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
)

// Speaches is the HTTP client for a Speaches server (OpenAI-compatible faster-whisper + Kokoro).
type Speaches struct {
	base     string
	sttModel string
	ttsModel string
	hc       *http.Client // no overall timeout: TTS bodies are read as a stream; callers pass a ctx
}

func NewSpeaches(cfg Config) *Speaches {
	return &Speaches{base: strings.TrimRight(cfg.SpeachesURL, "/"), sttModel: cfg.STTModel, ttsModel: cfg.TTSModel, hc: &http.Client{}}
}

func (s *Speaches) Synthesize(ctx context.Context, text, voice string, sampleRate int) (io.ReadCloser, error) {
	body, err := json.Marshal(map[string]any{
		"model": s.ttsModel, "input": text, "voice": voice, "response_format": "pcm", "sample_rate": sampleRate,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal speech request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base+"/v1/audio/speech", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build speech request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("speech request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, &BackendError{Status: resp.StatusCode, Msg: strings.TrimSpace(string(msg))}
	}
	return resp.Body, nil
}

func (s *Speaches) Transcribe(ctx context.Context, pcm []byte, sampleRate int, language string) (string, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "audio.wav")
	if err != nil {
		return "", fmt.Errorf("build transcription form: %w", err)
	}
	_, _ = fw.Write(wavHeader(len(pcm), sampleRate))
	_, _ = fw.Write(pcm)
	fields := map[string]string{"model": s.sttModel, "response_format": "json", "vad_filter": "true"}
	if language != "" {
		fields["language"] = language
	}
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			return "", fmt.Errorf("build transcription form: %w", err)
		}
	}
	if err := mw.Close(); err != nil {
		return "", fmt.Errorf("build transcription form: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base+"/v1/audio/transcriptions", &body)
	if err != nil {
		return "", fmt.Errorf("build transcription request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := s.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("transcription request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", &BackendError{Status: resp.StatusCode, Msg: strings.TrimSpace(string(msg))}
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", fmt.Errorf("decode transcription: %w", err)
	}
	return strings.TrimSpace(out.Text), nil
}

// Health reports whether Speaches answers; models are loaded lazily by Speaches itself.
func (s *Speaches) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"/health", nil)
	if err != nil {
		return fmt.Errorf("build health request: %w", err)
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return fmt.Errorf("speaches health: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("speaches health: status %d", resp.StatusCode)
	}
	return nil
}

// wavHeader is the 44-byte RIFF header for PCM16 mono of dataLen bytes.
func wavHeader(dataLen, sampleRate int) []byte {
	h := make([]byte, 44)
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], uint32(36+dataLen))
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1) // PCM
	binary.LittleEndian.PutUint16(h[22:], 1) // mono
	binary.LittleEndian.PutUint32(h[24:], uint32(sampleRate))
	binary.LittleEndian.PutUint32(h[28:], uint32(sampleRate*2))
	binary.LittleEndian.PutUint16(h[32:], 2)  // block align
	binary.LittleEndian.PutUint16(h[34:], 16) // bits per sample
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], uint32(dataLen))
	return h
}
