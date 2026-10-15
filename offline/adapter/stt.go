package adapter

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

type sttMsg struct {
	Type        string `json:"type"`
	ID          string `json:"id,omitempty"`
	Text        string `json:"text,omitempty"`
	Language    string `json:"language,omitempty"`
	Message     string `json:"message,omitempty"`
	Code        string `json:"code,omitempty"`
	IsFinal     bool   `json:"is_final,omitempty"`
	SpeechFinal bool   `json:"speech_final,omitempty"`
}

type sttJob struct {
	kind string // "interim" | "final"
	id   uint64 // the utterance this job belongs to
	pcm  []byte
}

const preRoll = 200 * time.Millisecond

// handleSTT serves xAI's streaming STT on top of batch Whisper. Whisper cannot stream, so an
// Endpointer cuts utterances, interims re-decode the utterance so far, and speech_final decodes it
// once more. A single worker owns the backend calls and every transcript write, so results are
// ordered; a generation id drops an interim that finishes after its utterance's final.
func (s *Server) handleSTT(w http.ResponseWriter, r *http.Request) {
	rate := queryInt(r, "sample_rate", 16000)
	lang := r.URL.Query().Get("language")
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()
	c.SetReadLimit(1 << 20)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	var idb [8]byte
	_, _ = rand.Read(idb[:])
	if writeJSON(ctx, c, sttMsg{Type: "transcript.created", ID: hex.EncodeToString(idb[:])}) != nil {
		return
	}

	var cur atomic.Uint64 // id of the utterance in progress
	jobs := make(chan sttJob, 4)
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		s.sttWorker(ctx, c, jobs, &cur, rate, lang)
	}()

	clean := s.sttReadLoop(ctx, c, jobs, &cur, rate)
	if !clean {
		cancel() // the client is gone: stop decoding
	}
	close(jobs)
	<-workerDone
	if clean && ctx.Err() == nil {
		_ = c.Close(websocket.StatusNormalClosure, "done")
	}
}

// sttReadLoop feeds frames to the endpointer and queues jobs. It returns true when the client sent
// audio.done and false on a read error or cancellation. It is the producer of jobs.
func (s *Server) sttReadLoop(ctx context.Context, c *websocket.Conn, jobs chan<- sttJob, cur *atomic.Uint64, rate int) bool {
	ep := NewEndpointer(rate, s.cfg.EndpointSilence, s.cfg.EnergyThreshold)
	maxBytes := int(s.cfg.MaxUtterance/time.Second) * rate * 2
	preBytes := int(preRoll*time.Duration(rate)/time.Second) * 2
	var utt, pre []byte
	inUtt := false
	var lastInterim time.Time

	flush := func() bool {
		if len(utt) > 0 {
			select {
			case jobs <- sttJob{kind: "final", id: cur.Load(), pcm: utt}:
			case <-ctx.Done():
				return false
			}
			cur.Add(1) // any interim still queued or decoding for the old id is now stale
		}
		utt, inUtt = nil, false
		return true
	}

	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			return false
		}
		if typ == websocket.MessageText {
			var m sttMsg
			if json.Unmarshal(data, &m) == nil && m.Type == "audio.done" {
				return flush()
			}
			continue
		}
		ev := ep.Feed(data)
		switch ev {
		case EventSpeechStart:
			inUtt = true
			utt = append(make([]byte, 0, len(pre)+len(data)), pre...)
			lastInterim = time.Now()
		case EventSpeechAbort:
			utt, inUtt = nil, false
		}
		if inUtt {
			utt = append(utt, data...)
		} else {
			pre = append(pre, data...)
			if len(pre) > preBytes {
				pre = pre[len(pre)-preBytes:]
			}
		}
		if ev == EventSpeechEnd || (inUtt && len(utt) >= maxBytes) {
			ep.Reset()
			if !flush() {
				return false
			}
			continue
		}
		if inUtt && time.Since(lastInterim) >= s.cfg.InterimEvery && len(jobs) == 0 {
			lastInterim = time.Now()
			select {
			case jobs <- sttJob{kind: "interim", id: cur.Load(), pcm: append([]byte(nil), utt...)}:
			default:
			}
		}
	}
}

// sttWorker is the only caller of the backend and the only writer of transcripts. It runs jobs in
// order and ends when jobs is closed (audio.done or a dead client) or ctx is cancelled.
func (s *Server) sttWorker(ctx context.Context, c *websocket.Conn, jobs <-chan sttJob, cur *atomic.Uint64, rate int, lang string) {
	var last string
	for j := range jobs {
		if ctx.Err() != nil {
			return
		}
		if j.kind == "interim" && j.id != cur.Load() {
			continue
		}
		text, err := s.transcribe(ctx, j.pcm, rate, lang)
		if err != nil {
			if j.kind == "interim" || ctx.Err() != nil {
				continue // an interim is best effort; a cancelled ctx means the client left
			}
			// Tell the client why, then close with a handshake: a cancelled context would reset the
			// connection and could lose the error frame. Drain jobs so the reader never blocks on a
			// worker that is gone (it ends when the closed connection fails its next read).
			_ = writeJSON(ctx, c, sttMsg{Type: "error", Message: err.Error(), Code: "BACKEND_ERROR"})
			_ = c.Close(websocket.StatusInternalError, "backend error")
			for range jobs {
			}
			return
		}
		if text == "" {
			continue
		}
		if j.kind == "interim" && j.id != cur.Load() {
			continue // the utterance was finalized while this decode ran
		}
		msg := sttMsg{Type: "transcript.partial", Text: text, Language: lang, SpeechFinal: j.kind == "final"}
		if writeJSON(ctx, c, msg) != nil {
			return
		}
		if j.kind == "final" {
			last = text
		}
	}
	if ctx.Err() == nil {
		_ = writeJSON(ctx, c, sttMsg{Type: "transcript.done", Text: last})
	}
}

func (s *Server) transcribe(ctx context.Context, pcm []byte, rate int, lang string) (string, error) {
	release, err := s.lim.Acquire(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	return s.stt.Transcribe(ctx, pcm, rate, lang)
}
