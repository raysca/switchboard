// Command offline-adapter serves the xAI STT/TTS wire protocols on top of a local Speaches server.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"govoice/offline/adapter"
)

func main() {
	healthcheck := flag.Bool("healthcheck", false, "probe /healthz on the configured port and exit (for container healthchecks)")
	flag.Parse()
	cfg, err := adapter.LoadConfig(os.Getenv)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if *healthcheck {
		os.Exit(probe(cfg.Listen))
	}
	sp := adapter.NewSpeaches(cfg)
	srv := &http.Server{Addr: cfg.Listen, Handler: adapter.New(cfg, sp, sp, sp.Health).Handler(), ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Printf("offline-adapter listening on %s, backend %s", cfg.Listen, cfg.SpeachesURL)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// probe is the container healthcheck: the image always listens on ":PORT".
func probe(listen string) int {
	resp, err := http.Get("http://127.0.0.1" + listen + "/healthz")
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
