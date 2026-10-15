package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func fastCfg(t *testing.T) Config {
	c := mustConfig(t)
	c.InterimEvery = 100 * time.Millisecond
	c.EndpointSilence = 300 * time.Millisecond
	c.MaxConcurrency = 4
	return c
}

// sendAudio streams pcm as 20 ms binary frames, paced faster than real time.
func sendAudio(t *testing.T, ctx context.Context, c *websocket.Conn, pcm []byte, pace time.Duration) {
	t.Helper()
	step := 640
	for i := 0; i < len(pcm); i += step {
		if err := c.Write(ctx, websocket.MessageBinary, pcm[i:min(i+step, len(pcm))]); err != nil {
			t.Fatalf("write audio: %v", err)
		}
		time.Sleep(pace)
	}
}

// sendAudioLoose is sendAudio for a server that may hang up mid-stream (a backend failure).
func sendAudioLoose(ctx context.Context, c *websocket.Conn, pcm []byte) {
	for i := 0; i < len(pcm); i += 640 {
		if c.Write(ctx, websocket.MessageBinary, pcm[i:min(i+640, len(pcm))]) != nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

func readSTT(t *testing.T, ctx context.Context, c *websocket.Conn) sttMsg {
	t.Helper()
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m sttMsg
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// drainSTT reads until transcript.done or error and returns every message.
func drainSTT(t *testing.T, ctx context.Context, c *websocket.Conn) []sttMsg {
	t.Helper()
	var all []sttMsg
	for {
		m := readSTT(t, ctx, c)
		all = append(all, m)
		if m.Type == "transcript.done" || m.Type == "error" {
			return all
		}
	}
}

func speechFinals(ms []sttMsg) (out []string) {
	for _, m := range ms {
		if m.Type == "transcript.partial" && m.SpeechFinal {
			out = append(out, m.Text)
		}
	}
	return out
}

func audioDone(t *testing.T, ctx context.Context, c *websocket.Conn) {
	t.Helper()
	if err := c.Write(ctx, websocket.MessageText, []byte(`{"type":"audio.done"}`)); err != nil {
		t.Fatal(err)
	}
}

func TestSTTCreatedThenInterimThenFinalThenDone(t *testing.T) {
	var n atomic.Int32
	stt := &fakeSTT{fn: func(_ context.Context, pcm []byte) (string, error) {
		return "decode " + string(rune('a'+n.Add(1)-1)), nil
	}}
	srv := httptest.NewServer(New(fastCfg(t), nil, stt, okReady).Handler())
	defer srv.Close()
	c, ctx := dialWS(t, srv, "/v1/stt?sample_rate=16000&encoding=pcm&language=en&interim_results=true")
	if m := readSTT(t, ctx, c); m.Type != "transcript.created" || m.ID == "" {
		t.Fatalf("first message %+v", m)
	}
	sendAudio(t, ctx, c, append(pcmTone(1200*time.Millisecond, 0.3), pcmSilence(500*time.Millisecond)...), 2*time.Millisecond)
	audioDone(t, ctx, c)
	all := drainSTT(t, ctx, c)
	last := all[len(all)-1]
	finals := speechFinals(all)
	if last.Type != "transcript.done" || len(finals) != 1 {
		t.Fatalf("messages %+v", all)
	}
	interims := 0
	for _, m := range all {
		if m.Type == "transcript.partial" && !m.SpeechFinal && m.Text != "" {
			interims++
		}
	}
	if interims == 0 {
		t.Fatalf("no interim before the final: %+v", all)
	}
	for _, m := range all {
		if m.IsFinal {
			t.Fatalf("is_final must never be sent: %+v", m)
		}
	}
}

func TestSTTSilenceNeverDecodes(t *testing.T) {
	stt := &fakeSTT{fn: func(context.Context, []byte) (string, error) { return "Thank you.", nil }}
	srv := httptest.NewServer(New(fastCfg(t), nil, stt, okReady).Handler())
	defer srv.Close()
	c, ctx := dialWS(t, srv, "/v1/stt")
	readSTT(t, ctx, c) // created
	sendAudio(t, ctx, c, pcmSilence(2*time.Second), 0)
	sendAudio(t, ctx, c, append(pcmTone(80*time.Millisecond, 0.5), pcmSilence(time.Second)...), 0) // a click
	audioDone(t, ctx, c)
	all := drainSTT(t, ctx, c)
	if stt.calls.Load() != 0 || len(speechFinals(all)) != 0 {
		t.Fatalf("decoded silence/click: calls=%d msgs=%+v", stt.calls.Load(), all)
	}
	if all[len(all)-1].Type != "transcript.done" {
		t.Fatalf("stream did not end cleanly: %+v", all)
	}
}

func TestSTTTwoUtterancesFinalizeInOrder(t *testing.T) {
	var n atomic.Int32
	stt := &fakeSTT{fn: func(_ context.Context, pcm []byte) (string, error) {
		time.Sleep(30 * time.Millisecond)
		return strings.Repeat("w", int(n.Add(1))), nil
	}}
	cfg := fastCfg(t)
	cfg.InterimEvery = time.Hour // finals only: the text of the Nth decode is N letters
	srv := httptest.NewServer(New(cfg, nil, stt, okReady).Handler())
	defer srv.Close()
	c, ctx := dialWS(t, srv, "/v1/stt")
	readSTT(t, ctx, c)
	for i := 0; i < 2; i++ {
		sendAudio(t, ctx, c, append(pcmTone(time.Second, 0.3), pcmSilence(500*time.Millisecond)...), time.Millisecond)
	}
	audioDone(t, ctx, c)
	finals := speechFinals(drainSTT(t, ctx, c))
	if len(finals) != 2 || finals[0] != "w" || finals[1] != "ww" {
		t.Fatalf("finals %v", finals)
	}
}

func TestSTTStaleInterimDropped(t *testing.T) {
	gate := make(chan struct{})
	stt := &fakeSTT{fn: func(_ context.Context, pcm []byte) (string, error) {
		if len(pcm) < 16000*2*2 { // an interim of a still-short utterance: held back until after the final
			<-gate
			return "STALE", nil
		}
		return "final text", nil
	}}
	cfg := fastCfg(t)
	cfg.InterimEvery = 50 * time.Millisecond
	srv := httptest.NewServer(New(cfg, nil, stt, okReady).Handler())
	defer srv.Close()
	c, ctx := dialWS(t, srv, "/v1/stt")
	readSTT(t, ctx, c)
	go func() { time.Sleep(900 * time.Millisecond); close(gate) }()
	sendAudio(t, ctx, c, append(pcmTone(2300*time.Millisecond, 0.3), pcmSilence(500*time.Millisecond)...), time.Millisecond)
	audioDone(t, ctx, c)
	for _, m := range drainSTT(t, ctx, c) {
		if m.Text == "STALE" {
			t.Fatalf("a stale interim was delivered: %+v", m)
		}
	}
}

func TestSTTUtteranceCap(t *testing.T) {
	stt := &fakeSTT{fn: func(context.Context, []byte) (string, error) { return "cut", nil }}
	cfg := fastCfg(t)
	cfg.MaxUtterance = time.Second
	cfg.InterimEvery = time.Hour
	srv := httptest.NewServer(New(cfg, nil, stt, okReady).Handler())
	defer srv.Close()
	c, ctx := dialWS(t, srv, "/v1/stt")
	readSTT(t, ctx, c)
	sendAudio(t, ctx, c, pcmTone(3500*time.Millisecond, 0.3), 0) // continuous speech, no pause
	audioDone(t, ctx, c)
	if finals := speechFinals(drainSTT(t, ctx, c)); len(finals) < 3 {
		t.Fatalf("a 3.5 s monologue with a 1 s cap gave %d finals", len(finals))
	}
}

func TestSTTFinalErrorSendsErrorAndCloses(t *testing.T) {
	stt := &fakeSTT{fn: func(context.Context, []byte) (string, error) { return "", errors.New("whisper crashed") }}
	cfg := fastCfg(t)
	cfg.InterimEvery = time.Hour
	srv := httptest.NewServer(New(cfg, nil, stt, okReady).Handler())
	defer srv.Close()
	c, ctx := dialWS(t, srv, "/v1/stt")
	readSTT(t, ctx, c)
	sendAudioLoose(ctx, c, append(pcmTone(time.Second, 0.3), pcmSilence(500*time.Millisecond)...))
	all := drainSTT(t, ctx, c)
	if last := all[len(all)-1]; last.Type != "error" || last.Message == "" {
		t.Fatalf("got %+v", all)
	}
}

func TestSTTWorkerStopsOnDisconnect(t *testing.T) {
	started, cancelled := make(chan struct{}), make(chan struct{})
	stt := &fakeSTT{fn: func(ctx context.Context, _ []byte) (string, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return "", ctx.Err()
	}}
	cfg := fastCfg(t)
	cfg.InterimEvery = time.Hour
	srv := httptest.NewServer(New(cfg, nil, stt, okReady).Handler())
	defer srv.Close()
	c, ctx := dialWS(t, srv, "/v1/stt")
	readSTT(t, ctx, c)
	sendAudio(t, ctx, c, append(pcmTone(time.Second, 0.3), pcmSilence(500*time.Millisecond)...), time.Millisecond)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("decode never started")
	}
	c.CloseNow()
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("decode was not cancelled after the client disconnected")
	}
}
