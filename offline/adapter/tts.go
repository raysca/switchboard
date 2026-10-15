package adapter

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/coder/websocket"
)

const maxTTSBody = 1 << 20 // a sentence unit is a few hundred bytes; 1 MiB is far beyond any real request

// Speech PCM is 8 to 48 kHz. Anything else is refused: a huge rate overflows the buffer sizes derived
// from it (and lets one connection hold hundreds of MiB), a tiny one makes a zero-length chunk.
const (
	minSampleRate = 8000
	maxSampleRate = 48000
)

// sampleRate reads the sample_rate query value: def when absent, false when present but unusable.
func sampleRate(r *http.Request, def int) (int, bool) {
	v := r.URL.Query().Get("sample_rate")
	if v == "" {
		return def, true
	}
	n, err := strconv.Atoi(v)
	return n, err == nil && n >= minSampleRate && n <= maxSampleRate
}

func badSampleRate(w http.ResponseWriter) {
	writeError(w, http.StatusBadRequest, "sample_rate must be between 8000 and 48000")
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
	if rate == 0 {
		rate = 24000
	}
	if rate < minSampleRate || rate > maxSampleRate {
		badSampleRate(w)
		return
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

const maxTTSText = 60000 // xAI's documented limit for one text.delta message

type ttsMsg struct {
	Type    string `json:"type"`
	Delta   string `json:"delta,omitempty"`
	Message string `json:"message,omitempty"`
}

func writeJSON(ctx context.Context, c *websocket.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.Write(ctx, websocket.MessageText, b)
}

// handleTTSWS serves xAI's streaming TTS: one connection per reply, reused for its units. A unit is
// text.delta... then text.done; the answer is audio.delta... then audio.done, and the connection
// stays open for the next unit. Closing the socket (barge-in) cancels the running synthesis.
func (s *Server) handleTTSWS(w http.ResponseWriter, r *http.Request) {
	voice := s.cfg.Voice(r.URL.Query().Get("voice"))
	rate, ok := sampleRate(r, 24000)
	if !ok {
		badSampleRate(w)
		return
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()
	c.SetReadLimit(2 << 20)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// One reader goroutine: it is the only place a close is noticed while a unit is synthesizing.
	// It owns msgs (producer-only close) and cancels ctx when the socket dies.
	msgs := make(chan ttsMsg, 4)
	go func() {
		defer close(msgs)
		defer cancel()
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			var m ttsMsg
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			select {
			case msgs <- m:
			case <-ctx.Done():
				return
			}
		}
	}()

	var text strings.Builder
	fail := func(msg string) { _ = writeJSON(ctx, c, ttsMsg{Type: "error", Message: msg}) }
	for m := range msgs {
		switch m.Type {
		case "text.delta":
			if text.Len()+len(m.Delta) > maxTTSText {
				fail("text exceeds the per-unit limit")
				return
			}
			text.WriteString(m.Delta)
		case "text.done":
			unit := text.String()
			text.Reset()
			if err := s.speak(ctx, c, unit, voice, rate); err != nil {
				if ctx.Err() == nil {
					fail(err.Error())
				}
				return
			}
		}
	}
}

func (s *Server) speak(ctx context.Context, c *websocket.Conn, text, voice string, rate int) error {
	if strings.TrimSpace(text) == "" {
		return writeJSON(ctx, c, ttsMsg{Type: "audio.done"})
	}
	release, err := s.lim.Acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	body, err := s.tts.Synthesize(ctx, text, voice, rate)
	if err != nil {
		return err
	}
	defer body.Close()
	// 100 ms of PCM16 mono per audio.delta.
	err = streamPCM(body, rate*2/10, func(b []byte) error {
		return writeJSON(ctx, c, ttsMsg{Type: "audio.delta", Delta: base64.StdEncoding.EncodeToString(b)})
	})
	if err != nil {
		return err
	}
	return writeJSON(ctx, c, ttsMsg{Type: "audio.done"})
}
