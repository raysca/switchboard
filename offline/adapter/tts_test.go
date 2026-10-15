package adapter

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func postTTS(t *testing.T, srv *httptest.Server, body string) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.Post(srv.URL+"/v1/tts", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func TestTTSPostReturnsPCMAndMapsVoice(t *testing.T) {
	ft := &fakeTTS{pcm: bytes.Repeat([]byte{7, 0}, 100)}
	srv := httptest.NewServer(New(mustConfig(t), ft, nil, okReady).Handler())
	defer srv.Close()
	resp, b := postTTS(t, srv, `{"text":"hi","voice_id":"eve","language":"en","output_format":{"codec":"pcm","sample_rate":24000}}`)
	if resp.StatusCode != 200 || len(b) != 200 || ft.voice.Load() != "af_heart" {
		t.Fatalf("status=%d len=%d voice=%v", resp.StatusCode, len(b), ft.voice.Load())
	}
}

func TestTTSPostBackendErrors(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{&BackendError{Status: 404, Msg: "no voice"}, 400},
		{&BackendError{Status: 500, Msg: "boom"}, 502},
		{ErrBusy, 503},
		{errors.New("connection refused"), 502},
	} {
		srv := httptest.NewServer(New(mustConfig(t), &fakeTTS{err: tc.err}, nil, okReady).Handler())
		resp, _ := postTTS(t, srv, `{"text":"hi","voice_id":"x"}`)
		srv.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("%v: got %d want %d", tc.err, resp.StatusCode, tc.want)
		}
	}
}

func TestTTSPostRejectsBadJSONAndEmptyText(t *testing.T) {
	srv := httptest.NewServer(New(mustConfig(t), &fakeTTS{}, nil, okReady).Handler())
	defer srv.Close()
	for _, body := range []string{`{`, `{"text":""}`} {
		if resp, _ := postTTS(t, srv, body); resp.StatusCode != 400 {
			t.Errorf("%q: got %d want 400", body, resp.StatusCode)
		}
	}
}

func TestStreamPCMDropsTrailingOddByte(t *testing.T) {
	var got [][]byte
	err := streamPCM(bytes.NewReader(make([]byte, 9)), 4, func(b []byte) error {
		got = append(got, append([]byte(nil), b...))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, b := range got {
		if len(b)%2 != 0 {
			t.Fatalf("odd chunk %d", len(b))
		}
		total += len(b)
	}
	if total != 8 {
		t.Fatalf("total %d, want 8 (one odd byte dropped)", total)
	}
}

func TestStreamPCMStopsOnSendError(t *testing.T) {
	boom := errors.New("write failed")
	err := streamPCM(bytes.NewReader(make([]byte, 100)), 10, func([]byte) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
}
