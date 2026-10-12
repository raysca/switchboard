// Command mcptest serves the MCP test servers (server/mcp/mcptest) on loopback for
// development and the browser suites: /ok, /open, /fail/tools, /fail/auth,
// /fail/list, /slow, plus a black-hole listener. Point a server started with
// mcp.allow_local_http at http://<addr>/<path>.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"govoice/server/mcp/mcptest"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18765", "listen address for the MCP test servers")
	holeAddr := flag.String("blackhole-addr", "127.0.0.1:18766", "listen address that accepts and never answers (empty: off)")
	token := flag.String("token", os.Getenv("MCPTEST_TOKEN"), "bearer token /ok expects (default "+mcptest.DefaultToken+")")
	flag.Parse()

	srv := &http.Server{Addr: *addr, Handler: mcptest.New(mcptest.Options{Token: *token}).Handler(), ReadHeaderTimeout: 5 * time.Second}
	l, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("mcptest: %v", err)
	}
	stopHole := func() {}
	if *holeAddr != "" {
		hl, err := net.Listen("tcp", *holeAddr)
		if err != nil {
			log.Fatalf("mcptest: black hole: %v", err)
		}
		_, stopHole = mcptest.ServeBlackHole(hl)
		fmt.Printf("mcptest: black hole on %s\n", hl.Addr())
	}
	fmt.Printf("mcptest: http://%s (paths /ok /open /fail/tools /fail/auth /fail/list /slow, GET /_record)\n", l.Addr())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
		stopHole()
	}()
	if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("mcptest: %v", err)
	}
}
