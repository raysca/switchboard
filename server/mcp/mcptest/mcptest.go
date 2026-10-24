// Package mcptest serves MCP servers with known behaviour over streamable HTTP, for
// testing the MCP integration: one that works (/ok, bearer token; /open, no auth),
// ones that fail in each distinct way (/fail/tools, /fail/auth, /fail/list), one that
// is slow (/slow), plus a black-hole listener (BlackHole). It records what each
// request carried (never a token value) for tests to assert on.
package mcptest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	DefaultToken  = "test-token"
	maxRecords    = 100
	maxSlowDelay  = 120 * time.Second
	defaultTooBig = 512 << 10 // over the client's result limit
)

// Options configures a Server. Zero values take the defaults.
type Options struct {
	Token       string // bearer token /ok expects (DefaultToken)
	TooBigBytes int    // size of the too_big result
}

// Record is one request a test server received.
type Record struct {
	Path      string          `json:"path"`
	Method    string          `json:"method"` // JSON-RPC method
	Tool      string          `json:"tool,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Auth      string          `json:"auth"`      // "none" | "match" | "mismatch"
	Cancelled bool            `json:"cancelled"` // the client closed the call before the tool finished
	At        time.Time       `json:"at"`
}

// Server is the set of test MCP servers behind one http.Handler.
type Server struct {
	opts    Options
	mu      sync.Mutex
	records []Record
	mux     *http.ServeMux
}

func New(opts Options) *Server {
	if opts.Token == "" {
		opts.Token = DefaultToken
	}
	if opts.TooBigBytes <= 0 {
		opts.TooBigBytes = defaultTooBig
	}
	s := &Server{opts: opts, mux: http.NewServeMux()}
	s.mount("/ok", s.healthy("ok"), true)
	s.mount("/open", s.healthy("open"), false)
	s.mount("/fail/tools", s.failing(), false)
	s.mount("/fail/list", s.failingList(), false)
	s.mount("/slow", s.slow(), false)
	s.mux.HandleFunc("/fail/auth", func(w http.ResponseWriter, r *http.Request) {
		s.add(Record{Path: "/fail/auth", Method: "", Auth: s.auth(r), At: time.Now()})
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
	// A plain REST endpoint for the HTTP tools: GET /rest/orders/{id} needs the bearer token and answers JSON.
	s.mux.HandleFunc("GET /rest/orders/{id}", func(w http.ResponseWriter, r *http.Request) {
		auth := s.auth(r)
		s.add(Record{Path: "/rest/orders/" + r.PathValue("id"), Method: "GET", Auth: auth, At: time.Now()})
		switch {
		case auth != "match":
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		case r.PathValue("id") == "missing":
			http.Error(w, "no such order", http.StatusNotFound)
		default:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": r.PathValue("id"), "status": "shipped", "eta_days": 2, "verbose": r.URL.Query().Get("verbose") == "true"})
		}
	})
	s.mux.HandleFunc("GET /_record", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		recs := s.Records()
		if recs == nil {
			recs = []Record{}
		}
		_ = json.NewEncoder(w).Encode(recs)
	})
	s.mux.HandleFunc("DELETE /_record", func(w http.ResponseWriter, _ *http.Request) {
		s.Reset()
		w.WriteHeader(http.StatusNoContent)
	})
	return s
}

func (s *Server) Handler() http.Handler { return s.mux }

// Records returns a copy of what the servers received, oldest first.
func (s *Server) Records() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Record(nil), s.records...)
}

func (s *Server) Reset() {
	s.mu.Lock()
	s.records = nil
	s.mu.Unlock()
}

func (s *Server) add(r Record) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.records) == maxRecords {
		s.records = append(s.records[:0], s.records[1:]...)
	}
	s.records = append(s.records, r)
	return len(s.records) - 1
}

// markCancelled flags the latest call of tool as cancelled.
func (s *Server) markCancelled(tool string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.records) - 1; i >= 0; i-- {
		if s.records[i].Tool == tool {
			s.records[i].Cancelled = true
			return
		}
	}
}

// auth says whether the request carried the expected bearer token, without keeping it.
func (s *Server) auth(r *http.Request) string {
	h := r.Header.Get("Authorization")
	switch {
	case h == "":
		return "none"
	case h == "Bearer "+s.opts.Token:
		return "match"
	default:
		return "mismatch"
	}
}

// mount serves an MCP server at path: it records each JSON-RPC request, enforces the
// token when requireToken, implements the drop tool by closing the connection, and
// marks a call cancelled when the client went away before it finished.
func (s *Server) mount(path string, srv *mcp.Server, requireToken bool) {
	// Stateful sessions: the client's notifications/cancelled reaches the session that
	// runs the call, so a slow tool sees its context end (stateless mode would route
	// it to a fresh session and the call would run on).
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	s.mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		r.Body = io.NopCloser(bytes.NewReader(body))
		var msg struct {
			Method string `json:"method"`
			Params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &msg)
		rec := Record{Path: path, Method: msg.Method, Auth: s.auth(r), At: time.Now()}
		if msg.Method == "tools/call" {
			rec.Tool, rec.Arguments = msg.Params.Name, msg.Params.Arguments
		}
		if r.Method == http.MethodPost && msg.Method != "" {
			s.add(rec)
		}
		if requireToken && rec.Auth != "match" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if rec.Tool == "drop" {
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					_ = conn.Close()
					return
				}
			}
		}
		h.ServeHTTP(w, r)
		// The client gave up on the call (timeout, barge-in) while the tool was running.
		if rec.Tool != "" && r.Context().Err() != nil {
			s.markCancelled(rec.Tool)
		}
	})
}

type orderIn struct {
	OrderID string `json:"order_id"`
}
type order struct {
	OrderID string `json:"order_id"`
	Status  string `json:"status"`
	Items   int    `json:"items"`
}
type textIn struct {
	Text string `json:"text"`
}
type textOut struct {
	Text string `json:"text"`
}
type empty struct{}
type slowIn struct {
	DelayMS int `json:"delay_ms"`
}
type slowOut struct {
	DelayMS int    `json:"delay_ms"`
	Status  string `json:"status"`
}

func (s *Server) healthy(name string) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "mcptest-" + name, Version: "1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "lookup_order", Description: "Look up an order by id."}, func(_ context.Context, _ *mcp.CallToolRequest, in orderIn) (*mcp.CallToolResult, order, error) {
		return nil, order{OrderID: in.OrderID, Status: "shipped", Items: 2}, nil
	})
	mcp.AddTool(srv, &mcp.Tool{Name: "echo", Description: "Echo the text back."}, func(_ context.Context, _ *mcp.CallToolRequest, in textIn) (*mcp.CallToolResult, textOut, error) {
		return nil, textOut(in), nil
	})
	return srv
}

func (s *Server) failing() *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "mcptest-fail", Version: "1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "tool_error", Description: "Always returns a tool error."}, func(context.Context, *mcp.CallToolRequest, empty) (*mcp.CallToolResult, empty, error) {
		return nil, empty{}, errors.New("order system unavailable")
	})
	mcp.AddTool(srv, &mcp.Tool{Name: "protocol_error", Description: "Always fails at the protocol level."}, func(context.Context, *mcp.CallToolRequest, empty) (*mcp.CallToolResult, empty, error) {
		return nil, empty{}, &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "mcptest protocol error"}
	})
	mcp.AddTool(srv, &mcp.Tool{Name: "drop", Description: "Closes the connection mid-call."}, func(context.Context, *mcp.CallToolRequest, empty) (*mcp.CallToolResult, empty, error) {
		return nil, empty{}, errors.New("unreachable: the connection is dropped before this runs")
	})
	mcp.AddTool(srv, &mcp.Tool{Name: "too_big", Description: "Returns a result over the size limit."}, func(context.Context, *mcp.CallToolRequest, empty) (*mcp.CallToolResult, textOut, error) {
		return nil, textOut{Text: strings.Repeat("x", s.opts.TooBigBytes)}, nil
	})
	return srv
}

func (s *Server) failingList() *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "mcptest-fail-list", Version: "1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "never_listed", Description: "Hidden by the failing tools/list."}, func(context.Context, *mcp.CallToolRequest, empty) (*mcp.CallToolResult, empty, error) {
		return nil, empty{}, nil
	})
	srv.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "tools/list" {
				return nil, &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "mcptest: tools/list fails"}
			}
			return next(ctx, method, req)
		}
	})
	return srv
}

func (s *Server) slow() *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "mcptest-slow", Version: "1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "slow_lookup", Description: "Answers after delay_ms milliseconds."}, func(ctx context.Context, _ *mcp.CallToolRequest, in slowIn) (*mcp.CallToolResult, slowOut, error) {
		d := time.Duration(in.DelayMS) * time.Millisecond
		if d > maxSlowDelay {
			d = maxSlowDelay
		}
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-t.C:
			return nil, slowOut{DelayMS: in.DelayMS, Status: "done"}, nil
		case <-ctx.Done():
			s.markCancelled("slow_lookup")
			return nil, slowOut{}, ctx.Err()
		}
	})
	return srv
}

// BlackHole accepts connections on l and never writes; Accepted counts them. stop
// closes the listener and every held connection.
type BlackHole struct {
	mu       sync.Mutex
	conns    []net.Conn
	accepted int
	done     chan struct{}
}

func ServeBlackHole(l net.Listener) (*BlackHole, func()) {
	b := &BlackHole{done: make(chan struct{})}
	go func() {
		defer close(b.done)
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			b.mu.Lock()
			b.conns = append(b.conns, c)
			b.accepted++
			b.mu.Unlock()
		}
	}()
	var once sync.Once
	return b, func() {
		once.Do(func() {
			_ = l.Close()
			<-b.done
			b.mu.Lock()
			for _, c := range b.conns {
				_ = c.Close()
			}
			b.conns = nil
			b.mu.Unlock()
		})
	}
}
