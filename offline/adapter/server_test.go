package adapter

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthzReflectsBackend(t *testing.T) {
	var down error
	s := New(mustConfig(t), nil, nil, func(context.Context) error { return down })
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	get := func() int {
		resp, err := http.Get(srv.URL + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := get(); got != 200 {
		t.Fatalf("healthy: %d", got)
	}
	down = errors.New("speaches down")
	if got := get(); got != 503 {
		t.Fatalf("backend down: %d", got)
	}
}

func TestHealthzIsOpenButV1RequiresToken(t *testing.T) {
	cfg := mustConfig(t)
	cfg.Token = "s3"
	srv := httptest.NewServer(New(cfg, nil, nil, func(context.Context) error { return nil }).Handler())
	defer srv.Close()
	for path, want := range map[string]int{"/healthz": 200, "/v1/tts": 401, "/v1/stt": 401} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("%s: got %d want %d", path, resp.StatusCode, want)
		}
	}
}
