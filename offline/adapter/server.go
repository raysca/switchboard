package adapter

import (
	"context"
	"net/http"
	"time"
)

// Server serves the xAI-compatible STT and TTS endpoints on top of two model backends.
type Server struct {
	cfg   Config
	tts   TTSBackend
	stt   STTBackend
	ready func(context.Context) error
	lim   *Limiter
}

func New(cfg Config, tts TTSBackend, stt STTBackend, ready func(context.Context) error) *Server {
	return &Server{cfg: cfg, tts: tts, stt: stt, ready: ready, lim: NewLimiter(cfg.MaxConcurrency, cfg.QueueWait)}
}

func (s *Server) Handler() http.Handler {
	authed := http.NewServeMux()
	authed.HandleFunc("POST /v1/tts", s.handleTTSPost)
	authed.HandleFunc("GET /v1/tts", s.handleTTSWS)
	authed.HandleFunc("GET /v1/stt", s.handleSTT)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.Handle("/v1/", requireToken(s.cfg.Token, authed))
	return mux
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := s.ready(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}

// Stubs, removed by the task that implements each endpoint.
