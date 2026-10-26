// Command atlas-mcp serves the Atlas Travel Concierge demo fixture server
// (server/mcp/atlasmcp): the MCP tools on /mcp, the two JSON fixtures under /api, and a health
// check. PORT (default 18768) is the listen port; ATLAS_MCP_TOKEN is the bearer token (required).
// It listens on loopback by default because the two /api fixtures are deliberately open;
// ATLAS_MCP_ADDR overrides the listen address when the demo stack needs another host.
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

	"govoice/server/mcp/atlasmcp"
)

func main() {
	srv, err := atlasmcp.New(atlasmcp.Options{Token: os.Getenv("ATLAS_MCP_TOKEN"), Delay: 2 * time.Second})
	if err != nil {
		log.Fatalf("atlas-mcp: set ATLAS_MCP_TOKEN: %v", err)
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "18768"
	}
	addr := os.Getenv("ATLAS_MCP_ADDR")
	if addr == "" {
		addr = "127.0.0.1:" + port
	}
	hs := &http.Server{Addr: addr, Handler: srv.Handler(), ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hs.Shutdown(shutdown)
	}()
	log.Printf("atlas-mcp: listening on http://%s (/mcp, /health, /api/weather, /api/advisory)", addr)
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("atlas-mcp: %v", err)
	}
}
