package server

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"govoice/server/eval"
	"govoice/transport/token"
	"govoice/transport/ws"
)

// The internal eval listener: the worker's calls dial this instance over loopback instead of the public
// listener, whose browser rules (Origin allowlist, TLS or trusted-proxy evidence) a server-side client cannot
// and should not satisfy. It binds 127.0.0.1 only, so no other host can reach it, serves only /ws, and
// accepts only voice tokens of the eval principal. It is the same handler as the public /ws (same token
// validation, same call setup, so the same admission: global, per principal, per agent), with the Origin and
// TLS checks off. It exists only while the worker does (evaluation.max_concurrent_calls > 0).
func (s *Server) startEvalListener() error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen eval: %w", err)
	}
	h := s.newWSServer(s.wsServerConfig(ws.SecurityConfig{AllowMissingOrigin: true}))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws", func(w http.ResponseWriter, r *http.Request) {
		if s.shuttingDown.Load() {
			http.Error(w, DrainReason, http.StatusServiceUnavailable)
			return
		}
		// A pre-filter only: the handler still verifies the signature, so a forged claim gets no further.
		// An unfit token is refused before it is consumed.
		var c token.Claims
		if _, _, err := jwt.NewParser().ParseUnverified(r.URL.Query().Get("token"), &c); err != nil || (c.Principal != eval.Principal) {
			http.Error(w, "the internal listener serves eval calls only", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	s.evalServer, s.evalAddr, s.evalDone = srv, ln.Addr().String(), make(chan struct{})
	go func() {
		defer close(s.evalDone)
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			s.logger.Error("eval listener stopped", slog.String("err", err.Error()))
		}
	}()
	return nil
}

// stopEvalListener closes the internal listener and waits for its serve loop; upgraded sockets are hijacked,
// so only plain HTTP is dropped here. Safe when it never started.
func (s *Server) stopEvalListener() {
	if s.evalServer != nil {
		_ = s.evalServer.Close()
		<-s.evalDone
	}
}
