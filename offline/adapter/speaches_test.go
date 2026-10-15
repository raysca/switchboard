package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func speachesFor(t *testing.T, h http.HandlerFunc) *Speaches {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	cfg := mustConfig(t)
	cfg.SpeachesURL = srv.URL
	return NewSpeaches(cfg)
}

func TestSpeachesSynthesizeRequestAndBody(t *testing.T) {
	var got map[string]any
	sp := speachesFor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/speech" || r.Method != "POST" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte{1, 2, 3, 4})
	})
	rc, err := sp.Synthesize(context.Background(), "hello", "af_heart", 24000)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	if len(b) != 4 || got["input"] != "hello" || got["voice"] != "af_heart" || got["response_format"] != "pcm" ||
		got["sample_rate"] != float64(24000) || got["model"] != mustConfig(t).TTSModel {
		t.Fatalf("body=%v request=%v", b, got)
	}
}

func TestSpeachesSynthesizeBackendError(t *testing.T) {
	sp := speachesFor(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no such voice", 404) })
	_, err := sp.Synthesize(context.Background(), "x", "nope", 24000)
	var be *BackendError
	if !errors.As(err, &be) || be.Status != 404 || !strings.Contains(be.Msg, "no such voice") {
		t.Fatalf("got %v", err)
	}
}

func TestSpeachesTranscribeSendsWavAndVadFilter(t *testing.T) {
	pcm := make([]byte, 3200)
	sp := speachesFor(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			t.Fatal(err)
		}
		wav, _ := io.ReadAll(f)
		if len(wav) != 44+len(pcm) || string(wav[:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
			t.Errorf("wav: len=%d head=%q", len(wav), wav[:12])
		}
		if r.FormValue("vad_filter") != "true" || r.FormValue("language") != "en" || r.FormValue("model") != mustConfig(t).STTModel {
			t.Errorf("form: %v", r.MultipartForm.Value)
		}
		_, _ = w.Write([]byte(`{"text":" hello there "}`))
	})
	text, err := sp.Transcribe(context.Background(), pcm, 16000, "en")
	if err != nil || text != "hello there" {
		t.Fatalf("text=%q err=%v", text, err)
	}
}

func TestSpeachesHealth(t *testing.T) {
	status := 200
	sp := speachesFor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("path %s", r.URL.Path)
		}
		w.WriteHeader(status)
	})
	if err := sp.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	status = 500
	if err := sp.Health(context.Background()); err == nil {
		t.Fatal("500 reported healthy")
	}
}

func TestWavHeaderFields(t *testing.T) {
	h := wavHeader(100, 16000)
	if len(h) != 44 || string(h[:4]) != "RIFF" || string(h[36:40]) != "data" {
		t.Fatalf("header: %q", h)
	}
}
