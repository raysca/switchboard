// Command dental-mcp serves the Smile Dental demo scheduling MCP server (demos/dental/mcp/dentalmcp).
// PORT (default 8081) is the listen port; DENTAL_MCP_TOKEN is the bearer token (required).
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"govoice/demos/dental/mcp/dentalmcp"
)

func main() {
	srv, err := dentalmcp.New(dentalmcp.Options{Token: os.Getenv("DENTAL_MCP_TOKEN")})
	if err != nil {
		log.Fatalf("dental-mcp: set DENTAL_MCP_TOKEN: %v", err)
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8081"
	}
	hs := &http.Server{Addr: ":" + port, Handler: srv.Handler(), ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hs.Shutdown(shutdown)
	}()
	log.Printf("dental-mcp: listening on :%s (/mcp, /health)", port)
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("dental-mcp: %v", err)
	}
}
