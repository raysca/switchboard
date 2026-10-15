package adapter

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// Regression (second review, finding 1): a sample_rate outside what PCM speech uses must be refused
// before the upgrade. 50e9 overflowed the pre-roll size and panicked the handler (leaking its worker);
// 1e9 made one connection buffer hundreds of MiB; 1..9 made the TTS chunk size zero.
func TestSampleRateIsBounded(t *testing.T) {
	srv := httptest.NewServer(New(mustConfig(t), &fakeTTS{pcm: []byte{1, 0}}, &fakeSTT{fn: func(context.Context, []byte) (string, error) { return "x", nil }}, okReady).Handler())
	defer srv.Close()
	for _, path := range []string{"/v1/stt", "/v1/tts"} {
		for _, rate := range []string{"1", "7999", "48001", "1000000000", "50000000000", "-5", "abc"} {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+path+"?sample_rate="+rate, nil)
			cancel()
			if err == nil {
				t.Errorf("%s sample_rate=%s was upgraded", path, rate)
				continue
			}
			if resp == nil || resp.StatusCode != http.StatusBadRequest {
				t.Errorf("%s sample_rate=%s: want 400, got %v (%v)", path, rate, resp, err)
			}
		}
	}
	for _, rate := range []int{1, 7999, 48001, 1000000000} {
		body := `{"text":"hi","output_format":{"sample_rate":` + strconv.Itoa(rate) + `}}`
		resp, err := http.Post(srv.URL+"/v1/tts", "application/json", bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("POST sample_rate=%d: want 400, got %d", rate, resp.StatusCode)
		}
	}
}

func TestSampleRateDefaultsAndLimitsStillWork(t *testing.T) {
	srv := httptest.NewServer(New(mustConfig(t), &fakeTTS{pcm: []byte{1, 0}}, nil, okReady).Handler())
	defer srv.Close()
	for _, q := range []string{"", "?sample_rate=8000", "?sample_rate=48000", "?sample_rate=44100"} {
		c, _ := dialWS(t, srv, "/v1/tts"+q)
		c.CloseNow()
	}
}

// Regression (second review, finding 4): the final must not wait for an interim decode that is still
// running. The xAI client gives a stream about 2 s after audio.done to deliver speech_final; a slow
// interim of a long utterance used to sit in front of the final and burn that window.
func TestSTTFinalIsNotBlockedByARunningInterim(t *testing.T) {
	var calls atomic.Int32
	stt := &fakeSTT{fn: func(ctx context.Context, _ []byte) (string, error) {
		if calls.Add(1) == 1 { // the first decode is an interim: it never finishes on its own
			<-ctx.Done()
			return "", ctx.Err()
		}
		return "final", nil
	}}
	cfg := fastCfg(t)
	cfg.InterimEvery = 50 * time.Millisecond
	srv := httptest.NewServer(New(cfg, nil, stt, okReady).Handler())
	defer srv.Close()
	c, ctx := dialWS(t, srv, "/v1/stt")
	readSTT(t, ctx, c)
	sendAudio(t, ctx, c, append(pcmTone(time.Second, 0.3), pcmSilence(500*time.Millisecond)...), 5*time.Millisecond)
	audioDone(t, ctx, c)
	got := make(chan []sttMsg, 1)
	go func() { got <- drainSTT(t, ctx, c) }()
	select {
	case msgs := <-got:
		if f := speechFinals(msgs); len(f) != 1 || f[0] != "final" {
			t.Fatalf("finals %v", f)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the final was stuck behind a running interim decode")
	}
}

// Kills the mutant that stamps every interim with utterance id 0: from the second utterance on all
// interims would be fenced out and nothing else would notice.
func TestSTTSecondUtteranceStillGetsInterims(t *testing.T) {
	var n atomic.Int32
	stt := &fakeSTT{fn: func(context.Context, []byte) (string, error) {
		time.Sleep(10 * time.Millisecond)
		return "t" + strconv.Itoa(int(n.Add(1))), nil
	}}
	cfg := fastCfg(t)
	cfg.InterimEvery = 50 * time.Millisecond
	srv := httptest.NewServer(New(cfg, nil, stt, okReady).Handler())
	defer srv.Close()
	c, ctx := dialWS(t, srv, "/v1/stt")
	readSTT(t, ctx, c)
	for i := 0; i < 2; i++ {
		sendAudio(t, ctx, c, append(pcmTone(time.Second, 0.3), pcmSilence(500*time.Millisecond)...), 5*time.Millisecond)
	}
	audioDone(t, ctx, c)
	finals, interimsAfterFirstFinal := 0, 0
	for _, m := range drainSTT(t, ctx, c) {
		switch {
		case m.Type == "transcript.partial" && m.SpeechFinal:
			finals++
		case m.Type == "transcript.partial" && finals == 1:
			interimsAfterFirstFinal++
		}
	}
	if finals != 2 || interimsAfterFirstFinal == 0 {
		t.Fatalf("finals=%d interims during the second utterance=%d", finals, interimsAfterFirstFinal)
	}
}
