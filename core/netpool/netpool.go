// Package netpool gives providers process-wide, tuned HTTP transports and an
// optional one-connection TCP/TLS pre-dial ("warm-up").
//
// Why: a per-session http.Client throws its connections away with the
// session, so every call's first request pays DNS + TCP + TLS (+ the HTTP/2
// handshake). A Pool is shared per (provider, stage, variant) across sessions,
// so the second call reuses pooled HTTP/2 connections (0 dials). WebSocket
// upgrades take their connection from the same dialer, so a pre-dialed spare
// removes the TCP/TLS cost from the first unit/turn.
//
// A spare is a bare, anonymous TLS connection (no request, no credentials, no
// provider session), so pre-dialing cannot bill or open a breaker. It is closed
// after SpareTTL if nobody took it.
package netpool

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Warmer is implemented by providers whose first request benefits from a
// pre-opened connection. Warm must be bounded by ctx, never bill, never
// touch a circuit breaker's failure count and be safe to call repeatedly.
type Warmer interface {
	Warm(ctx context.Context) error
}

// Settings are process-wide; call Configure once at startup, before providers
// are built.
type Settings struct {
	Shared   bool          // share pools across sessions (false: one private pool per Get)
	Predial  bool          // Warm pre-dials a spare connection (false: Warm is a counted no-op)
	SpareTTL time.Duration // how long an untaken spare lives (default 10s)
}

var (
	setMu    sync.RWMutex
	settings = Settings{Shared: true, SpareTTL: 10 * time.Second}
)

// Configure replaces the settings. Existing pools pick up Predial/SpareTTL
// immediately; Shared only affects later Get calls.
func Configure(s Settings) {
	if s.SpareTTL <= 0 {
		s.SpareTTL = 10 * time.Second
	}
	setMu.Lock()
	settings = s
	setMu.Unlock()
}

func current() Settings { setMu.RLock(); defer setMu.RUnlock(); return settings }

// Options identify a pool. Provider and Stage are metric labels and must stay
// a small fixed set.
type Options struct {
	Provider, Stage string
	Variant         string // distinguishes pools with different dial rules (e.g. SSRF guard mode)
	HTTP1Only       bool   // WebSocket pools: never negotiate h2
	// Control runs on every dialled address (SSRF guard); applies to spares too.
	Control func(network, address string, c syscall.RawConn) error
	TLS     *tls.Config // base config (tests inject RootCAs)
}

const maxSpares = 64

type spare struct {
	conn  net.Conn
	timer *time.Timer
}

// Pool is a shared transport plus its spare-connection slot map.
type Pool struct {
	o      Options
	tr     *http.Transport
	dialer *net.Dialer
	st     *stats

	sessions tls.ClientSessionCache // per pool: tickets never cross guard modes or trust roots

	mu     sync.Mutex
	spares map[string]*spare // key: scheme + "|" + host:port
	closed bool
}

var (
	regMu sync.Mutex
	reg   = map[string]*Pool{}
)

// Get returns the shared pool for o, creating it on first use.
func Get(o Options) *Pool {
	// Control is part of the key so a guarded pool can never be shared with an
	// unguarded one that forgot its Variant. Nothing user-supplied (base URL,
	// credential) is in the key: the pool count is bounded by the code's call sites.
	key := fmt.Sprintf("%s|%s|%s|%t|%t", o.Provider, o.Stage, o.Variant, o.HTTP1Only, o.Control != nil)
	if !current().Shared {
		return newPool(o)
	}
	regMu.Lock()
	defer regMu.Unlock()
	if p, ok := reg[key]; ok {
		return p
	}
	p := newPool(o)
	reg[key] = p
	return p
}

func newPool(o Options) *Pool {
	p := &Pool{o: o, spares: map[string]*spare{}, st: statsFor(o.Provider, o.Stage), sessions: tls.NewLRUClientSessionCache(64)}
	p.dialer = &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second, Control: o.Control}
	t := &http.Transport{
		DialContext:           func(ctx context.Context, _, addr string) (net.Conn, error) { return p.dial(ctx, addr, false) },
		DialTLSContext:        func(ctx context.Context, _, addr string) (net.Conn, error) { return p.dial(ctx, addr, true) },
		ForceAttemptHTTP2:     !o.HTTP1Only,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 0, // per-request deadlines belong to the caller
		ExpectContinueTimeout: time.Second,
		// ponytail: HTTP/2 health pings so a silently dead pooled conn is dropped
		// in ~40s instead of failing the next turn.
		HTTP2: &http.HTTP2Config{SendPingTimeout: 30 * time.Second, PingTimeout: 10 * time.Second},
	}
	if o.Control == nil { // guarded pools never use a proxy: the guard would only see the proxy
		t.Proxy = http.ProxyFromEnvironment
	}
	p.tr = t
	return p
}

// Transport is the shared round tripper.
func (p *Pool) Transport() *http.Transport { return p.tr }

// Client returns a client over the shared transport. timeout 0 means none
// (required for WebSocket dials; use contexts instead).
func (p *Pool) Client(timeout time.Duration) *http.Client {
	return &http.Client{Transport: countingRT{p.tr, p.st}, Timeout: timeout}
}

// countingRT counts every request (HTTP call or WebSocket upgrade) a provider client sends
// through the pool: the measured "provider requests" figure live harnesses cap on.
type countingRT struct {
	http.RoundTripper
	st *stats
}

func (c countingRT) RoundTrip(r *http.Request) (*http.Response, error) {
	c.st.requests.Add(1)
	return c.RoundTripper.RoundTrip(r)
}

// CountRequest counts one request a provider made outside a Pool (a gRPC RPC or stream open).
func CountRequest(provider, stage string) { statsFor(provider, stage).requests.Add(1) }

// Requests returns the cumulative request count per "provider|stage" (tests, harnesses).
func Requests() map[string]uint64 {
	statMu.Lock()
	defer statMu.Unlock()
	out := make(map[string]uint64, len(statBy))
	for k, s := range statBy {
		out[k] = s.requests.Load()
	}
	return out
}

func (p *Pool) tlsConfig(host string) *tls.Config {
	var c *tls.Config
	if p.o.TLS != nil {
		c = p.o.TLS.Clone()
	} else {
		c = &tls.Config{}
	}
	c.ServerName = host
	if p.o.HTTP1Only {
		c.NextProtos = []string{"http/1.1"}
	} else {
		c.NextProtos = []string{"h2", "http/1.1"}
	}
	if c.ClientSessionCache == nil {
		c.ClientSessionCache = p.sessions
	}
	return c
}

func (p *Pool) dial(ctx context.Context, addr string, useTLS bool) (net.Conn, error) {
	if c := p.take(spareKey(addr, useTLS)); c != nil {
		p.st.hit.Add(1)
		return c, nil
	}
	p.st.miss.Add(1)
	return p.connect(ctx, addr, useTLS, false)
}

// connect dials TCP(+TLS). A spare's socket is wrapped in a replayConn so take
// can read ahead of TLS to find a peer close (see alive).
func (p *Pool) connect(ctx context.Context, addr string, useTLS, spare bool) (net.Conn, error) {
	start := time.Now()
	raw, err := p.dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	if spare {
		raw = &replayConn{Conn: raw}
	}
	var c net.Conn = raw
	if useTLS {
		host, _, _ := net.SplitHostPort(addr)
		tc := tls.Client(raw, p.tlsConfig(host))
		if err := tc.HandshakeContext(ctx); err != nil {
			raw.Close()
			return nil, err
		}
		c = tc
	}
	p.st.observe(time.Since(start))
	return c, nil
}

// replayConn hands bytes alive read ahead (TLS 1.3 session tickets, the h2
// server's SETTINGS) back to the first Reads, so the read-ahead is invisible to
// TLS and HTTP/2.
type replayConn struct {
	net.Conn
	pending []byte
}

func (c *replayConn) Read(b []byte) (int, error) {
	if len(c.pending) > 0 {
		n := copy(b, c.pending)
		c.pending = c.pending[n:]
		return n, nil
	}
	return c.Conn.Read(b)
}

func spareKey(addr string, useTLS bool) string {
	if useTLS {
		return "tls|" + addr
	}
	return "tcp|" + addr
}

// take removes and returns a live spare, or nil.
func (p *Pool) take(key string) net.Conn {
	p.mu.Lock()
	s := p.spares[key]
	if s == nil || s.conn == nil {
		p.mu.Unlock()
		return nil
	}
	delete(p.spares, key)
	s.timer.Stop()
	p.mu.Unlock()
	if !alive(s.conn) {
		s.conn.Close()
		return nil
	}
	return s.conn
}

// Warm pre-dials one TCP(+TLS) connection to rawURL's host (http, https, ws,
// wss) and parks it for the next dial. It is a no-op when Predial is off, a
// spare already exists, a proxy would be used, or the spare table is full.
func (p *Pool) Warm(ctx context.Context, rawURL string) error {
	if !current().Predial {
		p.st.warmSkipped.Add(1)
		return nil
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		p.st.warmErr.Add(1)
		return fmt.Errorf("netpool: bad warm url")
	}
	useTLS := u.Scheme == "https" || u.Scheme == "wss"
	port := u.Port()
	if port == "" {
		port = map[bool]string{true: "443", false: "80"}[useTLS]
	}
	addr := net.JoinHostPort(u.Hostname(), port)
	if p.tr.Proxy != nil {
		hu := *u
		hu.Scheme = map[bool]string{true: "https", false: "http"}[useTLS]
		if pu, _ := p.tr.Proxy(&http.Request{URL: &hu}); pu != nil {
			p.st.warmSkipped.Add(1)
			return nil
		}
	}
	key := spareKey(addr, useTLS)
	p.mu.Lock()
	if p.closed || p.spares[key] != nil || len(p.spares) >= maxSpares {
		p.mu.Unlock()
		p.st.warmSkipped.Add(1)
		return nil
	}
	ph := &spare{} // placeholder: concurrent Warm calls dial once
	p.spares[key] = ph
	p.mu.Unlock()

	c, err := p.connect(ctx, addr, useTLS, true)
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil || p.closed || p.spares[key] != ph {
		if p.spares[key] == ph {
			delete(p.spares, key)
		}
		if err == nil {
			c.Close()
			return nil
		}
		p.st.warmErr.Add(1)
		return err
	}
	ph.conn = c
	ph.timer = time.AfterFunc(current().SpareTTL, func() {
		p.mu.Lock()
		if p.spares[key] == ph {
			delete(p.spares, key)
			ph.conn.Close()
		}
		p.mu.Unlock()
	})
	p.st.warmOK.Add(1)
	return nil
}

// Close drops idle connections and any spare and unregisters the pool.
// Shared pools are process-lifetime; this is for tests and shutdown.
func (p *Pool) Close() {
	p.mu.Lock()
	p.closed = true
	for k, s := range p.spares {
		if s.conn != nil {
			s.timer.Stop()
			s.conn.Close()
		}
		delete(p.spares, k)
	}
	p.mu.Unlock()
	p.tr.CloseIdleConnections()
	regMu.Lock()
	for k, v := range reg {
		if v == p {
			delete(reg, k)
		}
	}
	regMu.Unlock()
}

// ---- metrics -------------------------------------------------------------

var connectBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5}

type stats struct {
	hit, miss, warmOK, warmErr, warmSkipped, requests atomic.Uint64
	buckets                                           [10]atomic.Uint64 // last is +Inf
	sumNanos, count                                   atomic.Uint64
}

func (s *stats) observe(d time.Duration) {
	i := 0
	for i < len(connectBuckets) && d.Seconds() > connectBuckets[i] {
		i++
	}
	s.buckets[i].Add(1)
	s.sumNanos.Add(uint64(d))
	s.count.Add(1)
}

var (
	statMu sync.Mutex
	statBy = map[string]*stats{}
)

func statsFor(provider, stage string) *stats {
	statMu.Lock()
	defer statMu.Unlock()
	k := provider + "|" + stage
	if s := statBy[k]; s != nil {
		return s
	}
	s := &stats{}
	statBy[k] = s
	return s
}

// WriteMetrics emits Prometheus text. Labels are provider and stage only.
func WriteMetrics(w io.Writer) {
	statMu.Lock()
	keys := make([]string, 0, len(statBy))
	for k := range statBy {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	snap := make([]*stats, len(keys))
	for i, k := range keys {
		snap[i] = statBy[k]
	}
	statMu.Unlock()
	if len(keys) == 0 {
		return
	}
	lbl := func(k string) string {
		for i := 0; i < len(k); i++ {
			if k[i] == '|' {
				return fmt.Sprintf(`provider=%q,stage=%q`, k[:i], k[i+1:])
			}
		}
		return ""
	}
	fmt.Fprint(w, "# HELP voice_provider_requests_total Requests sent to a provider (HTTP calls, WebSocket dials, gRPC RPCs/stream opens), retries included.\n# TYPE voice_provider_requests_total counter\n")
	for i, k := range keys {
		fmt.Fprintf(w, "voice_provider_requests_total{%s} %d\n", lbl(k), snap[i].requests.Load())
	}
	fmt.Fprint(w, "# HELP voice_provider_warm_dials_total Dials served by a pre-dialed spare (hit) or by a fresh connect (miss).\n# TYPE voice_provider_warm_dials_total counter\n")
	for i, k := range keys {
		fmt.Fprintf(w, "voice_provider_warm_dials_total{%s,result=\"hit\"} %d\nvoice_provider_warm_dials_total{%s,result=\"miss\"} %d\n", lbl(k), snap[i].hit.Load(), lbl(k), snap[i].miss.Load())
	}
	fmt.Fprint(w, "# HELP voice_provider_warmups_total Warm-up attempts by result.\n# TYPE voice_provider_warmups_total counter\n")
	for i, k := range keys {
		s := snap[i]
		fmt.Fprintf(w, "voice_provider_warmups_total{%s,result=\"ok\"} %d\nvoice_provider_warmups_total{%s,result=\"error\"} %d\nvoice_provider_warmups_total{%s,result=\"skipped\"} %d\n",
			lbl(k), s.warmOK.Load(), lbl(k), s.warmErr.Load(), lbl(k), s.warmSkipped.Load())
	}
	fmt.Fprint(w, "# HELP voice_provider_connect_seconds TCP+TLS connect time of fresh provider connections.\n# TYPE voice_provider_connect_seconds histogram\n")
	for i, k := range keys {
		s := snap[i]
		var cum uint64
		for j, le := range connectBuckets {
			cum += s.buckets[j].Load()
			fmt.Fprintf(w, "voice_provider_connect_seconds_bucket{%s,le=\"%g\"} %d\n", lbl(k), le, cum)
		}
		cum += s.buckets[len(connectBuckets)].Load()
		fmt.Fprintf(w, "voice_provider_connect_seconds_bucket{%s,le=\"+Inf\"} %d\nvoice_provider_connect_seconds_sum{%s} %g\nvoice_provider_connect_seconds_count{%s} %d\n",
			lbl(k), cum, lbl(k), float64(s.sumNanos.Load())/1e9, lbl(k), s.count.Load())
	}
}

// NoteSkipped counts a warm-up a provider skipped itself (breaker open).
func (p *Pool) NoteSkipped() { p.st.warmSkipped.Add(1) }

// PredialEnabled reports whether warm-ups should open connections.
func PredialEnabled() bool { return current().Predial }

// Observe records a warm-up the provider performed itself (e.g. a gRPC
// channel brought to READY): ok adds its connect time to the histogram.
func Observe(provider, stage string, d time.Duration, err error) {
	st := statsFor(provider, stage)
	if err != nil {
		st.warmErr.Add(1)
		return
	}
	st.warmOK.Add(1)
	st.observe(d)
}

// NoteSkipped counts a skipped warm-up for providers that do not use a Pool.
func NoteSkipped(provider, stage string) { statsFor(provider, stage).warmSkipped.Add(1) }
