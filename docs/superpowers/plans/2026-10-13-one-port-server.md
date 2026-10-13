# One-Port Production Server Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let one Cloud Run-shaped process serve the voice WebSocket, the management API and the admin UI on a single public port, safely: behind the management key, with failed-auth throttling, same-origin WebSockets and an embedded UI.

**Architecture:** An explicit `public_admin.enabled` setting mounts the existing management handler (`/api/v1`) and an embedded UI on the public mux. A per-client-IP throttle on failed management authentication protects the key. `origin_allow_same_host` lets the page that the server itself serves open the WebSocket without an allow-list entry. The UI is built into the binary with `go:embed`; the Dockerfile gains a Node stage. A `config/cloudrun.yaml` profile runs it all on SQLite with the right proxy trust.

**Tech Stack:** Go (`server`, `transport/ws`), `golang.org/x/time/rate`, `embed`, Node 22 + Vite (build only), Docker, Playwright.

**Spec:** `docs/superpowers/specs/2026-10-13-cloudrun-dentistry-demo-design.md` (items 5-9 and 11). Plan 1 (demo features) is independent of this plan; plan 3 (deploy) needs both.

## Global Constraints

- The management API stays on its own listener by default. `public_admin.enabled` defaults to false; production refuses it unless `tls.trusted_proxy_mode` is on (the throttle and the rate limits key on the proxy-trusted client IP) and the management key is set (>= 32 bytes, already enforced by `auth.NewManagement`).
- Never on the public port: `/metrics`, `/ready`, `/debug/timeline` (they stay on the admin and probe listeners).
- API responses carry `Cache-Control: no-store`. The UI sets a Content-Security-Policy.
- Bounded: the throttle table holds at most 10,000 client entries; a failed-auth allowance is 10 attempts, refilled at one every 6 s (10 per minute sustained).
- No secret in a URL, a log line or a metric label. The throttle key is the client IP only, never the presented key.
- `go test -race` clean; `gofmt -w .`; conventional commits ending with the session's attribution trailer; no model identifier in subjects.
- A plain `go build ./...` must work on a clean checkout without Node: the embedded UI directory ships a committed placeholder.
- Test commands: `cd server && go test -race ./...`, `cd transport && go test -race ./ws/...` (check the module root with `ls transport`; if `transport/ws` belongs to the root module run `go test -race ./transport/ws/...` from the repo root), `make e2e-fake`.

## Review Focus

- A client that guesses the key from one address must be limited without locking out other addresses, and the correct key from a blocked address is also refused until the allowance refills (no oracle). Pinned in Task 2.
- A spoofed `X-Forwarded-For` from a client must not become its throttle key; only a trusted proxy's header counts. Pinned in Task 2 (through `ClientIP`) and Task 3 (`client_ip` in `server-config`).
- `/api/...`, `/ws`, `/health`, `/metrics`, `/ready` and `/debug/...` must never be answered by the UI's single-page fallback, and the fallback must not serve `index.html` for a missing asset (`/assets/x.js` must 404 so a stale deploy is visible). Pinned in Task 4 (with a built UI standing in, because the committed placeholder hides this).
- A page on another origin (`https://evil.example`) must still be refused for the WebSocket when `origin_allow_same_host` is on; same host but different scheme or port is also not "same". Pinned in Task 1.
- The server started with `public_admin.enabled` but no management key, or in production without trusted-proxy mode, must refuse to start, not start open or unthrottled. Pinned in Task 3.
- A binary built without the UI (the placeholder only) must say so on `/` rather than serve an empty page. Pinned in Task 4.

---

### Task 1: same-origin WebSocket check

**Files:**
- Modify: `transport/ws/tls.go` (`SecurityConfig`, `CheckOrigin`), `server/config.go`, `server/server.go` (`setupRoutes`), `server/config_validate.go`
- Test: `transport/ws/ws_test.go` (or a new `transport/ws/origin_test.go`), `server/config_test.go`

**Interfaces:**
- Produces: `ws.SecurityConfig.AllowSameHost bool`; config key `origin_allow_same_host` (bool, default false); used by Task 5's `config/cloudrun.yaml`.

- [ ] **Step 1: Write the failing tests** (`transport/ws/origin_test.go`)

```go
package ws

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCheckOriginSameHost(t *testing.T) {
	cfg := &SecurityConfig{AllowedOrigins: []string{"https://app.example"}, AllowSameHost: true}
	for _, tc := range []struct {
		name, host, origin string
		want               bool
	}{
		{"same host https", "voice.run.app", "https://voice.run.app", true},
		{"same host http (local rehearsal)", "localhost:8080", "http://localhost:8080", true},
		{"host case differs", "Voice.Run.App", "https://voice.run.app", true},
		{"listed origin still works", "voice.run.app", "https://app.example", true},
		{"other site", "voice.run.app", "https://evil.example", false},
		{"other port", "voice.run.app", "https://voice.run.app:8443", false},
		{"suffix trick", "voice.run.app", "https://voice.run.app.evil.example", false},
		{"not a web origin", "voice.run.app", "chrome-extension://voice.run.app", false},
		{"null origin", "voice.run.app", "null", false},
		{"unparseable", "voice.run.app", "https://%zz", false},
	} {
		r := httptest.NewRequest(http.MethodGet, "/ws", nil)
		r.Host = tc.host
		r.Header.Set("Origin", tc.origin)
		if got := cfg.CheckOrigin(r) == nil; got != tc.want {
			t.Errorf("%s: allowed=%v, want %v", tc.name, got, tc.want)
		}
	}
	// Off by default: the same host alone is not enough.
	off := &SecurityConfig{AllowedOrigins: []string{"https://app.example"}}
	r := httptest.NewRequest(http.MethodGet, "/ws", nil)
	r.Host = "voice.run.app"
	r.Header.Set("Origin", "https://voice.run.app")
	if off.CheckOrigin(r) == nil {
		t.Fatal("same-host must be opt-in")
	}
}
```

- [ ] **Step 2: Run to see failure**

Run: `go test ./transport/ws -run TestCheckOriginSameHost 2>&1 | tail -5` (from the module root that owns `transport/ws`)
Expected: build error (`AllowSameHost` undefined).

- [ ] **Step 3: Implement** in `transport/ws/tls.go`: add the field with a comment, import `net/url`, and in `CheckOrigin` after the `origin == ""` block and before the allow-list loop:

```go
	if cfg.AllowSameHost {
		// The page and the socket share an origin (the server serves the page): browsers send the
		// page's origin and the socket's own host, so no per-deployment allow-list entry is needed.
		// Scheme and port are part of Host/Origin: a different port is a different origin.
		if u, err := url.Parse(origin); err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && strings.EqualFold(u.Host, r.Host) {
			return nil
		}
	}
```

```go
	AllowSameHost bool // Also accept an Origin whose host (with port) equals the request's Host
```

- [ ] **Step 4: Config wiring.** `server/config.go`: add `OriginAllowSameHost bool `yaml:"origin_allow_same_host"`` next to `OriginAllowlist`. `server/server.go` `setupRoutes`: `AllowSameHost: s.cfg.OriginAllowSameHost,` in the `ws.SecurityConfig` literal. `server/config_validate.go`, in `validateProductionSecurity` after the proxy checks: reject `cfg.OriginAllowSameHost && !cfg.TLS.TrustedProxyMode` in production with `origin_allow_same_host in production needs tls.trusted_proxy_mode (the Host header must come from your proxy)`; add a test in `server/config_test.go` using the file's existing `ValidateConfig` pattern (a production config that sets the flag without proxy mode fails; with proxy mode passes).

- [ ] **Step 5: Gate and commit**

Run: `go test -race ./transport/... && cd server && go test -race ./... 2>&1 | tail -5`
Expected: PASS.

```bash
git add transport server
git commit -m "feat(ws): origin_allow_same_host accepts the origin the server itself serves"
```

---

### Task 2: failed-authentication throttle

**Files:**
- Create: `server/auth/throttle.go`, `server/auth/throttle_test.go`
- Modify: `server/server.go` (field, construction, client-IP helper), `server/api_routes.go` (`handleAPI`), `server/metrics.go`, `server/go.mod` (`golang.org/x/time` becomes a direct dependency: `cd server && go mod tidy`)
- Test: `server/api_throttle_test.go`

**Interfaces:**
- Produces:
  - `auth.NewThrottle(burst int, every time.Duration) *Throttle`; `(*Throttle).Blocked(key string) bool`; `(*Throttle).Fail(key string)`; the counters live in the server's metrics (below).
  - `Server.clientIP func(*http.Request) string` (the proxy-aware client address) and `Server.authThrottle *auth.Throttle`.
  - Metrics `voice_management_auth_failures_total` and `voice_management_auth_throttled_total` (`Metrics.ManagementAuthFailuresTotal`, `Metrics.ManagementAuthThrottledTotal`).
  - HTTP 429, error code `too_many_attempts`, header `Retry-After: 10`.

- [ ] **Step 1: Write the failing throttle tests** (`server/auth/throttle_test.go`)

```go
package auth

import (
	"testing"
	"time"
)

func newTestThrottle(t *testing.T) (*Throttle, *time.Time) {
	t.Helper()
	now := time.Unix(1000, 0)
	th := NewThrottle(10, 6*time.Second)
	th.now = func() time.Time { return now }
	return th, &now
}

func TestThrottleBlocksAfterTheAllowanceAndRefills(t *testing.T) {
	th, now := newTestThrottle(t)
	for i := 0; i < 10; i++ {
		if th.Blocked("1.2.3.4") {
			t.Fatalf("blocked after only %d failures", i)
		}
		th.Fail("1.2.3.4")
	}
	if !th.Blocked("1.2.3.4") {
		t.Fatal("10 failures must block")
	}
	if th.Blocked("5.6.7.8") {
		t.Fatal("another address must be unaffected")
	}
	*now = now.Add(6 * time.Second)
	if th.Blocked("1.2.3.4") {
		t.Fatal("one token is back after 6 s")
	}
	th.Fail("1.2.3.4")
	if !th.Blocked("1.2.3.4") {
		t.Fatal("and is used up again")
	}
}

func TestThrottleTableIsBounded(t *testing.T) {
	th, _ := newTestThrottle(t)
	for i := 0; i < maxThrottleEntries+500; i++ {
		th.Fail("10." + itoa(i>>16&255) + "." + itoa(i>>8&255) + "." + itoa(i&255))
	}
	th.mu.Lock()
	n := len(th.limits)
	th.mu.Unlock()
	if n > maxThrottleEntries+1 { // +1: the shared overflow bucket
		t.Fatalf("table grew to %d entries", n)
	}
}

func TestThrottleIdleEntriesAreSwept(t *testing.T) {
	th, now := newTestThrottle(t)
	for i := 0; i < maxThrottleEntries; i++ {
		th.Fail("a" + itoa(i))
	}
	*now = now.Add(2 * time.Minute) // every bucket is full again
	th.Fail("fresh")
	th.mu.Lock()
	_, ok := th.limits["fresh"]
	th.mu.Unlock()
	if !ok {
		t.Fatal("an idle table must make room for a new address instead of sharing the overflow bucket")
	}
}

func itoa(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{digits[i%10]}, b...)
	}
	return string(b)
}
```

- [ ] **Step 2: Run to see failure**

Run: `cd server && go test ./auth 2>&1 | tail -5` — Expected: FAIL (undefined `NewThrottle`).

- [ ] **Step 3: Implement** `server/auth/throttle.go`

```go
package auth

import (
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

// maxThrottleEntries bounds the per-address table.
const maxThrottleEntries = 10000

// overflowKey is the shared bucket new addresses fall into when the table is full of active ones.
const overflowKey = "*"

// Throttle limits failed authentication attempts per client address: each address may fail
// burst times, then one more attempt every `every`. A blocked address is refused before its
// credential is looked at, so the correct key from a blocked address is not an oracle.
// Only addresses that have failed hold an entry.
type Throttle struct {
	burst int
	every time.Duration
	now   func() time.Time // monotonic clock readings come from time.Now

	mu     sync.Mutex
	limits map[string]*rate.Limiter
}

func NewThrottle(burst int, every time.Duration) *Throttle {
	return &Throttle{burst: burst, every: every, now: time.Now, limits: make(map[string]*rate.Limiter)}
}

func (t *Throttle) limiter(key string, now time.Time) *rate.Limiter {
	if l, ok := t.limits[key]; ok {
		return l
	}
	if len(t.limits) >= maxThrottleEntries {
		for k, l := range t.limits { // sweep addresses whose allowance is whole again
			if l.TokensAt(now) >= float64(t.burst) {
				delete(t.limits, k)
			}
		}
		if len(t.limits) >= maxThrottleEntries {
			key = overflowKey // a flood from many addresses shares one bucket
			if l, ok := t.limits[key]; ok {
				return l
			}
		}
	}
	l := rate.NewLimiter(rate.Every(t.every), t.burst)
	t.limits[key] = l
	return l
}

// Blocked reports whether key has used up its failures.
func (t *Throttle) Blocked(key string) bool {
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	l, ok := t.limits[key]
	return ok && l.TokensAt(now) < 1
}

// Fail records one failed attempt by key.
func (t *Throttle) Fail(key string) {
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.limiter(key, now).AllowN(now, 1)
}
```

Run `cd server && go mod tidy` so `golang.org/x/time` moves to the direct requires.

- [ ] **Step 4: Run to see the throttle tests pass**

Run: `cd server && go test -race ./auth -v 2>&1 | tail -10` — Expected: PASS.

- [ ] **Step 5: Write the failing handler tests** (`server/api_throttle_test.go`)

```go
package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func apiRequestFrom(s *Server, remote, xff, method, path, bearer string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(""))
	r.RemoteAddr = remote
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	s.adminMux.ServeHTTP(w, r)
	return w
}

func TestManagementAuthFailuresAreThrottledPerAddress(t *testing.T) {
	s := apiTestServer(t)
	for i := 0; i < 10; i++ {
		if w := apiRequestFrom(s, "203.0.113.9:1000", "", http.MethodGet, "/api/v1/server-config", "wrong-key"); w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i, w.Code)
		}
	}
	w := apiRequestFrom(s, "203.0.113.9:1001", "", http.MethodGet, "/api/v1/server-config", "wrong-key")
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" || !strings.Contains(w.Body.String(), "too_many_attempts") {
		t.Fatalf("11th attempt: %d %s", w.Code, w.Body.String())
	}
	// Even the right key is refused from a blocked address.
	if w := apiRequestFrom(s, "203.0.113.9:1002", "", http.MethodGet, "/api/v1/server-config", testMgmtKey); w.Code != http.StatusTooManyRequests {
		t.Fatalf("right key while blocked: %d", w.Code)
	}
	// Another address is unaffected, and a success never counts.
	for i := 0; i < 20; i++ {
		if w := apiRequestFrom(s, "198.51.100.7:1", "", http.MethodGet, "/api/v1/server-config", testMgmtKey); w.Code != http.StatusOK {
			t.Fatalf("good key from a clean address: %d", w.Code)
		}
	}
	if s.metrics.ManagementAuthFailuresTotal.Load() != 10 || s.metrics.ManagementAuthThrottledTotal.Load() != 2 {
		t.Fatalf("counters: failures=%d throttled=%d", s.metrics.ManagementAuthFailuresTotal.Load(), s.metrics.ManagementAuthThrottledTotal.Load())
	}
}

func TestThrottleIgnoresAnUntrustedForwardedFor(t *testing.T) {
	s := apiTestServer(t) // no trusted proxies: the header must not pick the key
	for i := 0; i < 10; i++ {
		apiRequestFrom(s, "203.0.113.9:1", "10.0.0."+string(rune('1'+i)), http.MethodGet, "/api/v1/server-config", "wrong")
	}
	if w := apiRequestFrom(s, "203.0.113.9:1", "10.9.9.9", http.MethodGet, "/api/v1/server-config", "wrong"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("a client must not dodge the throttle by rotating X-Forwarded-For: %d", w.Code)
	}
}

func TestAPIResponsesAreNotCached(t *testing.T) {
	s := apiTestServer(t)
	if w := apiRequest(s, http.MethodGet, "/api/v1/server-config", testMgmtKey, ""); w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", w.Header().Get("Cache-Control"))
	}
}

func TestThrottleCountersAreExported(t *testing.T) {
	s := apiTestServer(t)
	apiRequest(s, http.MethodGet, "/api/v1/server-config", "wrong", "")
	w := httptest.NewRecorder()
	s.handleMetrics(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(w.Body.String(), "voice_management_auth_failures_total 1") {
		t.Fatalf("metrics = %s", w.Body.String())
	}
}
```

Run: `cd server && go test -run 'TestManagementAuthFailures|TestThrottleIgnores|TestAPIResponsesAreNotCached|TestThrottleCounters' ./ 2>&1 | tail` — Expected: FAIL (`s.authThrottle` undefined).

- [ ] **Step 6: Wire it**

`server/server.go`: add to `Server`: `authThrottle *auth.Throttle` and `clientIP func(*http.Request) string`. In `NewServer`, after the struct literal: `s.authThrottle = auth.NewThrottle(10, 6*time.Second)`. In `setupRoutes`, after building `wsCfg`: 

```go
	sec := wsCfg.Security
	s.clientIP = func(r *http.Request) string {
		if ip := sec.ClientIP(r); ip != "" {
			return ip
		}
		return "unknown" // an unparseable peer shares one bucket instead of skipping the throttle
	}
```

`server/api_routes.go` `handleAPI`: at the top after setting `X-Request-ID` add `w.Header().Set("Cache-Control", "no-store")`; replace the authentication block with:

```go
	ip := s.clientIP(r)
	if s.authThrottle.Blocked(ip) {
		w.Header().Set("Retry-After", "10")
		api.WriteError(w, http.StatusTooManyRequests, "too_many_attempts", "too many failed attempts; try again shortly", requestID)
		return
	}
	if !s.management.Authenticate(r) {
		s.authThrottle.Fail(ip)
		w.Header().Set("WWW-Authenticate", `Bearer realm="management"`)
		api.WriteError(w, http.StatusUnauthorized, "unauthorized", "valid management credential required", requestID)
		return
	}
```

`server/metrics.go`: add two counters `ManagementAuthFailuresTotal, ManagementAuthThrottledTotal atomic.Uint64` to `Metrics` (beside `MaxDurationTermTotal`), write them next to `voice_max_duration_terminations_total` (same `# HELP`, `# TYPE ... counter`, value format), and increment them in `handleAPI`: `s.metrics.ManagementAuthFailuresTotal.Add(1)` next to `Fail`, `s.metrics.ManagementAuthThrottledTotal.Add(1)` next to the 429.

- [ ] **Step 7: Gate and commit**

Run: `cd server && go test -race ./... 2>&1 | tail -10` — Expected: PASS (existing API tests send fewer than 10 bad keys per server; if one trips the limit, give that test its own server rather than raising the limit).

```bash
git add server
git commit -m "feat(server): throttle failed management authentication per client address"
```

---

### Task 3: `public_admin` — the management API on the public port

**Files:**
- Modify: `server/config.go`, `server/config_validate.go`, `server/server.go` (`NewServer`, `setupRoutes`), `server/api_routes.go` (`server-config`), `server/api/api.go` (`ServerConfig`)
- Test: `server/public_admin_test.go`, `server/config_test.go`

**Interfaces:**
- Consumes: `Server.clientIP` (Task 2).
- Produces: config `public_admin: {enabled: bool}`; `api.ServerConfig` gains `ClientIP string `json:"client_ip"`` and `Secure bool `json:"secure"`` (the address and TLS state the server sees for this request, used by plan 3's proxy-trust check); on the public mux `/api/v1` and `/api/v1/` served by `handleAPI`.

- [ ] **Step 1: Write the failing tests** (`server/public_admin_test.go`)

```go
package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func publicAdminServer(t *testing.T, mods ...func(*Config)) *Server {
	t.Helper()
	t.Setenv("GOVOICE_MANAGEMENT_API_KEY", testMgmtKey)
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Token.Secret = "voice-test-secret-32-bytes-long-and-separate"
	cfg.PublicAdmin.Enabled = true
	for _, m := range mods {
		m(cfg)
	}
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.env = testProviderEnv(t)
	return s
}

func publicRequest(s *Server, method, path, bearer string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	s.publicMux.ServeHTTP(w, r)
	return w
}

func TestPublicAdminServesTheManagementAPIBehindTheKey(t *testing.T) {
	s := publicAdminServer(t)
	if w := publicRequest(s, http.MethodGet, "/api/v1/server-config", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("no key: %d", w.Code)
	}
	w := publicRequest(s, http.MethodGet, "/api/v1/server-config", testMgmtKey)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"client_ip"`) {
		t.Fatalf("with key: %d %s", w.Code, w.Body.String())
	}
}

func TestPublicAdminDoesNotExposeTheOperationalSurfaces(t *testing.T) {
	s := publicAdminServer(t)
	for _, p := range []string{"/metrics", "/ready", "/debug/timeline"} {
		if w := publicRequest(s, http.MethodGet, p, testMgmtKey); w.Code == http.StatusOK {
			t.Errorf("%s must not be served on the public port", p)
		}
	}
	if w := publicRequest(s, http.MethodGet, "/health", ""); w.Code != http.StatusOK {
		t.Fatalf("/health: %d", w.Code)
	}
}

func TestAPIIsNotOnThePublicPortByDefault(t *testing.T) {
	s := apiTestServer(t)
	if w := publicRequest(s, http.MethodGet, "/api/v1/server-config", testMgmtKey); w.Code == http.StatusOK {
		t.Fatal("the management API must stay on the admin listener unless public_admin is on")
	}
}

func TestPublicAdminNeedsAManagementKey(t *testing.T) {
	t.Setenv("GOVOICE_MANAGEMENT_API_KEY", "")
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Token.Secret = "voice-test-secret-32-bytes-long-and-separate"
	cfg.PublicAdmin.Enabled = true
	if _, err := NewServer(cfg); err == nil {
		t.Fatal("public_admin without a management key must refuse to start")
	}
}

func TestServerConfigReportsTheClientAddressTheServerSees(t *testing.T) {
	s := publicAdminServer(t, func(c *Config) {
		c.TLS.TrustedProxyMode = true
		c.TLS.TrustedProxies = []string{"169.254.0.0/16"}
	})
	r := httptest.NewRequest(http.MethodGet, "/api/v1/server-config", nil)
	r.RemoteAddr = "169.254.8.129:5555"
	r.Header.Set("X-Forwarded-For", "9.9.9.9, 203.0.113.50") // the first entry is client-supplied; the proxy appended the real one
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("Authorization", "Bearer "+testMgmtKey)
	w := httptest.NewRecorder()
	s.publicMux.ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), `"client_ip":"203.0.113.50"`) || !strings.Contains(w.Body.String(), `"secure":true`) {
		t.Fatalf("%s", w.Body.String())
	}
}
```

Production validation test in `config_test.go` (mirror the file's production-config helper): a production config with `PublicAdmin.Enabled` and `TLS.TrustedProxyMode=false` fails `ValidateConfig` with a message containing `public_admin`; with trusted proxy mode and proxies it passes.

- [ ] **Step 2: Run to see failure**

Run: `cd server && go test -run 'TestPublicAdmin|TestAPIIsNotOnThePublicPort|TestServerConfigReports' ./ 2>&1 | tail -5` — Expected: build error (`cfg.PublicAdmin` undefined).

- [ ] **Step 3: Implement**

`server/config.go`:

```go
	// PublicAdmin serves the management API (/api/v1) and the admin UI on the public listener,
	// behind the management key, for single-port deployments (Cloud Run). Off by default: the
	// API normally lives on the loopback admin listener. /metrics, /ready and /debug never move.
	PublicAdmin struct {
		Enabled bool `yaml:"enabled"`
	} `yaml:"public_admin"`
```

`server/config_validate.go`, in `validateProductionSecurity` after the proxy checks:

```go
	if cfg.PublicAdmin.Enabled && !cfg.TLS.TrustedProxyMode {
		return errors.New("production public_admin needs tls.trusted_proxy_mode: the failed-login throttle and the rate limits key on the proxy-trusted client address")
	}
```

`server/server.go` `NewServer`, right after the management key check: 

```go
	if cfg.PublicAdmin.Enabled && !management.Enabled() {
		return nil, fmt.Errorf("public_admin.enabled needs the management API key in %s", managementEnv)
	}
```

`setupRoutes`, after `s.publicMux.HandleFunc("GET /ws", ...)`:

```go
	if s.cfg.PublicAdmin.Enabled {
		s.logger.Warn("public_admin is on: the management API is served on the public port behind the management key")
		s.publicMux.HandleFunc("/api/v1", s.handleAPI)
		s.publicMux.HandleFunc("/api/v1/", s.handleAPI)
	}
```

(The UI is mounted in Task 4.) `server/api/api.go`: add `ClientIP string `json:"client_ip"`` and `Secure bool `json:"secure"`` to `ServerConfig`; in `api_routes.go` fill them in the `server-config` case: `ClientIP: s.clientIP(r), Secure: s.secure(r)` with, in `server.go`, `s.secure = sec.IsSecure` assigned beside `s.clientIP` (add the field `secure func(*http.Request) bool`).

- [ ] **Step 4: Gate and commit**

Run: `cd server && go test -race ./... 2>&1 | tail -10` — Expected: PASS.

```bash
git add server
git commit -m "feat(server): public_admin serves the management API on the public port"
```

---

### Task 4: the embedded admin UI

**Files:**
- Create: `server/adminui/adminui.go`, `server/adminui/adminui_test.go`, `server/adminui/static/.gitkeep`
- Modify: `server/server.go` (`setupRoutes` mounts the UI), `.gitignore`
- Test: `server/adminui/adminui_test.go`, `server/public_admin_test.go`

**Interfaces:**
- Produces: `adminui.Embedded() fs.FS` (the build output, or just the placeholder), `adminui.Handler(fsys fs.FS) http.Handler`, `adminui.ErrNotBuilt`-free API: `adminui.Built(fsys fs.FS) bool`.

- [ ] **Step 1: Write the failing tests** (`server/adminui/adminui_test.go`)

```go
package adminui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

var site = fstest.MapFS{
	"index.html":        {Data: []byte(`<!doctype html><title>Switchboard</title><script type="module" src="/assets/app-abc.js"></script>`)},
	"assets/app-abc.js": {Data: []byte(`console.log(1)`)},
	"favicon.svg":       {Data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)},
}

func get(h http.Handler, method, target string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	r.Host = "voice.run.app"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestServesIndexAndAssets(t *testing.T) {
	h := Handler(site)
	if w := get(h, "GET", "/"); w.Code != 200 || !strings.Contains(w.Body.String(), "Switchboard") || w.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("/: %d %q cache=%q", w.Code, w.Body.String(), w.Header().Get("Cache-Control"))
	}
	w := get(h, "GET", "/assets/app-abc.js")
	if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), "javascript") || !strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("asset: %d type=%q cache=%q", w.Code, w.Header().Get("Content-Type"), w.Header().Get("Cache-Control"))
	}
}

func TestClientRoutesFallBackToIndexButMissingFilesDoNot(t *testing.T) {
	h := Handler(site)
	for _, p := range []string{"/flows", "/flows/abc/edit", "/calls/42"} {
		if w := get(h, "GET", p); w.Code != 200 || !strings.Contains(w.Body.String(), "Switchboard") {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
	for _, p := range []string{"/assets/missing.js", "/robots.txt", "/x.map"} {
		if w := get(h, "GET", p); w.Code != 404 {
			t.Errorf("%s must be a real 404 (a stale deploy must show), got %d", p, w.Code)
		}
	}
}

func TestOnlyReadsAreServed(t *testing.T) {
	if w := get(Handler(site), "POST", "/"); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /: %d", w.Code)
	}
}

func TestSecurityHeaders(t *testing.T) {
	w := get(Handler(site), "GET", "/")
	csp := w.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'self'", "script-src 'self'", "connect-src 'self' wss://voice.run.app", "frame-ancestors 'none'", "base-uri 'none'", "worker-src 'self' blob:"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP lacks %q: %s", want, csp)
		}
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("headers: %v", w.Header())
	}
	// A hostile Host header cannot inject policy.
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "evil.example; script-src *"
	rec := httptest.NewRecorder()
	Handler(site).ServeHTTP(rec, r)
	if strings.Contains(rec.Header().Get("Content-Security-Policy"), "script-src *") {
		t.Fatalf("CSP built from an unvalidated Host: %s", rec.Header().Get("Content-Security-Policy"))
	}
}

func TestAnUnbuiltUIsaysSo(t *testing.T) {
	empty := fstest.MapFS{".gitkeep": {}}
	if Built(empty) {
		t.Fatal("a directory without index.html is not a built UI")
	}
	w := get(Handler(empty), "GET", "/")
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "not built") {
		t.Fatalf("%d %q", w.Code, w.Body.String())
	}
}
```

- [ ] **Step 2: Run to see failure**

Run: `cd server && go test ./adminui 2>&1 | tail -5` — Expected: FAIL (package has no Go files / undefined).

- [ ] **Step 3: Implement** `server/adminui/adminui.go`

```go
// Package adminui serves the admin UI that is built into the binary. The build output goes in
// ./static (make ui-embed, or the Dockerfile's UI stage); a plain `go build` embeds only the
// placeholder, and the handler then says the UI is not built.
package adminui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strings"
)

//go:embed all:static
var embedded embed.FS

// Embedded is the build output directory.
func Embedded() fs.FS {
	sub, err := fs.Sub(embedded, "static")
	if err != nil { // unreachable: "static" is embedded above
		return embedded
	}
	return sub
}

// Built reports whether fsys holds a built UI.
func Built(fsys fs.FS) bool {
	_, err := fs.Stat(fsys, "index.html")
	return err == nil
}

// hostRe is what a Host header may be to appear in the policy: a DNS name or IP with an optional port.
var hostRe = regexp.MustCompile(`^[A-Za-z0-9.\-\[\]:]+$`)

func csp(host string) string {
	connect := "'self'"
	if hostRe.MatchString(host) {
		connect += " wss://" + host + " ws://" + host // the voice socket; ws: only matters for local rehearsal
	}
	return "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; " +
		"media-src 'self' blob:; font-src 'self'; connect-src " + connect + "; worker-src 'self' blob:; " +
		"frame-ancestors 'none'; base-uri 'none'; form-action 'self'"
}

// Handler serves fsys: files as they are, an unknown path without a file extension as index.html
// (client-side routes), and an unknown path with an extension as 404 (a stale deploy must show).
func Handler(fsys fs.FS) http.Handler {
	files := http.FileServerFS(fsys)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !Built(fsys) {
			http.Error(w, "the admin UI is not built into this binary (make ui-embed)", http.StatusNotFound)
			return
		}
		h := w.Header()
		h.Set("Content-Security-Policy", csp(r.Host))
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name == "" || name == "index.html" {
			h.Set("Cache-Control", "no-cache")
			r.URL.Path = "/"
			files.ServeHTTP(w, r)
			return
		}
		if info, err := fs.Stat(fsys, name); err == nil && !info.IsDir() {
			if strings.HasPrefix(name, "assets/") { // content-hashed by the build
				h.Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				h.Set("Cache-Control", "no-cache")
			}
			files.ServeHTTP(w, r)
			return
		}
		if path.Ext(name) != "" {
			http.NotFound(w, r)
			return
		}
		h.Set("Cache-Control", "no-cache")
		r.URL.Path = "/"
		files.ServeHTTP(w, r)
	})
}
```

Create `server/adminui/static/.gitkeep` (empty). `.gitignore` additions:

```
# The built admin UI is embedded from here (make ui-embed); only the placeholder is committed.
server/adminui/static/*
!server/adminui/static/.gitkeep
```

Note the repository-wide `dist/` ignore does not matter: the directory is named `static`.

- [ ] **Step 4: Mount it** in `setupRoutes` inside the `if s.cfg.PublicAdmin.Enabled` block (Task 3). The UI is a package variable so tests can stand in a built UI:

```go
// adminUIFS is the admin UI served with public_admin (the embedded build; tests replace it).
var adminUIFS = adminui.Embedded()
```

```go
		// The operational surfaces and unknown API paths are explicit 404s: otherwise the UI's
		// client-route fallback would answer them with index.html and a 200.
		for _, p := range []string{"/metrics", "/ready", "/debug/", "/api/"} {
			s.publicMux.HandleFunc(p, http.NotFound)
		}
		s.publicMux.Handle("/", adminui.Handler(adminUIFS)) // the catch-all (no method: "GET /" would conflict with the method-less /api/ patterns; the handler refuses non-reads itself). /ws, /health and /api/v1 are more specific and win
		if !adminui.Built(adminUIFS) {
			s.logger.Warn("public_admin is on but this binary has no admin UI (run make ui-embed before building)")
		}
```

Add to `public_admin_test.go`:

```go
func TestUIIsMountedWithoutShadowingTheAPIOrTheSocket(t *testing.T) {
	s := publicAdminServer(t)
	// This test binary embeds only the placeholder: "/" says so, and the real routes still win.
	if w := publicRequest(s, http.MethodGet, "/", ""); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "not built") {
		t.Fatalf("/: %d %q", w.Code, w.Body.String())
	}
	if w := publicRequest(s, http.MethodGet, "/api/v1/server-config", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("/api/v1 must reach the management handler, got %d", w.Code)
	}
	if w := publicRequest(s, http.MethodGet, "/ws", ""); w.Code == http.StatusNotFound {
		t.Fatalf("/ws must reach the socket handler, got %d", w.Code)
	}
	if w := publicRequest(s, http.MethodGet, "/health", ""); w.Code != http.StatusOK {
		t.Fatalf("/health: %d", w.Code)
	}
}
```

Add a second test with a built UI standing in (add `"testing/fstest"` to the test file's imports):

```go
func TestAFallingBackUIDoesNotAnswerForServerPaths(t *testing.T) {
	prev := adminUIFS
	adminUIFS = fstest.MapFS{"index.html": {Data: []byte(`<div id="root"></div>`)}}
	t.Cleanup(func() { adminUIFS = prev })
	s := publicAdminServer(t)
	for _, p := range []string{"/metrics", "/ready", "/debug/timeline", "/api/other"} {
		if w := publicRequest(s, http.MethodGet, p, testMgmtKey); w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), `id="root"`) {
			t.Errorf("%s: %d %q (must be a plain 404, not the UI)", p, w.Code, w.Body.String())
		}
	}
	if w := publicRequest(s, http.MethodGet, "/flows/abc", ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `id="root"`) {
		t.Fatalf("a client route must get the UI: %d", w.Code)
	}
	if w := publicRequest(s, http.MethodGet, "/assets/gone.js", ""); w.Code != http.StatusNotFound {
		t.Fatalf("a missing asset must 404: %d", w.Code)
	}
	if w := publicRequest(s, http.MethodGet, "/api/v1/server-config", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("/api/v1 must still reach the management handler: %d", w.Code)
	}
}
```

- [ ] **Step 5: Gate and commit**

Run: `cd server && go test -race ./... 2>&1 | tail -10; cd .. && go build ./... && gofmt -l server transport` — Expected: PASS, a plain build works with only the placeholder embedded, no gofmt output.

```bash
git add server .gitignore
git commit -m "feat(server): embed and serve the admin UI on the public port"
```

---

### Task 5: build integration, the Cloud Run profile and the image

**Files:**
- Create: `config/cloudrun.yaml`
- Modify: `Makefile` (target `ui-embed`, `build` depends on nothing new), `deploy/Dockerfile`, `server/deploy_contract_test.go`, `server/config_test.go`
- Test: `server/cloudrun_config_test.go`

**Interfaces:**
- Consumes: `public_admin`, `origin_allow_same_host` (Tasks 1, 3), `adminui` static directory (Task 4).
- Produces: `config/cloudrun.yaml` (loaded by the image at `/etc/govoice/cloudrun.yaml`), `make ui-embed`.

- [ ] **Step 1: Write the failing config test** (`server/cloudrun_config_test.go`)

```go
package server

import "testing"

func TestCloudRunConfigLoadsInProduction(t *testing.T) {
	t.Setenv("TOKEN_SECRET", "cloudrun-test-token-secret-0123456789abcdef")
	t.Setenv("GOVOICE_DATABASE_DSN", t.TempDir()+"/govoice.db")
	cfg, err := LoadConfig("../config/cloudrun.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Profile != ProfileProduction || cfg.Storage.Dialect != "sqlite" {
		t.Fatalf("profile=%s dialect=%s", cfg.Profile, cfg.Storage.Dialect)
	}
	if !cfg.PublicAdmin.Enabled || !cfg.OriginAllowSameHost || !cfg.TLS.TrustedProxyMode || len(cfg.TLS.TrustedProxies) == 0 {
		t.Fatalf("one-port settings missing: %+v", cfg.PublicAdmin)
	}
	if cfg.PublicListenAddr != "0.0.0.0:8080" || cfg.ProbeListenAddr != "" || cfg.AdminListenAddr != "127.0.0.1:9090" {
		t.Fatalf("listeners: public=%s admin=%s probe=%q", cfg.PublicListenAddr, cfg.AdminListenAddr, cfg.ProbeListenAddr)
	}
	if cfg.MCP.AllowLocalHTTP || cfg.Recording.Enabled {
		t.Fatal("a public demo has no loopback MCP and no recording")
	}
	if cfg.Limits.MaxSessions > 20 || cfg.Limits.MaxCallDuration > 30*60*1e9 {
		t.Fatalf("a one-instance demo is bounded: sessions=%d duration=%v", cfg.Limits.MaxSessions, cfg.Limits.MaxCallDuration)
	}
}
```

Also in `server/deploy_contract_test.go`:

```go
func TestImageShipsTheCloudRunConfigAndBuildsTheUI(t *testing.T) {
	body, err := os.ReadFile("../deploy/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"AS ui", "npm ci", "npm run build", "server/adminui/static", "/etc/govoice/cloudrun.yaml"} {
		if !strings.Contains(string(body), needle) {
			t.Fatalf("Dockerfile missing %q", needle)
		}
	}
}
```

Run: `cd server && go test -run 'TestCloudRunConfig|TestImageShips' ./ 2>&1 | tail -5` — Expected: FAIL (no such file / missing needles).

- [ ] **Step 2: Write `config/cloudrun.yaml`**

```yaml
# Cloud Run: ONE service, ONE public port (8080). The management API and the admin UI are served
# on it behind the management key (public_admin). One instance, SQLite on the instance's memory
# disk: the demo is rebuilt by demo.auto_install on every start. See docs/deploy/cloudrun.md.
profile: production

public_listen_addr: "0.0.0.0:8080"
admin_listen_addr: "127.0.0.1:9090"   # unused by the demo; required by the config checks
probe_listen_addr: ""

public_admin:
  enabled: true

tls:
  cert_file: ""
  key_file: ""
  trusted_proxy_mode: true
  # Cloud Run's front end is the only peer the container ever sees. Its addresses are not documented;
  # 169.254.0.0/16 is what the first deploy shows, and the docker bridge (172.16.0.0/12) lets the
  # local rehearsal (deploy/rehearsal) use the same file. plan 3 verifies the real peer against
  # GET /api/v1/server-config (client_ip) on every deploy.
  trusted_proxies:
    - "169.254.0.0/16"
    - "172.16.0.0/12"
    - "127.0.0.1/32"
    - "::1/128"

# The page is served by this service, so the browser's own origin is allowed without listing it.
origin_allow_same_host: true
origin_allowlist: []

token:
  secret_env: "TOKEN_SECRET"
  issuer: "govoice-v1"
  audience: "govoice-v1-client"
  max_lifetime: 5m
  clock_skew: 30s
  replay_cache_limit: 50000
  key_id: "v1"

limits:
  max_pending_handshakes: 20
  max_sessions: 10
  max_sessions_per_principal: 5
  max_sessions_per_ip: 5
  upgrade_timeout: 5s
  ready_timeout: 10s
  max_call_duration: 20m
  idle_timeout: 30s
  ping_interval: 15s
  ping_timeout: 10s
  drain_timeout: 45s
  immediate_cancel: false

vad:
  mode: "process"
  workers: 2
  batch: 8
  model_path: "/opt/govoice/models/silero_vad.onnx"
  ort_lib_path: "/opt/govoice/lib/libonnxruntime.so"
  queue_cap: 2048
  stop_ms: 200

providers:
  google_stt:
    project_id: ""
    location: "us-central1"
    rotation_threshold: 240s
  google_tts:
    sample_rate: 24000
  xai_llm:
    endpoint: "https://api.x.ai/v1/responses"

observability:
  log_level: "info"
  json_format: true
  timeline_max_events: 1000
  timeline_ttl: 15m

# GOVOICE_DATABASE_DSN is the file path, e.g. /tmp/govoice.db (Cloud Run's writable in-memory disk).
storage:
  dialect: "sqlite"

retention:
  enabled: true
  interval: 1h
  content_retention_days: 1
  metadata_retention_days: 7
  batch_size: 200

warmup:
  enabled: true
  predial: false
  on_start: false

idle: {user_timeout: 30s, max_prompts: 2}

# The dentistry demo installs itself on every start (needs DENTAL_MCP_URL and MCP_TOKEN_DENTAL).
# Delete this block to make it a button in the admin UI only. Added by plan 1; keep in sync.
demo:
  auto_install: "dental"
```

If plan 1 has not landed when this file is created, omit the `demo:` block (the strict YAML decoder rejects unknown keys) and add it with plan 1's Task 6; the test above does not assert it. Compare every key against `config/production.yaml` and `server/config.go`: a key the decoder does not know fails `LoadConfig`.

- [ ] **Step 3: Run the config test**

Run: `cd server && go test -run TestCloudRunConfig ./ -v 2>&1 | tail -8` — Expected: PASS. If a production validation rejects a value (for example `origin_allowlist: []` or the VAD worker count), change the config to satisfy the rule, never the rule.

- [ ] **Step 4: Dockerfile.** Add a UI stage before the Go builder and copy the build in before `go build`:

```dockerfile
# Admin UI (embedded into the server binary): Node is a build tool only and is not in the runtime image.
FROM node:22-bookworm-slim@sha256:<digest of node:22-bookworm-slim at build time> AS ui
WORKDIR /ui
COPY ui/package.json ui/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY ui/ ./
RUN npm run build
```

Resolve the digest with `docker buildx imagetools inspect node:22-bookworm-slim --format '{{json .Manifest.Digest}}'` and pin it (the existing stages are digest-pinned; keep the convention). In the builder stage after `COPY . .` add `COPY --from=ui /ui/dist/ /src/server/adminui/static/`. In the runtime stage add `COPY --from=builder /src/config/cloudrun.yaml /etc/govoice/cloudrun.yaml` and add `/etc/govoice/cloudrun.yaml` to the `chmod 644` list. Default `CMD` is unchanged (production.yaml). Check that `ui/` has no `.dockerignore` rule hiding `package-lock.json`.

- [ ] **Step 5: Makefile**

```make
ui-embed: ## build|Build the admin UI into server/adminui/static (so bin/server serves it with public_admin)
	cd ui && npm ci --no-audit --no-fund && npm run build
	find server/adminui/static -mindepth 1 ! -name .gitkeep -delete
	cp -R ui/dist/. server/adminui/static/
.PHONY: ui-embed
```

- [ ] **Step 6: Build the image and prove it boots** 

Run: `docker build -f deploy/Dockerfile -t govoice:onport .` (needs the third_party models; if `make ort`/model files are absent follow the Makefile's `image` target), then

```bash
docker run --rm -d --name govoice-oneport -p 18080:8080 \
  -e TOKEN_SECRET=$(openssl rand -hex 32) -e GOVOICE_MANAGEMENT_API_KEY=$(openssl rand -hex 32) \
  -e GOVOICE_DATABASE_DSN=/tmp/govoice.db govoice:onport \
  /opt/govoice/bin/server -config /etc/govoice/cloudrun.yaml
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:18080/health
curl -s http://127.0.0.1:18080/ | head -c 200
docker stop govoice-oneport
```

Expected: `200` for `/health`; `/` returns the UI's `index.html`; no `panic` in `docker logs`.

- [ ] **Step 7: Gate and commit**

Run: `cd server && go test -race ./... 2>&1 | tail -5` — Expected: PASS.

```bash
git add config/cloudrun.yaml deploy/Dockerfile Makefile server
git commit -m "build: Cloud Run profile, UI stage in the image and make ui-embed"
```

---

### Task 6: browser end-to-end of the one-port server

**Files:**
- Create: `e2e/playwright.oneport.config.ts`, `e2e/global-setup-oneport.ts`, `e2e/tests-oneport/10-oneport.spec.ts`
- Modify: `e2e/lib/stack.ts` (`oneportStackFile`), `e2e/global-teardown.ts` only if it needs the new stack file name (read it), `Makefile` (`e2e-oneport`, run by `e2e-fake`), `e2e/README.md`, `e2e/package.json` only if scripts are listed there

**Interfaces:**
- Consumes: the fake-provider binary built with the embedded UI (`make ui-embed` first), `public_admin`, `origin_allow_same_host`, the throttle.

- [ ] **Step 1: Global setup.** Copy `global-setup-flows.ts` into `global-setup-oneport.ts` and reduce it to: build the UI into the binary (`make ui-embed` is run by the Makefile target before Playwright; the setup asserts `server/adminui/static/index.html` exists and fails with that hint if not), build `bin/server-fake` (`-tags "xaispeech fakeproviders"`), write a config from `config/development.yaml` with `public_listen_addr` on a new port (18582) and these lines appended:

```yaml
public_admin:
  enabled: true
origin_allow_same_host: true
```

and `origin_allowlist` replaced by a single unrelated origin (`https://not-this-site.example`) so only the same-host rule can admit the browser. Do not start Vite, `mcptest` or a second listener check (the admin listener still binds loopback: give it 19592). The fake providers' placeholder credentials are the same as the flows suite. Record the stack in `.stack-oneport.json` through a new `oneportStackFile` export in `lib/stack.ts`; `uiUrl` equals `publicUrl`. Reuse `stopStack` for teardown (pass the file name; add a `global-teardown-oneport.ts` one-liner like `global-teardown-flows.ts`).

- [ ] **Step 2: Config** `playwright.oneport.config.ts`: copy `playwright.flows.config.ts` (fake only), set `testDir: "tests-oneport"`, `outputDir: "test-results-oneport"`, the new setup and teardown files, and a distinct HTML report folder.

- [ ] **Step 3: Write the spec** `tests-oneport/10-oneport.spec.ts` (use a `test` fixture like `lib/flows-stack.ts` that reads `oneportStackFile`; reuse `lib/flows-helpers.ts` helpers: `connectAdmin`, `openDesigner`, the Test-call panel helpers; if they hard-code the flows stack file, parameterise them by passing the `stack`, which they already take)

```ts
test("the UI, the API and the socket all work from the one public origin", async ({ page, stack }) => {
  const cspViolations: string[] = [];
  page.on("console", (m) => { if (/Content Security Policy|Refused to/.test(m.text())) cspViolations.push(m.text()); });

  await page.goto(stack.publicUrl);                       // the embedded UI, not Vite
  await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
  await page.goto(`${stack.publicUrl}/flows`);            // a client route survives a hard load (SPA fallback)
  await expect(page).toHaveURL(/\/flows$/);

  await connectAdmin(page, stack);                         // enters the management key; the API is same-origin
  // create a one-step fake flow from the designer helper, then a Test call with one text turn:
  // the socket is same-origin and admitted only by origin_allow_same_host
  // ... (use the helpers exactly as 20-flow-lifecycle.spec.ts does for its first call)
  expect(cspViolations).toEqual([]);
});

test("the management and operational surfaces are as exposed as intended", async ({ request, stack }) => {
  expect((await request.get(`${stack.publicUrl}/health`)).status()).toBe(200);
  for (const p of ["/metrics", "/ready", "/debug/timeline"]) expect((await request.get(`${stack.publicUrl}${p}`)).status(), p).not.toBe(200);
  const noKey = await request.get(`${stack.publicUrl}/api/v1/server-config`);
  expect(noKey.status()).toBe(401);
  const withKey = await request.get(`${stack.publicUrl}/api/v1/server-config`, { headers: { Authorization: `Bearer ${stack.managementKey}` } });
  expect(withKey.status()).toBe(200);
  expect(withKey.headers()["cache-control"]).toBe("no-store");
  expect(((await withKey.json()) as { data: { client_ip: string } }).data.client_ip).toMatch(/^(127\.0\.0\.1|::1)$/);
  expect((await request.get(`${stack.publicUrl}/assets/does-not-exist.js`)).status()).toBe(404);
});

test("repeated wrong keys are throttled and the page says so", async ({ page, request, stack }) => {
  // Run last in the file: it uses up this client address's allowance.
  for (let i = 0; i < 10; i++)
    expect((await request.get(`${stack.publicUrl}/api/v1/server-config`, { headers: { Authorization: "Bearer wrong" } })).status()).toBe(401);
  expect((await request.get(`${stack.publicUrl}/api/v1/server-config`, { headers: { Authorization: "Bearer wrong" } })).status()).toBe(429);
  await page.goto(stack.publicUrl);
  // enter the real key: while blocked the UI must show the server's message, not an empty page
  // (read how the key form reports an error in ui/src/app and assert that text)
});
```

Fill the three bracketed passages by reading `20-flow-lifecycle.spec.ts` (how it creates a flow and opens the Test call panel) and `ui/src/app` (how a rejected key is shown); do not invent helper names. If the 429 case shows the UI renders a raw error, fix it in the UI with a test (the message from the API is `too many failed attempts; try again shortly`).

- [ ] **Step 4: Makefile and CI.** Add

```make
e2e-oneport: ui-embed ## free|Playwright e2e of the one-port server (embedded UI, same-origin API and socket) on fake providers
	cd e2e && npm ci --no-audit --no-fund && npm run typecheck && npx playwright install chromium && npx playwright test -c playwright.oneport.config.ts
.PHONY: e2e-oneport
```

and append `e2e-oneport` to the `e2e-fake` recipe's last line (so the required `graph-e2e` check covers it) or add it as a dependency of `e2e-free`; choose the first, because CI runs `make e2e-fake`. Add `e2e/tsconfig.json` coverage for the new folders if `include` lists directories.

- [ ] **Step 5: Run it**

Run: `make e2e-oneport`
Expected: all three tests pass; `cspViolations` is empty. If the CSP blocks the voice client (an inline worklet, a blob URL, a font), widen the narrowest directive in `adminui.csp` and add the case to `TestSecurityHeaders`; never switch the policy off.

- [ ] **Step 6: Docs and commit**

Add a section "Public admin mode (single port)" to `docs/operations_runbook.md` (what it is, that it is off by default, the production requirements, the throttle, why `/metrics` is not on it) and the `public_admin` / `origin_allow_same_host` keys wherever the config reference lists keys (run `cd server && go test -run Docs ./` and satisfy the docs contract). Add the spec row to `e2e/README.md`.

```bash
git add e2e Makefile docs
git commit -m "test(e2e): the one-port server end to end"
```

- [ ] **Step 7: Final gate and review**

Run: `gofmt -l . ; make verify ; make e2e-free`
Expected: clean. Then dispatch the independent reviewer (AGENTS.md) with the Review Focus list; fix Critical/Important findings test-first.
