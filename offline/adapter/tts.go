package adapter

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
)

const maxTTSBody = 1 << 20 // a sentence unit is a few hundred bytes; 1 MiB is far beyond any real request

func queryInt(r *http.Request, key string, def int) int {
	if n, err := strconv.Atoi(r.URL.Query().Get(key)); err == nil && n > 0 {
		return n
	}
	return def
}

// handleTTSPost is the unary fallback the xAI adapter uses when the stream fails before audio.
func (s *Server) handleTTSPost(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text         string `json:"text"`
		VoiceID      string `json:"voice_id"`
		OutputFormat struct {
			SampleRate int `json:"sample_rate"`
		} `json:"output_format"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxTTSBody)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		writeError(w, http.StatusBadRequest, "text is required")
		return
	}
	rate := req.OutputFormat.SampleRate
	if rate <= 0 {
		rate = 24000
	}
	release, err := s.lim.Acquire(r.Context())
	if err != nil {
		writeError(w, httpStatusFor(err), err.Error())
		return
	}
	defer release()
	body, err := s.tts.Synthesize(r.Context(), req.Text, s.cfg.Voice(req.VoiceID), rate)
	if err != nil {
		writeError(w, httpStatusFor(err), err.Error())
		return
	}
	defer body.Close()
	w.Header().Set("Content-Type", "audio/pcm")
	// The status is already sent once the copy starts; a mid-body failure surfaces to the xAI client
	// as a truncated body, which it treats as a retryable transport error.
	_ = streamPCM(body, 9600, func(b []byte) error { _, werr := w.Write(b); return werr })
}

// streamPCM reads r in chunk-byte pieces and hands each to send. Every piece has an even length:
// PCM16 backends do not send an odd total, so a trailing odd byte is dropped.
func streamPCM(r io.Reader, chunk int, send func([]byte) error) error {
	buf := make([]byte, chunk&^1)
	for {
		n, err := io.ReadFull(r, buf)
		n &^= 1
		if n > 0 {
			if serr := send(buf[:n]); serr != nil {
				return serr
			}
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
